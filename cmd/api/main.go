// Command api is the HTTP entry point. It NEVER decodes pixels (ADR-002,
// Q-034): every decode happens in cmd/worker, so a decoder bomb cannot take
// down the surface that serves every other request.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/yegamble/vizra-core/internal/buildinfo"
	"github.com/yegamble/vizra-core/internal/cache"
	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/db"
	"github.com/yegamble/vizra-core/internal/httpapi"
	"github.com/yegamble/vizra-core/internal/jobs"
	"github.com/yegamble/vizra-core/internal/migrate"
	"github.com/yegamble/vizra-core/internal/obs"
	"github.com/yegamble/vizra-core/internal/search"
	"github.com/yegamble/vizra-core/internal/site"
)

func main() {
	if err := run(); err != nil {
		// The configuration validator already formatted its problems; printing
		// them once, to stderr, is the whole boot-refusal UX.
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

// newAPIServer builds the public listener.
//
// It is a function rather than a struct literal inline in run() because the
// timeouts are a security property with no test otherwise: the verifier removed
// ReadTimeout and WriteTimeout and every lane stayed green. cmd/api has no
// other seam, so this is the seam.
func newAPIServer(cfg *config.Config, h http.Handler) *http.Server {
	return &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: h,
		// ReadHeaderTimeout alone is not enough: once the headers are in, a
		// slow-body client holds the connection indefinitely. ReadTimeout and
		// WriteTimeout bound the whole exchange, and IdleTimeout bounds a
		// keep-alive connection that is doing nothing at all.
		//
		// 30 s is generous for the M0 probe surface. M1's upload routes need a
		// longer, per-route budget — they will set it on their own handler
		// rather than widening this default, because a global timeout sized for
		// the slowest route protects nothing.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("vizra-api refused to start.\n%w", err)
	}

	log := obs.NewLogger(os.Stdout, cfg.Mode == config.ModeProduction)
	slog.SetDefault(log)
	log.Info("vizra-api starting",
		"release", buildinfo.Release, "commit", buildinfo.Commit,
		"go", buildinfo.GoVersion(), "mode", string(cfg.Mode))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	resolver := site.NewResolver(cfg)

	pools, err := db.Open(ctx, resolver)
	if err != nil {
		return err
	}
	defer pools.Close()

	cacheClient, err := cache.Open(cfg.CacheURL, cfg.CacheNamespace)
	if err != nil {
		return err
	}
	defer func() { _ = cacheClient.Close() }()
	limiter := cache.NewFallbackLimiter(cacheClient)

	var remote *search.RemoteClient
	if cfg.SearchMode != config.SearchOff {
		remote = search.NewRemote(cfg.SearchURL, []byte(cfg.SearchHMACKey), cfg.SearchTimeout)
	}
	searchSvc := search.NewService(search.NewSQL(), remote, log)

	embedded, err := migrate.EmbeddedVersion()
	if err != nil {
		return err
	}

	registry := prometheus.NewRegistry()
	jobMetrics := jobs.NewMetrics(registry)

	srv := httpapi.New(httpapi.Deps{
		Config:                cfg,
		Resolver:              resolver,
		Pools:                 pools,
		Cache:                 cacheClient,
		Limiter:               limiter,
		Search:                searchSvc,
		Logger:                log,
		EmbeddedSchemaVersion: embedded,
		QueueSnapshot: func(ctx context.Context) (jobs.Snapshot, error) {
			return jobs.Collect(ctx, pools.Default(), jobMetrics)
		},
	})

	// Metrics listen on their OWN address, never on the public API server.
	// api/openapi.yaml is the public contract and the both-direction check
	// would otherwise have to carry /metrics, which no client should see.
	metricsSrv := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           promhttp.HandlerFor(registry, promhttp.HandlerOpts{Registry: registry}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics listener stopped", "error", err.Error())
		}
	}()

	apiSrv := newAPIServer(cfg, srv.Handler())

	errCh := make(chan error, 1)
	go func() {
		log.Info("vizra-api listening", "addr", cfg.ListenAddr, "metrics_addr", cfg.MetricsAddr)
		if err := apiSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// Drain first so probes turn red and the load balancer stops sending work,
	// THEN stop accepting: shutting down immediately drops requests that were
	// already routed here.
	log.Info("vizra-api draining", "grace", cfg.ShutdownGrace.String())
	srv.Drain()

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownGrace)
	defer cancel()
	_ = metricsSrv.Shutdown(shutdownCtx)
	if err := apiSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("vizra-api: shutdown did not finish within the grace period: %w", err)
	}
	log.Info("vizra-api stopped")
	return nil
}
