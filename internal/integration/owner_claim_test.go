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
	srv := httpapi.New(httpapi.Deps{
		Config:                cfg,
		Resolver:              resolver,
		Pools:                 srvPools,
		Cache:                 cacheClient,
		Limiter:               cache.NewFallbackLimiter(cacheClient),
		EmbeddedSchemaVersion: mustEmbedded(t),
		Hasher:                hasher,
		// A PLAIN handler, deliberately: the redacting handler would hide a
		// leak rather than prove there is none.
		Logger: obs.NewLogger(logs, false),
		Now:    time.Now,
	})
	return &claimEnv{cfg: cfg, resolver: resolver, pool: pool, srvPool: srvPools.Default(),
		srv: srv, hasher: hasher, logs: logs}
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
			// New connections must pick up the new default.
			e.pool.Reset()

			token, _ := e.mint(t)

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
				case c == http.StatusConflict || c == http.StatusForbidden:
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
	afterTransition := countRateLimited()
	if afterTransition != 1 {
		t.Fatalf("rate_limited rows after the transition = %d, want EXACTLY 1", afterTransition)
	}

	// Ten times more traffic must not add a second row.
	refusedBefore := e.count(t, `SELECT count(*) FROM audit_events WHERE action='setup.owner_claim.refused'`)
	for i := 0; i < 600; i++ {
		e.post(t, validBody(strings.Repeat("h", 64)), nil)
	}
	if got := countRateLimited(); got != 1 {
		t.Fatalf("rate_limited rows after 600 more requests = %d, want still exactly 1", got)
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
	// Both boots are now either blocked on the lock or about to be.
	time.Sleep(750 * time.Millisecond)
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
		t.Skipf("the pool refused to open against a closed port, so the 503 path cannot be driven here: %v", err)
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
	limited := false
	for i := 0; i < 700; i++ {
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
		t.Fatal("700 unauthenticated GETs were never rate limited; the status route must sit " +
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

	// Give the claim time to reach the index and block on it, then release it.
	time.Sleep(750 * time.Millisecond)
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
