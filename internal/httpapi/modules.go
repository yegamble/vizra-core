package httpapi

import (
	"runtime/debug"
	"strings"
	"sync"
)

// reportedModules are the pins an operator diagnosing an instance actually
// needs: the HTTP framework, the database driver, the cache client and the
// tracing library. The full module graph is not served, because /version is a
// public endpoint and a complete dependency inventory is a gift to anyone
// matching an instance against a CVE feed.
var reportedModules = []string{
	"github.com/labstack/echo/v5",
	"github.com/jackc/pgx/v5",
	"github.com/redis/go-redis/v9",
	"github.com/golang-migrate/migrate/v4",
	"go.opentelemetry.io/otel",
}

var moduleVersionsOnce = sync.OnceValue(func() map[string]string {
	out := map[string]string{}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return out
	}
	for _, dep := range bi.Deps {
		for _, want := range reportedModules {
			if dep.Path == want {
				out[shortName(dep.Path)] = dep.Version
			}
		}
	}
	return out
})

func moduleVersions() map[string]string { return moduleVersionsOnce() }

func shortName(path string) string {
	parts := strings.Split(path, "/")
	return parts[len(parts)-1]
}
