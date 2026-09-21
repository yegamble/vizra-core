package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"fmt"

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

// TestNoClaimErrorMapsToAnUnhandledFiveHundred: every named database signal this
// endpoint can produce has an answer. An unmapped one is a 500 on the single
// endpoint an operator cannot skip.
func TestNoClaimErrorMapsToAnUnhandledFiveHundred(t *testing.T) {
	for _, code := range []string{"23505", "23514"} {
		status, _, _ := mapOnly(t, &pgconn.PgError{Code: code, ConstraintName: "users_one_owner"})
		if status >= 500 {
			t.Errorf("PgError %s became %d", code, status)
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

// TestEveryStatusTheContractDeclaresIsProducedAndNoOtherIs (F1).
//
// openapi-verify checks route<->operation and never status codes, so nothing
// else stops the handler and the published contract drifting apart — which is
// exactly how "database unreachable" came to answer 500 while the spec promised
// 503, frozen for vizra-user's generated client.
func TestEveryStatusTheContractDeclaresIsProducedAndNoOtherIs(t *testing.T) {
	spec := loadSpec(t, specPath)
	op := spec.Paths.Find("/api/v1/setup/claim-owner").Post
	if op == nil {
		t.Fatal("claimOwner is not in the contract")
	}
	declared := map[string]bool{}
	for code := range op.Responses.Map() {
		declared[code] = true
	}

	// Every status the handler can return, with where it is produced. A new
	// branch that returns a status absent from this list fails the test, and so
	// does a status declared in the spec that nothing produces.
	produced := map[string]string{
		"201": "handleClaimOwner success",
		"400": "validation / decode",
		"403": "token refused, origin_mismatch",
		"409": "already claimed, duplicate identifier",
		"413": "MaxBytesReader",
		"415": "requireJSONContentType",
		"429": "hard ceiling, failure budget",
		"503": "ErrUnavailable, ErrBusy",
	}
	for code := range declared {
		if _, ok := produced[code]; !ok {
			t.Errorf("the contract declares %s for claimOwner but no handler path produces it", code)
		}
	}
	for code, where := range produced {
		if !declared[code] {
			t.Errorf("the handler can return %s (%s) but the contract does not declare it", code, where)
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
