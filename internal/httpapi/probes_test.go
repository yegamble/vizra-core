package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/jobs"
	"github.com/yegamble/vizra-core/internal/site"
)

func probeConfig() *config.Config {
	return &config.Config{
		Mode:              config.ModeDevelopment,
		PublicOrigin:      "http://localhost:8080",
		SiteHandle:        "default",
		DatabaseURL:       "postgres://localhost:5432/vizra",
		QueueAgeThreshold: 15 * time.Minute,
	}
}

func newProbeServer(t *testing.T, mutate func(*Deps)) *Server {
	t.Helper()
	cfg := probeConfig()
	deps := Deps{
		Config:                cfg,
		Resolver:              site.NewResolver(cfg),
		EmbeddedSchemaVersion: 4,
		PingDatabase:          func(context.Context) error { return nil },
		PingCache:             func(context.Context) error { return nil },
	}
	if mutate != nil {
		mutate(&deps)
	}
	return New(deps)
}

func get(t *testing.T, s *Server, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var body map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s returned a body that is not JSON: %q", path, rec.Body.String())
		}
	}
	return rec.Code, body
}

func componentStatus(t *testing.T, body map[string]any, name string) string {
	t.Helper()
	comps, _ := body["components"].([]any)
	for _, c := range comps {
		m, _ := c.(map[string]any)
		if m["name"] == name {
			s, _ := m["status"].(string)
			return s
		}
	}
	t.Fatalf("readiness body has no component %q: %+v", name, body)
	return ""
}

// /healthz is liveness only: it must stay 200 while every dependency is down.
// A liveness probe that checks PostgreSQL restarts a healthy process during a
// database blip and turns a brief outage into a crash loop.
func TestHealthzIsLivenessOnly(t *testing.T) {
	s := newProbeServer(t, func(d *Deps) {
		d.PingDatabase = func(context.Context) error { return errors.New("down") }
		d.PingCache = func(context.Context) error { return errors.New("down") }
	})
	code, body := get(t, s, "/healthz")
	if code != http.StatusOK {
		t.Fatalf("/healthz = %d with every dependency down; liveness must stay 200", code)
	}
	if body["status"] != "ok" {
		t.Fatalf("status = %v, want ok", body["status"])
	}
}

func TestHealthzReportsDraining(t *testing.T) {
	s := newProbeServer(t, nil)
	s.Drain()
	code, body := get(t, s, "/healthz")
	if code != http.StatusServiceUnavailable || body["status"] != "draining" {
		t.Fatalf("draining /healthz = %d %v, want 503 draining", code, body["status"])
	}
}

func TestReadyzHealthy(t *testing.T) {
	s := newProbeServer(t, nil)
	code, body := get(t, s, "/readyz")
	if code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("/readyz = %d %v, want 200 ok", code, body["status"])
	}
	// Search unconfigured is `off`, not a fault (Q-001).
	if got := componentStatus(t, body, "search"); got != "off" {
		t.Fatalf("search component = %q with no search configured, want off", got)
	}
}

// The ONE condition that is 503.
func TestReadyzDatabaseDownIs503(t *testing.T) {
	s := newProbeServer(t, func(d *Deps) {
		d.PingDatabase = func(context.Context) error { return errors.New("connection refused") }
	})
	code, body := get(t, s, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d with PostgreSQL down, want 503", code)
	}
	if body["status"] != "unavailable" {
		t.Fatalf("status = %v, want unavailable", body["status"])
	}
	if got := componentStatus(t, body, "database"); got != "unavailable" {
		t.Fatalf("database component = %q, want unavailable", got)
	}
	// The detail must not contain the DSN.
	comps, _ := body["components"].([]any)
	for _, c := range comps {
		m, _ := c.(map[string]any)
		if d, _ := m["detail"].(string); d != "" {
			for _, secret := range []string{"postgres://", "localhost:5432", "password"} {
				if containsFold(d, secret) {
					t.Fatalf("readiness detail leaked connection information: %q", d)
				}
			}
		}
	}
}

// Everything that is NOT the database is 200 degraded, with the component
// named. A degraded instance must keep serving reads.
func TestReadyzCacheDownIs200Degraded(t *testing.T) {
	s := newProbeServer(t, func(d *Deps) {
		d.PingCache = func(context.Context) error { return errors.New("connection refused") }
	})
	code, body := get(t, s, "/readyz")
	if code != http.StatusOK {
		t.Fatalf("/readyz = %d with the cache down; a cache outage must not take the API out of rotation", code)
	}
	if body["status"] != "degraded" {
		t.Fatalf("status = %v, want degraded", body["status"])
	}
	if got := componentStatus(t, body, "cache"); got != "degraded" {
		t.Fatalf("cache component = %q, want degraded", got)
	}
}

// Q-028: the oldest queued job above the configured threshold degrades
// readiness.
func TestReadyzQueueAgeAboveThresholdIsDegraded(t *testing.T) {
	s := newProbeServer(t, func(d *Deps) {
		d.QueueSnapshot = func(context.Context) (jobs.Snapshot, error) {
			return jobs.Snapshot{OldestQueuedAge: 16 * time.Minute}, nil
		}
	})
	code, body := get(t, s, "/readyz")
	if code != http.StatusOK || body["status"] != "degraded" {
		t.Fatalf("/readyz = %d %v with a 16m old queued job and a 15m threshold, want 200 degraded", code, body["status"])
	}
	if got := componentStatus(t, body, "worker"); got != "degraded" {
		t.Fatalf("worker component = %q, want degraded", got)
	}
}

func TestReadyzQueueAgeBelowThresholdIsOK(t *testing.T) {
	s := newProbeServer(t, func(d *Deps) {
		d.QueueSnapshot = func(context.Context) (jobs.Snapshot, error) {
			return jobs.Snapshot{OldestQueuedAge: 14 * time.Minute}, nil
		}
	})
	code, body := get(t, s, "/readyz")
	if code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("/readyz = %d %v just below the threshold, want 200 ok", code, body["status"])
	}
}

// The database result must win over a degraded component: an instance with a
// dead database and a degraded cache is unavailable, not degraded.
func TestReadyzUnavailableBeatsDegraded(t *testing.T) {
	s := newProbeServer(t, func(d *Deps) {
		d.PingDatabase = func(context.Context) error { return errors.New("down") }
		d.PingCache = func(context.Context) error { return errors.New("down") }
	})
	code, body := get(t, s, "/readyz")
	if code != http.StatusServiceUnavailable || body["status"] != "unavailable" {
		t.Fatalf("/readyz = %d %v, want 503 unavailable", code, body["status"])
	}
}

func TestReadyzDrainingIs503(t *testing.T) {
	s := newProbeServer(t, nil)
	s.Drain()
	code, _ := get(t, s, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("draining /readyz = %d, want 503", code)
	}
}

// The 2 s single-flight cache: a readiness storm must not open one connection
// per probe.
func TestReadinessIsSingleFlightAndCached(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	now := time.Now()
	s := newProbeServer(t, func(d *Deps) {
		d.Now = func() time.Time { return now }
		d.PingDatabase = func(context.Context) error {
			mu.Lock()
			calls++
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			return nil
		}
	})

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); get(t, s, "/readyz") }()
	}
	wg.Wait()

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("20 concurrent readiness probes performed %d database checks; the single-flight cache did not hold", got)
	}

	// Past the TTL it recomputes, so a recovered dependency is noticed without
	// a restart.
	now = now.Add(3 * time.Second)
	get(t, s, "/readyz")
	mu.Lock()
	got = calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("after the TTL the check ran %d times in total, want 2", got)
	}
}

// /schemaz ALWAYS returns 200, including when the ledger cannot be read
// (ADR-002 § Probes): `vizra update` must be able to tell "behind" from
// "unreachable", and a transport error erases that distinction.
func TestSchemazIsAlways200(t *testing.T) {
	s := newProbeServer(t, nil) // nil Pools: the ledger cannot be read
	code, body := get(t, s, "/schemaz")
	if code != http.StatusOK {
		t.Fatalf("/schemaz = %d with no database; it must always be 200", code)
	}
	if body["state"] != "unknown" {
		t.Fatalf("state = %v, want unknown", body["state"])
	}
	if body["embedded_version"].(float64) != 4 {
		t.Fatalf("embedded_version = %v, want 4", body["embedded_version"])
	}
}

func TestVersionReportsBuildIdentity(t *testing.T) {
	s := newProbeServer(t, nil)
	code, body := get(t, s, "/version")
	if code != http.StatusOK {
		t.Fatalf("/version = %d", code)
	}
	for _, k := range []string{"release", "commit", "built_at", "go_version", "schema_version"} {
		if _, ok := body[k]; !ok {
			t.Errorf("/version body has no %q", k)
		}
	}
	// Built from source, so there is no image and no libvips layer. Reporting
	// either would be a claim the build cannot support.
	if body["libvips"] != nil {
		t.Errorf("libvips = %v in a source build, want null", body["libvips"])
	}
	if body["image_digest"] != nil {
		t.Errorf("image_digest = %v outside a container, want null", body["image_digest"])
	}
}

// Every response carries a request id, and a client-supplied one is only
// honoured when it is already a UUID: it reaches log lines.
func TestRequestIDIsAlwaysSetAndUntrustedInputIsReplaced(t *testing.T) {
	s := newProbeServer(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("no X-Request-Id on the response")
	}

	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", "\n injected log line \n")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-Id"); got == "\n injected log line \n" {
		t.Fatal("a non-UUID client request id was echoed back verbatim")
	}
}

func TestUnknownRouteIsAJSONError(t *testing.T) {
	s := newProbeServer(t, nil)
	code, body := get(t, s, "/does-not-exist")
	if code != http.StatusNotFound {
		t.Fatalf("unknown route = %d, want 404", code)
	}
	e, _ := body["error"].(map[string]any)
	if e == nil || e["code"] != "not_found" {
		t.Fatalf("404 body = %+v, want the Error schema with code not_found", body)
	}
}

func containsFold(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		indexFold(haystack, needle) >= 0
}

func indexFold(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if equalFold(s[i:i+len(substr)], substr) {
			return i
		}
	}
	return -1
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Security Finding 9 — hardening headers on EVERY route, including errors
// ---------------------------------------------------------------------------

// Driven off s.Routes() rather than a hand-written path list, so a route added
// in a later slice cannot miss the headers without this test noticing.
func TestEveryRouteCarriesHardeningHeaders(t *testing.T) {
	s := newProbeServer(t, nil)
	routes := s.Routes()
	if len(routes) == 0 {
		t.Fatal("the server registered no routes; this test would pass vacuously")
	}

	want := map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Resource-Policy": "same-origin",
		// The one that matters on a photo host: the DEFAULT must be no-store so
		// the public-derivative path opts IN to caching. The reverse is how a
		// private derivative reaches a shared cache (ADR-007 row 21).
		"Cache-Control": "no-store",
	}

	checked := 0
	for _, r := range routes {
		if strings.Contains(r.Path, "*") {
			continue // Echo's automatic not-found entry; covered below explicitly
		}
		t.Run(r.Method+" "+r.Path, func(t *testing.T) {
			req := httptest.NewRequest(r.Method, r.Path, nil)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			for h, v := range want {
				if got := rec.Header().Get(h); got != v {
					t.Errorf("%s = %q, want %q", h, got, v)
				}
			}
		})
		checked++
	}
	if checked < 4 {
		t.Fatalf("only %d route(s) were checked; the M0 contract has four probes", checked)
	}
}

// The error paths are where a per-handler approach always misses one.
func TestErrorResponsesCarryHardeningHeaders(t *testing.T) {
	s := newProbeServer(t, nil)
	for _, tc := range []struct {
		name string
		path string
		code int
	}{
		{"unknown route 404", "/does-not-exist", http.StatusNotFound},
		{"unknown nested route 404", "/v1/nope/deeper", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.code {
				t.Fatalf("status = %d, want %d", rec.Code, tc.code)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("the error path lost the hardening headers (X-Content-Type-Options = %q)", got)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q on an error response, want no-store", got)
			}
		})
	}
}

// Deps.Limiter is plumbed through but no M0 route uses it. Named so it is not
// mistaken for coverage.
func TestMetricsAreNotOnThePublicServer(t *testing.T) {
	s := newProbeServer(t, nil)
	for _, p := range []string{"/metrics", "/debug/pprof/", "/debug/vars"} {
		code, _ := get(t, s, p)
		if code != http.StatusNotFound {
			t.Errorf("%s returned %d on the public listener; metrics bind their own address", p, code)
		}
	}
}
