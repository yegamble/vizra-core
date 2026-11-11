package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/site"
)

// api/openapi.yaml is the API SOURCE (ADR-002 § Contracts): routes are written
// there first and implemented second. This file is the gate that makes that
// true, and it fails in BOTH directions.
//
// The internal core<->search contract is deliberately NOT checked here: core is
// the client of /internal/v1/* and never serves those routes. It lives in
// api/search-internal.openapi.yaml and is validated by
// TestInternalSearchContractIsValid below and drift-checked by vizra-search.

const (
	specPath         = "../../api/openapi.yaml"
	internalSpecPath = "../../api/search-internal.openapi.yaml"
	testVectorPath   = "../../api/search-hmac-testvectors.json"
)

func loadSpec(t *testing.T, path string) *openapi3.T {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = false
	doc, err := loader.LoadFromFile(abs)
	if err != nil {
		t.Fatalf("%s does not parse as OpenAPI: %v", path, err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("%s is not a valid OpenAPI document: %v", path, err)
	}
	return doc
}

// specOperations returns "METHOD PATH" for every operation in the document.
func specOperations(doc *openapi3.T) map[string]string {
	out := map[string]string{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			out[method+" "+path] = op.OperationID
		}
	}
	return out
}

// testServer builds a server with nil handles. Routes() only reads the router,
// so no database, cache or search service is needed — and requiring one would
// make the contract gate depend on infrastructure, which is exactly how a
// contract gate ends up skipped.
func testServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{
		Mode:         config.ModeDevelopment,
		PublicOrigin: "http://localhost:8080",
		SiteHandle:   "default",
		DatabaseURL:  "postgres://localhost:5432/vizra",
	}
	return New(Deps{
		Config:                cfg,
		Resolver:              site.NewResolver(cfg),
		EmbeddedSchemaVersion: 4,
		// A claimed instance: this test enumerates the ROUTE TABLE against the
		// contract, which the unclaimed guard would otherwise mask.
		InstanceClaimed: func(context.Context) (bool, error) { return true, nil },
	})
}

// contractOperations turns the registered routes into the "METHOD PATH" keys
// the spec uses. STEP 0: the behaviour before the fix, extracted unchanged.
func contractOperations(routes []RegisteredRoute) (map[string]bool, []string) {
	out := map[string]bool{}
	for _, r := range routes {
		if strings.Contains(r.Path, "*") {
			continue
		}
		out[r.Method+" "+r.Path] = true
	}
	return out, nil
}

func serverOperations(t *testing.T) map[string]bool {
	t.Helper()
	ops, problems := contractOperations(testServer(t).Routes())
	if len(problems) > 0 {
		t.Fatalf("cmd/api registers %d route(s) the contract gate cannot map to an OpenAPI path:\n  %s\n\n"+
			"The gate fails closed: a route it cannot compare is a route nothing checks.",
			len(problems), strings.Join(problems, "\n  "))
	}
	return ops
}

// contractDrift compares a route table with a set of spec operations and
// returns both directions of drift plus the routes that could not be mapped.
func contractDrift(routes []RegisteredRoute, spec map[string]bool) (unspecified, unserved, unmappable []string) {
	ops, unmappable := contractOperations(routes)
	for op := range ops {
		if !spec[op] {
			unspecified = append(unspecified, op)
		}
	}
	for op := range spec {
		if !ops[op] {
			unserved = append(unserved, op)
		}
	}
	sort.Strings(unspecified)
	sort.Strings(unserved)
	return unspecified, unserved, unmappable
}

// VZ-FOUND-002 S1 on synthetic route tables. The server has no wildcard or
// parameter route today, so the real route table cannot show that such a route
// is compared; these cases do.
func TestContractDriftOnSyntheticRouteTables(t *testing.T) {
	rt := func(method, path string) RegisteredRoute { return RegisteredRoute{Method: method, Path: path} }
	cases := []struct {
		name           string
		routes         []RegisteredRoute
		spec           []string
		wantUnspec     []string
		wantUnserved   []string
		wantUnmappable int
	}{
		{name: "plain route and its operation", routes: []RegisteredRoute{rt("GET", "/healthz")}, spec: []string{"GET /healthz"}},
		{name: "plain route without spec", routes: []RegisteredRoute{rt("GET", "/healthz")}, wantUnspec: []string{"GET /healthz"}},
		{name: "spec without route", spec: []string{"GET /healthz"}, wantUnserved: []string{"GET /healthz"}},

		{name: "wildcard route without spec", routes: []RegisteredRoute{rt("GET", "/files/*")}, wantUnspec: []string{"GET /files/{path}"}},
		{name: "wildcard route with its mapped spec path", routes: []RegisteredRoute{rt("GET", "/files/*")}, spec: []string{"GET /files/{path}"}},
		{name: "wildcard route, spec has another method",
			routes: []RegisteredRoute{rt("GET", "/files/*")}, spec: []string{"PUT /files/{path}"},
			wantUnspec: []string{"GET /files/{path}"}, wantUnserved: []string{"PUT /files/{path}"}},
		{name: "wildcard route, spec names the parameter differently",
			routes: []RegisteredRoute{rt("GET", "/files/*")}, spec: []string{"GET /files/{name}"},
			wantUnspec: []string{"GET /files/{path}"}, wantUnserved: []string{"GET /files/{name}"}},
		{name: "spec wildcard path without route", spec: []string{"GET /files/{path}"}, wantUnserved: []string{"GET /files/{path}"}},
		{name: "root wildcard without spec", routes: []RegisteredRoute{rt("GET", "/*")}, wantUnspec: []string{"GET /{path}"}},

		{name: "parameter route with its spec path", routes: []RegisteredRoute{rt("GET", "/photos/:id")}, spec: []string{"GET /photos/{id}"}},
		{name: "parameter route without spec", routes: []RegisteredRoute{rt("GET", "/photos/:id")}, wantUnspec: []string{"GET /photos/{id}"}},
		{name: "parameter and wildcard", routes: []RegisteredRoute{rt("GET", "/u/:user/files/*")}, spec: []string{"GET /u/{user}/files/{path}"}},

		{name: "unmappable: wildcard in the middle", routes: []RegisteredRoute{rt("GET", "/files/*/meta")}, spec: []string{"GET /files/{path}/meta"},
			wantUnserved: []string{"GET /files/{path}/meta"}, wantUnmappable: 1},
		{name: "unmappable: wildcard glued to a segment", routes: []RegisteredRoute{rt("GET", "/files/img*")}, wantUnmappable: 1},
		{name: "unmappable: two wildcards", routes: []RegisteredRoute{rt("GET", "/a/*/b/*")}, wantUnmappable: 1},
		{name: "unmappable: parameter glued to a segment", routes: []RegisteredRoute{rt("GET", "/photos/:id.jpg")}, wantUnmappable: 1},
		{name: "unmappable: parameter already named path next to a wildcard", routes: []RegisteredRoute{rt("GET", "/x/:path/*")}, wantUnmappable: 1},
		{name: "unmappable: literal brace", routes: []RegisteredRoute{rt("GET", "/x/{id}")}, spec: []string{"GET /x/{id}"},
			wantUnserved: []string{"GET /x/{id}"}, wantUnmappable: 1},
		{name: "unmappable: not an HTTP method", routes: []RegisteredRoute{rt("echo_route_not_found", "/*")}, wantUnmappable: 1},
		{name: "unmappable: no leading slash", routes: []RegisteredRoute{rt("GET", "files")}, wantUnmappable: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := map[string]bool{}
			for _, op := range tc.spec {
				spec[op] = true
			}
			unspec, unserved, unmappable := contractDrift(tc.routes, spec)
			if strings.Join(unspec, ",") != strings.Join(tc.wantUnspec, ",") {
				t.Errorf("routes without a spec operation: got %v, want %v", unspec, tc.wantUnspec)
			}
			if strings.Join(unserved, ",") != strings.Join(tc.wantUnserved, ",") {
				t.Errorf("spec operations without a route: got %v, want %v", unserved, tc.wantUnserved)
			}
			if len(unmappable) != tc.wantUnmappable {
				t.Errorf("unmappable routes: got %d %v, want %d", len(unmappable), unmappable, tc.wantUnmappable)
			}
		})
	}
}

// Direction 1: a route the server serves with no operation in the spec.
// D1 in the PR evidence: adding a route without a spec entry fails CI.
func TestEveryRouteHasASpecOperation(t *testing.T) {
	spec := specOperations(loadSpec(t, specPath))
	var missing []string
	for route := range serverOperations(t) {
		if _, ok := spec[route]; !ok {
			missing = append(missing, route)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("cmd/api serves %d route(s) that api/openapi.yaml does not describe:\n  %s\n\n"+
			"api/openapi.yaml is the API source (ADR-002 § Contracts): write the operation first.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// Direction 2: an operation in the spec that no route serves.
// D2 in the PR evidence: a spec entry without a route fails CI.
func TestEverySpecOperationHasARoute(t *testing.T) {
	routes := serverOperations(t)
	var missing []string
	for op := range specOperations(loadSpec(t, specPath)) {
		if !routes[op] {
			missing = append(missing, op)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("api/openapi.yaml describes %d operation(s) cmd/api does not serve:\n  %s\n\n"+
			"A published contract that 404s is worse than an undocumented route: "+
			"vizra-user generates a client from this file.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

func TestSpecOperationIDsAreUniqueAndPresent(t *testing.T) {
	seen := map[string]string{}
	for route, id := range specOperations(loadSpec(t, specPath)) {
		if id == "" {
			t.Errorf("%s has no operationId; the generated TypeScript client needs one", route)
			continue
		}
		if prev, dup := seen[id]; dup {
			t.Errorf("operationId %q is used by both %s and %s", id, prev, route)
		}
		seen[id] = route
	}
}

// The public contract is an EXHAUSTIVE enumeration, so a later slice cannot
// quietly widen the surface without the ledger noticing.
//
// Amended in M1-A (VZ-INSTALL-003) from "the four probes" to "the four probes
// plus the two setup operations". The assertion is not weakened: it is still a
// complete list compared for exact equality, and adding any further operation
// without editing this list turns it red. The two additions are the owner-claim
// surfaces named in VZ-INSTALL-003 `surfaces.api` (plus the claim-status read
// the approved "already claimed" screen requires, which the chair is recording
// on that ledger row).
func TestPublicContractIsTheProbesPlusTheSetupOperations(t *testing.T) {
	want := []string{
		"GET /api/v1/setup/claim-status",
		"GET /healthz",
		"GET /readyz",
		"GET /schemaz",
		"GET /version",
		"POST /api/v1/setup/claim-owner",
	}
	var got []string
	for op := range specOperations(loadSpec(t, specPath)) {
		got = append(got, op)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the public contract is\n  %v\nwant\n  %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// The internal core<->search contract
// ---------------------------------------------------------------------------

func TestInternalSearchContractIsValid(t *testing.T) {
	doc := loadSpec(t, internalSpecPath)
	ops := specOperations(doc)
	want := []string{
		"GET /healthz", "GET /readyz", "GET /version",
		"POST /internal/v1/events", "POST /internal/v1/search", "POST /internal/v1/suggestions",
	}
	var got []string
	for op := range ops {
		got = append(got, op)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the internal contract is\n  %v\nwant\n  %v\n\n"+
			"vizra-search implements this file; changing it here is a coordinated change.", got, want)
	}

	// The three internal operations must be HMAC-secured. An unauthenticated
	// internal operation is an unauthenticated search index.
	for _, p := range []string{"/internal/v1/search", "/internal/v1/suggestions", "/internal/v1/events"} {
		op := doc.Paths.Find(p).Post
		if op == nil || op.Security == nil || len(*op.Security) == 0 {
			t.Errorf("%s declares no security requirement", p)
		}
	}
	if doc.Components.SecuritySchemes["hmacSignature"] == nil {
		t.Error("the hmacSignature security scheme is missing")
	}
}

// The internal operations must NOT appear in the public contract: vizra-user
// generates its browser client from api/openapi.yaml.
func TestInternalOperationsAreNotInThePublicContract(t *testing.T) {
	for op := range specOperations(loadSpec(t, specPath)) {
		if strings.Contains(op, "/internal/") {
			t.Fatalf("%s is in the public contract; internal HMAC operations must stay out of the browser client", op)
		}
	}
}

// The shared HMAC vectors must exist: vizra-search tests against them.
func TestHMACTestVectorsArePublished(t *testing.T) {
	if _, err := os.Stat(testVectorPath); err != nil {
		t.Fatalf("api/search-hmac-testvectors.json is missing; vizra-search has nothing to verify its signer against: %v", err)
	}
}
