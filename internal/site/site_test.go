package site_test

import (
	"context"
	"strings"
	"testing"

	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/site"
)

func resolver(t *testing.T, origin string) *site.Resolver {
	t.Helper()
	cfg, err := config.LoadFrom(func(k string) (string, bool) {
		switch k {
		case "VIZRA_PUBLIC_ORIGIN":
			return origin, true
		case "DATABASE_URL":
			return "postgres://vizra:pw@db:5432/vizra", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("test configuration: %v", err)
	}
	return site.NewResolver(cfg)
}

// ADR-007: core has exactly one site, and there is NO DSN column anywhere — the
// registry is that single row plus the one DATABASE_URL.
func TestCoreHasExactlyOneSite(t *testing.T) {
	r := resolver(t, "https://photos.example.org")
	sites := r.Sites()
	if len(sites) != 1 {
		t.Fatalf("Sites() returned %d entries, want exactly 1 in core", len(sites))
	}
	s := sites[0]
	if s.Handle != "default" {
		t.Errorf("handle = %q, want default", s.Handle)
	}
	if s.DSN == "" {
		t.Error("the site carries no DSN; the worker and the migrator iterate Sites() and need one")
	}
}

// Sites() must hand out a copy: a caller that mutates the slice must not be able
// to change the registry for every other caller.
func TestSitesReturnsACopy(t *testing.T) {
	r := resolver(t, "https://photos.example.org")
	got := r.Sites()
	got[0].Handle = "tampered"
	if r.Sites()[0].Handle != "default" {
		t.Fatal("Sites() handed out the backing array; a caller can rewrite the registry")
	}
}

// Q-008 item 2: hostname resolution, in one place. The port is ignored so a
// development instance on :8080 resolves the same as production on :443.
func TestByHostIgnoresPortAndCase(t *testing.T) {
	r := resolver(t, "https://photos.example.org")
	for _, host := range []string{
		"photos.example.org",
		"photos.example.org:443",
		"PHOTOS.EXAMPLE.ORG",
		"photos.example.org.", // a fully-qualified name with the trailing dot
	} {
		s, err := r.ByHost(host)
		if err != nil {
			t.Errorf("ByHost(%q) = %v", host, err)
			continue
		}
		if s.Handle != "default" {
			t.Errorf("ByHost(%q) resolved to %q", host, s.Handle)
		}
	}
}

// Core falls back to the single site for an unknown Host, because a reverse
// proxy, a health checker and an IP-literal request all arrive with a Host the
// operator never configured. Under tenancy this becomes ErrUnknownHost; the
// seam is here so that change is one function.
func TestUnknownHostFallsBackToTheSingleSiteInCore(t *testing.T) {
	r := resolver(t, "https://photos.example.org")
	for _, host := range []string{"10.0.0.7:8080", "kube-probe", "", "[::1]:8080"} {
		s, err := r.ByHost(host)
		if err != nil {
			t.Errorf("ByHost(%q) = %v; core must not 404 an unconfigured Host", host, err)
			continue
		}
		if s.Handle != "default" {
			t.Errorf("ByHost(%q) resolved to %q", host, s.Handle)
		}
	}
}

func TestContextRoundTrip(t *testing.T) {
	r := resolver(t, "https://photos.example.org")
	if _, ok := site.FromContext(context.Background()); ok {
		t.Fatal("a bare context reported a site; a handler would silently use a fabricated one")
	}
	ctx := site.NewContext(context.Background(), r.Default())
	got, ok := site.FromContext(ctx)
	if !ok || got.Handle != "default" {
		t.Fatalf("FromContext = %+v, %v", got, ok)
	}
}

// Q-008 items 3 and 4: the storage prefix and the cache namespace come from the
// resolver. These helpers exist so nothing builds a key by hand.
func TestKeysCarryTheResolverPrefixAndNamespace(t *testing.T) {
	cfg, err := config.LoadFrom(func(k string) (string, bool) {
		switch k {
		case "VIZRA_PUBLIC_ORIGIN":
			return "https://photos.example.org", true
		case "DATABASE_URL":
			return "postgres://vizra:pw@db:5432/vizra", true
		case "VIZRA_STORAGE_PREFIX":
			return "tenant-a", true
		case "VIZRA_CACHE_NAMESPACE":
			return "ns-a", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	s := site.NewResolver(cfg).Default()

	key := s.StorageKey("originals", "2026", "09", "uuid", "token.jpg")
	if !strings.HasPrefix(key, "tenant-a/") {
		t.Fatalf("StorageKey = %q, want the resolver prefix", key)
	}
	ck := s.CacheKey("asset", "abc", "v3")
	if !strings.HasPrefix(ck, "ns-a:") {
		t.Fatalf("CacheKey = %q, want the resolver namespace", ck)
	}
}
