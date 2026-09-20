package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// RemoteClient speaks the internal contract to vizra-search.
//
// It never returns `not_indexed` as an error: that is a successful answer the
// Service turns into a SQL fallback with health ok. Anything else — connection
// refused, timeout, 5xx, a rejected signature, an unparseable body — is an
// error, and the Service turns it into a SQL fallback with health DEGRADED.
type RemoteClient struct {
	baseURL string
	key     []byte
	http    *http.Client

	// faulted records the outcome of the most recent call, so readiness can say
	// `search: degraded` without making a probe request of its own on every
	// /readyz. It is set on every call, so a service that recovers reports ok
	// again without a restart.
	faulted  atomic.Bool
	everUsed atomic.Bool
}

// NewRemote builds the client. timeout bounds every call; the caller's context
// deadline still applies and whichever is shorter wins.
func NewRemote(baseURL string, key []byte, timeout time.Duration) *RemoteClient {
	return &RemoteClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		key:     key,
		http: &http.Client{
			Timeout: timeout,
			// NEVER follow a redirect.
			//
			// Go's stdlib strips Authorization, WWW-Authenticate and Cookie on a
			// cross-host redirect. It knows nothing about X-Vizra-Signature,
			// X-Vizra-Timestamp or X-Vizra-Nonce, so those would be forwarded to
			// whatever host the redirect names — handing a valid signature to a
			// third party, and turning core into a server-side fetch whose
			// destination an attacker chooses, loopback and link-local included.
			//
			// The whole point of HMAC-signing this hop is that vizra-search is a
			// SEPARATE trust domain; if it were fully trusted the signature would
			// be pointless. The internal contract is a fixed set of endpoints on
			// a configured base URL, so a redirect is never a legitimate answer
			// and refusing to follow it is both the secure and the correct
			// behaviour. The 3xx then falls through to the non-200 branch, the
			// Service falls back to SQL, and readiness reports degraded.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

var _ Searcher = (*RemoteClient)(nil)

func (c *RemoteClient) Search(ctx context.Context, req SearchRequest) (SearchResult, error) {
	var out SearchResult
	err := c.call(ctx, "/internal/v1/search", req, &out)
	if err != nil {
		return SearchResult{}, err
	}
	if out.Results == nil {
		out.Results = []Hit{}
	}
	return out, nil
}

func (c *RemoteClient) Suggest(ctx context.Context, req SuggestRequest) (SuggestResult, error) {
	var out SuggestResult
	err := c.call(ctx, "/internal/v1/suggestions", req, &out)
	if err != nil {
		return SuggestResult{}, err
	}
	if out.Suggestions == nil {
		out.Suggestions = []Suggestion{}
	}
	return out, nil
}

func (c *RemoteClient) Publish(ctx context.Context, batch EventBatch) (PublishResult, error) {
	var out PublishResult
	if err := c.call(ctx, "/internal/v1/events", batch, &out); err != nil {
		return PublishResult{}, err
	}
	return out, nil
}

// Probe reports the health recorded by the most recent call. It deliberately
// makes no request: /readyz has a 2 s single-flight cache and must not turn a
// readiness check into an outbound call on the request path.
func (c *RemoteClient) Probe(ctx context.Context) Health {
	if !c.everUsed.Load() {
		// Nothing has been asked of it yet. Reporting ok here would be a claim we
		// have not earned, and reporting degraded would fail readiness on a fresh
		// boot; a single cheap liveness call settles it honestly.
		if err := c.ping(ctx); err != nil {
			return HealthDegraded
		}
		return HealthOK
	}
	if c.faulted.Load() {
		return HealthDegraded
	}
	return HealthOK
}

func (c *RemoteClient) ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.faulted.Store(true)
		c.everUsed.Store(true)
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	c.everUsed.Store(true)
	ok := resp.StatusCode == http.StatusOK
	c.faulted.Store(!ok)
	if !ok {
		return fmt.Errorf("search: /healthz returned %d", resp.StatusCode)
	}
	return nil
}

// maxResponseBytes bounds what a misbehaving or hostile search service can make
// core allocate. AGENTS.md requires request, file and decoder resources bounded;
// a response body is the one an internal client usually forgets.
const maxResponseBytes = 8 << 20

func (c *RemoteClient) call(ctx context.Context, path string, in, out any) (err error) {
	c.everUsed.Store(true)
	defer func() { c.faulted.Store(err != nil) }()

	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("search: encoding %s request: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	if err := SignRequest(req, c.key, body, time.Now()); err != nil {
		return fmt.Errorf("search: signing %s: %w", path, err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Never wrap the URL into the message: a misconfigured VIZRA_SEARCH_URL
		// can carry userinfo, and this string reaches logs.
		return fmt.Errorf("search: %s is unreachable: %w", path, redactedTransportError(err))
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("search: reading %s response: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("search: %s returned HTTP %d", path, resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("search: %s returned an unparseable body", path)
	}
	return nil
}

// redactedTransportError strips a URL out of a *url.Error, which formats as
// `Post "https://user:pw@host/path": dial tcp ...`.
func redactedTransportError(err error) error {
	msg := err.Error()
	if i := strings.Index(msg, `": `); i != -1 {
		return fmt.Errorf("%s", msg[i+3:])
	}
	return err
}
