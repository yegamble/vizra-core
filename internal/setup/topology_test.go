package setup

import (
	"strings"
	"testing"
)

// Each topology answer takes effect through COMPOSE_FILE/COMPOSE_PROFILES,
// golden per supported prod shape (meta scripts/compose-shapes.json).
func TestTopologyComposeGolden(t *testing.T) {
	const b, p = "docker-compose.yml:docker-compose.prod.yml", "core,frontend"
	cases := []struct {
		name  string
		set   map[string]string
		file  string
		profs string
	}{
		{"prod-default", nil, b, p + ",edge"},
		{"prod-external-tls", map[string]string{"tls": "external"}, b, p},
		{"prod-external-postgres", map[string]string{"postgres": "external", "postgres_dsn": "postgres://db.invalid/v"}, b + ":docker-compose.external-postgres.yml", p + ",edge"},
		{"prod-external-redis", map[string]string{"redis": "external", "redis_url": "rediss://c.invalid/0"}, b + ":docker-compose.external-redis.yml", p + ",edge"},
		{"prod-external-both", map[string]string{"postgres": "external", "postgres_dsn": "postgres://db.invalid/v", "redis": "external", "redis_url": "rediss://c.invalid/0"},
			b + ":docker-compose.external-postgres.yml:docker-compose.external-redis.yml", p + ",edge"},
		{"prod-external-clickhouse", map[string]string{"analytics": "external"}, b + ":docker-compose.external-clickhouse.yml", p + ",edge,analytics"},
		{"prod-external-ipfs", map[string]string{"ipfs": "external"}, b + ":docker-compose.external-ipfs.yml", p + ",edge,ipfs"},
		{"prod-all-optional", map[string]string{"analytics": "managed", "ipfs": "managed", "search": "managed"}, b, p + ",edge,worker,search,analytics,ipfs"},
	}
	for _, c := range cases {
		a := base()
		for k, v := range c.set {
			a[k] = v
		}
		env, err := resolve(t, nil, a)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if env["COMPOSE_FILE"] != c.file || env["COMPOSE_PROFILES"] != c.profs {
			t.Errorf("%s: COMPOSE_FILE=%q COMPOSE_PROFILES=%q, want %q %q", c.name, env["COMPOSE_FILE"], env["COMPOSE_PROFILES"], c.file, c.profs)
		}
		for _, k := range []string{"VIZRA_POSTGRES_MODE", "VIZRA_REDIS_MODE", "VIZRA_ANALYTICS_MODE", "VIZRA_IPFS_MODE"} {
			if _, ok := env[k]; ok {
				t.Errorf("%s: dead key %s written", c.name, k)
			}
		}
		// re-run recovers the topology answers from the written file
		again, err := resolve(t, env, map[string]string{})
		if err != nil || again["COMPOSE_FILE"] != c.file || again["COMPOSE_PROFILES"] != c.profs {
			t.Errorf("%s: re-run changed topology: %v %v", c.name, err, again["COMPOSE_FILE"])
		}
	}
}

// A combination no supported shape covers is refused, not rendered.
func TestTopologyUnsupportedRefused(t *testing.T) {
	for _, set := range []map[string]string{
		{"analytics": "external", "ipfs": "external"},
		{"postgres": "external", "postgres_dsn": "postgres://db.invalid/v", "analytics": "managed"},
		{"search": "managed"},
	} {
		a := base()
		for k, v := range set {
			a[k] = v
		}
		if _, err := resolve(t, nil, a); err == nil || !strings.Contains(err.Error(), "not a supported compose shape") {
			t.Errorf("%v: err=%v, want refusal", set, err)
		}
	}
}

// Questions whose keys no merged reader consumes are not asked.
func TestRemovedQuestionsNotAsked(t *testing.T) {
	for _, id := range []string{"storage", "s3_endpoint", "s3_bucket", "s3_access_key", "s3_secret_key", "smtp", "smtp_url", "smtp_from", "registration", "import"} {
		if Lookup(id).ID != "" {
			t.Errorf("question %q is still asked; no core reader exists", id)
		}
	}
	env, err := resolve(t, nil, base())
	if err != nil {
		t.Fatal(err)
	}
	for k := range env {
		if strings.HasPrefix(k, "VIZRA_S3_") || strings.HasPrefix(k, "VIZRA_SMTP_") || k == "VIZRA_STORAGE_BACKEND" || k == "VIZRA_REGISTRATION_MODE" || k == "VIZRA_IMPORT_MODE" {
			t.Errorf("dead key %s written", k)
		}
	}
}
