// Package site is the tenancy seam of Q-008. In core it resolves exactly one
// site, built from configuration; under tenancy (M5) the same Resolver is
// backed by a control database mapping hostname -> DSN. Callers never learn
// which it is.
//
// Two rules this package exists to enforce, both from the Q-008 M0 plumbing
// checklist:
//
//   - Item (3) and (4): the storage key prefix and the cache key namespace come
//     from the resolved Site. Nothing in the codebase may hardcode a storage
//     root or a cache namespace; `make lint-imports` fails on an attempt.
//   - Item (2): hostname -> site resolution happens in exactly one middleware.
//     The Echo adapter for it lives in internal/httpapi (ADR-001 confines Echo
//     types to that package); this package owns the resolution and the context
//     plumbing, so there is still exactly one place that decides.
package site

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/yegamble/vizra-core/internal/config"
)

// Site is what every caller sees. DSN is part of the struct from M0 even though
// core has one, because the worker and the migrator iterate Sites() and must
// not grow a second code path when tenancy arrives (ADR-004, ADR-007).
type Site struct {
	Handle         string
	BaseURL        string
	DSN            string
	CacheNamespace string
	StoragePrefix  string
}

// Host returns the hostname of BaseURL, without port.
func (s Site) Host() string {
	u, err := url.Parse(s.BaseURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// Resolver resolves sites. Core's implementation returns one entry.
type Resolver struct {
	sites  []Site
	byHost map[string]Site
}

// ErrUnknownHost is returned when a request arrives for a host no site claims.
var ErrUnknownHost = errors.New("site: no site is configured for this host")

// NewResolver builds the core resolver: one site, from configuration. There is
// no DSN column anywhere in core (ADR-007 § The DSN source) — the registry is
// the single sites row plus the one DATABASE_URL.
func NewResolver(cfg *config.Config) *Resolver {
	s := Site{
		Handle:         cfg.SiteHandle,
		BaseURL:        cfg.PublicOrigin,
		DSN:            cfg.DatabaseURL,
		CacheNamespace: cfg.CacheNamespace,
		StoragePrefix:  cfg.StoragePrefix,
	}
	r := &Resolver{sites: []Site{s}, byHost: map[string]Site{}}
	if h := s.Host(); h != "" {
		r.byHost[strings.ToLower(h)] = s
	}
	return r
}

// Sites is what the worker and the migrator iterate. Exactly one entry in core.
func (r *Resolver) Sites() []Site {
	out := make([]Site, len(r.sites))
	copy(out, r.sites)
	return out
}

// Default is the single core site. It panics only if a Resolver was built by
// hand with no sites, which no constructor does.
func (r *Resolver) Default() Site { return r.sites[0] }

// ByHost performs item (2) of the Q-008 checklist. The port is ignored: a site
// is identified by hostname, so a development instance on :8080 resolves the
// same as production on :443.
//
// Core deliberately falls back to the single site rather than 404ing an unknown
// Host, because a reverse proxy, a health checker and an IP-literal request all
// arrive with a Host the operator never configured. Under tenancy this fallback
// is removed and ErrUnknownHost is returned; the seam is here so that change is
// one function, not a search across handlers.
func (r *Resolver) ByHost(host string) (Site, error) {
	h := strings.ToLower(host)
	if i := strings.LastIndex(h, ":"); i != -1 && !strings.Contains(h, "]") {
		h = h[:i]
	}
	h = strings.TrimSuffix(strings.Trim(h, "[]"), ".")
	if s, ok := r.byHost[h]; ok {
		return s, nil
	}
	if len(r.sites) == 1 {
		return r.sites[0], nil
	}
	return Site{}, ErrUnknownHost
}

type ctxKey struct{}

// NewContext carries the resolved site. Every handle — database pool, cache
// client, storage client, search client — is derived from this, never from a
// package global (Q-008 checklist item 1).
func NewContext(ctx context.Context, s Site) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// FromContext returns the resolved site. ok is false when the single site
// middleware did not run, which is a programming error in a request path.
func FromContext(ctx context.Context) (Site, bool) {
	s, ok := ctx.Value(ctxKey{}).(Site)
	return s, ok
}

// CacheKey builds a namespaced cache key. Nothing may build one by hand.
func (s Site) CacheKey(parts ...string) string {
	return s.CacheNamespace + ":" + strings.Join(parts, ":")
}

// StorageKey builds a prefixed storage key. Nothing may build one by hand
// (ADR-005 § Key grammar).
func (s Site) StorageKey(parts ...string) string {
	return s.StoragePrefix + "/" + strings.Join(parts, "/")
}
