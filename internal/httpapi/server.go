// Package httpapi is the ONLY package allowed to import Echo.
//
// ADR-001 confines Echo types here so the framework major is replaceable in one
// package, and `make lint-imports` fails the build if `echo/v5` is imported
// anywhere else. Handlers in other packages therefore take plain
// context.Context and return plain values; the adapter is here.
//
// It is also the only place that registers a route, which is what makes the
// both-direction OpenAPI contract test possible: Routes() enumerates exactly
// what the server serves, and the test compares it with api/openapi.yaml.
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/yegamble/vizra-core/internal/cache"
	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/db"
	"github.com/yegamble/vizra-core/internal/jobs"
	"github.com/yegamble/vizra-core/internal/search"
	"github.com/yegamble/vizra-core/internal/site"
)

// Deps are the handles the server needs. Every one of them is passed in, never
// reached for through a package global (Q-008 checklist item 1).
type Deps struct {
	Config   *config.Config
	Resolver *site.Resolver
	Pools    *db.Pools
	Cache    *cache.Client
	Limiter  cache.Limiter
	Search   *search.Service
	Logger   *slog.Logger

	// EmbeddedSchemaVersion is the highest migration compiled into this binary.
	EmbeddedSchemaVersion int64

	// QueueSnapshot reads job-queue state for readiness (Q-028). It is a
	// function so the api process does not have to import the worker loop, and
	// so a test can drive the threshold without a database.
	QueueSnapshot func(ctx context.Context) (jobs.Snapshot, error)

	// PingDatabase and PingCache are seams. They default to the real probes in
	// New; a test overrides them so the readiness RULES — which condition is
	// 503 and which is 200 degraded — can be exercised without standing up
	// PostgreSQL. The rules are the part that has been wrong in production
	// before; the ping itself is one library call.
	PingDatabase func(ctx context.Context) error
	PingCache    func(ctx context.Context) error

	// Now is injectable for tests.
	Now func() time.Time
}

var (
	errNoDatabase = errors.New("no database pool is configured")
	errNoCache    = errors.New("no cache is configured")
)

// Server is the public API server.
type Server struct {
	e    *echo.Echo
	deps Deps

	readiness *readinessCache
	draining  atomicBool
}

// New builds the server and registers every route. Routes are registered in one
// place so Routes() is a complete, honest enumeration.
func New(deps Deps) *Server {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.PingDatabase == nil {
		pools := deps.Pools
		deps.PingDatabase = func(ctx context.Context) error {
			if pools == nil {
				return errNoDatabase
			}
			return db.Ping(ctx, pools.Default())
		}
	}
	if deps.PingCache == nil {
		c := deps.Cache
		deps.PingCache = func(ctx context.Context) error {
			if c == nil {
				return errNoCache
			}
			return c.Ping(ctx)
		}
	}

	e := echo.New()
	e.HTTPErrorHandler = errorHandler(deps.Logger)

	s := &Server{e: e, deps: deps}
	s.readiness = newReadinessCache(2*time.Second, deps.Now, s.computeReadiness)

	// Order matters. requestID first so every later log line and every error
	// body carries one; then the single Host -> site middleware (Q-008 checklist
	// item 2); then the route attribute for tracing.
	e.Pre(requestIDMiddleware())
	e.Use(siteMiddleware(deps.Resolver, deps.Logger))
	e.Use(routeAttributeMiddleware())

	e.GET("/healthz", s.handleHealthz)
	e.GET("/readyz", s.handleReadyz)
	e.GET("/version", s.handleVersion)
	e.GET("/schemaz", s.handleSchemaz)

	return s
}

// RegisteredRoute is one method+path the server actually serves.
type RegisteredRoute struct {
	Method string
	Path   string
}

// Routes enumerates what the server serves. The OpenAPI contract test compares
// this with api/openapi.yaml in BOTH directions, so neither a route without a
// spec nor a spec without a route can merge.
func (s *Server) Routes() []RegisteredRoute {
	infos := s.e.Router().Routes()
	out := make([]RegisteredRoute, 0, len(infos))
	for _, r := range infos {
		out = append(out, RegisteredRoute{Method: r.Method, Path: r.Path})
	}
	return out
}

// Handler returns the http.Handler to serve, wrapped in otelhttp.
//
// ADR-001: tracing is otelhttp wrapping the Echo server handler, PLUS an
// in-house Echo middleware setting http.route from the matched route —
// otelhttp alone cannot know the route pattern, so without the second half
// every span would be labelled with the raw path and high-cardinality URLs
// would flood the backend. `labstack/echo-opentelemetry` is adopted only at
// >= 1.0.
func (s *Server) Handler() http.Handler {
	return otelhttp.NewHandler(s.e, "vizra-core",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			// A placeholder until the route middleware refines it; see
			// routeAttributeMiddleware.
			return r.Method + " " + r.URL.Path
		}),
	)
}

// Drain marks the server as draining. /healthz and /readyz turn non-200 so a
// load balancer stops sending new work before in-flight requests are cut off.
func (s *Server) Drain() { s.draining.Store(true) }

// Draining reports the drain state.
func (s *Server) Draining() bool { return s.draining.Load() }
