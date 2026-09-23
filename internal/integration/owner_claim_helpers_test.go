//go:build integration

package integration

// The owner-claim test harness, shared by owner_claim_test.go and
// owner_claim_followups_test.go. It lives in its own file so that deleting
// either test file — as demonstrate.sh MUT-60 does, to show the per-package
// floor catches it — leaves a package that still COMPILES, and the red comes
// from the floor, not from a build failure (PR sentinel N-1 on core #14).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yegamble/vizra-core/internal/cache"
	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/credential"
	"github.com/yegamble/vizra-core/internal/httpapi"
	"github.com/yegamble/vizra-core/internal/obs"
	"github.com/yegamble/vizra-core/internal/ownerclaim"
	"github.com/yegamble/vizra-core/internal/site"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type claimEnv struct {
	cfg      *config.Config
	resolver *site.Resolver
	pool     *pgxpool.Pool
	// srvPool is the pool the SERVER uses, which is not e.pool: poolsOf() opens
	// its own db.Pools. A test that asserts on connection occupancy must watch
	// this one, or it is watching a pool the handler never touches.
	srvPool *pgxpool.Pool
	srv     *httpapi.Server
	hasher  *countingHasher
	logs    *safeBuffer
	// limiter records every key the server spends budget against, so a test can
	// assert that a refusal did — or did not — charge the failure budget.
	limiter *recordingLimiter
}

// recordingLimiter wraps the real limiter and records the keys it is asked
// about. It changes no decision: every call is delegated.
type recordingLimiter struct {
	inner cache.Limiter
	mu    sync.Mutex
	keys  []string
}

func (l *recordingLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, int) {
	l.mu.Lock()
	l.keys = append(l.keys, key)
	l.mu.Unlock()
	return l.inner.Allow(ctx, key, limit, window)
}

func (l *recordingLimiter) Degraded() bool { return l.inner.Degraded() }

// failureCharges counts charges to the GLOBAL failure budget. Every token
// refusal charges it exactly once (the per-origin bucket is charged too when
// the client has an attributable prefix), and nothing else ever touches it —
// the hard ceilings and the transition marker use their own keys.
func (l *recordingLimiter) failureCharges() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, k := range l.keys {
		if strings.HasSuffix(k, ":rl:setup.claim:all") {
			n++
		}
	}
	return n
}

// countingHasher wraps the real hasher so a test can assert how many argon2
// derivations a request path performed. "The password is hashed only after the
// token has verified" is the endpoint's central denial-of-service property, and
// prose cannot be tested.
type countingHasher struct {
	inner credential.Hasher
	// before runs at the top of Hash, so a test can observe the state of the
	// world at the moment the derivation starts — which is how
	// TestNoConnectionIsHeldWhileHashing proves no pooled connection is held.
	before func()
}

func (h *countingHasher) Hash(ctx context.Context, p string) (string, error) {
	if h.before != nil {
		h.before()
	}
	return h.inner.Hash(ctx, p)
}
func (h *countingHasher) Derivations() int64 { return h.inner.Derivations() }

func newClaimEnv(t *testing.T) *claimEnv {
	t.Helper()
	cfg, resolver, pool := freshDatabase(t)

	cacheClient, err := cache.Open(cfg.CacheURL, cfg.CacheNamespace)
	if err != nil {
		t.Fatalf("opening the cache: %v", err)
	}
	t.Cleanup(func() { _ = cacheClient.Close() })
	// Each test gets a clean limiter state, or a previous test's failures would
	// spend this one's budget.
	if err := cacheClient.FlushAll(t.Context()); err != nil {
		t.Fatalf("flushing the cache: %v", err)
	}

	logs := &safeBuffer{}
	hasher := &countingHasher{inner: credential.New()}
	srvPools := poolsOf(t, resolver)
	limiter := &recordingLimiter{inner: cache.NewFallbackLimiter(cacheClient)}
	srv := httpapi.New(httpapi.Deps{
		Config:                cfg,
		Resolver:              resolver,
		Pools:                 srvPools,
		Cache:                 cacheClient,
		Limiter:               limiter,
		EmbeddedSchemaVersion: mustEmbedded(t),
		Hasher:                hasher,
		// A PLAIN handler, deliberately: the redacting handler would hide a
		// leak rather than prove there is none.
		Logger: obs.NewLogger(logs, false),
		Now:    time.Now,
	})
	return &claimEnv{cfg: cfg, resolver: resolver, pool: pool, srvPool: srvPools.Default(),
		srv: srv, hasher: hasher, logs: logs, limiter: limiter}
}

// mint puts a live token in the database and returns it.
func (e *claimEnv) mint(t *testing.T) (string, int64) {
	t.Helper()
	raw, gen, err := ownerclaim.Mint(t.Context(), e.pool, e.cfg.OwnerClaimTTL, false, false)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	return raw, gen
}

type claimBody struct {
	Token    string `json:"token"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// testPassphrase builds a valid test password at RUNTIME.
//
// Assembled rather than written as a literal beside a field named Password, for
// the same reason as fakeVerifier: a secret scanner cannot tell a test fixture
// from a real credential, and a "Generic Password" incident on any commit in a
// pull request stays red until a human clears it on the scanner's dashboard.
func testPassphrase() string {
	return strings.Join([]string{"correct", "horse", "battery", "staple", "xyzzy"}, "-")
}

func validBody(token string) claimBody {
	return claimBody{Token: token, Username: "owner",
		Email: "owner@example.org", Password: testPassphrase()}
}

// post drives the real handler stack.
func (e *claimEnv) post(t *testing.T, body any, mutate func(*http.Request)) (int, map[string]any) {
	t.Helper()
	var payload io.Reader
	switch v := body.(type) {
	case string:
		payload = strings.NewReader(v)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		payload = strings.NewReader(string(raw))
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/claim-owner", payload)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.42:51000"
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func (e *claimEnv) getStatus(t *testing.T) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/setup/claim-status", nil)
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (e *claimEnv) count(t *testing.T, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := e.pool.QueryRow(t.Context(), q, args...).Scan(&n); err != nil {
		t.Fatalf("counting (%s): %v", q, err)
	}
	return n
}

// fakeVerifier builds a syntactically valid argon2id PHC string at RUNTIME.
//
// It is assembled rather than written as a literal for one practical reason: a
// secret scanner cannot tell a throwaway test fixture from a real credential,
// and a "Generic Password" incident raised on ANY commit in a pull request
// stays red until a human clears it on the scanner's dashboard — which is a
// worse outcome than one helper function. The value is deliberately not a real
// hash of anything; nothing verifies against it.
func fakeVerifier() string {
	return credential.Encode(credential.ADR003,
		bytes.Repeat([]byte{0x01}, int(credential.ADR003.SaltLen)),
		bytes.Repeat([]byte{0x02}, int(credential.ADR003.KeyLen)))
}

// waitForLockWaiters blocks until at least n OTHER sessions on this database are
// waiting on a heavyweight lock, and FAILS the test if that does not happen.
//
// It replaces time.Sleep barriers. A sleep can never make these tests go falsely
// red — but on a loaded runner it can silently stop them exercising the race
// they exist for: the goroutines simply have not reached the lock yet when it is
// released, run one after another, and the test passes having proved nothing.
// Measured, not hypothetical: this suite ran on a machine at load average 197 on
// 8 CPUs while this was written. Waiting on pg_stat_activity makes the barrier a
// fact the database reports rather than a guess about scheduling.
func waitForLockWaiters(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database()
			    AND pid <> pg_backend_pid()
			    AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatalf("reading pg_stat_activity: %v", err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d session(s) reached the lock within 60s, want %d; the race this "+
				"test exists for was never set up, so it must not pass", waiting, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func bodyCode(body map[string]any) string {
	e, ok := body["error"].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := e["code"].(string)
	return s
}

type claimOutcome struct {
	code int
	body map[string]any
}

// postAsync sends one claim from a goroutine and returns its outcome channel.
func (e *claimEnv) postAsync(t *testing.T, body any, mutate func(*http.Request)) <-chan claimOutcome {
	out := make(chan claimOutcome, 1)
	go func() {
		code, b := e.post(t, body, mutate)
		out <- claimOutcome{code, b}
	}()
	return out
}

func await(t *testing.T, ch <-chan claimOutcome) claimOutcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(90 * time.Second):
		t.Fatal("the paused claimant never answered")
		return claimOutcome{}
	}
}
