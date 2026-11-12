package setup

import (
	"fmt"
	"slices"
	"strings"
)

// Topology answers (postgres, redis, analytics, ipfs, plus tls and search)
// take effect only through COMPOSE_FILE and COMPOSE_PROFILES, which Docker
// Compose reads from the file passed with `--env-file`. No per-mode key is
// written: nothing would read it.

const composeBase = "docker-compose.yml:docker-compose.prod.yml"

// overlay files, in -f order, keyed by the answer that selects them.
var overlays = []struct{ id, file string }{
	{"postgres", "docker-compose.external-postgres.yml"},
	{"redis", "docker-compose.external-redis.yml"},
	{"analytics", "docker-compose.external-clickhouse.yml"},
	{"ipfs", "docker-compose.external-ipfs.yml"},
}

// supportedShapes mirrors the production shapes of the meta repo's
// scripts/compose-shapes.json (docs/META_REPO.md §2) that an interview can
// produce: file chain -> profile set. Anything else is refused.
var supportedShapes = map[string][]string{
	"prod-default":             {composeBase, "core,frontend,edge"},
	"prod-external-tls":        {composeBase, "core,frontend"},
	"prod-all-optional":        {composeBase, "core,frontend,edge,worker,search,analytics,ipfs"},
	"prod-external-postgres":   {composeBase + ":docker-compose.external-postgres.yml", "core,frontend,edge"},
	"prod-external-redis":      {composeBase + ":docker-compose.external-redis.yml", "core,frontend,edge"},
	"prod-external-both":       {composeBase + ":docker-compose.external-postgres.yml:docker-compose.external-redis.yml", "core,frontend,edge"},
	"prod-external-clickhouse": {composeBase + ":docker-compose.external-clickhouse.yml", "core,frontend,edge,analytics"},
	"prod-external-ipfs":       {composeBase + ":docker-compose.external-ipfs.yml", "core,frontend,edge,ipfs"},
}

// composeFor maps answers to COMPOSE_FILE and COMPOSE_PROFILES, refusing a
// combination no supported shape covers.
func composeFor(a map[string]string) (file, profiles string, err error) {
	files := []string{composeBase}
	for _, o := range overlays {
		if a[o.id] == "external" {
			files = append(files, o.file)
		}
	}
	profs := []string{"core", "frontend"}
	if a["tls"] != "external" {
		profs = append(profs, "edge")
	}
	if a["search"] == "managed" && a["analytics"] == "managed" && a["ipfs"] == "managed" {
		profs = append(profs, "worker")
	}
	if a["search"] == "managed" {
		profs = append(profs, "search")
	}
	// an external ClickHouse/IPFS overlay suppresses the container on its profile
	for _, id := range []string{"analytics", "ipfs"} {
		if a[id] != "off" && a[id] != "" {
			profs = append(profs, id)
		}
	}
	file, profiles = strings.Join(files, ":"), strings.Join(profs, ",")
	for _, s := range supportedShapes {
		if s[0] == file && s[1] == profiles {
			return file, profiles, nil
		}
	}
	names := make([]string, 0, len(supportedShapes))
	for n := range supportedShapes {
		names = append(names, n)
	}
	slices.Sort(names)
	return "", "", fmt.Errorf("topology: -f %s with profiles %s is not a supported compose shape (docs/META_REPO.md §2); supported: %s",
		file, profiles, strings.Join(names, ", "))
}

// topologyAnswer recovers a topology answer from a written env file, so a
// re-run keeps it (R1) without a per-mode key.
func topologyAnswer(id string, env map[string]string) (string, bool) {
	file, ok := env["COMPOSE_FILE"]
	if !ok {
		return "", false
	}
	for _, o := range overlays {
		if o.id == id && slices.Contains(strings.Split(file, ":"), o.file) {
			return "external", true
		}
	}
	if id == "analytics" || id == "ipfs" {
		if slices.Contains(strings.Split(env["COMPOSE_PROFILES"], ","), id) {
			return "managed", true
		}
		return "off", true
	}
	return "managed", true
}
