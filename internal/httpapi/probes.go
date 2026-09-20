package httpapi

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/yegamble/vizra-core/internal/buildinfo"
	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/migrate"
	"github.com/yegamble/vizra-core/internal/search"
)

// ---------------------------------------------------------------------------
// Response shapes. These mirror api/openapi.yaml; the contract test asserts the
// routes, and these structs are what the handlers actually serialise.
// ---------------------------------------------------------------------------

type healthResponse struct {
	Status string `json:"status"`
}

type readinessComponent struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type readinessResponse struct {
	Status     string               `json:"status"`
	Components []readinessComponent `json:"components"`
	CheckedAt  time.Time            `json:"checked_at"`
}

type libvipsInfo struct {
	Version string   `json:"version"`
	Loaders []string `json:"loaders"`
}

type versionResponse struct {
	Release       string            `json:"release"`
	Commit        string            `json:"commit"`
	BuiltAt       string            `json:"built_at"`
	GoVersion     string            `json:"go_version"`
	ImageDigest   *string           `json:"image_digest"`
	Libvips       *libvipsInfo      `json:"libvips"`
	SchemaVersion int64             `json:"schema_version"`
	Modules       map[string]string `json:"modules,omitempty"`
}

type schemaResponse struct {
	AppliedVersion  int64  `json:"applied_version"`
	EmbeddedVersion int64  `json:"embedded_version"`
	Dirty           bool   `json:"dirty"`
	State           string `json:"state"`
	Detail          string `json:"detail,omitempty"`
}

// ---------------------------------------------------------------------------
// /healthz — liveness ONLY. No dependency I/O: a liveness probe that checks
// PostgreSQL restarts a healthy process during a database blip.
// ---------------------------------------------------------------------------

func (s *Server) handleHealthz(c *echo.Context) error {
	if s.Draining() {
		return c.JSON(http.StatusServiceUnavailable, healthResponse{Status: "draining"})
	}
	return c.JSON(http.StatusOK, healthResponse{Status: "ok"})
}

// ---------------------------------------------------------------------------
// /readyz — drain-aware, with a 2 s single-flight cache (ADR-002 § Probes).
//
// Exactly one condition yields 503: PostgreSQL unreachable (or draining).
// Everything else — cache down, search misconfigured or unreachable, queue age
// above the Q-028 threshold — yields 200 `degraded` with the component named,
// so a degraded instance keeps serving reads instead of being removed from
// rotation and taking the site down with it.
// ---------------------------------------------------------------------------

type readinessCache struct {
	ttl     time.Duration
	now     func() time.Time
	compute func(ctx context.Context) readinessResponse

	mu       sync.Mutex
	value    readinessResponse
	taken    time.Time
	inFlight *sync.WaitGroup
}

func newReadinessCache(ttl time.Duration, now func() time.Time, compute func(context.Context) readinessResponse) *readinessCache {
	return &readinessCache{ttl: ttl, now: now, compute: compute}
}

// Get returns a cached result up to ttl old. Concurrent callers during a
// recomputation wait for the one in flight rather than each opening their own
// connection, which is what "single-flight" buys: a readiness storm from a
// load balancer must not itself exhaust the pool.
func (r *readinessCache) Get(ctx context.Context) readinessResponse {
	r.mu.Lock()
	if !r.taken.IsZero() && r.now().Sub(r.taken) < r.ttl {
		v := r.value
		r.mu.Unlock()
		return v
	}
	if wg := r.inFlight; wg != nil {
		r.mu.Unlock()
		wg.Wait()
		r.mu.Lock()
		v := r.value
		r.mu.Unlock()
		return v
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	r.inFlight = wg
	r.mu.Unlock()

	v := r.compute(ctx)

	r.mu.Lock()
	r.value = v
	r.taken = r.now()
	r.inFlight = nil
	r.mu.Unlock()
	wg.Done()
	return v
}

const (
	statusOK          = "ok"
	statusDegraded    = "degraded"
	statusUnavailable = "unavailable"
	statusOff         = "off"
)

func (s *Server) computeReadiness(ctx context.Context) readinessResponse {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	resp := readinessResponse{Status: statusOK, CheckedAt: s.deps.Now().UTC()}
	worst := statusOK
	demote := func(to string) {
		switch {
		case worst == statusUnavailable:
		case to == statusUnavailable:
			worst = statusUnavailable
		case to == statusDegraded && worst == statusOK:
			worst = statusDegraded
		}
	}

	// database — the only 503.
	dbc := readinessComponent{Name: "database", Status: statusOK}
	if err := s.deps.PingDatabase(ctx); err != nil {
		dbc.Status = statusUnavailable
		dbc.Detail = "PostgreSQL is unreachable"
		demote(statusUnavailable)
	}
	resp.Components = append(resp.Components, dbc)

	// cache — degraded, never fatal. The api falls back to a per-process
	// in-memory rate limiter and says so (ADR-003).
	cc := readinessComponent{Name: "cache", Status: statusOK}
	if err := s.deps.PingCache(ctx); err != nil {
		cc.Status = statusDegraded
		cc.Detail = "cache unreachable; rate limits are per-process only"
		demote(statusDegraded)
	} else if s.deps.Limiter != nil && s.deps.Limiter.Degraded() {
		cc.Status = statusDegraded
		cc.Detail = "rate limiting is running on the in-process fallback"
		demote(statusDegraded)
	}
	resp.Components = append(resp.Components, cc)

	// search — off | ok | degraded (Q-001). `off` is not a fault.
	sc := readinessComponent{Name: "search", Status: statusOff}
	if s.deps.Search != nil {
		switch s.deps.Search.Health(ctx) {
		case search.HealthOff:
			sc.Status = statusOff
		case search.HealthOK:
			sc.Status = statusOK
		default:
			sc.Status = statusDegraded
			sc.Detail = "search is configured but unreachable or misconfigured; results are served from SQL"
			demote(statusDegraded)
		}
	}
	resp.Components = append(resp.Components, sc)

	// worker queue age — Q-028.
	if s.deps.QueueSnapshot != nil {
		wc := readinessComponent{Name: "worker", Status: statusOK}
		snap, err := s.deps.QueueSnapshot(ctx)
		switch {
		case err != nil:
			wc.Status = statusDegraded
			wc.Detail = "the job queue could not be read"
			demote(statusDegraded)
		case s.deps.Config != nil && snap.OldestQueuedAge > s.deps.Config.QueueAgeThreshold:
			wc.Status = statusDegraded
			wc.Detail = "the oldest queued job is older than the configured threshold"
			demote(statusDegraded)
		}
		resp.Components = append(resp.Components, wc)
	}

	resp.Status = worst
	return resp
}

func (s *Server) handleReadyz(c *echo.Context) error {
	if s.Draining() {
		return c.JSON(http.StatusServiceUnavailable, readinessResponse{
			Status:     statusUnavailable,
			Components: []readinessComponent{{Name: "database", Status: statusOK, Detail: "the process is draining"}},
			CheckedAt:  s.deps.Now().UTC(),
		})
	}
	resp := s.readiness.Get(c.Request().Context())
	code := http.StatusOK
	if resp.Status == statusUnavailable {
		code = http.StatusServiceUnavailable
	}
	return c.JSON(code, resp)
}

// ---------------------------------------------------------------------------
// /version
// ---------------------------------------------------------------------------

func (s *Server) handleVersion(c *echo.Context) error {
	resp := versionResponse{
		Release:       buildinfo.Release,
		Commit:        buildinfo.Commit,
		BuiltAt:       buildinfo.BuiltAt,
		GoVersion:     buildinfo.GoVersion(),
		SchemaVersion: s.deps.EmbeddedSchemaVersion,
	}
	if buildinfo.ImageDigest != "" {
		d := buildinfo.ImageDigest
		resp.ImageDigest = &d
	}
	if buildinfo.HasLibvips() {
		resp.Libvips = &libvipsInfo{Version: buildinfo.LibvipsVersion, Loaders: buildinfo.Loaders()}
	}
	resp.Modules = moduleVersions()
	return c.JSON(http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// /schemaz — ALWAYS 200 (ADR-002 § Probes).
//
// `vizra update` and the deploy scripts read this to decide whether an image
// may be rolled out, and must be able to distinguish "behind" from
// "unreachable". A dirty or unreadable ledger is reported in the body.
// ---------------------------------------------------------------------------

func (s *Server) handleSchemaz(c *echo.Context) error {
	st := migrate.Probe(c.Request().Context(), s.deps.Pools.Default(), s.deps.EmbeddedSchemaVersion)
	return c.JSON(http.StatusOK, schemaResponse{
		AppliedVersion:  st.AppliedVersion,
		EmbeddedVersion: st.EmbeddedVersion,
		Dirty:           st.Dirty,
		State:           string(st.State),
		Detail:          st.Detail,
	})
}

// modeString keeps config imported where it is used, and documents that the
// probe body never reveals the mode's secrets.
var _ = config.ModeProduction
