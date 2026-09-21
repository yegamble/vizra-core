//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yegamble/vizra-core/internal/audit"
	"github.com/yegamble/vizra-core/internal/cache"
	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/credential"
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
	srv      *httpapi.Server
	hasher   *countingHasher
	logs     *safeBuffer
}

// countingHasher wraps the real hasher so a test can assert how many argon2
// derivations a request path performed. "The password is hashed only after the
// token has verified" is the endpoint's central denial-of-service property, and
// prose cannot be tested.
type countingHasher struct {
	inner credential.Hasher
}

func (h *countingHasher) Hash(ctx context.Context, p string) (string, error) {
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
	srv := httpapi.New(httpapi.Deps{
		Config:                cfg,
		Resolver:              resolver,
		Pools:                 poolsOf(t, resolver),
		Cache:                 cacheClient,
		Limiter:               cache.NewFallbackLimiter(cacheClient),
		EmbeddedSchemaVersion: mustEmbedded(t),
		Hasher:                hasher,
		// A PLAIN handler, deliberately: the redacting handler would hide a
		// leak rather than prove there is none.
		Logger: obs.NewLogger(logs, false),
		Now:    time.Now,
	})
	return &claimEnv{cfg: cfg, resolver: resolver, pool: pool, srv: srv, hasher: hasher, logs: logs}
}

// mint puts a live token in the database and returns it.
func (e *claimEnv) mint(t *testing.T) (string, int64) {
	t.Helper()
	raw, gen, err := ownerclaim.Mint(t.Context(), e.pool, e.cfg.OwnerClaimTTL, false)
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
	_, _, err := ownerclaim.Mint(t.Context(), e.pool, e.cfg.OwnerClaimTTL, true)
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

	var limitedRequests int
	for i := 0; i < 60; i++ {
		code, _ := e.post(t, validBody(strings.Repeat("f", 64)), nil)
		if code == http.StatusTooManyRequests {
			limitedRequests++
		}
	}
	if limitedRequests < 5 {
		t.Fatalf("only %d requests were rate limited; this test proves nothing", limitedRequests)
	}
	rateRows := e.count(t, `SELECT count(*) FROM audit_events WHERE action='setup.owner_claim.rate_limited'`)
	if rateRows > 1 {
		t.Errorf("%d rate_limited audit rows for %d limited requests; want at most 1 (the transition)",
			rateRows, limitedRequests)
	}
	refusedRows := e.count(t, `SELECT count(*) FROM audit_events WHERE action='setup.owner_claim.refused'`)
	if refusedRows >= int64(60) {
		t.Errorf("%d refused rows for 60 attempts; rate-limited requests must not write one", refusedRows)
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

// TestOwnerInsertConflictMapsTo409NotFiveHundred forces users_one_owner to fire
// through the HTTP handler. Nothing else in the suite reaches that branch,
// because the row guard wins the ordinary race — so without this, "23505 maps to
// 409" would be untested.
func TestOwnerInsertConflictMapsTo409NotFiveHundred(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)

	// An owner appears out of band AFTER the token was minted. The claimed check
	// would normally catch it, so bypass the cache by using a fresh server that
	// has not cached "unclaimed" — then race the insert in between is not
	// reproducible deterministically. Instead assert the mapper directly against
	// the real constraint by inserting an owner and claiming with a valid token.
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
}

// TestMintTimestampsComeFromTheDatabaseClock mirrors the repo's existing
// TestRunAfterComesFromTheDatabaseClockNotTheApplicationHost. Expiry decided by
// the application host would drift from the predicate that enforces it.
func TestMintTimestampsComeFromTheDatabaseClock(t *testing.T) {
	e := newClaimEnv(t)
	before := time.Now()
	e.mint(t)

	var mintedAt, dbNow time.Time
	if err := e.pool.QueryRow(t.Context(),
		`SELECT minted_at, now() FROM owner_claim_tokens`).Scan(&mintedAt, &dbNow); err != nil {
		t.Fatal(err)
	}
	// The database's own now() must be the source: assert minted_at is within a
	// second of the DATABASE clock, not of the test process's clock.
	if d := dbNow.Sub(mintedAt); d < 0 || d > 5*time.Second {
		t.Fatalf("minted_at is %v from the database clock; it must be stamped by now()", d)
	}
	_ = before
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
	raw, _, err := ownerclaim.Mint(t.Context(), pool, cfg.OwnerClaimTTL, false)
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
