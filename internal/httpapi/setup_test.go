package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/credential"
	"github.com/yegamble/vizra-core/internal/ownerclaim"
	"github.com/yegamble/vizra-core/internal/site"
)

// TestEveryRouteIsEitherUnclaimedAllowlistedOrGuarded is the structural half of
// "while unclaimed, every signup path answers 403".
//
// A guard a future author must remember to attach is fail-open, so this walks
// the ACTUAL route table and demands that every route has been classified
// deliberately. When M1-B adds POST /api/v1/auth/register, this test goes red
// until its author puts it in one of the two sets on purpose — which is the
// whole point, because "someone forgot" is exactly how the acceptance bullet
// would quietly become false.
func TestEveryRouteIsEitherUnclaimedAllowlistedOrGuarded(t *testing.T) {
	s := testServer(t)
	for _, r := range s.Routes() {
		// Echo registers an automatic RouteNotFound entry; it is not a route an
		// author declares, and the guard covers the 404 path anyway.
		if r.Path == "" || r.Method == "echo_route_not_found" {
			continue
		}
		key := r.Method + " " + r.Path
		inExempt := claimExemptRoutes[key]
		inGuarded := claimGuardedRoutes[key]
		switch {
		case inExempt && inGuarded:
			t.Errorf("%s is in BOTH claimExemptRoutes and claimGuardedRoutes; decide which", key)
		case !inExempt && !inGuarded:
			t.Errorf("%s is in neither claimExemptRoutes nor claimGuardedRoutes.\n"+
				"Every route must be classified deliberately: may it be reached before this\n"+
				"instance has an owner? Add it to exactly one set.", key)
		}
	}
}

// TestClaimExemptRoutesAreExactlyTheProbesAndSetup pins the allowlist itself, so
// widening it is a visible edit rather than a side effect.
func TestClaimExemptRoutesAreExactlyTheProbesAndSetup(t *testing.T) {
	want := map[string]bool{
		"GET /healthz": true, "GET /readyz": true, "GET /version": true, "GET /schemaz": true,
		"GET /api/v1/setup/claim-status": true, "POST /api/v1/setup/claim-owner": true,
	}
	if len(claimExemptRoutes) != len(want) {
		t.Fatalf("the unclaimed allowlist has %d entries, want %d: %v",
			len(claimExemptRoutes), len(want), claimExemptRoutes)
	}
	for k := range want {
		if !claimExemptRoutes[k] {
			t.Errorf("%s must be reachable on an unclaimed instance", k)
		}
	}
}

// TestUnclaimedInstanceRefusesANonAllowlistedRoute covers the guard's behaviour,
// including the router's 404 path — a 404 that leaked route existence before the
// instance had an owner would be a free map of the API.
func TestUnclaimedInstanceRefusesANonAllowlistedRoute(t *testing.T) {
	s := newProbeServer(t, func(d *Deps) {
		d.InstanceClaimed = func(context.Context) (bool, error) { return false, nil }
	})
	for _, path := range []string{"/does-not-exist", "/api/v1/anything", "/api/v1/setup"} {
		code, body := get(t, s, path)
		if code != http.StatusForbidden {
			t.Errorf("%s on an unclaimed instance = %d, want 403", path, code)
		}
		if got := errCode(body); got != "instance_unclaimed" {
			t.Errorf("%s returned code %q, want instance_unclaimed", path, got)
		}
	}
	// The allowlist still works.
	if code, _ := get(t, s, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz on an unclaimed instance = %d, want 200", code)
	}
}

// TestClaimGuardReturns503WhenTheStateCannotBeRead: a lookup failure must never
// be 403 and must never be "allow". ADR-003: a database outage is 503.
func TestClaimGuardReturns503WhenTheStateCannotBeRead(t *testing.T) {
	s := newProbeServer(t, func(d *Deps) {
		d.InstanceClaimed = func(context.Context) (bool, error) {
			return false, errors.New("database is down")
		}
	})
	code, body := get(t, s, "/api/v1/anything")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("guard with an unreadable state = %d, want 503", code)
	}
	if got := errCode(body); got != "unavailable" {
		t.Errorf("code = %q, want unavailable", got)
	}
}

// TestTheClaimedBitIsCachedMonotonically: once true it can never go back, so the
// guard must stop querying. Without this the guard is a database read on every
// request for the life of the instance.
func TestTheClaimedBitIsCachedMonotonically(t *testing.T) {
	calls := 0
	s := newProbeServer(t, func(d *Deps) {
		d.InstanceClaimed = func(context.Context) (bool, error) {
			calls++
			return true, nil
		}
	})
	for i := 0; i < 5; i++ {
		if code, _ := get(t, s, "/does-not-exist"); code != http.StatusNotFound {
			t.Fatalf("a claimed instance must route normally, got %d", code)
		}
	}
	if calls != 1 {
		t.Fatalf("the claimed lookup ran %d times; it is monotonic and must be cached after the first true", calls)
	}
}

// TestClaimErrorMapping is the unit half of the error contract. The SQL guard
// proves the DATABASE behaves; this proves the handler answers correctly for
// what the database returns. Before it existed, the only coverage for 23505->409
// was a mutation of the SQL, which says nothing about the mapper.
func TestClaimErrorMapping(t *testing.T) {
	// wantMessage is asserted, not just the status and code. Without it, deleting
	// the users_one_owner branch falls through to the generic 23505 branch, which
	// also answers 409/conflict — so the mutation stayed GREEN and the
	// operator-facing message for the two-owners case was unpinned.
	cases := []struct {
		name        string
		err         error
		wantCode    int
		wantName    string
		wantMessage string
	}{
		{"already claimed", ownerclaim.ErrAlreadyClaimed, http.StatusConflict, "conflict", claimedMessage},
		{"two live owners", &pgconn.PgError{Code: "23505", ConstraintName: "users_one_owner"},
			http.StatusConflict, "conflict", claimedMessage},
		{"duplicate username", &pgconn.PgError{Code: "23505", ConstraintName: "users_username_fold_key"},
			http.StatusConflict, "conflict", "that username or email address is already taken"},
		{"duplicate email", &pgconn.PgError{Code: "23505", ConstraintName: "users_email_fold_key"},
			http.StatusConflict, "conflict", "that username or email address is already taken"},
		{"a CHECK the validator should have caught",
			&pgconn.PgError{Code: "23514", ConstraintName: "users_email_shape"},
			http.StatusBadRequest, "bad_request", "one of the submitted values is not acceptable"},
		// sentinel S-0003: a value `text` cannot hold at all (NUL) is refused by
		// the encoder with 22021 before any CHECK runs. It is the caller's input,
		// never a 500.
		{"a value the database encoding cannot hold (22021)", &pgconn.PgError{Code: "22021"},
			http.StatusBadRequest, "bad_request", "one of the submitted values is not acceptable"},
		{"a CHECK on the username the validator mirrors",
			&pgconn.PgError{Code: "23514", ConstraintName: "users_username_shape"},
			http.StatusBadRequest, "bad_request", "one of the submitted values is not acceptable"},
		// sentinel PR #14 F-1: a CHECK the caller's input cannot have violated —
		// an audit-shape or any other server-side invariant — is OUR defect. It
		// used to be answered 400 "one of the submitted values is not acceptable",
		// which told the operator their input was wrong.
		{"a CHECK on something that is not the caller's input",
			&pgconn.PgError{Code: "23514", ConstraintName: "audit_events_some_server_invariant"},
			http.StatusInternalServerError, "internal_error", "an internal error occurred"},
		{"a CHECK violation with no constraint name",
			&pgconn.PgError{Code: "23514"},
			http.StatusInternalServerError, "internal_error", "an internal error occurred"},
		{"hashing capacity exhausted", credentialBusy(), http.StatusServiceUnavailable,
			"unavailable", "the server is busy; try again shortly"},
		// F1: a connection failure is NOT a *pgconn.PgError, so without the
		// ErrUnavailable sentinel it falls through every branch to a 500 — on the
		// one endpoint an operator cannot skip, with the contract promising 503.
		{"database unreachable", fmt.Errorf("%w: beginning claim: %v",
			ownerclaim.ErrUnavailable, &pgconn.ConnectError{}),
			http.StatusServiceUnavailable, "unavailable", "the instance state could not be read"},
		{"a bare network error wrapped as unavailable", fmt.Errorf("%w: dial: %v",
			ownerclaim.ErrUnavailable, errors.New("connection refused")),
			http.StatusServiceUnavailable, "unavailable", "the instance state could not be read"},
		// backend NEW-2: the 409-vs-403 re-read runs on a server with no pools, so
		// poolFor fails. That is the server's failure, not a wrong token, and must
		// not be laundered into "that claim token was not accepted".
		// A server-SIGNALLED outage arrives as a *pgconn.PgError, and used to match
		// no branch and fall through to a 500.
		{"admin shutdown (57P01)", &pgconn.PgError{Code: "57P01"},
			http.StatusServiceUnavailable, "unavailable", "the instance state could not be read"},
		{"connection failure (08006)", &pgconn.PgError{Code: "08006"},
			http.StatusServiceUnavailable, "unavailable", "the instance state could not be read"},
		{"too many connections (53300)", &pgconn.PgError{Code: "53300"},
			http.StatusServiceUnavailable, "unavailable", "the instance state could not be read"},
		// The claimed re-read that classifies a token refusal now lives in
		// ownerclaim.classifyRefusal, and a failure there arrives here as
		// ErrUnavailable — this is the shape it produces. It must stay 503: an
		// outage laundered into "token not accepted" would charge the caller's
		// failure budget for the server's failure.
		{"the claimed re-read cannot reach the database", fmt.Errorf(
			"%w: re-reading the claimed state: %v", ownerclaim.ErrUnavailable, errors.New("closed pool")),
			http.StatusServiceUnavailable, "unavailable", "the instance state could not be read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, code, message := mapOnly(t, tc.err)
			if status != tc.wantCode || code != tc.wantName {
				t.Fatalf("%v mapped to %d/%s, want %d/%s", tc.err, status, code, tc.wantCode, tc.wantName)
			}
			if message != tc.wantMessage {
				t.Fatalf("%v produced message %q, want %q — two branches answering the same "+
					"status and code are only distinguishable by what the operator reads",
					tc.err, message, tc.wantMessage)
			}
		})
	}
}

// TestTheHandlerDoesNotDecideBetween409And403 (closing slice).
//
// The 409-vs-403 decision for a token refusal is made in exactly ONE place,
// ownerclaim.classifyRefusal. This layer used to carry a second copy for the
// redeem's empty result (a pgx.ErrNoRows branch that re-read the claimed state
// itself), while the read phase's refusals went unclassified — two copies, and
// the one that mattered was missing. ownerclaim.Claim never returns a bare
// pgx.ErrNoRows any more; if one ever reaches the mapper it is a defect and
// must surface as one, not be quietly turned into a token refusal (which audits
// and charges the budget) or into "claimed".
func TestTheHandlerDoesNotDecideBetween409And403(t *testing.T) {
	status, code, _ := mapOnly(t, pgx.ErrNoRows)
	if status == http.StatusForbidden || status == http.StatusConflict {
		t.Fatalf("a bare pgx.ErrNoRows mapped to %d/%s: the handler is deciding 409-vs-403 again; "+
			"that decision belongs to ownerclaim.classifyRefusal alone", status, code)
	}
}

// TestNoClaimErrorMapsToAnUnhandledFiveHundred: every named database signal this
// endpoint can produce has an answer. An unmapped one is a 500 on the single
// endpoint an operator cannot skip.
func TestNoClaimErrorMapsToAnUnhandledFiveHundred(t *testing.T) {
	// The input-shaped database signals this endpoint can produce. A 23514 is
	// named by its constraint: only the input CHECKs the validator mirrors are
	// the caller's (sentinel PR #14 F-1) — any other 23514 is deliberately a 500,
	// asserted in TestClaimErrorMapping. This loop used to pair 23514 with
	// users_one_owner, a unique index that can never raise it.
	for _, pe := range []*pgconn.PgError{
		{Code: "23505", ConstraintName: "users_one_owner"},
		{Code: "23505", ConstraintName: "users_username_fold_key"},
		{Code: "23514", ConstraintName: "users_email_shape"},
		{Code: "23514", ConstraintName: "users_username_shape"},
		{Code: "22021"},
	} {
		status, _, _ := mapOnly(t, pe)
		if status >= 500 {
			t.Errorf("PgError %s (%s) became %d", pe.Code, pe.ConstraintName, status)
		}
	}
	// 503 is a HANDLED answer, so the property is "nothing falls through to the
	// generic internal_error", not "nothing is >= 500".
	for _, err := range []error{
		fmt.Errorf("%w: x", ownerclaim.ErrUnavailable),
		ownerclaim.ErrAlreadyClaimed,
		ownerclaim.ErrTokenNotAccepted,
		credential.ErrBusy,
	} {
		status, code, _ := mapOnly(t, err)
		if code == "internal_error" {
			t.Errorf("%v fell through to an unhandled %d internal_error", err, status)
		}
	}
}

// TestEveryStatusTheContractDeclaresIsProducedAndNoOtherIs (F1, backend NEW-1).
//
// openapi-verify checks route<->operation and never status codes, so nothing
// else stops the handler and the published contract drifting apart. That is how
// "database unreachable" came to answer 500 while the spec promised 503, and how
// claim-status came to answer 429 that the spec did not declare at all.
//
// It is a TABLE over EVERY setup operation, not just the POST: the next route
// added to this group is then covered by construction, because an operation with
// no entry here fails the test rather than being silently unchecked.
func TestEveryStatusTheContractDeclaresIsProducedAndNoOtherIs(t *testing.T) {
	// Every status each handler can return, and where. A new branch returning a
	// status absent from this map fails; so does a status the spec declares that
	// nothing produces.
	produced := map[string]map[string]string{
		pathSetupClaimOwner: {
			"201": "handleClaimOwner success",
			"400": "validation / decode / empty body",
			"403": "token refused, origin_mismatch",
			"409": "already claimed, duplicate identifier, users_one_owner",
			"413": "MaxBytesReader",
			"415": "requireJSONContentType",
			"429": "claimOwnerCeiling, failure budget",
			"503": "ErrUnavailable, ErrBusy, claimed re-read failure",
		},
		pathSetupClaimStatus: {
			"200": "handleClaimStatus",
			"429": "claimStatusCeiling",
			"503": "the claimed lookup failed",
		},
	}

	spec := loadSpec(t, specPath)
	seen := map[string]bool{}
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			if !strings.HasPrefix(path, "/api/v1/setup/") {
				continue
			}
			seen[path] = true
			want, ok := produced[path]
			if !ok {
				t.Errorf("%s %s is a setup operation with no entry in this test; every status "+
					"it can return must be enumerated here", method, path)
				continue
			}
			declared := map[string]bool{}
			for code := range op.Responses.Map() {
				declared[code] = true
			}
			for code := range declared {
				if _, produces := want[code]; !produces {
					t.Errorf("%s declares %s but no handler path produces it", path, code)
				}
			}
			for code, where := range want {
				if !declared[code] {
					t.Errorf("%s can return %s (%s) but the contract does not declare it",
						path, code, where)
				}
			}
		}
	}
	for path := range produced {
		if !seen[path] {
			t.Errorf("%s is enumerated here but is not in the contract at all", path)
		}
	}
}

// TestTheAnnounceDefaultIsOff guards the value, which config-template-check does
// not: it checks that a key HAS a template entry, never what the entry says.
//
// The mutation that matters — flipping this default to "stderr" — writes a live
// owner credential into every log pipeline on every default install.
func TestTheAnnounceDefaultIsOff(t *testing.T) {
	var found bool
	for _, k := range config.Registry {
		if k.Name != "VIZRA_OWNER_CLAIM_ANNOUNCE" {
			continue
		}
		found = true
		if k.Default != "off" {
			t.Fatalf("VIZRA_OWNER_CLAIM_ANNOUNCE defaults to %q, want \"off\".\n"+
				"Any other default writes a live owner-claim credential to the container log,\n"+
				"which every log driver captures, ships and retains (VZ-INSTALL-003 privacy case).", k.Default)
		}
	}
	if !found {
		t.Fatal("VIZRA_OWNER_CLAIM_ANNOUNCE is not in the config registry")
	}
}

// TestTheTwoSetupRoutesDoNotShareAHardCeilingBucket (security NEW-1).
//
// Sharing one counter meant 600 body-less anonymous GETs — no token, no body, no
// content type, no origin header — spent the budget the operator's POST needs,
// holding an unclaimed instance shut for fifteen minutes at a time on the very
// endpoint that advertises `claimed:false`.
func TestTheTwoSetupRoutesDoNotShareAHardCeilingBucket(t *testing.T) {
	if claimStatusCeiling == claimOwnerCeiling {
		t.Logf("the two ceilings are numerically equal (%d); that is allowed, "+
			"but they must still be SEPARATE counters", claimOwnerCeiling)
	}
	var keys []string
	s := newProbeServer(t, func(d *Deps) {
		d.InstanceClaimed = func(context.Context) (bool, error) { return false, nil }
		d.Limiter = recordingLimiter{seen: &keys}
	})
	// One request to each route.
	get(t, s, pathSetupClaimStatus)
	req := httptest.NewRequest(http.MethodPost, pathSetupClaimOwner, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(httptest.NewRecorder(), req)

	var status, claim string
	for _, k := range keys {
		switch {
		case strings.Contains(k, "ceiling.status"):
			status = k
		case strings.Contains(k, "ceiling.claim"):
			claim = k
		}
	}
	if status == "" || claim == "" {
		t.Fatalf("expected one ceiling key per route; saw %v", keys)
	}
	if status == claim {
		t.Fatalf("both setup routes spend the SAME ceiling key %q; a status flood can then "+
			"deny the operator's claim", status)
	}
}

// recordingLimiter captures the keys the handler spends, so bucket separation is
// asserted on the key itself rather than inferred from behaviour.
type recordingLimiter struct{ seen *[]string }

func (r recordingLimiter) Allow(_ context.Context, key string, _ int, _ time.Duration) (bool, int) {
	*r.seen = append(*r.seen, key)
	return true, 1
}
func (r recordingLimiter) Degraded() bool { return false }

// TestAnUnnormalisableOriginNeverMatches (security NEW-4).
//
// checkOrigin compares NORMALISED origins, and NormalizeOrigin returns "" for
// anything it cannot render. Comparing for equality alone would make two
// failures agree: `Origin: null` against an unnormalisable configured value is
// "" == "" and would ALLOW. Boot refuses such a configuration, so this is
// unreachable through LoadFrom — which is why the test builds the Config
// directly: the comparison itself must not fail open on the strength of a guard
// somewhere else.
func TestAnUnnormalisableOriginNeverMatches(t *testing.T) {
	for _, configured := range []string{"not-an-origin", "", "https://фотки.example"} {
		for _, sent := range []string{"null", "not-an-origin", "https://фотки.example"} {
			s := newProbeServer(t, func(d *Deps) {
				d.InstanceClaimed = func(context.Context) (bool, error) { return false, nil }
			})
			s.deps.Config.PublicOrigin = configured
			req := httptest.NewRequest(http.MethodPost, pathSetupClaimOwner, nil)
			req.Header.Set("Origin", sent)
			err := s.checkOrigin(req)
			var ce *codedError
			if !errors.As(err, &ce) || ce.status != http.StatusForbidden || ce.code != "origin_mismatch" {
				t.Errorf("configured %q, Origin %q: got %v, want 403 origin_mismatch — two "+
					"unnormalisable values must never compare equal", configured, sent, err)
			}
		}
	}
}

// --- helpers ---------------------------------------------------------------

func credentialBusy() error { return credential.ErrBusy }

// mapOnly drives the real mapper with a real request context and reports the
// status and application code the client would see.
func mapOnly(t *testing.T, err error) (status int, code, message string) {
	t.Helper()
	s := newProbeServer(t, func(d *Deps) {
		d.InstanceClaimed = func(context.Context) (bool, error) { return true, nil }
	})
	req := httptest.NewRequest(http.MethodPost, pathSetupClaimOwner, nil)
	req = req.WithContext(site.NewContext(req.Context(), s.deps.Resolver.Default()))
	rec := httptest.NewRecorder()
	c := s.e.NewContext(req, rec)

	mapped := s.mapClaimError(c, err)
	var ce *codedError
	if errors.As(mapped, &ce) {
		return ce.status, ce.code, ce.message
	}
	return http.StatusInternalServerError, "internal_error", "an internal error occurred"
}

func errCode(body map[string]any) string {
	e, ok := body["error"].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := e["code"].(string)
	return s
}
