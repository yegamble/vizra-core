//go:build integration

package integration

// The M1-A sentinel follow-ups (queue 2t): one test per finding, each red on
// a6bc77d. The findings, their reproducers and the mechanism are in the meta
// repository's docs/sentinel/sweeps/2026-09-23-bugs-core-m1a-owner-claim.md.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/yegamble/vizra-core/internal/httpapi"
	"github.com/yegamble/vizra-core/internal/obs"
	"github.com/yegamble/vizra-core/internal/ownerclaim"
	"github.com/yegamble/vizra-core/internal/store/sqlcgen"
)

// ---------------------------------------------------------------------------
// S-0002: a re-mint racing an in-flight claim must not mint on the claimed
// instance, and must not erase the consumed generation (RULES R16).
// ---------------------------------------------------------------------------

type mintOutcome struct {
	raw string
	gen int64
	err error
}

// assertNoMintOnClaimedInstance is the state both S-0002 tests require after
// the claim committed and the concurrent Mint returned.
func assertNoMintOnClaimedInstance(t *testing.T, e *claimEnv, m mintOutcome, claimedGen int64) {
	t.Helper()
	var live bool
	var consumed *time.Time
	var gen int64
	if err := e.pool.QueryRow(t.Context(), `
SELECT consumed_at IS NULL AND superseded_at IS NULL AND expires_at > now(), consumed_at, generation
  FROM owner_claim_tokens`).Scan(&live, &consumed, &gen); err != nil {
		t.Fatal(err)
	}
	users := e.count(t, `SELECT count(*) FROM users`)
	if users != 1 {
		t.Fatalf("users = %d after the claim, want 1; the race this test exists for did not happen", users)
	}
	if !errors.Is(m.err, ownerclaim.ErrHasUsers) {
		t.Errorf("a re-mint that waited on the claim's row lock returned err=%v gen=%d token_printed=%v, "+
			"want ErrHasUsers: an instance with an owner must never be handed a fresh owner-creating credential",
			m.err, m.gen, m.raw != "")
	}
	if live {
		t.Errorf("the claimed instance holds a LIVE owner-claim token (generation %d)", gen)
	}
	if consumed == nil {
		t.Errorf("the redeemed generation's consumed_at was erased by the re-mint")
	}
	if gen != claimedGen {
		t.Errorf("owner_claim_tokens.generation = %d after the race, want the redeemed generation %d", gen, claimedGen)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_events WHERE action = 'setup.owner_claim.minted'`); n != 1 {
		t.Errorf("%d `minted` audit rows, want only the original one", n)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_events WHERE action = 'setup.owner_claim.superseded'`); n != 0 {
		t.Errorf("%d `superseded` audit rows, want none: the refused re-mint superseded nothing", n)
	}
}

// The raw window: the claim's redeem statement has run and holds the token row
// lock; its COMMIT is pending. `vizra claim-token`'s Mint (refuseIfUsersExist)
// is started, blocks on that lock, and is released by the COMMIT.
func TestARemintRacingAClaimDoesNotMintOnTheClaimedInstance(t *testing.T) {
	e := newClaimEnv(t)
	token, gen := e.mint(t)
	ctx := t.Context()

	tx, err := e.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	uid, _ := uuid.NewV7()
	cid, _ := uuid.NewV7()
	if _, err := sqlcgen.New(tx).ClaimOwner(ctx, sqlcgen.ClaimOwnerParams{
		TokenSha256: ownerclaim.Digest(token), UserID: uid, Username: "owner",
		Email: "owner@example.org", CredentialID: cid, PasswordHash: fakeVerifier(),
	}); err != nil {
		t.Fatalf("the claim's redeem statement: %v", err)
	}

	done := make(chan mintOutcome, 1)
	go func() {
		raw, g, err := ownerclaim.Mint(context.Background(), e.pool, e.cfg.OwnerClaimTTL, true, false)
		done <- mintOutcome{raw, g, err}
	}()
	waitForLockWaiters(t, e.pool, 1)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("committing the claim: %v", err)
	}
	var m mintOutcome
	select {
	case m = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the re-mint never returned")
	}
	assertNoMintOnClaimedInstance(t, e, m, gen)
}

// The same race through the REAL handler. A trigger that exists only in this
// test's database sleeps inside the claim's own `succeeded` audit insert, which
// widens — without changing — the window Claim already has between its redeem
// statement and COMMIT.
func TestARemintDuringARealHTTPClaimMintsNothing(t *testing.T) {
	e := newClaimEnv(t)
	token, gen := e.mint(t)
	ctx := t.Context()
	if _, err := e.pool.Exec(ctx, `
CREATE FUNCTION test_slow_claim_succeeded() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.action = 'setup.owner_claim.succeeded' THEN PERFORM pg_sleep(2); END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER test_slow_claim_succeeded BEFORE INSERT ON audit_events
  FOR EACH ROW EXECUTE FUNCTION test_slow_claim_succeeded();`); err != nil {
		t.Fatal(err)
	}

	claim := e.postAsync(t, validBody(token), nil)
	deadline := time.Now().Add(60 * time.Second)
	for {
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event = 'PgSleep'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the claim never reached its audit insert; the window was never opened")
		}
		time.Sleep(10 * time.Millisecond)
	}
	raw, g, merr := ownerclaim.Mint(context.Background(), e.pool, e.cfg.OwnerClaimTTL, true, false)
	if o := await(t, claim); o.code != http.StatusCreated {
		t.Fatalf("the claim answered %d %s, want 201", o.code, bodyCode(o.body))
	}
	assertNoMintOnClaimedInstance(t, e, mintOutcome{raw, g, merr}, gen)
}

// ---------------------------------------------------------------------------
// S-0003: the email validator must never accept what users_email_shape refuses
// (RULES R17), and NUL — which `text` cannot store — is a 4xx, not a 500.
// ---------------------------------------------------------------------------

// emailCheckExpr extracts users_email_shape's CHECK expression from the
// migration's own bytes, so the database evaluates exactly what it enforces.
func emailCheckExpr(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../migrations/0005_users_credentials_owner_claim.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)CONSTRAINT users_email_shape CHECK \((.*?AND octet_length\(email\) BETWEEN 3 AND 254)\)`).
		FindSubmatch(raw)
	if m == nil {
		t.Fatal("users_email_shape's CHECK expression is no longer where this test reads it from")
	}
	return string(m[1])
}

func TestEveryEmailTheValidatorAcceptsTheDatabaseAccepts(t *testing.T) {
	_, _, pool := freshDatabase(t)
	expr := emailCheckExpr(t)

	corpus := []string{
		"owner@example.org",
		"a.b+c@sub.example.org",
		"ünïcødé@example.org",
		"a\u000bb@example.org", // vertical tab: Go's \s misses it, [:space:] does not
		"a\u2003b@example.org", // em space: [:space:] under en_US.utf8
		"a\u00a0b@example.org", // no-break space
		"a\u0085b@example.org", // next line
		"a\u3000b@example.org", // ideographic space
		"a\u0000b@example.org", // NUL: text cannot store it at all (22021)
		"a\u0001b@example.org", // a control character
		"a\tb@example.org",
		"a b@example.org",
		"a@b.c",
		strings.Repeat("a", 250) + "@b.co",
	}
	for _, email := range corpus {
		in := ownerclaim.Input{Token: strings.Repeat("a", 64), Username: "owner", Email: email, Password: testPassphrase()}
		goAccepts := in.Validate() == nil

		var dbAccepts bool
		err := pool.QueryRow(t.Context(),
			`SELECT (`+expr+`) FROM (SELECT $1::text AS email) AS candidate`, email).Scan(&dbAccepts)
		if err != nil {
			// The database cannot even hold the value (NUL -> 22021): a refusal.
			dbAccepts = false
		}
		if goAccepts && !dbAccepts {
			t.Errorf("the Go validator accepts %q but the database refuses it (err=%v): that input "+
				"reaches the CHECK (or the encoder) and becomes a 23514/22021 instead of a clean 400", email, err)
		}
	}
}

func TestAnEmailTheDatabaseCannotHoldIsA400NotA500(t *testing.T) {
	for name, email := range map[string]string{
		"NUL":          "a\u0000b@example.org",
		"vertical tab": "a\u000bb@example.org",
		"em space":     "a\u2003b@example.org",
	} {
		t.Run(name, func(t *testing.T) {
			e := newClaimEnv(t)
			token, _ := e.mint(t)
			b := validBody(token)
			b.Email = email
			code, body := e.post(t, b, nil)
			if code != http.StatusBadRequest {
				t.Errorf("POST with email %q -> %d %s, want 400", email, code, bodyCode(body))
			}
			logs := e.logs.String()
			if strings.Contains(logs, "CHECK constraint refused a request the validator accepted") ||
				strings.Contains(logs, "http: request failed") {
				t.Errorf("the input reached the database and failed there:\n%s", logs)
			}
			if n := e.count(t, `SELECT count(*) FROM users`); n != 0 {
				t.Errorf("users = %d, want 0", n)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// S-0004: exactly ONE `rate_limited` row per bucket per window — the two hard
// ceilings included — each naming its bucket.
// ---------------------------------------------------------------------------

func rateLimitedRows(t *testing.T, e *claimEnv) map[string]int64 {
	t.Helper()
	rows, err := e.pool.Query(t.Context(), `SELECT coalesce(after->>'bucket', '<none>'), count(*)
	  FROM audit_events WHERE action = 'setup.owner_claim.rate_limited' GROUP BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var b string
		var n int64
		if err := rows.Scan(&b, &n); err != nil {
			t.Fatal(err)
		}
		out[b] = n
	}
	return out
}

func TestTheClaimCeilingRefusingTheValidTokenIsAudited(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)
	// 600 inert requests, each answered 415 before any body is read.
	for i := 0; i < 600; i++ {
		if code, _ := e.post(t, "x", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }); code != http.StatusUnsupportedMediaType {
			t.Fatalf("flood request %d: %d, want 415", i, code)
		}
	}
	code, body := e.post(t, validBody(token), nil)
	if code != http.StatusTooManyRequests {
		t.Fatalf("the operator's valid-token claim after the flood -> %d %s, want 429 (the accepted residual)",
			code, bodyCode(body))
	}
	for i := 0; i < 50; i++ {
		e.post(t, validBody(token), nil)
	}
	got := rateLimitedRows(t, e)
	if got["ceiling.claim"] != 1 || len(got) != 1 {
		t.Fatalf("rate_limited rows by bucket = %v, want exactly {ceiling.claim: 1}: the ceiling refused the "+
			"operator's VALID token and the audit trail must say so, once", got)
	}
}

func TestTheStatusCeilingIsAuditedOnce(t *testing.T) {
	e := newClaimEnv(t)
	var last int
	for i := 0; i < 3001+100; i++ {
		last, _ = e.getStatus(t)
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("claim-status past its ceiling -> %d, want 429", last)
	}
	if got := rateLimitedRows(t, e); got["ceiling.status"] != 1 || len(got) != 1 {
		t.Fatalf("rate_limited rows by bucket = %v, want exactly {ceiling.status: 1}", got)
	}
}

func TestPerOriginAndGlobalTransitionsEachWriteTheirOwnRow(t *testing.T) {
	e := newClaimEnv(t)
	e.mint(t)
	wrong := validBody(strings.Repeat("0", 64))
	send := func(remote string) int {
		code, _ := e.post(t, wrong, func(r *http.Request) { r.RemoteAddr = remote })
		return code
	}
	for i := 0; i < 11; i++ { // the per-origin budget is 10
		send("203.0.113.42:5000")
	}
	if got := rateLimitedRows(t, e); got["per_origin"] != 1 || len(got) != 1 {
		t.Fatalf("after the per-origin transition: rate_limited rows = %v, want exactly {per_origin: 1}", got)
	}
	// 11 global charges so far; ten each from five other /24s takes the global
	// counter past 60 without any of them crossing its own per-origin budget.
	globalLimited := 0
	for o := 1; o <= 5; o++ {
		for i := 0; i < 10; i++ {
			if send("198.51."+string(rune('0'+o))+".7:5000") == http.StatusTooManyRequests {
				globalLimited++
			}
		}
	}
	if globalLimited == 0 {
		t.Fatal("the global budget never limited; this test proves nothing")
	}
	// More from yet another origin: no new rows for either bucket.
	for i := 0; i < 20; i++ {
		send("192.0.2.9:5000")
	}
	got := rateLimitedRows(t, e)
	if got["per_origin"] != 1 || got["global"] != 1 || len(got) != 2 {
		t.Fatalf("rate_limited rows by bucket = %v, want exactly {per_origin: 1, global: 1}: two buckets "+
			"entered the limited state in one window", got)
	}
}

// ---------------------------------------------------------------------------
// S-0005: a request that ENDED (the client hung up) is not a database outage
// (RULES R9, R18).
// ---------------------------------------------------------------------------

func TestACancelledClaimIsNotLoggedAsADatabaseOutage(t *testing.T) {
	t.Run("during the claimed check", func(t *testing.T) {
		cfg, resolver, _ := freshDatabase(t)
		logs := &safeBuffer{}
		srv := httpapi.New(httpapi.Deps{
			Config: cfg, Resolver: resolver, Pools: poolsOf(t, resolver),
			EmbeddedSchemaVersion: mustEmbedded(t),
			Logger:                obs.NewLogger(logs, false),
			// The claimed read is interrupted by the client going away: the hook
			// cancels the request's own context and returns what a pgx call
			// returns when that happens.
			InstanceClaimed: func(ctx context.Context) (bool, error) {
				cancelRequest(ctx)
				<-ctx.Done()
				return false, ctx.Err()
			},
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ctx = context.WithValue(ctx, cancelKey{}, cancel)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/claim-owner", strings.NewReader(`{}`)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code >= 500 && rec.Code != http.StatusServiceUnavailable {
			t.Errorf("a cancelled request answered %d", rec.Code)
		}
		if strings.Contains(logs.String(), "could not reach the database") || strings.Contains(logs.String(), "level=ERROR") {
			t.Errorf("a client that hung up was logged as a server failure:\n%s", logs.String())
		}
	})

	t.Run("inside the claim transaction", func(t *testing.T) {
		e := newClaimEnv(t)
		token, _ := e.mint(t)
		// Hold the token row so the claim's redeem statement waits on it; the
		// client then hangs up while the database call is in flight.
		tx, err := e.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err := tx.Exec(t.Context(), `SELECT 1 FROM owner_claim_tokens FOR UPDATE`); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		claim := e.postAsync(t, validBody(token), func(r *http.Request) { *r = *r.WithContext(ctx) })
		waitForLockWaiters(t, e.pool, 1)
		cancel()
		o := await(t, claim)
		if o.code >= 500 && o.code != http.StatusServiceUnavailable {
			t.Errorf("a cancelled claim answered %d", o.code)
		}
		if strings.Contains(e.logs.String(), "could not reach the database") || strings.Contains(e.logs.String(), "level=ERROR") {
			t.Errorf("a client that hung up mid-claim was logged as a server failure:\n%s", e.logs.String())
		}
		if n := e.count(t, `SELECT count(*) FROM users`); n != 0 {
			t.Errorf("users = %d after a cancelled claim, want 0", n)
		}
	})
}

type cancelKey struct{}

func cancelRequest(ctx context.Context) {
	if cancel, ok := ctx.Value(cancelKey{}).(context.CancelFunc); ok {
		cancel()
	}
}

// ---------------------------------------------------------------------------
// S-0006: every 503 on the setup surface logs its cause once, redacted.
// ---------------------------------------------------------------------------

func TestEverySetup503LogsItsCause(t *testing.T) {
	const cause = "dial tcp 10.0.0.9:5432: connect: connection refused"
	cfg, resolver, _ := freshDatabase(t)
	newServer := func(pools bool, claimed func(context.Context) (bool, error)) (*httpapi.Server, *safeBuffer) {
		logs := &safeBuffer{}
		deps := httpapi.Deps{
			Config: cfg, Resolver: resolver,
			EmbeddedSchemaVersion: mustEmbedded(t),
			Logger:                obs.NewLogger(logs, false),
			InstanceClaimed:       claimed,
		}
		if pools {
			deps.Pools = poolsOf(t, resolver)
		}
		return httpapi.New(deps), logs
	}
	do := func(srv *httpapi.Server, method, path, body string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	failing := func(context.Context) (bool, error) { return false, errors.New(cause) }

	for _, tc := range []struct {
		name, method, path, body string
		pools                    bool
		claimed                  func(context.Context) (bool, error)
		want                     string
	}{
		{"GET claim-status", http.MethodGet, "/api/v1/setup/claim-status", "", true, failing, "connection refused"},
		{"the unclaimed guard", http.MethodGet, "/api/v1/no-such-route", "", true, failing, "connection refused"},
		{"POST claim-owner with no pool for the site", http.MethodPost, "/api/v1/setup/claim-owner",
			`{"token":"` + strings.Repeat("a", 64) + `","username":"owner","email":"owner@example.org","password":"` + testPassphrase() + `"}`,
			false, func(context.Context) (bool, error) { return false, nil }, "no database pools configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, logs := newServer(tc.pools, tc.claimed)
			if code := do(srv, tc.method, tc.path, tc.body); code != http.StatusServiceUnavailable {
				t.Fatalf("%s %s -> %d, want 503", tc.method, tc.path, code)
			}
			if !strings.Contains(logs.String(), tc.want) {
				t.Errorf("the 503 logged nothing about its cause %q:\n%s", tc.want, logs.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// S-0007: the `succeeded` row names the generation it consumed, and the
// database refuses one that does not (migration 0006).
// ---------------------------------------------------------------------------

func TestTheSucceededRowNamesTheConsumedGeneration(t *testing.T) {
	e := newClaimEnv(t)
	token, gen := e.mint(t)
	if code, body := e.post(t, validBody(token), nil); code != http.StatusCreated {
		t.Fatalf("claim: %d %s", code, bodyCode(body))
	}
	var recorded *int64
	if err := e.pool.QueryRow(t.Context(), `SELECT (after->>'token_generation')::bigint
	  FROM audit_events WHERE action = 'setup.owner_claim.succeeded'`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded == nil || *recorded != gen {
		t.Fatalf("the succeeded row's token_generation = %v, want the redeemed generation %d", recorded, gen)
	}
}

func TestTheDatabaseRefusesASucceededRowWithoutAGeneration(t *testing.T) {
	_, _, pool := freshDatabase(t)
	insert := func(after string) error {
		_, err := pool.Exec(t.Context(), `INSERT INTO audit_events
		  (id, actor_kind, action, subject_type, after)
		  VALUES (gen_random_uuid(), 'system', 'setup.owner_claim.succeeded', 'user', $1::jsonb)`, after)
		return err
	}
	for _, after := range []string{`{"username":"owner","role":"owner"}`, `{"token_generation":"1"}`, `null`} {
		if err := insert(after); err == nil {
			t.Errorf("the database accepted a succeeded row with after=%s; it must name a numeric token_generation", after)
		}
	}
	if err := insert(`{"username":"owner","role":"owner","token_generation":1}`); err != nil {
		t.Errorf("the database refused a well-formed succeeded row: %v", err)
	}
}

// ---------------------------------------------------------------------------
// S-0009: "strict decode" is strict — keys are case-sensitive and the body is
// exactly one JSON object.
// ---------------------------------------------------------------------------

func TestTheClaimBodyIsDecodedStrictly(t *testing.T) {
	for name, build := range map[string]func(token string) string{
		"upper-case keys": func(tok string) string {
			return `{"TOKEN":"` + tok + `","USERNAME":"owner","EMAIL":"owner@example.org","PASSWORD":"` + testPassphrase() + `"}`
		},
		"one mixed-case key": func(tok string) string {
			return `{"Token":"` + tok + `","username":"owner","email":"owner@example.org","password":"` + testPassphrase() + `"}`
		},
		"a trailing }]": func(tok string) string {
			return `{"token":"` + tok + `","username":"owner","email":"owner@example.org","password":"` + testPassphrase() + `"}}]`
		},
		"a second object": func(tok string) string {
			return `{"token":"` + tok + `","username":"owner","email":"owner@example.org","password":"` + testPassphrase() + `"} {}`
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newClaimEnv(t)
			token, _ := e.mint(t)
			code, body := e.post(t, build(token), nil)
			if code != http.StatusBadRequest {
				t.Errorf("-> %d %s, want 400: the body is not exactly one object with the schema's properties", code, bodyCode(body))
			}
			if n := e.count(t, `SELECT count(*) FROM users`); n != 0 {
				t.Errorf("users = %d, want 0", n)
			}
		})
	}
	// Control: the canonical body, with trailing whitespace, still claims.
	e := newClaimEnv(t)
	token, _ := e.mint(t)
	if code, body := e.post(t, `{"token":"`+token+`","username":"owner","email":"owner@example.org","password":"`+testPassphrase()+"\"}\n  ", nil); code != http.StatusCreated {
		t.Fatalf("the canonical body -> %d %s, want 201", code, bodyCode(body))
	}
}

// The ceilings keep answering 429 for the life of the instance, so on a CLAIMED
// instance they must write nothing: one row per bucket per window, integrated
// over the life of an instance, is still an unbounded writer into a table
// nothing can prune.
func TestACeilingOnAClaimedInstanceWritesNoRateLimitedRow(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)
	if code, body := e.post(t, validBody(token), nil); code != http.StatusCreated {
		t.Fatalf("claim: %d %s", code, bodyCode(body))
	}
	var status, claim int
	for i := 0; i < 3001+10; i++ {
		status, _ = e.getStatus(t)
	}
	for i := 0; i < 600+10; i++ {
		claim, _ = e.post(t, validBody(token), nil)
	}
	if status != http.StatusTooManyRequests || claim != http.StatusTooManyRequests {
		t.Fatalf("past both ceilings: claim-status %d, claim-owner %d, want 429 for both", status, claim)
	}
	if got := rateLimitedRows(t, e); len(got) != 0 {
		t.Fatalf("rate_limited rows on a CLAIMED instance = %v, want none", got)
	}
}
