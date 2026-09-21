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

func serverOperations(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, r := range testServer(t).Routes() {
		// Echo registers an automatic RouteNotFound entry; it is not an
		// operation and has no spec.
		if strings.Contains(r.Path, "*") {
			continue
		}
		out[r.Method+" "+r.Path] = true
	}
	return out
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
