// Command worker runs durable jobs (ADR-004). It is the ONLY process that
// decodes pixels, so a decoder bomb or a libvips crash costs a worker, not the
// API.
//
// It fans out over site.Resolver.Sites() — one entry in core — and leases from
// each site's database, so database-per-tenant is not precluded (Q-008 item 5).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/yegamble/vizra-core/internal/buildinfo"
	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/db"
	"github.com/yegamble/vizra-core/internal/jobs"
	"github.com/yegamble/vizra-core/internal/obs"
	"github.com/yegamble/vizra-core/internal/site"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("vizra-worker refused to start.\n%w", err)
	}

	log := obs.NewLogger(os.Stdout, cfg.Mode == config.ModeProduction)
	slog.SetDefault(log)
	log.Info("vizra-worker starting",
		"release", buildinfo.Release, "commit", buildinfo.Commit,
		"concurrency", cfg.WorkerConcurrency, "lease", cfg.JobLease.String())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	resolver := site.NewResolver(cfg)
	pools, err := db.Open(ctx, resolver)
	if err != nil {
		return err
	}
	defer pools.Close()

	byHandle := map[string]*pgxpool.Pool{}
	for _, s := range resolver.Sites() {
		p, ok := pools.For(s)
		if !ok {
			return fmt.Errorf("vizra-worker: no pool for site %q", s.Handle)
		}
		byHandle[s.Handle] = p
	}

	registry := prometheus.NewRegistry()
	metrics := jobs.NewMetrics(registry)

	metricsSrv := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           promhttp.HandlerFor(registry, promhttp.HandlerOpts{Registry: registry}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("metrics listener stopped", "error", err.Error())
		}
	}()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(sctx)
	}()

	w := jobs.NewWorker(resolver, byHandle, metrics, jobs.Options{
		Lease:       cfg.JobLease,
		Timeout:     cfg.JobTimeout,
		Concurrency: cfg.WorkerConcurrency,
		Logger:      log,
	})

	log.Info("vizra-worker running", "kinds", w.Kinds())
	err = w.Run(ctx)
	log.Info("vizra-worker stopped")
	if err != nil && err != context.Canceled {
		return err
	}
	return nil
}
