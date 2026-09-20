// Package db owns the PostgreSQL connection pool.
//
// Q-008 checklist item (1): there is NO package-global pool. A pool belongs to
// a site and is handed to callers through the request context, so tenancy is a
// connection-routing concern rather than a rewrite. `make lint-imports`
// enforces the absence of the global.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yegamble/vizra-core/internal/site"
)

// Pools holds one pool per site. In core there is one entry.
type Pools struct {
	pools map[string]*pgxpool.Pool
	order []string
}

// Open builds a pool for every site the resolver knows. A failure to open any
// one of them closes the pools already opened, so a partial failure never
// leaves connections behind.
func Open(ctx context.Context, r *site.Resolver) (*Pools, error) {
	p := &Pools{pools: map[string]*pgxpool.Pool{}}
	for _, s := range r.Sites() {
		cfg, err := pgxpool.ParseConfig(s.DSN)
		if err != nil {
			p.Close()
			// Never include the DSN: it carries the password.
			return nil, fmt.Errorf("db: DATABASE_URL for site %q is not a valid DSN", s.Handle)
		}
		cfg.MaxConnIdleTime = 5 * time.Minute
		cfg.MaxConnLifetime = time.Hour
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("db: opening pool for site %q: %w", s.Handle, err)
		}
		p.pools[s.Handle] = pool
		p.order = append(p.order, s.Handle)
	}
	return p, nil
}

// For returns the pool of a site.
func (p *Pools) For(s site.Site) (*pgxpool.Pool, bool) {
	pool, ok := p.pools[s.Handle]
	return pool, ok
}

// Default returns the pool of the first site. Core has exactly one.
//
// A nil receiver returns nil rather than panicking: /schemaz must answer 200
// even when there is no database at all (ADR-002 § Probes), and a probe that
// panics is worse than one that reports "unknown".
func (p *Pools) Default() *pgxpool.Pool {
	if p == nil || len(p.order) == 0 {
		return nil
	}
	return p.pools[p.order[0]]
}

// Close releases every pool.
func (p *Pools) Close() {
	for _, pool := range p.pools {
		pool.Close()
	}
}

// Ping verifies the default pool answers. A readiness probe uses this; a
// failure is the one condition that makes /readyz 503 rather than degraded
// (ADR-002 § Probes).
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return fmt.Errorf("db: no pool")
	}
	return pool.Ping(ctx)
}
