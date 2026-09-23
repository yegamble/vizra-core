//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yegamble/vizra-core/internal/audit"
	"github.com/yegamble/vizra-core/internal/cache"
	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/credential"
	"github.com/yegamble/vizra-core/internal/db"
	"github.com/yegamble/vizra-core/internal/httpapi"
	"github.com/yegamble/vizra-core/internal/obs"
	"github.com/yegamble/vizra-core/internal/ownerclaim"
	"github.com/yegamble/vizra-core/internal/site"
	"github.com/yegamble/vizra-core/internal/store/sqlcgen"
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

// ---------------------------------------------------------------------------
// Success, and what it writes
// ---------------------------------------------------------------------------

func TestOwnerClaimCreatesExactlyOneOwnerAndConsumesTheToken(t *testing.T) {
	e := newClaimEnv(t)
	token, gen := e.mint(t)

	code, body := e.post(t, validBody(token), nil)
	if code != http.StatusCreated {
		t.Fatalf("claim = %d, want 201. body=%v", code, body)
	}
	if body["username"] != "owner" || body["role"] != "owner" {
		t.Fatalf("response = %v, want username and role", body)
	}
	// The internal uuid must NOT be in an unauthenticated response: ADR-007
	// separates internal from public identifiers, and a required field cannot
	// be removed from a contract later.
	if _, leaked := body["id"]; leaked {
		t.Error("the claim response carries the internal user id")
	}
	if n := len(body); n != 2 {
		t.Errorf("the response has %d fields, want exactly 2 (username, role): %v", n, body)
	}

	if got := e.count(t, `SELECT count(*) FROM users WHERE role='owner' AND tombstoned_at IS NULL`); got != 1 {
		t.Fatalf("live owners = %d, want 1", got)
	}
	if got := e.count(t, `SELECT count(*) FROM credentials WHERE kind='password'`); got != 1 {
		t.Fatalf("password credentials = %d, want 1", got)
	}
	if got := e.count(t, `SELECT count(*) FROM owner_claim_tokens WHERE consumed_at IS NOT NULL`); got != 1 {
		t.Fatalf("consumed tokens = %d, want 1", got)
	}
	if got := e.count(t, `SELECT count(*) FROM audit_events WHERE action='setup.owner_claim.succeeded'`); got != 1 {
		t.Fatalf("succeeded audit rows = %d, want 1", got)
	}
	var subjectID string
	if err := e.pool.QueryRow(t.Context(),
		`SELECT subject_id FROM audit_events WHERE action='setup.owner_claim.minted'`).Scan(&subjectID); err != nil {
		t.Fatalf("reading the mint audit row: %v", err)
	}
	if subjectID != fmt.Sprintf("%d", gen) {
		t.Errorf("mint audit subject_id = %q, want the generation %d", subjectID, gen)
	}
}

// TestClaimStatusRevealsOnlyTheClaimedBit — the privacy case.
func TestClaimStatusRevealsOnlyTheClaimedBit(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)

	code, body := e.getStatus(t)
	if code != http.StatusOK {
		t.Fatalf("claim-status = %d, want 200", code)
	}
	if len(body) != 1 {
		t.Fatalf("claim-status returned %d fields, want exactly 1: %v", len(body), body)
	}
	if body["claimed"] != false {
		t.Fatalf("claimed = %v, want false", body["claimed"])
	}
	for _, forbidden := range []string{"minted_at", "expires_at", "generation", "live", "token", "users", "owner"} {
		if _, present := body[forbidden]; present {
			t.Errorf("claim-status leaked %q; the contract is exactly one bit", forbidden)
		}
	}

	if code, _ := e.post(t, validBody(token), nil); code != http.StatusCreated {
		t.Fatalf("claim = %d, want 201", code)
	}
	_, body = e.getStatus(t)
	if body["claimed"] != true {
		t.Fatalf("after claiming, claimed = %v, want true", body["claimed"])
	}
	if len(body) != 1 {
		t.Fatalf("claim-status returned %d fields after claiming, want 1", len(body))
	}
}

// ---------------------------------------------------------------------------
// The race — the ledger's headline negative case
// ---------------------------------------------------------------------------

// TestOwnerClaimRaceYieldsExactlyOneOwnerUnderEveryServerDefaultIsolation.
//
// The claim transaction pins READ COMMITTED explicitly rather than inheriting
// the server GUC. Under REPEATABLE READ a losing claimant gets 40001 instead of
// zero rows, which the handler would surface as a 500 — a false "zero 5xx" pass
// here and a real failure only in production. So the race runs under all three
// server defaults an operator, a managed provider or a pooler can set.
func TestOwnerClaimRaceYieldsExactlyOneOwnerUnderEveryServerDefaultIsolation(t *testing.T) {
	for _, iso := range []string{"read committed", "repeatable read", "serializable"} {
		t.Run(strings.ReplaceAll(iso, " ", "_"), func(t *testing.T) {
			e := newClaimEnv(t)
			dbName := quoteIdent(currentDatabase(t, e.pool))
			if _, err := e.pool.Exec(t.Context(),
				fmt.Sprintf("ALTER DATABASE %s SET default_transaction_isolation = %q",
					dbName, iso)); err != nil {
				t.Fatalf("setting the server default isolation: %v", err)
			}
			t.Cleanup(func() {
				// context.Background(), not t.Context(): the test's context is
				// already cancelled by the time cleanup runs, and leaving the GUC
				// set would silently change every later test's isolation level.
				_, _ = e.pool.Exec(context.Background(),
					fmt.Sprintf("ALTER DATABASE %s RESET default_transaction_isolation", dbName))
			})
			// New connections must pick up the new default — on BOTH pools. The
			// server's own pool is the one the claimants use; resetting only the
			// test's pool relied, unchecked, on the server's pool not having
			// connected before the ALTER. True today; now it is asserted.
			e.pool.Reset()
			e.srvPool.Reset()
			var got string
			if err := e.srvPool.QueryRow(t.Context(), "SHOW default_transaction_isolation").Scan(&got); err != nil {
				t.Fatalf("reading the server pool's isolation default: %v", err)
			}
			if got != iso {
				t.Fatalf("the server's pool runs under %q, want %q; this sub-test would not be testing "+
					"what its name says", got, iso)
			}

			token, _ := e.mint(t)
			refusedBefore := e.count(t, `SELECT count(*) FROM audit_events WHERE action='setup.owner_claim.refused'`)
			chargesBefore := e.limiter.failureCharges()

			const n = 32
			var wg sync.WaitGroup
			codes := make([]int, n)
			bodies := make([]map[string]any, n)
			start := make(chan struct{})
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					b := validBody(token)
					b.Username = fmt.Sprintf("owner%02d", i)
					b.Email = fmt.Sprintf("owner%02d@example.org", i)
					codes[i], bodies[i] = e.post(t, b, nil)
				}(i)
			}
			close(start)
			wg.Wait()

			created, declined, serverErrors := 0, 0, 0
			for i, c := range codes {
				switch {
				case c == http.StatusCreated:
					created++
				// 409 exactly, never 403: by the time a loser is answered the winner
				// has committed, the instance is claimed, and OQ-4 says a claimed
				// instance answers 409. A 403 here would also charge the loser's
				// failure budget and write a `refused` row for the server's race.
				case c == http.StatusConflict:
					declined++
				case c >= 500:
					serverErrors++
					t.Errorf("claimant %d got %d: %v", i, c, bodies[i])
				default:
					t.Errorf("claimant %d got an unexpected %d: %v", i, c, bodies[i])
				}
			}
			if created != 1 {
				t.Fatalf("%d claimants were told they created the owner, want exactly 1", created)
			}
			if declined != n-1 {
				t.Fatalf("%d claimants were declined, want %d", declined, n-1)
			}
			if serverErrors != 0 {
				t.Fatalf("%d claimants got a 5xx; the race must be decided cleanly", serverErrors)
			}
			if got := e.count(t, `SELECT count(*) FROM users WHERE role='owner' AND tombstoned_at IS NULL`); got != 1 {
				t.Fatalf("live owners = %d, want exactly 1", got)
			}
			if got := e.count(t, `SELECT count(*) FROM users`); got != 1 {
				t.Fatalf("users = %d, want exactly 1 (a loser wrote a row)", got)
			}
			if got := e.count(t, `SELECT count(*) FROM credentials`); got != 1 {
				t.Fatalf("credentials = %d, want exactly 1 (an orphan credential survived)", got)
			}
			if got := e.count(t, `SELECT count(*) FROM audit_events WHERE action='setup.owner_claim.succeeded'`); got != 1 {
				t.Fatalf("succeeded audit rows = %d, want exactly 1", got)
			}
			// A loser is declined by the server's race, not refused for its token:
			// no `refused` row and no failure-budget charge, whichever gate
			// declined it (read phase, in-transaction gate, or an empty redeem).
			if got := e.count(t, `SELECT count(*) FROM audit_events WHERE action='setup.owner_claim.refused'`) - refusedBefore; got != 0 {
				t.Fatalf("%d `refused` audit rows were written by losers of the race, want 0", got)
			}
			if got := e.limiter.failureCharges() - chargesBefore; got != 0 {
				t.Fatalf("losers of the race charged the failure budget %d time(s), want 0", got)
			}
		})
	}
}

func currentDatabase(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&name); err != nil {
		t.Fatalf("reading the current database: %v", err)
	}
	return name
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// ---------------------------------------------------------------------------
// Database invariants
// ---------------------------------------------------------------------------

// TestASecondLiveOwnerIsRefusedByTheDatabase — the index is the only defence the
// day a second writer exists (admin create-user, an import, a restore).
func TestASecondLiveOwnerIsRefusedByTheDatabase(t *testing.T) {
	e := newClaimEnv(t)
	insertOwner(t, e.pool, "first", "first@example.org")
	err := tryInsertOwner(t, e.pool, "second", "second@example.org")
	if err == nil {
		t.Fatal("a second LIVE owner was accepted; users_one_owner is not enforcing")
	}
	if !strings.Contains(err.Error(), "users_one_owner") {
		t.Fatalf("the refusal did not come from users_one_owner: %v", err)
	}
}

// TestATombstonedOwnerDoesNotPermanentlyBlockOwnership.
//
// Without the tombstone predicate, an admin deleting the wrong account, a
// compromised-owner containment or a GDPR request would leave the instance with
// no route back: the index would still hold the key, ADR-003 forbids demotion,
// no token can be minted because users exist, and the row cannot be deleted
// because audit_events RESTRICTs it. The only recovery would be hand-written SQL.
func TestATombstonedOwnerDoesNotPermanentlyBlockOwnership(t *testing.T) {
	e := newClaimEnv(t)
	insertOwner(t, e.pool, "first", "first@example.org")
	if _, err := e.pool.Exec(t.Context(),
		`UPDATE users SET tombstoned_at = now() WHERE username = 'first'`); err != nil {
		t.Fatalf("tombstoning: %v", err)
	}
	if err := tryInsertOwner(t, e.pool, "second", "second@example.org"); err != nil {
		t.Fatalf("a replacement owner must be possible once the previous one is tombstoned: %v", err)
	}
	if got := e.count(t, `SELECT count(*) FROM users WHERE role='owner' AND tombstoned_at IS NULL`); got != 1 {
		t.Fatalf("live owners = %d, want 1", got)
	}
	// Tombstoning must NOT reopen the claim endpoint: that gate is EXISTS(users).
	code, _ := e.getStatus(t)
	_ = code
	_, body := e.getStatus(t)
	if body["claimed"] != true {
		t.Fatal("tombstoning the owner reopened the claim endpoint; the gate must be EXISTS(users)")
	}
}

func insertOwner(t *testing.T, pool *pgxpool.Pool, username, email string) {
	t.Helper()
	if err := tryInsertOwner(t, pool, username, email); err != nil {
		t.Fatalf("inserting owner %s: %v", username, err)
	}
}

func tryInsertOwner(t *testing.T, pool *pgxpool.Pool, username, email string) error {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(t.Context(),
		`INSERT INTO users (id, username, email, role) VALUES ($1,$2,$3,'owner')`, id, username, email)
	return err
}

func TestCaseVariantUsernameAndEmailAreRejectedAsDuplicates(t *testing.T) {
	e := newClaimEnv(t)
	insertOwner(t, e.pool, "Owner", "Owner@Example.COM")

	var fold, emailFold string
	if err := e.pool.QueryRow(t.Context(),
		`SELECT username_fold, email_fold FROM users`).Scan(&fold, &emailFold); err != nil {
		t.Fatalf("reading the generated folds: %v", err)
	}
	if fold != "owner" || emailFold != "owner@example.com" {
		t.Fatalf("folds = %q/%q; they must be produced by PostgreSQL's lower()", fold, emailFold)
	}

	id, _ := uuid.NewV7()
	_, err := e.pool.Exec(t.Context(),
		`INSERT INTO users (id, username, email) VALUES ($1,'OWNER','other@example.org')`, id)
	if err == nil || !strings.Contains(err.Error(), "users_username_fold_key") {
		t.Fatalf("a case-variant username must be a duplicate, got %v", err)
	}
	id, _ = uuid.NewV7()
	_, err = e.pool.Exec(t.Context(),
		`INSERT INTO users (id, username, email) VALUES ($1,'other','OWNER@example.com')`, id)
	if err == nil || !strings.Contains(err.Error(), "users_email_fold_key") {
		t.Fatalf("a case-variant email must be a duplicate, got %v", err)
	}
}

func TestASecondPasswordCredentialForOneUserIsRefused(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)
	if code, _ := e.post(t, validBody(token), nil); code != http.StatusCreated {
		t.Fatal("setup claim failed")
	}
	var userID uuid.UUID
	if err := e.pool.QueryRow(t.Context(), `SELECT id FROM users`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	id, _ := uuid.NewV7()
	_, err := e.pool.Exec(t.Context(),
		`INSERT INTO credentials (id,user_id,kind,secret) VALUES ($1,$2,'password',$3)`,
		id, userID, fakeVerifier())
	if err == nil || !strings.Contains(err.Error(), "credentials_one_per_user_per_kind") {
		t.Fatalf("a second password credential must be refused, got %v", err)
	}
}

func TestTokenSingletonAndTerminalStateChecks(t *testing.T) {
	e := newClaimEnv(t)
	e.mint(t)
	if _, err := e.pool.Exec(t.Context(),
		`INSERT INTO owner_claim_tokens (id, token_sha256, expires_at) VALUES (false, sha256('x'::bytea), now()+interval '1h')`,
	); err == nil || !strings.Contains(err.Error(), "owner_claim_tokens_singleton") {
		t.Fatalf("a second token row must be refused, got %v", err)
	}
	if _, err := e.pool.Exec(t.Context(),
		`UPDATE owner_claim_tokens SET consumed_at = now(), superseded_at = now()`,
	); err == nil || !strings.Contains(err.Error(), "owner_claim_tokens_one_terminal_state") {
		t.Fatalf("a token cannot be both consumed and superseded, got %v", err)
	}
}

// TestOwnerIndexNameMatchesTheMapper: the handler maps 23505 to 409 by matching
// the constraint NAME, so a rename silently turns the one path the index exists
// for into an unhandled 500.
func TestOwnerIndexNameMatchesTheMapper(t *testing.T) {
	e := newClaimEnv(t)
	n := e.count(t, `SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND indexname='users_one_owner'`)
	if n != 1 {
		t.Fatalf("the index users_one_owner does not exist under that name; the error mapper matches on it")
	}
}

// TestUserRoleEnumOrderMatchesAuthzRanking: the enum order freezes here, and
// M1-C's role matrix depends on it. 'anonymous' is absent by intent — it is a
// request-time subject, never a stored principal.
func TestUserRoleEnumOrderMatchesAuthzRanking(t *testing.T) {
	e := newClaimEnv(t)
	rows, err := e.pool.Query(t.Context(), `SELECT unnest(enum_range(NULL::user_role))::text`)
	if err != nil {
		t.Fatalf("reading the enum: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	want := []string{"guest", "member", "manager", "admin", "owner"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("user_role order = %v, want %v (authz.Role's stored ranking)", got, want)
	}
	for _, r := range got {
		if r == "anonymous" {
			t.Error("'anonymous' must never be a storable role")
		}
	}
}

// ---------------------------------------------------------------------------
// Audit immutability and the erasure consequence
// ---------------------------------------------------------------------------

func TestAuditEventsCannotBeUpdatedOrDeleted(t *testing.T) {
	e := newClaimEnv(t)
	e.mint(t) // writes a minted row

	if _, err := e.pool.Exec(t.Context(), `UPDATE audit_events SET action = 'tampered'`); err == nil {
		t.Error("audit_events accepted an UPDATE")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("UPDATE was refused for the wrong reason: %v", err)
	}
	if _, err := e.pool.Exec(t.Context(), `DELETE FROM audit_events`); err == nil {
		t.Error("audit_events accepted a DELETE")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("DELETE was refused for the wrong reason: %v", err)
	}
}

// TestAuditEventsCannotBeTruncated: a row-level trigger does not fire on
// TRUNCATE, so without the statement-level trigger one statement destroys the
// entire trail the other trigger exists to protect.
func TestAuditEventsCannotBeTruncated(t *testing.T) {
	e := newClaimEnv(t)
	e.mint(t)
	if _, err := e.pool.Exec(t.Context(), `TRUNCATE audit_events`); err == nil {
		t.Fatal("audit_events accepted a TRUNCATE; the row trigger alone does not stop it")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("TRUNCATE was refused for the wrong reason: %v", err)
	}
}

// TestAUserWithAuditRowsCannotBeDeleted documents the consequence of
// ON DELETE RESTRICT, which the frozen actor-identity CHECK forces. Erasure is
// scrub-and-tombstone, never DELETE — and that is why no audit row may carry an
// email address. This test IS the documentation.
func TestAUserWithAuditRowsCannotBeDeleted(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)
	if code, _ := e.post(t, validBody(token), nil); code != http.StatusCreated {
		t.Fatal("setup claim failed")
	}
	_, err := e.pool.Exec(t.Context(), `DELETE FROM users`)
	if err == nil {
		t.Fatal("a user named by an audit row was deleted")
	}
	if !strings.Contains(err.Error(), "audit_events_actor_user_fk") {
		t.Fatalf("the refusal did not come from the audit FK: %v", err)
	}
}

// TestOwnerClaimAuditEventsCarryNoSecretMaterial.
func TestOwnerClaimAuditEventsCarryNoSecretMaterial(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)
	body := validBody(token)
	if code, _ := e.post(t, body, nil); code != http.StatusCreated {
		t.Fatal("setup claim failed")
	}
	// A refusal row too.
	e.post(t, validBody(strings.Repeat("b", 64)), nil)

	rows, err := e.pool.Query(t.Context(),
		`SELECT coalesce(actor_label,'') || ' ' || coalesce(before::text,'') || ' ' ||
		        coalesce(after::text,'') || ' ' || coalesce(subject_id,'') || ' ' || action
		   FROM audit_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var seen int
	for rows.Next() {
		var blob string
		if err := rows.Scan(&blob); err != nil {
			t.Fatal(err)
		}
		seen++
		for _, secret := range []string{token, body.Password, body.Email, "argon2id"} {
			if strings.Contains(strings.ToLower(blob), strings.ToLower(secret)) {
				t.Errorf("an audit row carries %q-like material: %s", secret, blob)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no audit rows were written at all")
	}
	// The username IS allowed: it is a public identifier.
	if got := e.count(t,
		`SELECT count(*) FROM audit_events WHERE after->>'username' = 'owner'`); got != 1 {
		t.Errorf("the succeeded row should record the username; got %d", got)
	}
}

// ---------------------------------------------------------------------------
// Token lifecycle
// ---------------------------------------------------------------------------

func TestOwnerClaimRejectsAReusedToken(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)
	if code, _ := e.post(t, validBody(token), nil); code != http.StatusCreated {
		t.Fatal("the first claim must succeed")
	}
	b := validBody(token)
	b.Username = "second"
	b.Email = "second@example.org"
	code, body := e.post(t, b, nil)
	if code != http.StatusConflict {
		t.Fatalf("reusing a token on a claimed instance = %d, want 409. body=%v", code, body)
	}
	if got := e.count(t, `SELECT count(*) FROM users`); got != 1 {
		t.Fatalf("users = %d, want 1", got)
	}
}

func TestOwnerClaimRejectsASupersededOrExpiredToken(t *testing.T) {
	e := newClaimEnv(t)
	old, _ := e.mint(t)
	fresh, _ := e.mint(t) // supersedes `old`

	code, body := e.post(t, validBody(old), nil)
	if code != http.StatusForbidden {
		t.Fatalf("a superseded token = %d, want 403. body=%v", code, body)
	}
	if bodyCode(body) != "forbidden" {
		t.Errorf("code = %q, want forbidden", bodyCode(body))
	}

	// Expire the live one using the DATABASE clock.
	if _, err := e.pool.Exec(t.Context(),
		`UPDATE owner_claim_tokens SET minted_at = now() - interval '4h', expires_at = now() - interval '3h'`); err != nil {
		t.Fatal(err)
	}
	code, body = e.post(t, validBody(fresh), nil)
	if code != http.StatusForbidden {
		t.Fatalf("an expired token = %d, want 403. body=%v", code, body)
	}
	if got := e.count(t, `SELECT count(*) FROM users`); got != 0 {
		t.Fatal("a refused claim created a user")
	}
}

// TestARestartDoesNotInvalidateALiveToken.
//
// Re-minting on every boot made any restart — a compose `up -d` after an env
// edit, an OOM, a health-check flap, an updater — silently invalidate the token
// in the operator's clipboard, and the uniform "not accepted" message by design
// told them nothing. Boot now mints only when there is no live token.
func TestARestartDoesNotInvalidateALiveToken(t *testing.T) {
	e := newClaimEnv(t)
	token, gen := e.mint(t)

	for i := 0; i < 3; i++ {
		var sink strings.Builder
		out, err := ownerclaim.Boot(t.Context(), e.pool,
			ownerclaim.AnnounceStderr, e.cfg.OwnerClaimTTL, &sink)
		if err != nil {
			t.Fatalf("boot %d: %v", i, err)
		}
		if out.Minted {
			t.Fatalf("boot %d minted a new token while a live one existed", i)
		}
		if strings.Contains(sink.String(), token) {
			t.Fatalf("boot %d reprinted the live token", i)
		}
	}
	code, body := e.post(t, validBody(token), nil)
	if code != http.StatusCreated {
		t.Fatalf("the token the operator holds must still work after restarts: %d %v", code, body)
	}
	if got := e.count(t, `SELECT count(*) FROM owner_claim_tokens WHERE generation = $1`, gen); got != 1 {
		t.Fatalf("the generation changed across restarts")
	}
}

// TestBootMintsOnlyUnderTheStderrOptIn, and never prints a credential by default.
func TestBootMintsOnlyUnderTheStderrOptIn(t *testing.T) {
	e := newClaimEnv(t)

	var off strings.Builder
	out, err := ownerclaim.Boot(t.Context(), e.pool, ownerclaim.AnnounceOff, e.cfg.OwnerClaimTTL, &off)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if out.Minted {
		t.Error("the default announce mode must not mint")
	}
	if e.count(t, `SELECT count(*) FROM owner_claim_tokens`) != 0 {
		t.Error("the default announce mode minted a token row")
	}
	if !strings.Contains(off.String(), "vizra claim-token") {
		t.Errorf("the default boot line must name the command; got %q", off.String())
	}
	if hex64(off.String()) {
		t.Errorf("the default boot line contains a 64-hex value: %q", off.String())
	}

	var on strings.Builder
	out, err = ownerclaim.Boot(t.Context(), e.pool, ownerclaim.AnnounceStderr, e.cfg.OwnerClaimTTL, &on)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if !out.Minted {
		t.Fatal("the stderr opt-in must mint when no live token exists")
	}
	if !hex64(on.String()) {
		t.Error("the stderr opt-in must print the token")
	}
}

// TestBootSupersedesALeftoverTokenOnAClaimedInstance: an implicitly claimed
// instance must never hold a live credential that creates an owner.
func TestBootSupersedesALeftoverTokenOnAClaimedInstance(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)
	insertOwner(t, e.pool, "outofband", "oob@example.org")

	var sink strings.Builder
	out, err := ownerclaim.Boot(t.Context(), e.pool, ownerclaim.AnnounceStderr, e.cfg.OwnerClaimTTL, &sink)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if !out.Claimed || !out.Superseded {
		t.Fatalf("boot on a claimed instance must supersede the leftover token: %+v", out)
	}
	if sink.Len() != 0 {
		t.Errorf("boot announced something on a claimed instance: %q", sink.String())
	}
	code, _ := e.post(t, validBody(token), nil)
	if code != http.StatusConflict {
		t.Fatalf("the leftover token = %d, want 409", code)
	}
}

// TestClaimTokenCLIRefusesOnAClaimedInstance: minting on a running claimed
// instance would manufacture a live owner-creating credential.
func TestClaimTokenCLIRefusesOnAClaimedInstance(t *testing.T) {
	e := newClaimEnv(t)
	insertOwner(t, e.pool, "owner", "owner@example.org")
	_, _, err := ownerclaim.Mint(t.Context(), e.pool, e.cfg.OwnerClaimTTL, true, false)
	if err == nil {
		t.Fatal("minting on a claimed instance was allowed")
	}
	if !strings.Contains(err.Error(), "already has users") {
		t.Fatalf("wrong refusal: %v", err)
	}
	if e.count(t, `SELECT count(*) FROM owner_claim_tokens`) != 0 {
		t.Fatal("a token row was written despite the refusal")
	}
}

func TestClaimWithNoMintedTokenIsIndistinguishableFromAWrongToken(t *testing.T) {
	e := newClaimEnv(t)
	// No mint at all.
	codeA, bodyA := e.post(t, validBody(strings.Repeat("a", 64)), nil)

	e2 := newClaimEnv(t)
	e2.mint(t)
	codeB, bodyB := e2.post(t, validBody(strings.Repeat("b", 64)), nil)

	if codeA != codeB {
		t.Fatalf("never-minted = %d but wrong-token = %d; the two must be indistinguishable", codeA, codeB)
	}
	if fmt.Sprint(bodyA) != fmt.Sprint(bodyB) {
		// request_id differs, so compare the meaningful parts.
		if bodyCode(bodyA) != bodyCode(bodyB) || msgOf(bodyA) != msgOf(bodyB) {
			t.Fatalf("different bodies: %v vs %v", bodyA, bodyB)
		}
	}
}

func msgOf(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	s, _ := e["message"].(string)
	return s
}

func hex64(s string) bool {
	for _, f := range strings.Fields(s) {
		f = strings.Trim(f, ".,:;()")
		if len(f) != 64 {
			continue
		}
		ok := true
		for _, r := range f {
			if !strings.ContainsRune("0123456789abcdef", r) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Request posture
// ---------------------------------------------------------------------------

func TestClaimRefusesANonJSONContentType(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)
	form := fmt.Sprintf("token=%s&username=owner&email=owner@example.org&password=%s", token, testPassphrase())

	for _, ct := range []string{
		"application/x-www-form-urlencoded",
		"multipart/form-data; boundary=x",
		"text/plain",
		"",
	} {
		code, _ := e.post(t, form, func(r *http.Request) {
			if ct == "" {
				r.Header.Del("Content-Type")
			} else {
				r.Header.Set("Content-Type", ct)
			}
		})
		if code != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type %q = %d, want 415", ct, code)
		}
	}
	if got := e.count(t, `SELECT count(*) FROM users`); got != 0 {
		t.Fatal("a non-JSON request created an owner")
	}
	if e.hasher.Derivations() != 0 {
		t.Errorf("a 415 cost %d argon2 derivations, want 0", e.hasher.Derivations())
	}
	// A charset parameter is permitted: it does not change the parse.
	code, _ := e.post(t, validBody(token), func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json; charset=utf-8")
	})
	if code != http.StatusCreated {
		t.Errorf("application/json; charset=utf-8 = %d, want 201", code)
	}
}

func TestClaimRefusesAnUnknownField(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)
	raw := fmt.Sprintf(`{"token":%q,"username":"owner","email":"owner@example.org",`+
		`"password":%q,"role":"owner"}`, token, testPassphrase())
	code, body := e.post(t, raw, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("an unknown field = %d, want 400. body=%v", code, body)
	}
	if got := e.count(t, `SELECT count(*) FROM users`); got != 0 {
		t.Fatal("a request with an unknown field created an owner")
	}
}

func TestClaimRefusesAnEmptyBody(t *testing.T) {
	e := newClaimEnv(t)
	e.mint(t)
	code, _ := e.post(t, "", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("an empty body = %d, want 400", code)
	}
}

// TestClaimBodyLimitAppliesToAChunkedRequest: a Content-Length check is not a
// bound, because a chunked request has no Content-Length to check.
func TestClaimBodyLimitAppliesToAChunkedRequest(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)
	big := fmt.Sprintf(`{"token":%q,"username":"owner","email":"owner@example.org","password":%q}`,
		token, strings.Repeat("x", 9<<10))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/claim-owner", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1 // streamed: no Content-Length
	req.TransferEncoding = []string{"chunked"}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a 9 KiB chunked body = %d, want 413. body=%s", rec.Code, rec.Body.String())
	}
	if e.hasher.Derivations() != 0 {
		t.Errorf("a 413 cost %d argon2 derivations, want 0", e.hasher.Derivations())
	}
}

func TestClaimRefusesACrossOriginRequest(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)

	code, body := e.post(t, validBody(token), func(r *http.Request) {
		r.Header.Set("Origin", "https://evil.example")
	})
	if code != http.StatusForbidden {
		t.Fatalf("a cross-origin claim = %d, want 403", code)
	}
	if bodyCode(body) != "origin_mismatch" {
		t.Fatalf("code = %q, want origin_mismatch — a misconfigured public origin must be "+
			"diagnosable and must not look like a rejected token", bodyCode(body))
	}
	if got := e.count(t, `SELECT count(*) FROM users`); got != 0 {
		t.Fatal("a cross-origin request created an owner")
	}

	code, body = e.post(t, validBody(token), func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	})
	if code != http.StatusForbidden || bodyCode(body) != "origin_mismatch" {
		t.Fatalf("a cross-site fetch = %d/%s, want 403/origin_mismatch", code, bodyCode(body))
	}
}

// TestClaimAcceptsAHeaderlessCLIRequest: the credential is in the body, not
// ambient, so curl and the installer must keep working.
func TestClaimAcceptsAHeaderlessCLIRequest(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)
	code, body := e.post(t, validBody(token), nil) // no Origin, no Sec-Fetch-Site
	if code != http.StatusCreated {
		t.Fatalf("a headerless claim = %d, want 201. body=%v", code, body)
	}
}

func TestClaimAcceptsTheConfiguredOrigin(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)
	code, _ := e.post(t, validBody(token), func(r *http.Request) {
		r.Header.Set("Origin", e.cfg.PublicOrigin)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	})
	if code != http.StatusCreated {
		t.Fatalf("a same-origin claim = %d, want 201", code)
	}
}

// TestOriginsAreComparedNormalisedNotAsStrings (N-3).
//
// Config stores the normalised origin, so a trailing slash, letter case, an
// explicitly written default port or a trailing dot in .env cannot turn a
// same-origin browser claim into 403 `origin_mismatch` while curl still works.
// The cross-origin case below must stay red on mutation.
func TestOriginsAreComparedNormalisedNotAsStrings(t *testing.T) {
	cases := []struct {
		name   string
		origin func(configured string) string
		want   int
	}{
		{"exactly the configured value", func(c string) string { return c }, http.StatusCreated},
		{"trailing slash", func(c string) string { return c + "/" }, http.StatusCreated},
		{"uppercased", strings.ToUpper, http.StatusCreated},
		// The configured origin in this environment already carries a non-default
		// port, so the default-port-elision case is exercised in the unit table
		// (internal/config/origin_test.go) where the base can be chosen freely.
		{"trailing dot on the host", func(c string) string {
			return strings.Replace(c, "://localhost", "://localhost.", 1)
		}, http.StatusCreated},
		{"a genuinely different host", func(string) string { return "https://evil.example" }, http.StatusForbidden},
		// security NEW-4: `Origin: null` — a sandboxed iframe, a data: document,
		// some cross-origin redirects — normalises to "". Comparing normalised
		// values for equality would make two failures agree, so an unnormalisable
		// origin must DENY rather than match.
		{"Origin: null", func(string) string { return "null" }, http.StatusForbidden},
		{"an origin that is not an origin", func(string) string { return "not-an-origin" }, http.StatusForbidden},
		{"an origin carrying a path", func(c string) string { return c + "/setup/claim" }, http.StatusForbidden},
		{"a different scheme", func(c string) string {
			return strings.Replace(c, "http://", "https://", 1)
		}, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newClaimEnv(t)
			token, _ := e.mint(t)
			origin := tc.origin(e.cfg.PublicOrigin)
			code, body := e.post(t, validBody(token), func(r *http.Request) {
				r.Header.Set("Origin", origin)
			})
			if code != tc.want {
				t.Fatalf("Origin %q = %d, want %d. body=%v", origin, code, tc.want, body)
			}
			if tc.want == http.StatusForbidden && bodyCode(body) != "origin_mismatch" {
				t.Errorf("code = %q, want origin_mismatch", bodyCode(body))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Validation, hashing order, and secrets
// ---------------------------------------------------------------------------

func TestMalformedFieldsYield400AndDoNotConsumeTheToken(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)

	cases := []struct {
		name string
		edit func(*claimBody)
	}{
		{"short username", func(b *claimBody) { b.Username = "ab" }},
		{"leading hyphen", func(b *claimBody) { b.Username = "-owner" }},
		{"31 characters", func(b *claimBody) { b.Username = strings.Repeat("a", 31) }},
		{"dotless email", func(b *claimBody) { b.Email = "owner@localhost" }},
		{"email over 254 bytes", func(b *claimBody) { b.Email = strings.Repeat("é", 130) + "@example.org" }},
		{"11-character password", func(b *claimBody) { b.Password = strings.Repeat("a", 11) }},
		{"257-character password", func(b *claimBody) { b.Password = strings.Repeat("a", 257) }},
		{"1 KiB of astral characters", func(b *claimBody) { b.Password = strings.Repeat("𝕏", 300) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := validBody(token)
			tc.edit(&b)
			code, body := e.post(t, b, nil)
			if code != http.StatusBadRequest {
				t.Fatalf("%s = %d, want 400 (never a 5xx from a CHECK). body=%v", tc.name, code, body)
			}
			if msgOf(body) == "" {
				t.Error("a 400 must name what is wrong")
			}
		})
	}

	if got := e.count(t, `SELECT count(*) FROM owner_claim_tokens WHERE consumed_at IS NOT NULL`); got != 0 {
		t.Fatal("a malformed request consumed the token")
	}
	if e.hasher.Derivations() != 0 {
		t.Errorf("malformed requests cost %d argon2 derivations, want 0", e.hasher.Derivations())
	}
	// The token still works afterwards.
	if code, _ := e.post(t, validBody(token), nil); code != http.StatusCreated {
		t.Fatal("the token must survive malformed attempts")
	}
}

// TestNoPasswordHashingOccursWithoutAValidToken is the endpoint's central
// denial-of-service property, made assertable.
//
// argon2id at ADR-003's parameters costs 19 MiB and tens of milliseconds. An
// unauthenticated endpoint that runs it before checking the credential hands the
// attacker a memory and CPU amplifier.
func TestNoPasswordHashingOccursWithoutAValidToken(t *testing.T) {
	e := newClaimEnv(t)
	e.mint(t)

	for i := 0; i < 5; i++ {
		b := validBody(strings.Repeat("c", 64))
		if code, _ := e.post(t, b, nil); code != http.StatusForbidden {
			t.Fatalf("a wrong token should be 403, got %d", code)
		}
	}
	if got := e.hasher.Derivations(); got != 0 {
		t.Fatalf("%d argon2 derivations ran for rejected tokens; want 0", got)
	}

	// Already claimed: also zero.
	insertOwner(t, e.pool, "oob", "oob@example.org")
	if code, _ := e.post(t, validBody(strings.Repeat("d", 64)), nil); code != http.StatusConflict {
		t.Fatalf("a claimed instance should be 409, got %d", code)
	}
	if got := e.hasher.Derivations(); got != 0 {
		t.Fatalf("%d derivations ran on the 409 path; want 0", got)
	}
}

// TestACorrectButDeadTokenCostsNoDerivation (N-4).
//
// A token that is CORRECT but expired, consumed or superseded used to pass the
// digest compare and buy a full 19 MiB / ~26 ms derivation before the redeem
// CTE's guard refused it — so "zero derivations on every non-201 path", which
// AGENTS.md and the PR body both assert, was false on that path and no test
// would have gone red. A dead token is one an attacker may already hold: a
// superseded one after an operator re-mint, or one recovered from a log under
// the stderr opt-in.
func TestACorrectButDeadTokenCostsNoDerivation(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		e := newClaimEnv(t)
		token, _ := e.mint(t)
		if _, err := e.pool.Exec(t.Context(),
			`UPDATE owner_claim_tokens SET minted_at = now() - interval '4h', expires_at = now() - interval '3h'`); err != nil {
			t.Fatal(err)
		}
		code, body := e.post(t, validBody(token), nil)
		if code != http.StatusForbidden || bodyCode(body) != "forbidden" {
			t.Fatalf("an expired token = %d/%s, want 403/forbidden", code, bodyCode(body))
		}
		if got := e.hasher.Derivations(); got != 0 {
			t.Fatalf("a correct-but-expired token cost %d derivations, want 0", got)
		}
	})

	t.Run("consumed", func(t *testing.T) {
		e := newClaimEnv(t)
		token, _ := e.mint(t)
		// Consume it without creating a user, so the claimed gate does not
		// short-circuit and the liveness check is what must refuse.
		if _, err := e.pool.Exec(t.Context(),
			`UPDATE owner_claim_tokens SET consumed_at = now()`); err != nil {
			t.Fatal(err)
		}
		code, body := e.post(t, validBody(token), nil)
		if code != http.StatusForbidden || bodyCode(body) != "forbidden" {
			t.Fatalf("a consumed token = %d/%s, want 403/forbidden", code, bodyCode(body))
		}
		if got := e.hasher.Derivations(); got != 0 {
			t.Fatalf("a correct-but-consumed token cost %d derivations, want 0", got)
		}
	})

	t.Run("superseded", func(t *testing.T) {
		e := newClaimEnv(t)
		token, _ := e.mint(t)
		if _, err := e.pool.Exec(t.Context(),
			`UPDATE owner_claim_tokens SET superseded_at = now()`); err != nil {
			t.Fatal(err)
		}
		code, body := e.post(t, validBody(token), nil)
		if code != http.StatusForbidden || bodyCode(body) != "forbidden" {
			t.Fatalf("a superseded token = %d/%s, want 403/forbidden", code, bodyCode(body))
		}
		if got := e.hasher.Derivations(); got != 0 {
			t.Fatalf("a correct-but-superseded token cost %d derivations, want 0", got)
		}
	})
}

func TestClaimTokenIsStoredOnlyAsASHA256Digest(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)

	var digest []byte
	if err := e.pool.QueryRow(t.Context(), `SELECT token_sha256 FROM owner_claim_tokens`).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if len(digest) != 32 {
		t.Fatalf("stored digest is %d bytes, want 32", len(digest))
	}
	want := ownerclaim.Digest(token)
	if string(digest) != string(want) {
		t.Fatal("the stored value is not SHA-256 of the token")
	}
	if strings.Contains(string(digest), token) {
		t.Fatal("the raw token is stored")
	}
	// And nowhere else in the table.
	var dump string
	if err := e.pool.QueryRow(t.Context(),
		`SELECT coalesce(string_agg(t::text, ' '), '') FROM owner_claim_tokens t`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dump, token) {
		t.Fatalf("the raw token appears in owner_claim_tokens: %s", dump)
	}
}

func TestOwnerClaimTokenNeverReachesTheStructuredLogOrAResponse(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/claim-owner",
		strings.NewReader(fmt.Sprintf(
			`{"token":%q,"username":"owner","email":"owner@example.org","password":%q}`,
			token, testPassphrase())))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("claim = %d", rec.Code)
	}

	if strings.Contains(rec.Body.String(), token) {
		t.Error("the response body echoes the claim token")
	}
	for k, vs := range rec.Header() {
		for _, v := range vs {
			if strings.Contains(v, token) {
				t.Errorf("header %s echoes the claim token", k)
			}
		}
	}
	// The logger here is a PLAIN handler, so a leak would show.
	if strings.Contains(e.logs.String(), token) {
		t.Errorf("the claim token reached the log stream: %s", firstRunes(e.logs.String(), 400))
	}
	if strings.Contains(e.logs.String(), testPassphrase()) {
		t.Error("the password reached the log stream")
	}
}

// ---------------------------------------------------------------------------
// Rate limiting
// ---------------------------------------------------------------------------

// TestAValidTokenIsNeverRateLimitedByTheFailureLimiter.
//
// With attempt-based limiting, an anonymous attacker sent junk until the
// operator's CORRECT token was answered 429 — a denial of claim at two requests
// a minute of attacker cost. Only failures spend budget now.
func TestAValidTokenIsNeverRateLimitedByTheFailureLimiter(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)

	limited := false
	for i := 0; i < 40; i++ {
		code, _ := e.post(t, validBody(strings.Repeat("e", 64)), nil)
		if code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("40 failed attempts did not exhaust the failure budget; the limiter is not working")
	}

	code, body := e.post(t, validBody(token), nil)
	if code != http.StatusCreated {
		t.Fatalf("the operator's valid token was answered %d after an attacker flooded the endpoint. "+
			"body=%v", code, body)
	}
}

// TestARateLimitedClaimWritesNoAuditRow.
//
// A 429 answers requests that by definition have no upper bound, so auditing
// each one would make the limiter an unbounded writer into a table nothing can
// delete — the limiter would cause the write instead of bounding it.
func TestARateLimitedClaimWritesNoAuditRow(t *testing.T) {
	e := newClaimEnv(t)
	e.mint(t)

	countRateLimited := func() int64 {
		return e.count(t, `SELECT count(*) FROM audit_events WHERE action='setup.owner_claim.rate_limited'`)
	}
	if got := countRateLimited(); got != 0 {
		t.Fatalf("rate_limited rows before any request = %d, want 0", got)
	}

	// Spend the failure budget without crossing it.
	var limitedRequests int
	for i := 0; i < 10; i++ {
		if code, _ := e.post(t, validBody(strings.Repeat("f", 64)), nil); code == http.StatusTooManyRequests {
			limitedRequests++
		}
	}
	if got := countRateLimited(); got != 0 {
		t.Fatalf("rate_limited rows before the transition = %d, want 0 — the row marks the "+
			"transition into the limited state, not an attempt", got)
	}

	// Cross it, and keep going well past it.
	for i := 0; i < 60; i++ {
		if code, _ := e.post(t, validBody(strings.Repeat("g", 64)), nil); code == http.StatusTooManyRequests {
			limitedRequests++
		}
	}
	if limitedRequests < 5 {
		t.Fatalf("only %d requests were rate limited; this test proves nothing", limitedRequests)
	}
	// One row per bucket per window (sentinel S-0004): 70 refusals from one
	// origin cross BOTH failure buckets — per-origin at the 11th, global at the
	// 61st — and each transition writes exactly one row naming its bucket. (This
	// asserted "exactly 1 row" in total while one site-wide marker was shared by
	// both buckets and every row said {"bucket":"failure"}.)
	if got := rateLimitedRows(t, e); got["per_origin"] != 1 || got["global"] != 1 || len(got) != 2 {
		t.Fatalf("rate_limited rows by bucket after the transitions = %v, want EXACTLY {per_origin: 1, global: 1}", got)
	}

	// Ten times more traffic must not add a second row for any bucket. It does
	// cross the claim route's hard ceiling (600 per window), which is a third
	// bucket with its own single row.
	refusedBefore := e.count(t, `SELECT count(*) FROM audit_events WHERE action='setup.owner_claim.refused'`)
	for i := 0; i < 600; i++ {
		e.post(t, validBody(strings.Repeat("h", 64)), nil)
	}
	if got := rateLimitedRows(t, e); got["per_origin"] != 1 || got["global"] != 1 || got["ceiling.claim"] != 1 || len(got) != 3 {
		t.Fatalf("rate_limited rows by bucket after 600 more requests = %v, want still exactly one per bucket: "+
			"{per_origin: 1, global: 1, ceiling.claim: 1}", got)
	}
	refusedAfter := e.count(t, `SELECT count(*) FROM audit_events WHERE action='setup.owner_claim.refused'`)
	if refusedAfter != refusedBefore {
		t.Fatalf("600 rate-limited requests added %d `refused` rows; a 429 writes none",
			refusedAfter-refusedBefore)
	}
	if got := e.count(t,
		`SELECT count(*) FROM audit_events WHERE after->>'reason' = 'rate_limited'`); got != 0 {
		t.Errorf("%d audit rows carry reason='rate_limited'; that shape was removed", got)
	}
}

// TestForwardedHeaderWithoutTrustedProxyYieldsNullIPPrefix.
//
// Behind a proxy with no trusted-proxy configuration there is no honest
// per-client identity. Writing the proxy's own private prefix would make every
// audit row attribute the attempt to the proxy and collapse the per-origin
// bucket into one shared by the whole internet. VIZRA_TRUSTED_PROXIES is M1-B's.
func TestForwardedHeaderWithoutTrustedProxyYieldsNullIPPrefix(t *testing.T) {
	e := newClaimEnv(t)
	e.mint(t)

	e.post(t, validBody(strings.Repeat("a", 64)), func(r *http.Request) {
		r.RemoteAddr = "172.18.0.5:44000" // the proxy's container address
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
	})
	var nulls, nonNull int64
	if err := e.pool.QueryRow(t.Context(),
		`SELECT count(*) FILTER (WHERE ip_prefix IS NULL), count(*) FILTER (WHERE ip_prefix IS NOT NULL)
		   FROM audit_events WHERE action='setup.owner_claim.refused'`).Scan(&nulls, &nonNull); err != nil {
		t.Fatal(err)
	}
	if nonNull != 0 {
		t.Errorf("%d refusal rows carry an ip_prefix behind an untrusted proxy; want NULL", nonNull)
	}
	if nulls == 0 {
		t.Fatal("no refusal row was written at all")
	}
}

// TestOwnerClaimSucceedsFromIPv6Loopback.
//
// Masking ::1 to /64 yields "::/64", which the frozen 0003 CHECK REFUSES. Since
// the audit insert shares the claim's transaction, a naive writer would abort
// the claim and the operator could never claim the instance from that address.
func TestOwnerClaimSucceedsFromIPv6Loopback(t *testing.T) {
	for _, addr := range []string{"[::1]:8080", "[::]:8080", "127.0.0.1:8080", "[::ffff:127.0.0.1]:8080", "garbage"} {
		t.Run(addr, func(t *testing.T) {
			e := newClaimEnv(t)
			token, _ := e.mint(t)
			code, body := e.post(t, validBody(token), func(r *http.Request) { r.RemoteAddr = addr })
			if code != http.StatusCreated {
				t.Fatalf("a claim from %s = %d, want 201. body=%v", addr, code, body)
			}
			if got := e.count(t, `SELECT count(*) FROM audit_events WHERE action='setup.owner_claim.succeeded'`); got != 1 {
				t.Fatalf("the audit row was not written for %s", addr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Out-of-band owner, clock, and degraded dependencies
// ---------------------------------------------------------------------------

// TestAClaimAgainstAnOutOfBandOwnerIs409NotFiveHundred.
//
// RENAMED from TestOwnerInsertConflictMapsTo409NotFiveHundred, which claimed
// more than it proved. An owner inserted out of band makes AnyUserExists true,
// so Claim returns ErrAlreadyClaimed before the token is ever read and
// `users_one_owner` never fires. The name, the comment and the ruling it was
// said to discharge all asserted an end-to-end path this test does not take.
//
// What it DOES prove is worth keeping and is what it is now named for: an owner
// appearing between the mint and the claim yields 409 and not a 5xx, with no
// owner duplicated, no orphan credential and the token unconsumed.
//
// The mapper's `users_one_owner` branch is covered instead by the synthetic
// *pgconn.PgError case in TestClaimErrorMapping (internal/httpapi/setup_test.go)
// and by MUT-17, which deletes that branch. Making the index fire through the
// handler is not reachable without a seam that would bypass Claim's own
// in-transaction gate — and that gate is the thing the slice must not weaken.
func TestAClaimAgainstAnOutOfBandOwnerIs409NotFiveHundred(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)

	insertOwner(t, e.pool, "outofband", "oob@example.org")

	code, body := e.post(t, validBody(token), nil)
	if code != http.StatusConflict {
		t.Fatalf("claiming against an out-of-band owner = %d, want 409. body=%v", code, body)
	}
	if bodyCode(body) != "conflict" {
		t.Errorf("code = %q, want conflict", bodyCode(body))
	}
	if got := e.count(t, `SELECT count(*) FROM users`); got != 1 {
		t.Fatalf("users = %d, want 1", got)
	}
	if got := e.count(t, `SELECT count(*) FROM credentials`); got != 0 {
		t.Fatalf("an orphan credential survived: %d", got)
	}
	if got := e.count(t, `SELECT count(*) FROM owner_claim_tokens WHERE consumed_at IS NOT NULL`); got != 0 {
		t.Fatal("the token was consumed by a refused claim")
	}
	if e.hasher.Derivations() != 0 {
		t.Errorf("the already-claimed path cost %d derivations, want 0", e.hasher.Derivations())
	}
}

// TestMintTimestampsComeFromTheDatabaseClock.
//
// The first round asserted `dbNow - minted_at` was within FIVE SECONDS, which a
// timestamp stamped from the Go host clock passes on any machine whose clocks
// agree — that is, every machine. It could not go red, so it was not evidence.
//
// This asserts the two stamps came from ONE statement's now(): minted_at and
// expires_at are written by the same INSERT as `now()` and `now() + ttl`, so
// `minted_at = expires_at - ttl` holds exactly, to the microsecond, and becomes
// false the instant either is supplied by the application. MUT-30 proves it.
func TestMintTimestampsComeFromTheDatabaseClock(t *testing.T) {
	e := newClaimEnv(t)

	assertOneStatement := func(t *testing.T, path string) {
		t.Helper()
		var sameStatement bool
		if err := e.pool.QueryRow(t.Context(),
			`SELECT minted_at = expires_at - $1::interval FROM owner_claim_tokens`,
			e.cfg.OwnerClaimTTL).Scan(&sameStatement); err != nil {
			t.Fatalf("comparing the two stamps: %v", err)
		}
		if !sameStatement {
			var minted, expires time.Time
			_ = e.pool.QueryRow(t.Context(),
				`SELECT minted_at, expires_at FROM owner_claim_tokens`).Scan(&minted, &expires)
			t.Fatalf("[%s] minted_at (%s) is not exactly expires_at - %s (%s): the two stamps "+
				"did not come from one statement's now(), so at least one is the application "+
				"host's clock", path, minted.UTC(), e.cfg.OwnerClaimTTL,
				expires.Add(-e.cfg.OwnerClaimTTL).UTC())
		}
	}

	// The INSERT path.
	e.mint(t)
	assertOneStatement(t, "first mint")

	// And the ON CONFLICT DO UPDATE path, which is a SEPARATE set of now() calls
	// and was exercised by no test — a skew introduced only there would have gone
	// unnoticed.
	e.mint(t)
	assertOneStatement(t, "re-mint")
	if got := e.count(t, `SELECT generation FROM owner_claim_tokens`); got != 2 {
		t.Fatalf("generation = %d after a re-mint, want 2 (the ON CONFLICT path did not run)", got)
	}
}

// TestAClaimSucceedsWhileTheCacheIsStopped: ADR-003 makes the limiter fail open
// to a per-process counter. That is right here, because the blast radius is
// bounded by a different mechanism entirely — users_one_owner means the endpoint
// can succeed exactly once whatever the limiter does. Failing closed would turn
// a cache outage into "the operator cannot claim their new instance".
func TestAClaimSucceedsWhileTheCacheIsStopped(t *testing.T) {
	cfg, resolver, pool := freshDatabase(t)
	dead, err := cache.Open("redis://127.0.0.1:1/0", "test")
	if err != nil {
		t.Fatalf("opening the unreachable cache: %v", err)
	}
	defer func() { _ = dead.Close() }()

	srv := httpapi.New(httpapi.Deps{
		Config:                cfg,
		Resolver:              resolver,
		Pools:                 poolsOf(t, resolver),
		Cache:                 dead,
		Limiter:               cache.NewFallbackLimiter(dead),
		EmbeddedSchemaVersion: mustEmbedded(t),
		Now:                   time.Now,
	})
	raw, _, err := ownerclaim.Mint(t.Context(), pool, cfg.OwnerClaimTTL, false, false)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	body, _ := json.Marshal(validBody(raw))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/claim-owner", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("a claim with the cache down = %d, want 201. body=%s", rec.Code, rec.Body.String())
	}
}

// TestUnclaimedInstanceRefusesEveryNonAllowlistedRoute over the real stack.
func TestUnclaimedInstanceRefusesEveryNonAllowlistedRoute(t *testing.T) {
	e := newClaimEnv(t)
	for _, p := range []string{"/does-not-exist", "/api/v1/anything", "/api/v1/setup"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s on an unclaimed instance = %d, want 403", p, rec.Code)
		}
	}
	for _, p := range []string{"/healthz", "/version", "/api/v1/setup/claim-status"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s on an unclaimed instance = %d, want 200", p, rec.Code)
		}
	}

	// Once claimed, ordinary routing resumes.
	token, _ := e.mint(t)
	if code, _ := e.post(t, validBody(token), nil); code != http.StatusCreated {
		t.Fatal("claim failed")
	}
	req := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("after claiming, an unknown route = %d, want 404", rec.Code)
	}
}

// TestEmailFoldIsProducedByPostgresOnEveryPath — a non-ASCII address through the
// real handler. M1-B must look this row up with PostgreSQL's lower(), never Go's.
func TestEmailFoldIsProducedByPostgresOnEveryPath(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)
	b := validBody(token)
	b.Email = "Öwner@Example.ORG"
	if code, body := e.post(t, b, nil); code != http.StatusCreated {
		t.Fatalf("claim = %d, want 201. body=%v", code, body)
	}
	var stored, fold string
	if err := e.pool.QueryRow(t.Context(), `SELECT email, email_fold FROM users`).Scan(&stored, &fold); err != nil {
		t.Fatal(err)
	}
	if stored != b.Email {
		t.Errorf("email stored as %q, want the submitted value %q", stored, b.Email)
	}
	var pgLower string
	if err := e.pool.QueryRow(t.Context(), `SELECT lower($1::text)`, b.Email).Scan(&pgLower); err != nil {
		t.Fatal(err)
	}
	if fold != pgLower {
		t.Fatalf("email_fold = %q but PostgreSQL's lower() gives %q; the fold must be generated, not supplied", fold, pgLower)
	}
}

var _ = audit.IPPrefix
var _ = sqlcgen.New

// ---------------------------------------------------------------------------
// Fix round 1 — F1…F5, N-1…N-6
// ---------------------------------------------------------------------------

// TestRepeatedClaimsOnAClaimedInstanceDoNotGrowTheAuditTrail (F3 / N-1).
//
// After day one every instance is claimed, so this is the path every anonymous
// POST takes for the rest of the product's life. Each one used to write a
// permanent row into a table migration 0005 makes undeletable — no DELETE, no
// TRUNCATE, no retention path — bounded only by 600 per 15 minutes, which is
// ~57,600 immutable rows a day, forever. A bound per window that integrates to
// unbounded over the life of an instance is not a bound.
func TestRepeatedClaimsOnAClaimedInstanceDoNotGrowTheAuditTrail(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)
	if code, _ := e.post(t, validBody(token), nil); code != http.StatusCreated {
		t.Fatal("the first claim must succeed")
	}
	before := e.count(t, `SELECT count(*) FROM audit_events`)
	// AcquireCount on the SERVER's pool: the cache short-circuit's whole job is
	// that a claimed instance stops doing database work per anonymous request.
	acquiresBefore := e.srvPool.Stat().AcquireCount()

	for i := 0; i < 100; i++ {
		b := validBody(token)
		b.Username = fmt.Sprintf("later%03d", i)
		b.Email = fmt.Sprintf("later%03d@example.org", i)
		code, body := e.post(t, b, nil)
		if code != http.StatusConflict {
			t.Fatalf("POST %d on a claimed instance = %d, want 409. body=%v", i, code, body)
		}
		if bodyCode(body) != "conflict" {
			t.Fatalf("POST %d code = %q, want conflict", i, bodyCode(body))
		}
		if msgOf(body) == "" {
			t.Fatalf("POST %d lost its message", i)
		}
	}

	after := e.count(t, `SELECT count(*) FROM audit_events`)
	if after != before {
		t.Fatalf("100 refused claims added %d audit rows (%d -> %d); a permanently closed "+
			"endpoint must not be an unauthenticated writer into an undeletable table",
			after-before, before, after)
	}
	if got := e.count(t,
		`SELECT count(*) FROM audit_events WHERE after->>'reason' = 'already_claimed'`); got != 0 {
		t.Errorf("%d already_claimed audit rows exist; that shape was removed", got)
	}
	if e.hasher.Derivations() != 1 {
		t.Errorf("derivations = %d, want exactly 1 (the successful claim only)", e.hasher.Derivations())
	}

	acquires := e.srvPool.Stat().AcquireCount() - acquiresBefore
	if acquires > 1 {
		t.Fatalf("100 refused claims acquired %d pooled connections; once the claimed bit is "+
			"cached the endpoint must stop touching the database entirely", acquires)
	}
}

// TestNoConnectionIsHeldWhileHashing (F4).
//
// argon2id costs 19 MiB and tens of milliseconds, and a caller that loses the
// semaphore race waits until its request deadline. Inside a transaction that
// meant every waiter pinned a pooled connection "idle in transaction" for the
// derivation plus the queue, so the POOL became the limit and the symptom
// pointed at PostgreSQL rather than at the hasher. M1-B's sign-in is instructed
// to import this hasher and would have copied the shape.
func TestNoConnectionIsHeldWhileHashing(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)

	// Watch the SERVER's pool, not the test's. Observing the wrong pool is how
	// this assertion was vacuous in the first place.
	var acquiredDuringHash int32 = -1
	e.hasher.before = func() {
		acquiredDuringHash = e.srvPool.Stat().AcquiredConns()
	}

	code, body := e.post(t, validBody(token), nil)
	if code != http.StatusCreated {
		t.Fatalf("claim = %d, want 201. body=%v", code, body)
	}
	if acquiredDuringHash < 0 {
		t.Fatal("the hasher was never called, so this test proved nothing")
	}
	if acquiredDuringHash != 0 {
		t.Fatalf("%d pooled connection(s) were checked out while hashing; the derivation must "+
			"happen outside the transaction", acquiredDuringHash)
	}
}

// TestConcurrentBootsMintExactlyOneToken (F2 / N-5).
//
// The liveness decision used to be read before the advisory lock, so two
// replicas cold-starting together both observed "no live token", both minted,
// and the second overwrote the first's digest in place. Two byte-identical-
// looking 64-hex lines then sat in the log, one dead, and the operator who
// picked the wrong one got the deliberately uninformative refusal.
func TestConcurrentBootsMintExactlyOneToken(t *testing.T) {
	e := newClaimEnv(t)

	// The verifier reproduced the original defect on only 1 run in 3, so a bare
	// pair of goroutines is not enough: the window is the gap between reading
	// liveness and taking the lock. This forces it open deterministically — a
	// third connection HOLDS the mint advisory lock while both boots start, so
	// both are guaranteed to have passed any pre-lock read before either can
	// proceed. Releasing it then runs them through the lock back to back.
	blocker, err := e.pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("opening the lock holder: %v", err)
	}
	if _, err := blocker.Exec(t.Context(), "SELECT pg_advisory_xact_lock(1)"); err != nil {
		t.Fatalf("taking the mint lock: %v", err)
	}

	const boots = 2
	var sinks [boots]strings.Builder
	var outs [boots]ownerclaim.BootOutcome
	var errs [boots]error
	var wg sync.WaitGroup
	for i := 0; i < boots; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i], errs[i] = ownerclaim.Boot(t.Context(), e.pool,
				ownerclaim.AnnounceStderr, e.cfg.OwnerClaimTTL, &sinks[i])
		}(i)
	}
	// Release only once BOTH boots are observably queued on the advisory lock.
	waitForLockWaiters(t, e.pool, boots)
	if err := blocker.Commit(t.Context()); err != nil {
		t.Fatalf("releasing the mint lock: %v", err)
	}
	wg.Wait()

	minted, printed := 0, 0
	for i := 0; i < boots; i++ {
		if errs[i] != nil {
			t.Fatalf("boot %d: %v", i, errs[i])
		}
		if outs[i].Minted {
			minted++
		}
		if hex64(sinks[i].String()) {
			printed++
		}
		if !strings.Contains(sinks[i].String(), "unclaimed") {
			t.Errorf("boot %d announced nothing useful: %q", i, sinks[i].String())
		}
	}
	if minted != 1 {
		t.Fatalf("%d of %d concurrent boots minted, want exactly 1", minted, boots)
	}
	if printed != 1 {
		t.Fatalf("%d of %d concurrent boots printed a credential, want exactly 1 — two "+
			"indistinguishable 64-hex lines in a log, one of them dead, is the operator "+
			"failure this guards", printed, boots)
	}
	if got := e.count(t, `SELECT count(*) FROM owner_claim_tokens`); got != 1 {
		t.Fatalf("token rows = %d, want 1", got)
	}
	if got := e.count(t, `SELECT generation FROM owner_claim_tokens`); got != 1 {
		t.Fatalf("generation = %d, want 1 — a second mint advanced it", got)
	}
	if got := e.count(t, `SELECT count(*) FROM audit_events WHERE action='setup.owner_claim.minted'`); got != 1 {
		t.Fatalf("mint audit rows = %d, want 1", got)
	}
}

// TestMintIsRefusedByTheDatabaseOnAClaimedInstance (N-6 / F-2).
//
// Both callers check first — the CLI under the lock, boot by branching earlier —
// so there is no live defect. The guard exists because this slice's thesis is
// that an invariant of this class belongs in the database, and a future
// owner-transfer or re-claim route is exactly where a Go-only guard breaks.
func TestMintIsRefusedByTheDatabaseOnAClaimedInstance(t *testing.T) {
	e := newClaimEnv(t)
	insertOwner(t, e.pool, "owner", "owner@example.org")

	// Call the generated query DIRECTLY, bypassing every Go-side check, so what
	// is under test is the statement and not the callers.
	_, err := sqlcgen.New(e.pool).MintOwnerClaimToken(t.Context(), sqlcgen.MintOwnerClaimTokenParams{
		TokenSha256: ownerclaim.Digest(strings.Repeat("a", 64)),
		Ttl:         pgtype.Interval{Microseconds: int64(time.Hour / time.Microsecond), Valid: true},
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("minting on a claimed instance returned %v, want pgx.ErrNoRows from the "+
			"statement's own WHERE NOT EXISTS (SELECT 1 FROM users)", err)
	}
	if got := e.count(t, `SELECT count(*) FROM owner_claim_tokens`); got != 0 {
		t.Fatalf("token rows = %d, want 0", got)
	}
}

// TestASupersedingRemintRecordsTheGenerationItKilled (F-3).
func TestASupersedingRemintRecordsTheGenerationItKilled(t *testing.T) {
	e := newClaimEnv(t)
	_, first := e.mint(t)
	_, second := e.mint(t)
	if second != first+1 {
		t.Fatalf("generations %d then %d, want consecutive", first, second)
	}
	if got := e.count(t,
		`SELECT count(*) FROM audit_events WHERE action='setup.owner_claim.superseded' AND subject_id=$1`,
		fmt.Sprintf("%d", first)); got != 1 {
		t.Fatalf("a re-mint must record the generation it killed; superseded rows for %d = %d", first, got)
	}
}

// TestOwnerClaimAnswers503WhenTheDatabaseIsDown (F1).
//
// MECHANISM, stated so this cannot decay into a skip: the server is built with a
// pool whose DSN points at a closed port (127.0.0.1:1), exactly as
// TestAClaimSucceedsWhileTheCacheIsStopped points the cache at redis://127.0.0.1:1/0.
// Every query therefore fails with a connection error, which is NOT a
// *pgconn.PgError and so reaches none of the constraint branches.
func TestOwnerClaimAnswers503WhenTheDatabaseIsDown(t *testing.T) {
	cfg, resolver, _ := freshDatabase(t)

	dead := *cfg
	dead.DatabaseURL = "postgres://vizra:vizra@127.0.0.1:1/vizra_test?sslmode=disable&connect_timeout=1"
	deadResolver := site.NewResolver(&dead)
	pools, err := db.Open(t.Context(), deadResolver)
	if err != nil {
		// FATAL, never SKIP. A lane that did not run is not a pass (AGENTS.md),
		// and core PR #9 lands executed-test floors with an EMPTY skip allowlist,
		// so a skip here would go red by name rather than quietly not proving the
		// 503 contract.
		t.Fatalf("this lane is BLOCKED, not skipped: the pool would not open against a closed port: %v", err)
	}
	t.Cleanup(pools.Close)

	srv := httpapi.New(httpapi.Deps{
		Config:                &dead,
		Resolver:              deadResolver,
		Pools:                 pools,
		EmbeddedSchemaVersion: mustEmbedded(t),
		// The unclaimed guard must not answer first: this test is about the
		// claim handler, not the middleware (which has its own 503 test).
		InstanceClaimed: func(context.Context) (bool, error) { return false, nil },
		Now:             time.Now,
	})

	body, _ := json.Marshal(validBody(strings.Repeat("a", 64)))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/claim-owner", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a claim against an unreachable database = %d, want 503. body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if bodyCode(out) != "unavailable" {
		t.Fatalf("code = %q, want unavailable (never internal_error, and never forbidden — a "+
			"database error must not be laundered into \"token not accepted\")", bodyCode(out))
	}
	_ = resolver
}

// TestClaimStatusIsServedFromTheMonotonicCacheOnceClaimed (N-2 / R-A).
//
// The endpoint used to call the UNCACHED path, so an unauthenticated, unlimited
// GET ran `SELECT EXISTS (SELECT 1 FROM users)` on every request for the life of
// the instance — on the only unauthenticated read surface the product has, with
// Cache-Control: no-store so nothing upstream absorbs a flood, and post-claim
// the answer is a constant.
//
// The count is taken through Deps.InstanceClaimed, the seam that already exists.
func TestClaimStatusIsServedFromTheMonotonicCacheOnceClaimed(t *testing.T) {
	cfg, resolver, _ := freshDatabase(t)
	var lookups int64
	srv := httpapi.New(httpapi.Deps{
		Config:                cfg,
		Resolver:              resolver,
		Pools:                 poolsOf(t, resolver),
		EmbeddedSchemaVersion: mustEmbedded(t),
		InstanceClaimed: func(context.Context) (bool, error) {
			atomic.AddInt64(&lookups, 1)
			return true, nil
		},
		Now: time.Now,
	})

	get := func() int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/setup/claim-status", nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < 100; i++ {
		if code := get(); code != http.StatusOK {
			t.Fatalf("GET %d = %d, want 200", i, code)
		}
	}
	if n := atomic.LoadInt64(&lookups); n != 1 {
		t.Fatalf("100 GETs performed %d claimed lookups; once the bit is true it is monotonic "+
			"and must be answered from the cache", n)
	}
}

// TestClaimStatusIsBoundedByTheHardCeiling (N-2 / R-A).
func TestClaimStatusIsBoundedByTheHardCeiling(t *testing.T) {
	e := newClaimEnv(t)
	// Drive past the STATUS route's own ceiling (3000), not the claim route's
	// (600). This test sent 700 when the two shared a bucket; after they were
	// split it no longer reached the ceiling at all and failed on correct code.
	limited := false
	for i := 0; i < 3100; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/setup/claim-status", nil)
		req.RemoteAddr = "203.0.113.42:51000"
		rec := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("3100 unauthenticated GETs were never rate limited; the status route must sit " +
			"under the same hard ceiling as the POST")
	}
}

// TestUsersOneOwnerFiresThroughTheHandler (verifier FINDING 9 / V7).
//
// The ruling asked for an integration test that forces `users_one_owner` to fire
// THROUGH the handler. Inserting a committed owner does not do it: Claim's own
// AnyUserExists sees that row and answers 409 before the token is read, so the
// index never fires. The technique that does work, and deterministically:
//
//   - open a second connection and INSERT an owner inside an UNCOMMITTED
//     transaction. Under READ COMMITTED the claim's AnyUserExists cannot see it,
//     so the claim proceeds past the gate;
//   - send the claim. Its INSERT blocks on the unique index, held by the
//     uncommitted row;
//   - commit the blocker. The claim's insert is released, loses, and raises
//     23505 on users_one_owner — the exact branch under test.
func TestUsersOneOwnerFiresThroughTheHandler(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)

	blocker, err := e.pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("opening the blocking transaction: %v", err)
	}
	blockerID, _ := uuid.NewV7()
	if _, err := blocker.Exec(t.Context(),
		`INSERT INTO users (id, username, email, role) VALUES ($1,'blocker','blocker@example.org','owner')`,
		blockerID); err != nil {
		t.Fatalf("inserting the uncommitted owner: %v", err)
	}

	type result struct {
		code int
		body map[string]any
	}
	done := make(chan result, 1)
	go func() {
		code, body := e.post(t, validBody(token), nil)
		done <- result{code, body}
	}()

	// Release only once the claim is observably blocked on the unique index.
	waitForLockWaiters(t, e.pool, 1)
	if err := blocker.Commit(t.Context()); err != nil {
		t.Fatalf("committing the blocker: %v", err)
	}

	var got result
	select {
	case got = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the claim never returned; it is still blocked on the index")
	}

	if got.code != http.StatusConflict {
		t.Fatalf("a claim losing on users_one_owner = %d, want 409. body=%v", got.code, got.body)
	}
	if bodyCode(got.body) != "conflict" {
		t.Errorf("code = %q, want conflict", bodyCode(got.body))
	}
	if msgOf(got.body) != "this instance already has an owner" {
		t.Errorf("message = %q; the two-owners case must read as an owner conflict, not as a "+
			"duplicate identifier", msgOf(got.body))
	}
	if n := e.count(t, `SELECT count(*) FROM users`); n != 1 {
		t.Fatalf("users = %d, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM credentials`); n != 0 {
		t.Fatalf("an orphan credential survived: %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM owner_claim_tokens WHERE consumed_at IS NOT NULL`); n != 0 {
		t.Fatal("the token was consumed by a claim that lost the race")
	}
}

// TestTheCredentialsCheckRefusesEveryNonArgon2idSecret (verifier FINDING 5 / V5).
//
// Loosening `credentials_password_is_argon2id` to `LIKE '%'` left the whole
// suite green: nothing tested the CHECK directly, because every test reaches it
// through a handler that only ever writes a real argon2id verifier. The CHECK's
// entire job is "never a plaintext value, never another scheme", so it needs a
// negative test of its own.
func TestTheCredentialsCheckRefusesEveryNonArgon2idSecret(t *testing.T) {
	e := newClaimEnv(t)
	insertOwner(t, e.pool, "owner", "owner@example.org")
	var userID uuid.UUID
	if err := e.pool.QueryRow(t.Context(), `SELECT id FROM users`).Scan(&userID); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, secret string }{
		{"plaintext", "hunter" + "2"},
		{"bcrypt", "$2y$" + "10$" + strings.Repeat("ab", 8)},
		{"scrypt", "$scrypt$" + "ln=16,r=8,p=1$" + strings.Repeat("c", 22)},
		{"argon2i, the wrong variant", "$argon2i$v=19$m=19456,t=2,p=1$" + strings.Repeat("d", 22)},
		{"sha256 hex", strings.Repeat("0", 64)},
		{"empty-ish", " "},
		{"a near miss", "argon2id$v=19$"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, _ := uuid.NewV7()
			_, err := e.pool.Exec(t.Context(),
				`INSERT INTO credentials (id,user_id,kind,secret) VALUES ($1,$2,'password',$3)`,
				id, userID, tc.secret)
			if err == nil {
				t.Fatalf("the database accepted a %s secret; credentials_password_is_argon2id "+
					"exists to refuse exactly this", tc.name)
			}
			if !strings.Contains(err.Error(), "credentials_password_is_argon2id") {
				t.Fatalf("refused for the wrong reason: %v", err)
			}
		})
	}

	// And the positive control, so the CHECK is not simply refusing everything.
	id, _ := uuid.NewV7()
	if _, err := e.pool.Exec(t.Context(),
		`INSERT INTO credentials (id,user_id,kind,secret) VALUES ($1,$2,'password',$3)`,
		id, userID, fakeVerifier()); err != nil {
		t.Fatalf("a real argon2id verifier must be accepted: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Fix round 2 — the regression round 1 introduced, and its neighbours
// ---------------------------------------------------------------------------

// TestAStatusFloodDoesNotStarveTheClaim (security NEW-1).
//
// Round 1 put claim-status under the hard ceiling, which was right, but through
// the SAME counter as the POST. A body-less GET needs no token, no body, no
// content type and no origin header, so 600 of them in a few seconds exhausted
// the window and the operator's POST carrying the CORRECT token was answered 429
// for the next fifteen minutes — repeatable indefinitely, on the very endpoint
// that advertises `claimed:false` to a scanner.
func TestAStatusFloodDoesNotStarveTheClaim(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)

	limited := false
	for i := 0; i < 4000 && !limited; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/setup/claim-status", nil)
		req.RemoteAddr = "203.0.113.42:51000"
		rec := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("the status route was never rate limited, so this test proves nothing")
	}

	// The operator's claim must still go through.
	code, body := e.post(t, validBody(token), nil)
	if code != http.StatusCreated {
		t.Fatalf("after a status flood exhausted its ceiling, the operator's VALID token was "+
			"answered %d — a stranger can deny the claim. body=%v", code, body)
	}
}

// TestAClaimFloodDoesNotSilenceTheStatusEndpoint — the reverse direction.
func TestAClaimFloodDoesNotSilenceTheStatusEndpoint(t *testing.T) {
	e := newClaimEnv(t)
	e.mint(t)

	limited := false
	for i := 0; i < 900 && !limited; i++ {
		if code, _ := e.post(t, validBody(strings.Repeat("a", 64)), nil); code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("the claim route was never rate limited, so this test proves nothing")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/setup/claim-status", nil)
	req.RemoteAddr = "203.0.113.42:51000"
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("after a claim flood, claim-status answered %d; the two routes must not share "+
			"a ceiling in either direction", rec.Code)
	}
}

// TestTheRedeemStatementRefusesADeadToken (FU-3, scoring MUT-6 and MUT-1b).
//
// Round 1 declared the CTE's expiry and consumption guards review-only, because
// the Go liveness pre-check refuses first and nothing reddened. Calling the
// generated query DIRECTLY bypasses every Go-side check, so the guards that are
// the enforcement of record become observable — two honest "MEASURED: nothing
// reddens" notes turn into two scored cases.
func TestTheRedeemStatementRefusesADeadToken(t *testing.T) {
	newIDs := func(t *testing.T) (uuid.UUID, uuid.UUID) {
		t.Helper()
		u, err := uuid.NewV7()
		if err != nil {
			t.Fatal(err)
		}
		c, err := uuid.NewV7()
		if err != nil {
			t.Fatal(err)
		}
		return u, c
	}

	for _, tc := range []struct {
		name string
		kill string
	}{
		{"expired", `UPDATE owner_claim_tokens SET minted_at = now() - interval '4h', expires_at = now() - interval '3h'`},
		{"already consumed", `UPDATE owner_claim_tokens SET consumed_at = now()`},
		{"superseded", `UPDATE owner_claim_tokens SET superseded_at = now()`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newClaimEnv(t)
			token, _ := e.mint(t)
			if _, err := e.pool.Exec(t.Context(), tc.kill); err != nil {
				t.Fatal(err)
			}
			userID, credID := newIDs(t)
			_, err := sqlcgen.New(e.pool).ClaimOwner(t.Context(), sqlcgen.ClaimOwnerParams{
				TokenSha256:  ownerclaim.Digest(token),
				UserID:       userID,
				Username:     "owner",
				Email:        "owner@example.org",
				CredentialID: credID,
				PasswordHash: fakeVerifier(),
			})
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("the redeem statement accepted a %s token (err=%v); the CTE's guard is "+
					"the enforcement of record and must refuse it whatever Go did first", tc.name, err)
			}
			if got := e.count(t, `SELECT count(*) FROM users`); got != 0 {
				t.Fatalf("a refused redeem created %d users", got)
			}
		})
	}
}

// TestTheRedeemStatementRefusesAClaimedInstance (FU-2).
//
// "An instance that already has users is implicitly claimed" was the one claim
// invariant enforced only in Go. The Go gate still decides the ANSWER (409 vs
// 403); this is the guarantee, and M1-B's registration route is the first thing
// that could race it.
func TestTheRedeemStatementRefusesAClaimedInstance(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)
	insertOwner(t, e.pool, "outofband", "oob@example.org")

	userID, _ := uuid.NewV7()
	credID, _ := uuid.NewV7()
	_, err := sqlcgen.New(e.pool).ClaimOwner(t.Context(), sqlcgen.ClaimOwnerParams{
		TokenSha256:  ownerclaim.Digest(token),
		UserID:       userID,
		Username:     "second",
		Email:        "second@example.org",
		CredentialID: credID,
		PasswordHash: fakeVerifier(),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the redeem statement ran on an instance that already has users (err=%v)", err)
	}
	if got := e.count(t, `SELECT count(*) FROM users`); got != 1 {
		t.Fatalf("users = %d, want 1", got)
	}
	if got := e.count(t, `SELECT count(*) FROM owner_claim_tokens WHERE consumed_at IS NOT NULL`); got != 0 {
		t.Fatal("a refused redeem consumed the token")
	}
}

// TestADatabaseOutageIsDiagnosableFromTheLog (security NEW-2, first half).
//
// The canned 503 body tells an attacker nothing, which is right — but round 1
// dropped the wrapped cause entirely, so a database outage on the one endpoint
// an operator cannot skip left no trace beyond "the instance state could not be
// read". The cause is now logged once, redacted, with the request id.
func TestADatabaseOutageIsDiagnosableFromTheLog(t *testing.T) {
	cfg, _, _ := freshDatabase(t)

	dead := *cfg
	const password = "s3kr1t-not-a-real-password"
	dead.DatabaseURL = "postgres://vizra:" + password + "@127.0.0.1:1/vizra_test?sslmode=disable&connect_timeout=1"
	deadResolver := site.NewResolver(&dead)
	pools, err := db.Open(t.Context(), deadResolver)
	if err != nil {
		t.Fatalf("this lane is BLOCKED, not skipped: the pool would not open: %v", err)
	}
	t.Cleanup(pools.Close)

	logs := &safeBuffer{}
	srv := httpapi.New(httpapi.Deps{
		Config:                &dead,
		Resolver:              deadResolver,
		Pools:                 pools,
		EmbeddedSchemaVersion: mustEmbedded(t),
		InstanceClaimed:       func(context.Context) (bool, error) { return false, nil },
		// A PLAIN handler: the redacting handler would hide a leak rather than
		// prove there is none.
		Logger: obs.NewLogger(logs, false),
		Now:    time.Now,
	})

	body, _ := json.Marshal(validBody(strings.Repeat("a", 64)))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/claim-owner", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("claim against a dead database = %d, want 503. body=%s", rec.Code, rec.Body.String())
	}
	out := logs.String()
	if !strings.Contains(out, "could not reach the database") {
		t.Fatalf("the 503 left nothing diagnosable in the log: %s", firstRunes(out, 400))
	}
	// ONCE. The shared error handler also writes a generic line for every 5xx,
	// but that line carries only the canned client message; the CAUSE must be
	// written exactly once, at the point the 503 is decided.
	if n := strings.Count(out, "could not reach the database"); n != 1 {
		t.Errorf("the cause was logged %d times, want exactly once: %s", n, firstRunes(out, 600))
	}
	if !strings.Contains(out, "request_id") {
		t.Error("the log line carries no request id, so it cannot be correlated with the caller's 503")
	}
	// And the cause must not drag the DSN or its password in with it.
	if strings.Contains(out, password) {
		t.Errorf("the database password reached a log line: %s", firstRunes(out, 400))
	}
	if strings.Contains(out, dead.DatabaseURL) {
		t.Errorf("the DSN reached a log line: %s", firstRunes(out, 400))
	}
}

// TestTheClaimTransactionPinsReadCommitted.
//
// The claim transaction pins READ COMMITTED explicitly rather than inheriting
// `default_transaction_isolation`, which an operator, a managed provider or a
// pooler can set. Under REPEATABLE READ a claimant that has already passed the
// in-transaction gate and then blocks on the redeem's row lock gets 40001
// `could not serialize access`, which no branch maps — a 500 on exactly the race
// the ledger's negative case names, and only in production.
//
// After the read/write split that window became narrow enough that the ordinary
// race test no longer reaches it (most losers are refused by a gate before they
// ever touch the lock), so this forces it deterministically: a third connection
// holds the token row locked until BOTH claimants are past every gate and
// queued on that exact row.
func TestTheClaimTransactionPinsReadCommitted(t *testing.T) {
	e := newClaimEnv(t)

	dbName := quoteIdent(currentDatabase(t, e.pool))
	if _, err := e.pool.Exec(t.Context(), fmt.Sprintf(
		"ALTER DATABASE %s SET default_transaction_isolation = %q", dbName, "repeatable read")); err != nil {
		t.Fatalf("setting the server default isolation: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), fmt.Sprintf(
			"ALTER DATABASE %s RESET default_transaction_isolation", dbName))
	})
	e.pool.Reset()

	token, _ := e.mint(t)

	// Lock the token row so every claimant queues on it rather than racing.
	blocker, err := e.pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("opening the blocking transaction: %v", err)
	}
	var locked bool
	if err := blocker.QueryRow(t.Context(),
		`SELECT true FROM owner_claim_tokens WHERE id FOR UPDATE`).Scan(&locked); err != nil {
		t.Fatalf("locking the token row: %v", err)
	}

	const claimants = 2
	codes := make([]int, claimants)
	bodies := make([]map[string]any, claimants)
	var wg sync.WaitGroup
	for i := 0; i < claimants; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b := validBody(token)
			b.Username = fmt.Sprintf("racer%d", i)
			b.Email = fmt.Sprintf("racer%d@example.org", i)
			codes[i], bodies[i] = e.post(t, b, nil)
		}(i)
	}
	// Release only once BOTH claimants are observably queued on the locked row —
	// that is, past the read phase and past the in-transaction gate.
	waitForLockWaiters(t, e.pool, claimants)
	if err := blocker.Commit(t.Context()); err != nil {
		t.Fatalf("releasing the token row: %v", err)
	}
	wg.Wait()

	created, declined := 0, 0
	for i, c := range codes {
		switch {
		case c == http.StatusCreated:
			created++
		// 409 exactly. The loser here passed every gate, blocked on the token row,
		// and got no row from the redeem once the winner consumed it — so its
		// answer is decided by the 409-vs-403 RE-READ, and the instance is claimed.
		case c == http.StatusConflict:
			declined++
		default:
			t.Errorf("claimant %d got %d (%v) — want 201 or 409. Under a REPEATABLE READ server default a "+
				"claimant blocked on the redeem raises 40001, which no branch maps; the claim "+
				"transaction must pin READ COMMITTED itself", i, c, bodies[i])
		}
	}
	if created != 1 {
		t.Fatalf("%d claimants created the owner, want exactly 1", created)
	}
	if declined != claimants-1 {
		t.Fatalf("%d claimants were cleanly declined, want %d", declined, claimants-1)
	}
	if got := e.count(t, `SELECT count(*) FROM users WHERE role='owner' AND tombstoned_at IS NULL`); got != 1 {
		t.Fatalf("live owners = %d, want 1", got)
	}
}

// TestABootRaceWithTheFirstAccountReportsClaimedNotDegraded (FU-1).
//
// If an account appears between Boot's AnyUserExists and the mint statement,
// the statement's own WHERE NOT EXISTS (SELECT 1 FROM users) refuses and Mint
// returns ErrHasUsers. That is a benign race whose correct outcome is
// "claimed". Reporting it as degraded readiness plus an error would tell the
// operator their instance is broken at the moment it was successfully claimed.
//
// Forced deterministically: a third connection holds the mint advisory lock, so
// Boot passes its own checks and queues inside Mint; the account is committed
// while it waits; then the lock is released.
func TestABootRaceWithTheFirstAccountReportsClaimedNotDegraded(t *testing.T) {
	e := newClaimEnv(t)

	blocker, err := e.pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("opening the lock holder: %v", err)
	}
	if _, err := blocker.Exec(t.Context(), "SELECT pg_advisory_xact_lock(1)"); err != nil {
		t.Fatalf("taking the mint lock: %v", err)
	}

	type result struct {
		out ownerclaim.BootOutcome
		err error
	}
	done := make(chan result, 1)
	var sink strings.Builder
	go func() {
		out, err := ownerclaim.Boot(t.Context(), e.pool, ownerclaim.AnnounceStderr, e.cfg.OwnerClaimTTL, &sink)
		done <- result{out, err}
	}()

	// Boot must be past AnyUserExists (no users yet) and observably queued on the
	// advisory lock before the account appears.
	waitForLockWaiters(t, e.pool, 1)
	insertOwner(t, e.pool, "first", "first@example.org")
	if err := blocker.Commit(t.Context()); err != nil {
		t.Fatalf("releasing the mint lock: %v", err)
	}

	var got result
	select {
	case got = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Boot never returned")
	}
	if got.err != nil {
		t.Fatalf("a benign race with the first account returned an error: %v", got.err)
	}
	if got.out.Degraded {
		t.Fatalf("a benign race with the first account reported DEGRADED readiness: %+v", got.out)
	}
	if !got.out.Claimed {
		t.Fatalf("the instance has an account, so Boot must report Claimed: %+v", got.out)
	}
	if hex64(sink.String()) {
		t.Fatal("Boot printed a credential for an instance that is already claimed")
	}
	if n := e.count(t, `SELECT count(*) FROM owner_claim_tokens`); n != 0 {
		t.Fatalf("token rows = %d, want 0 — nothing may be minted on a claimed instance", n)
	}
}

// TestAConnectionKilledMidClaimAnswers503NotFiveHundred.
//
// Round 1 added ErrUnavailable so a database outage answers 503, but it wrapped
// only NON-PgError failures, deliberately leaving *pgconn.PgError unwrapped so
// the mapper can key on constraint codes. A server-SIGNALLED outage also arrives
// as a *pgconn.PgError — 57P01 admin shutdown, 57P03 cannot-connect-now, 53300
// too-many-connections, class 08 — and matched no branch, so it fell through to
// a 500. And the audit insert inside the claim transaction was not wrapped at all.
//
// Forced deterministically: a blocker holds an ACCESS EXCLUSIVE lock on
// audit_events, so the claim runs its redeem and then queues on the audit insert
// INSIDE its transaction; the claim's backend is then terminated, which the
// server reports as 57P01.
func TestAConnectionKilledMidClaimAnswers503NotFiveHundred(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)

	blocker, err := e.pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("opening the blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	if _, err := blocker.Exec(t.Context(), `LOCK TABLE audit_events IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("locking audit_events: %v", err)
	}

	type result struct {
		code int
		body map[string]any
	}
	done := make(chan result, 1)
	go func() {
		code, body := e.post(t, validBody(token), nil)
		done <- result{code, body}
	}()

	// The claim is inside its transaction, queued on the audit insert.
	waitForLockWaiters(t, e.pool, 1)
	var killed bool
	if err := e.pool.QueryRow(t.Context(), `
		SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		 WHERE datname = current_database() AND pid <> pg_backend_pid()
		   AND wait_event_type = 'Lock' AND query ILIKE '%audit_events%'
		 LIMIT 1`).Scan(&killed); err != nil || !killed {
		t.Fatalf("could not terminate the claim's backend (killed=%v): %v", killed, err)
	}
	_ = blocker.Rollback(context.Background())

	var got result
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the claim never returned")
	}
	if got.code != http.StatusServiceUnavailable {
		t.Fatalf("a connection killed mid-claim answered %d, want 503 — a server-signalled "+
			"outage is still an outage. body=%v", got.code, got.body)
	}
	if bodyCode(got.body) != "unavailable" {
		t.Errorf("code = %q, want unavailable", bodyCode(got.body))
	}
	// And the transaction left nothing behind.
	if n := e.count(t, `SELECT count(*) FROM users`); n != 0 {
		t.Fatalf("users = %d after an aborted claim, want 0", n)
	}
	if n := e.count(t, `SELECT count(*) FROM owner_claim_tokens WHERE consumed_at IS NOT NULL`); n != 0 {
		t.Fatal("the token was consumed by a claim whose transaction never committed")
	}
}

// ---------------------------------------------------------------------------
// Fix round 2 — the verifier's R2-C and R2-D
// ---------------------------------------------------------------------------

// newColdServer builds a SECOND server over the same database: a freshly started
// process, whose claimed cache is empty whatever the database says.
func newColdServer(t *testing.T, e *claimEnv) (*httpapi.Server, *pgxpool.Pool) {
	t.Helper()
	cacheClient, err := cache.Open(e.cfg.CacheURL, e.cfg.CacheNamespace)
	if err != nil {
		t.Fatalf("opening the cache: %v", err)
	}
	t.Cleanup(func() { _ = cacheClient.Close() })
	pools := poolsOf(t, e.resolver)
	srv := httpapi.New(httpapi.Deps{
		Config:                e.cfg,
		Resolver:              e.resolver,
		Pools:                 pools,
		Cache:                 cacheClient,
		Limiter:               cache.NewFallbackLimiter(cacheClient),
		EmbeddedSchemaVersion: mustEmbedded(t),
		Hasher:                e.hasher,
		Logger:                obs.NewLogger(e.logs, false),
		Now:                   time.Now,
	})
	return srv, pools.Default()
}

// TestAColdCacheOnAClaimedInstanceAnswers409WithoutAuditRowsForAnyBody (R2-C).
//
// The round-1 short-circuit only fired when the cached bit was ALREADY true,
// and nothing primed it at boot. Claim then validated the token's SHAPE before
// reading the claimed state, so on a freshly restarted process over an instance
// claimed long ago, a malformed token was refused as a token problem: 403
// instead of the ruled 409, a failure-budget charge, and a permanent `refused`
// audit row per request — and the cache never warmed. The verifier measured 11
// permanent rows from 15 POSTs. Bots do not poll the status page first.
func TestAColdCacheOnAClaimedInstanceAnswers409WithoutAuditRowsForAnyBody(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)
	if code, _ := e.post(t, validBody(token), nil); code != http.StatusCreated {
		t.Fatal("setup claim failed")
	}

	bodies := []struct {
		name        string
		contentType string
		body        string
	}{
		{"malformed token", "application/json", `{"token":"x","username":"owner","email":"owner@example.org","password":"` + testPassphrase() + `"}`},
		{"well-formed wrong token", "application/json", `{"token":"` + strings.Repeat("b", 64) + `","username":"owner","email":"owner@example.org","password":"` + testPassphrase() + `"}`},
		{"empty token", "application/json", `{"token":"","username":"owner","email":"owner@example.org","password":"` + testPassphrase() + `"}`},
		{"empty body", "application/json", ``},
		{"unknown field", "application/json", `{"token":"x","role":"owner"}`},
		{"not JSON at all", "text/plain", `hello`},
	}
	send := func(srv *httpapi.Server, contentType, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/claim-owner", strings.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		req.RemoteAddr = "203.0.113.42:51000"
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	auditBefore := e.count(t, `SELECT count(*) FROM audit_events`)
	derivationsBefore := e.hasher.Derivations()

	// EACH body as the FIRST request of its own freshly started process. Sending
	// them in sequence to one server would let whichever body happens to warm the
	// cache mask the rest — the first version of this test did exactly that, so
	// a regression to a warm-only check stayed green behind it.
	for _, b := range bodies {
		cold, _ := newColdServer(t, e)
		if rec := send(cold, b.contentType, b.body); rec.Code != http.StatusConflict {
			t.Errorf("[%s] as the first request after a restart, a CLAIMED instance answered %d, "+
				"want 409 whatever the body (OQ-4: the claimed check strictly precedes any token "+
				"examination). body=%s", b.name, rec.Code, rec.Body.String())
		}
	}

	// And once warm, the process stops touching the database at all.
	warm, warmPool := newColdServer(t, e)
	send(warm, bodies[0].contentType, bodies[0].body)
	acquiresBefore := warmPool.Stat().AcquireCount()
	for round := 0; round < 3; round++ {
		for _, b := range bodies {
			if rec := send(warm, b.contentType, b.body); rec.Code != http.StatusConflict {
				t.Fatalf("[%s, warm] a claimed instance answered %d, want 409", b.name, rec.Code)
			}
		}
	}
	if acquires := warmPool.Stat().AcquireCount() - acquiresBefore; acquires != 0 {
		t.Fatalf("18 POSTs to a warm claimed instance acquired %d pooled connections, want 0", acquires)
	}

	if got := e.count(t, `SELECT count(*) FROM audit_events`) - auditBefore; got != 0 {
		t.Fatalf("%d permanent audit rows were written by refused POSTs to a claimed instance; a "+
			"permanently closed endpoint must write none", got)
	}
	if got := e.hasher.Derivations() - derivationsBefore; got != 0 {
		t.Fatalf("refused POSTs to a claimed instance cost %d derivations, want 0", got)
	}
}

// TestClaimReadsTheClaimedStateBeforeExaminingTheToken (R2-C, the library).
//
// The handler now answers a claimed instance before calling Claim at all, which
// would hide the library's own ordering. Claim is also what M1-B and any future
// caller will reuse, so its contract is tested directly: on a claimed instance
// a MALFORMED token must come back as ErrAlreadyClaimed, not ErrTokenNotAccepted.
func TestClaimReadsTheClaimedStateBeforeExaminingTheToken(t *testing.T) {
	e := newClaimEnv(t)
	insertOwner(t, e.pool, "owner", "owner@example.org")

	for _, tok := range []string{"x", "", strings.Repeat("z", 64), strings.Repeat("a", 63)} {
		_, err := ownerclaim.Claim(t.Context(), e.pool, e.hasher, ownerclaim.Input{
			Token: tok, Username: "someone", Email: "someone@example.org", Password: testPassphrase(),
		}, "", nil)
		if !errors.Is(err, ownerclaim.ErrAlreadyClaimed) {
			t.Errorf("token %q on a claimed instance: got %v, want ErrAlreadyClaimed — the claimed "+
				"check must precede any examination of the token, including its shape", tok, err)
		}
	}
	if e.hasher.Derivations() != 0 {
		t.Errorf("%d derivations on a claimed instance, want 0", e.hasher.Derivations())
	}
}

// TestAClaimRefusesWhenAUserAppearsDuringTheHash (R2-D).
//
// The read phase checks the claimed state OUTSIDE any transaction, then hashes.
// A user committed during that hash must not end with an owner being created on
// an instance that already had a user. The verifier's probe measured exactly
// that — HTTP 201 and a new owner — with the in-transaction gate deleted, before
// the redeem statement carried its own users guard.
//
// The user is committed from inside the hasher's `before` hook, so it lands
// deterministically after the read phase and before the write transaction.
func TestAClaimRefusesWhenAUserAppearsDuringTheHash(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)

	e.hasher.before = func() {
		// A MEMBER, not an owner: users_one_owner cannot stop this case.
		id, _ := uuid.NewV7()
		if _, err := e.pool.Exec(context.Background(),
			`INSERT INTO users (id, username, email, role) VALUES ($1,'member','member@example.org','member')`,
			id); err != nil {
			t.Errorf("committing the member mid-hash: %v", err)
		}
	}
	auditBefore := e.count(t, `SELECT count(*) FROM audit_events`)

	code, body := e.post(t, validBody(token), nil)
	if code != http.StatusConflict {
		t.Fatalf("a user appearing during the hash yielded %d, want 409 (the instance is claimed "+
			"the moment it has any user). body=%v", code, body)
	}
	if n := e.count(t, `SELECT count(*) FROM users WHERE role='owner'`); n != 0 {
		t.Fatalf("an owner was created on an instance that already had a user (owners=%d)", n)
	}
	if n := e.count(t, `SELECT count(*) FROM owner_claim_tokens WHERE consumed_at IS NOT NULL`); n != 0 {
		t.Fatal("the token was consumed by a refused claim")
	}
	if n := e.count(t, `SELECT count(*) FROM credentials`); n != 0 {
		t.Fatalf("an orphan credential survived: %d", n)
	}
	if got := e.count(t, `SELECT count(*) FROM audit_events`) - auditBefore; got != 0 {
		t.Fatalf("a refused claim on a now-claimed instance wrote %d audit rows, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Closing slice — a refusal decided in the read phase is classified against a
// FRESH claimed state (CI run 35814919455, valkey leg, seed 1790134723139270269)
// ---------------------------------------------------------------------------

// pauseFirstClaimantAfterItsClaimedCheck installs ownerclaim's test seam so the
// FIRST claimant to pass Claim's read-phase claimed check stops there — before
// any examination of its token — until release is called. Every later claimant
// passes straight through. This is the window the race test hit once in CI and
// never locally: it is now opened on purpose.
func pauseFirstClaimantAfterItsClaimedCheck(t *testing.T) (paused <-chan struct{}, release func()) {
	t.Helper()
	p := make(chan struct{})
	r := make(chan struct{})
	var first sync.Once
	restore := ownerclaim.SetAfterClaimedCheckHookForTest(func() {
		isFirst := false
		first.Do(func() { isFirst = true })
		if !isFirst {
			return
		}
		close(p)
		select {
		case <-r:
		case <-time.After(60 * time.Second): // never hang the suite
		}
	})
	var once sync.Once
	rel := func() { once.Do(func() { close(r) }) }
	t.Cleanup(func() { rel(); restore() })
	return p, rel
}

func waitPaused(t *testing.T, paused <-chan struct{}) {
	t.Helper()
	select {
	case <-paused:
	case <-time.After(60 * time.Second):
		t.Fatal("the claimant never reached the seam after its claimed check; the window this " +
			"test exists for was never opened, so it must not pass")
	}
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

// readPhaseBranch is one way the read phase can refuse a token. `present` is
// what the paused claimant sends, given the live token (empty when none was
// minted).
type readPhaseBranch struct {
	name    string
	mint    bool
	present func(live string) string
}

func readPhaseBranches() []readPhaseBranch {
	return []readPhaseBranch{
		// The CI failure exactly: the loser holds the SAME token as the winner,
		// and by the time it reads the row the winner has consumed it.
		{"not live (consumed by the winner)", true, func(live string) string { return live }},
		{"digest mismatch", true, func(string) string { return strings.Repeat("b", 64) }},
		{"malformed shape", true, func(string) string { return "x" }},
		{"no token row (never minted)", false, func(string) string { return strings.Repeat("c", 64) }},
	}
}

// TestAReadPhaseRefusalOnAnInstanceThatBecameClaimedAnswers409 (closing slice).
//
// For EACH read-phase refusal branch: claimant A passes the claimed check while
// the instance is unclaimed and is paused there. The instance then becomes
// claimed — through the endpoint where a token exists, out of band where none
// was ever minted — and A is released. A must answer OQ-4's 409, write no
// audit row, charge no failure budget, cost no derivation, and leave the
// monotonic claimed bit set.
//
// Before the fix every sub-test answered 403 `forbidden`, wrote a `refused`
// row and charged the budget: the read phase refused on a stale snapshot.
func TestAReadPhaseRefusalOnAnInstanceThatBecameClaimedAnswers409(t *testing.T) {
	for _, br := range readPhaseBranches() {
		t.Run(br.name, func(t *testing.T) {
			e := newClaimEnv(t)
			live := ""
			if br.mint {
				live, _ = e.mint(t)
			}
			paused, release := pauseFirstClaimantAfterItsClaimedCheck(t)

			refusedBefore := e.count(t, `SELECT count(*) FROM audit_events WHERE action='setup.owner_claim.refused'`)
			chargesBefore := e.limiter.failureCharges()

			a := validBody(br.present(live))
			a.Username, a.Email = "loser", "loser@example.org"
			outcome := e.postAsync(t, a, nil)
			waitPaused(t, paused)

			// The instance becomes claimed while A is paused.
			derivationsBefore := e.hasher.Derivations()
			if br.mint {
				if code, body := e.post(t, validBody(live), nil); code != http.StatusCreated {
					t.Fatalf("the winner's claim answered %d, want 201. body=%v", code, body)
				}
			} else {
				insertOwner(t, e.pool, "oob", "oob@example.org")
			}
			winnerDerivations := e.hasher.Derivations() - derivationsBefore

			release()
			got := await(t, outcome)

			if got.code != http.StatusConflict || bodyCode(got.body) != "conflict" {
				t.Fatalf("A answered %d %v, want 409 conflict: the instance is claimed, and a refusal "+
					"decided on the read phase's stale snapshot must be classified against a fresh one "+
					"(OQ-4)", got.code, got.body)
			}
			if n := e.count(t, `SELECT count(*) FROM audit_events WHERE action='setup.owner_claim.refused'`) - refusedBefore; n != 0 {
				t.Fatalf("A wrote %d `refused` audit row(s); a claimed instance writes none", n)
			}
			if n := e.limiter.failureCharges() - chargesBefore; n != 0 {
				t.Fatalf("A charged the failure budget %d time(s); the server's race is not the caller's failure", n)
			}
			if d := e.hasher.Derivations() - derivationsBefore - winnerDerivations; d != 0 {
				t.Fatalf("A cost %d derivation(s), want 0", d)
			}
			if n := e.count(t, `SELECT count(*) FROM users`); n != 1 {
				t.Fatalf("users = %d, want exactly 1", n)
			}

			// The monotonic claimed bit is set: the status read answers claimed
			// without touching the pool. (Where the winner came through the
			// endpoint its 201 set it too; the out-of-band branch is the one that
			// proves A's own 409 did.)
			acquires := e.srvPool.Stat().AcquireCount()
			if code, body := e.getStatus(t); code != http.StatusOK || body["claimed"] != true {
				t.Fatalf("claim-status after A = %d %v, want 200 claimed:true", code, body)
			}
			if n := e.srvPool.Stat().AcquireCount() - acquires; n != 0 {
				t.Fatalf("claim-status acquired %d pooled connection(s) after A's 409; the claimed bit "+
					"was not set", n)
			}
		})
	}
}

// TestAReadPhaseRefusalOnAnUnclaimedInstanceIsStillTheUniform403 (closing slice).
//
// The other half of the classification, through the same seam: when NOTHING
// claims the instance while A is paused, every branch is still the uniform 403
// with exactly one `refused` row and one failure-budget charge. This is what
// stops "classify" decaying into "always 409", and what proves the charge
// counter used above is not vacuous.
func TestAReadPhaseRefusalOnAnUnclaimedInstanceIsStillTheUniform403(t *testing.T) {
	type unclaimedBranch struct {
		name    string
		mint    bool
		present func(live string) string
		// during runs while A is paused; the instance stays unclaimed.
		during func(t *testing.T, e *claimEnv)
	}
	same := func(live string) string { return live }
	branches := []unclaimedBranch{
		{"digest mismatch", true, func(string) string { return strings.Repeat("b", 64) }, nil},
		{"malformed shape", true, func(string) string { return "x" }, nil},
		{"no token row (never minted)", false, func(string) string { return strings.Repeat("c", 64) }, nil},
		{"not live (expired while paused)", true, same, func(t *testing.T, e *claimEnv) {
			if _, err := e.pool.Exec(t.Context(),
				`UPDATE owner_claim_tokens SET minted_at = now() - interval '4h', expires_at = now() - interval '3h'`); err != nil {
				t.Fatal(err)
			}
		}},
		// A re-mint overwrites the digest in place, so A's token then fails the
		// digest compare: superseded is indistinguishable from mistyped, by design.
		{"superseded while paused", true, same, func(t *testing.T, e *claimEnv) { e.mint(t) }},
	}
	for _, br := range branches {
		t.Run(br.name, func(t *testing.T) {
			e := newClaimEnv(t)
			live := ""
			if br.mint {
				live, _ = e.mint(t)
			}
			paused, release := pauseFirstClaimantAfterItsClaimedCheck(t)
			refusedBefore := e.count(t, `SELECT count(*) FROM audit_events WHERE action='setup.owner_claim.refused'`)
			chargesBefore := e.limiter.failureCharges()

			outcome := e.postAsync(t, validBody(br.present(live)), nil)
			waitPaused(t, paused)
			if br.during != nil {
				br.during(t, e)
			}
			release()
			got := await(t, outcome)

			if got.code != http.StatusForbidden || bodyCode(got.body) != "forbidden" || msgOf(got.body) != "that claim token was not accepted" {
				t.Fatalf("A answered %d %v, want the uniform 403 forbidden", got.code, got.body)
			}
			if n := e.count(t, `SELECT count(*) FROM audit_events WHERE action='setup.owner_claim.refused'`) - refusedBefore; n != 1 {
				t.Fatalf("`refused` rows written = %d, want exactly 1", n)
			}
			if n := e.limiter.failureCharges() - chargesBefore; n != 1 {
				t.Fatalf("failure-budget charges = %d, want exactly 1", n)
			}
			if n := e.count(t, `SELECT count(*) FROM users`); n != 0 {
				t.Fatalf("users = %d, want 0", n)
			}
		})
	}
}

// TestAFailedClassificationReadAnswers503AndChargesNothing (closing slice).
//
// The classification's own read can fail. That is the server's failure and
// answers 503 — never "token not accepted", never an audit row, never a budget
// charge. Forced deterministically: while A is paused, another session takes
// ACCESS EXCLUSIVE on `users`; A's token read (a different table) proceeds and
// refuses, its classification blocks on `users`, and the test cancels A's
// request once pg_stat_activity shows it waiting there.
func TestAFailedClassificationReadAnswers503AndChargesNothing(t *testing.T) {
	e := newClaimEnv(t)
	e.mint(t)
	paused, release := pauseFirstClaimantAfterItsClaimedCheck(t)
	refusedBefore := e.count(t, `SELECT count(*) FROM audit_events WHERE action='setup.owner_claim.refused'`)
	chargesBefore := e.limiter.failureCharges()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outcome := e.postAsync(t, validBody(strings.Repeat("d", 64)), func(r *http.Request) {
		*r = *r.WithContext(ctx)
	})
	waitPaused(t, paused)

	locker, err := e.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locker.Rollback(context.Background()) }()
	if _, err := locker.Exec(t.Context(), `LOCK TABLE users IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("locking users: %v", err)
	}
	release()
	waitForLockWaiters(t, e.pool, 1) // A's classification is now blocked on `users`
	cancel()
	got := await(t, outcome)
	if err := locker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	if got.code != http.StatusServiceUnavailable || bodyCode(got.body) != "unavailable" {
		t.Fatalf("a classification read that could not complete answered %d %v, want 503 unavailable",
			got.code, got.body)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_events WHERE action='setup.owner_claim.refused'`) - refusedBefore; n != 0 {
		t.Fatalf("an outage wrote %d `refused` row(s)", n)
	}
	if n := e.limiter.failureCharges() - chargesBefore; n != 0 {
		t.Fatalf("an outage charged the caller's failure budget %d time(s)", n)
	}
}

// TestATokenSupersededDuringTheHashIsTheUniform403 (closing slice).
//
// The post-redeem path goes through the SAME classification. Here the token is
// superseded while A hashes (after the liveness pre-check), so the redeem finds
// no row on an instance that is still unclaimed: the answer is the uniform 403,
// with its audit row and budget charge — the "went terminal in that window"
// case AGENTS.md names. (The claimed half of this path is the loser in
// TestTheClaimTransactionPinsReadCommitted.)
func TestATokenSupersededDuringTheHashIsTheUniform403(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)
	e.hasher.before = func() {
		if _, _, err := ownerclaim.Mint(context.Background(), e.pool, e.cfg.OwnerClaimTTL, false, false); err != nil {
			t.Errorf("superseding mid-hash: %v", err)
		}
	}
	refusedBefore := e.count(t, `SELECT count(*) FROM audit_events WHERE action='setup.owner_claim.refused'`)
	chargesBefore := e.limiter.failureCharges()

	code, body := e.post(t, validBody(token), nil)
	if code != http.StatusForbidden || bodyCode(body) != "forbidden" {
		t.Fatalf("a token superseded mid-hash answered %d %v, want the uniform 403", code, body)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_events WHERE action='setup.owner_claim.refused'`) - refusedBefore; n != 1 {
		t.Fatalf("`refused` rows = %d, want exactly 1", n)
	}
	if n := e.limiter.failureCharges() - chargesBefore; n != 1 {
		t.Fatalf("failure-budget charges = %d, want exactly 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM users`); n != 0 {
		t.Fatalf("users = %d, want 0", n)
	}
}
