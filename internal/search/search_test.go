package search

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yegamble/vizra-core/internal/authz"
)

var testKey = []byte("Ar4Lo8Cq2Ei6Uk0Wn3Sv7Yb1Md5Pt9Xz")

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---------------------------------------------------------------------------
// HMAC scheme
// ---------------------------------------------------------------------------

type vectorFile struct {
	Scheme  string `json:"scheme"`
	KeyUTF8 string `json:"key_utf8"`
	Vectors []struct {
		Name      string `json:"name"`
		Method    string `json:"method"`
		Path      string `json:"path"`
		Timestamp string `json:"timestamp"`
		Nonce     string `json:"nonce"`
		BodyUTF8  string `json:"body_utf8"`
		BodyHash  string `json:"body_sha256_hex"`
		Canonical string `json:"canonical_string"`
		Signature string `json:"signature"`
	} `json:"vectors"`
}

// The vectors are a committed artifact in api/, shared with vizra-search. If
// this test and that file disagree, the two repositories would sign different
// bytes and every internal call would 401 in production.
func TestHMACTestVectors(t *testing.T) {
	raw, err := os.ReadFile("../../api/search-hmac-testvectors.json")
	if err != nil {
		t.Fatalf("the shared HMAC test vectors are missing: %v", err)
	}
	var vf vectorFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatalf("parsing vectors: %v", err)
	}
	if vf.Scheme != SignatureVersion {
		t.Fatalf("vector file scheme = %q, code implements %q", vf.Scheme, SignatureVersion)
	}
	if len(vf.Vectors) == 0 {
		t.Fatal("the vector file has no vectors")
	}
	key := []byte(vf.KeyUTF8)
	for _, v := range vf.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			body := []byte(v.BodyUTF8)
			if got := BodyHash(body); got != v.BodyHash {
				t.Fatalf("body hash = %s, vector says %s", got, v.BodyHash)
			}
			if got := CanonicalString(v.Method, v.Path, v.Timestamp, v.Nonce, v.BodyHash); got != v.Canonical {
				t.Fatalf("canonical string mismatch\n got: %q\nwant: %q", got, v.Canonical)
			}
			got, err := Sign(key, v.Method, v.Path, v.Timestamp, v.Nonce, body)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			if got != v.Signature {
				t.Fatalf("signature = %s, vector says %s", got, v.Signature)
			}
		})
	}
}

func TestEmptyBodyHashesToSHA256OfEmptyString(t *testing.T) {
	const want = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := BodyHash(nil); got != want {
		t.Fatalf("BodyHash(nil) = %s, want %s", got, want)
	}
	if got := BodyHash([]byte{}); got != want {
		t.Fatalf("BodyHash([]) = %s, want %s", got, want)
	}
}

func signedHeader(t *testing.T, method, path string, body []byte, at time.Time) http.Header {
	t.Helper()
	ts := strconv.FormatInt(at.Unix(), 10)
	nonce, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	sig, err := Sign(testKey, method, path, ts, nonce, body)
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	h.Set(HeaderTimestamp, ts)
	h.Set(HeaderNonce, nonce)
	h.Set(HeaderSignature, sig)
	return h
}

func TestVerifyRoundTrip(t *testing.T) {
	now := time.Now()
	body := []byte(`{"query":"sunset"}`)
	h := signedHeader(t, http.MethodPost, "/internal/v1/search", body, now)
	if err := Verify(testKey, http.MethodPost, "/internal/v1/search", h, body, now); err != nil {
		t.Fatalf("a signature we just produced did not verify: %v", err)
	}
}

// Each of these is a way a signature must fail. A verifier that passes any of
// them is not authenticating anything.
func TestVerifyRejects(t *testing.T) {
	now := time.Now()
	body := []byte(`{"query":"sunset"}`)
	base := func() http.Header { return signedHeader(t, http.MethodPost, "/internal/v1/search", body, now) }

	t.Run("tampered body", func(t *testing.T) {
		if err := Verify(testKey, http.MethodPost, "/internal/v1/search", base(), []byte(`{"query":"sunrise"}`), now); err == nil {
			t.Fatal("a changed body verified")
		}
	})
	t.Run("different path", func(t *testing.T) {
		if err := Verify(testKey, http.MethodPost, "/internal/v1/events", base(), body, now); err == nil {
			t.Fatal("a replay onto another path verified")
		}
	})
	t.Run("different method", func(t *testing.T) {
		if err := Verify(testKey, http.MethodGet, "/internal/v1/search", base(), body, now); err == nil {
			t.Fatal("a replay with another method verified")
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		other := []byte("Zq1Wx5Er9Ty3Ui7Op0As4Df8Gh2Jk6Lm")
		if err := Verify(other, http.MethodPost, "/internal/v1/search", base(), body, now); err == nil {
			t.Fatal("the wrong key verified")
		}
	})
	t.Run("stale timestamp", func(t *testing.T) {
		h := signedHeader(t, http.MethodPost, "/internal/v1/search", body, now.Add(-MaxClockSkew-time.Second))
		if err := Verify(testKey, http.MethodPost, "/internal/v1/search", h, body, now); err != ErrTimestampSkew {
			t.Fatalf("stale timestamp: err = %v, want %v", err, ErrTimestampSkew)
		}
	})
	t.Run("future timestamp", func(t *testing.T) {
		h := signedHeader(t, http.MethodPost, "/internal/v1/search", body, now.Add(MaxClockSkew+time.Second))
		if err := Verify(testKey, http.MethodPost, "/internal/v1/search", h, body, now); err != ErrTimestampSkew {
			t.Fatalf("future timestamp: err = %v, want %v", err, ErrTimestampSkew)
		}
	})
	t.Run("missing headers", func(t *testing.T) {
		for _, drop := range []string{HeaderTimestamp, HeaderNonce, HeaderSignature} {
			h := base()
			h.Del(drop)
			if err := Verify(testKey, http.MethodPost, "/internal/v1/search", h, body, now); err == nil {
				t.Fatalf("verified with %s missing", drop)
			}
		}
	})
	t.Run("short nonce", func(t *testing.T) {
		h := base()
		h.Set(HeaderNonce, "abcd")
		if err := Verify(testKey, http.MethodPost, "/internal/v1/search", h, body, now); err == nil {
			t.Fatal("a 2-byte nonce verified")
		}
	})
	t.Run("short key", func(t *testing.T) {
		if err := Verify([]byte("tooshort"), http.MethodPost, "/internal/v1/search", base(), body, now); err != ErrKeyTooShort {
			t.Fatalf("err = %v, want %v", err, ErrKeyTooShort)
		}
	})
}

// ---------------------------------------------------------------------------
// The fallback semantics Q-001 forbids collapsing
// ---------------------------------------------------------------------------

// fakeSearch is a vizra-search that verifies signatures and answers however the
// test tells it to.
func fakeSearch(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if err := Verify(testKey, r.Method, r.URL.EscapedPath(), r.Header, raw, time.Now()); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"signature_rejected","message":"rejected"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func newService(t *testing.T, srv *httptest.Server) *Service {
	t.Helper()
	var remote *RemoteClient
	if srv != nil {
		remote = NewRemote(srv.URL, testKey, 2*time.Second)
	}
	return NewService(NewSQL(), remote, quietLogger())
}

func TestSearchOffUsesSQLAndReportsOff(t *testing.T) {
	s := newService(t, nil)
	res, err := s.Search(context.Background(), SearchRequest{Query: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusOK || len(res.Results) != 0 {
		t.Fatalf("SQL path returned %+v; want an ok status with no results", res)
	}
	if got := s.Health(context.Background()); got != HealthOff {
		t.Fatalf("health = %s, want off", got)
	}
}

// not_indexed is a DECISION, not a fault: fall back to SQL and stay healthy.
func TestNotIndexedFallsBackToSQLAndStaysHealthy(t *testing.T) {
	srv := fakeSearch(t, http.StatusOK, `{"status":"not_indexed","results":[],"total":0}`)
	defer srv.Close()
	s := newService(t, srv)

	res, err := s.Search(context.Background(), SearchRequest{Query: "x"})
	if err != nil {
		t.Fatalf("not_indexed must not surface as an error: %v", err)
	}
	if res.Status != StatusOK {
		t.Fatalf("status = %s, want the SQL path's ok", res.Status)
	}
	if got := s.Health(context.Background()); got != HealthOK {
		t.Fatalf("health = %s after not_indexed, want ok: not_indexed is not a fault", got)
	}
}

// A fault is NEVER silent: SQL still answers, but health goes degraded.
func TestRemoteFaultFallsBackToSQLAndDegradesHealth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"500", http.StatusInternalServerError, `{"error":{"code":"internal_error","message":"boom"}}`},
		{"503", http.StatusServiceUnavailable, `{"error":{"code":"unavailable","message":"db down"}}`},
		{"unparseable 200", http.StatusOK, `this is not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeSearch(t, tc.status, tc.body)
			defer srv.Close()
			s := newService(t, srv)

			res, err := s.Search(context.Background(), SearchRequest{Query: "x"})
			if err != nil {
				t.Fatalf("reads must still be served from SQL during a fault: %v", err)
			}
			if res.Status != StatusOK {
				t.Fatalf("status = %s, want the SQL path's ok", res.Status)
			}
			if got := s.Health(context.Background()); got != HealthDegraded {
				t.Fatalf("health = %s after a %s fault, want degraded: Q-001 forbids a silent fallback", got, tc.name)
			}
		})
	}
}

func TestUnreachableSearchDegradesHealth(t *testing.T) {
	srv := fakeSearch(t, http.StatusOK, `{"status":"ok","results":[],"total":0}`)
	srv.Close() // now refusing connections
	s := newService(t, srv)

	if _, err := s.Search(context.Background(), SearchRequest{Query: "x"}); err != nil {
		t.Fatalf("an unreachable search must still be answered from SQL: %v", err)
	}
	if got := s.Health(context.Background()); got != HealthDegraded {
		t.Fatalf("health = %s, want degraded", got)
	}
}

// Health must recover without a restart once the service comes back.
func TestHealthRecoversAfterAFault(t *testing.T) {
	mode := "fault"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == "fault" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","results":[],"total":0}`))
	}))
	defer srv.Close()
	s := newService(t, srv)

	_, _ = s.Search(context.Background(), SearchRequest{Query: "x"})
	if s.Health(context.Background()) != HealthDegraded {
		t.Fatal("expected degraded after the fault")
	}
	mode = "ok"
	_, _ = s.Search(context.Background(), SearchRequest{Query: "x"})
	if got := s.Health(context.Background()); got != HealthOK {
		t.Fatalf("health = %s after recovery, want ok without a restart", got)
	}
}

// Publish is driven by a durable job, so a fault must be an error the job can
// retry — not a swallowed side-write.
func TestPublishFaultIsAnErrorSoTheJobRetries(t *testing.T) {
	srv := fakeSearch(t, http.StatusInternalServerError, `{"error":{"code":"internal_error","message":"boom"}}`)
	defer srv.Close()
	s := newService(t, srv)

	_, err := s.Publish(context.Background(), EventBatch{Events: []Event{{EventID: "e1"}}})
	if err == nil {
		t.Fatal("a failed publish was swallowed; the durable job would mark itself succeeded and the event would be lost")
	}
}

func TestPublishNotIndexedSucceedsSoTheJobDoesNotRetryForever(t *testing.T) {
	srv := fakeSearch(t, http.StatusOK, `{"status":"not_indexed","accepted":0,"duplicates":0}`)
	defer srv.Close()
	s := newService(t, srv)

	res, err := s.Publish(context.Background(), EventBatch{Events: []Event{{EventID: "e1"}}})
	if err != nil {
		t.Fatalf("not_indexed must not fail the job: %v", err)
	}
	if res.Status != StatusNotIndexed {
		t.Fatalf("status = %s, want not_indexed", res.Status)
	}
}

// The client signs correctly against a verifier that implements the contract.
func TestRemoteRequestsAreSignedAcceptably(t *testing.T) {
	srv := fakeSearch(t, http.StatusOK, `{"status":"ok","results":[],"total":3}`)
	defer srv.Close()
	c := NewRemote(srv.URL, testKey, 2*time.Second)

	res, err := c.Search(context.Background(), SearchRequest{Query: "sunset", Viewer: Viewer{IsAnonymous: true, Role: "anonymous"}})
	if err != nil {
		t.Fatalf("a correctly signed request was rejected: %v", err)
	}
	if res.Total != 3 {
		t.Fatalf("total = %d, want 3", res.Total)
	}
}

// A wrong key is a fault, not a not_indexed: a misconfigured shared secret must
// be visible in readiness, not look like an empty index.
func TestWrongKeyIsAFaultNotAFallback(t *testing.T) {
	srv := fakeSearch(t, http.StatusOK, `{"status":"ok","results":[],"total":1}`)
	defer srv.Close()
	bad := NewRemote(srv.URL, []byte("Zq1Wx5Er9Ty3Ui7Op0As4Df8Gh2Jk6Lm"), 2*time.Second)
	s := NewService(NewSQL(), bad, quietLogger())

	if _, err := s.Search(context.Background(), SearchRequest{Query: "x"}); err != nil {
		t.Fatalf("reads must still be answered: %v", err)
	}
	if got := s.Health(context.Background()); got != HealthDegraded {
		t.Fatalf("health = %s with a wrong HMAC key, want degraded", got)
	}
}

// A transport error must not carry the search URL into a log line: a
// misconfigured VIZRA_SEARCH_URL can hold userinfo.
func TestTransportErrorDoesNotLeakCredentials(t *testing.T) {
	c := NewRemote("http://user:sup3rsecret@127.0.0.1:1/", testKey, 200*time.Millisecond)
	_, err := c.Search(context.Background(), SearchRequest{Query: "x"})
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), "sup3rsecret") {
		t.Fatalf("the transport error leaked a credential: %q", err.Error())
	}
}

func TestViewerFromSubjectMarksAnonymous(t *testing.T) {
	v := ViewerFrom(authz.Subject{Role: authz.RoleAnonymous}, authz.ScopeSite)
	if !v.IsAnonymous || v.UserID != "" || v.Role != authz.RoleAnonymous {
		t.Fatalf("anonymous viewer projected as %+v", v)
	}
	// A subject with an id is never projected as anonymous: vizra-search trusts
	// this field for its projection.
	v = ViewerFrom(authz.Subject{UserID: "u1", Role: authz.RoleMember}, authz.ScopeOwnLibrary)
	if v.IsAnonymous || v.Scope != authz.ScopeOwnLibrary {
		t.Fatalf("member viewer projected as %+v", v)
	}
}
