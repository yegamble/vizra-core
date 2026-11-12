// Package setup is the question engine behind `vizra setup` (VZ-INSTALL-002).
// One engine serves the terminal interview, --non-interactive, --check and
// (later) --web: a question declares its id, the template keys it writes, a
// validator, a default rule, whether it is secret, and when it is asked.
package setup

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Question is one interview item.
type Question struct {
	ID     string
	Prompt string
	// Keys are the env template keys this answer writes. Keys[0] holds the
	// answer itself; the rest are derived by Write.
	Keys    []string
	Secret  bool
	Mint    bool // a blank answer mints a random value (secrets only)
	Choices []string
	// Default returns the offered default given answers so far ("" = none).
	Default func(a map[string]string) string
	// When reports whether the question applies given answers so far.
	When     func(a map[string]string) bool
	validate func(string) error
	// Write maps the answer to keys; nil writes it to Keys[0].
	Write func(v string, a map[string]string) map[string]string
	// derive recovers the answer from an existing env when Keys[0] does not
	// hold it verbatim (topology answers live in COMPOSE_FILE/PROFILES).
	derive func(id string, env map[string]string) (string, bool)
}

// answerFrom returns q's answer as recorded in env.
func (q Question) answerFrom(env map[string]string) (string, bool) {
	if q.derive != nil {
		return q.derive(q.ID, env)
	}
	v, ok := env[q.Keys[0]]
	return v, ok
}

var topoKeys = []string{"COMPOSE_FILE", "COMPOSE_PROFILES"}

func noKeys(string, map[string]string) map[string]string { return nil }

// Validate checks one answer.
func (q Question) Validate(v string) error {
	if len(q.Choices) > 0 && !slices.Contains(q.Choices, v) {
		return fmt.Errorf("%s: %q is not one of %s", q.ID, v, strings.Join(q.Choices, "|"))
	}
	if q.validate != nil {
		if err := q.validate(v); err != nil {
			return fmt.Errorf("%s: %w", q.ID, err)
		}
	}
	return nil
}

// PendingTemplateKeys are keys the interview writes that neither core's
// config.Registry nor the meta env/production.env.example carries yet; each
// has a merged reader (internal/setup/edge.go). TestS1 fails on any key that
// is in no template and not listed here, and on a listed key that has landed.
var PendingTemplateKeys = []string{"VIZRA_DOMAIN", "VIZRA_TLS_MODE"}

// ComposeKeys are read by Docker Compose itself from the `--env-file` file.
var ComposeKeys = topoKeys

var (
	hostRe   = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)
	handleRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	tagRe    = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
)

func is(k, v string) func(map[string]string) bool {
	return func(a map[string]string) bool { return a[k] == v }
}
func fixed(v string) func(map[string]string) string {
	return func(map[string]string) string { return v }
}

func nonEmpty(v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New("an answer is required")
	}
	return nil
}
func minLen(n int) func(string) error {
	return func(v string) error {
		if len(v) < n {
			return fmt.Errorf("must be at least %d bytes", n)
		}
		return nil
	}
}
func urlWith(schemes ...string) func(string) error {
	return func(v string) error {
		u, err := url.Parse(v)
		if err != nil || u.Host == "" || !slices.Contains(schemes, u.Scheme) {
			return fmt.Errorf("must be a %s URL", strings.Join(schemes, "/"))
		}
		return nil
	}
}

// Questions returns the interview in order.
func Questions() []Question {
	modes := []string{"off", "managed", "external"}
	return []Question{
		{ID: "domain", Prompt: "Domain the instance is reached at", Keys: []string{"VIZRA_DOMAIN"},
			validate: func(v string) error {
				if !hostRe.MatchString(v) {
					return errors.New("must be a bare host name, e.g. photos.example.org")
				}
				return nil
			}},
		{ID: "tls", Prompt: "TLS: acme (public certificate) | internal (local CA) | plain-http | external (your proxy terminates TLS)",
			Keys:    []string{"VIZRA_TLS_MODE", "VIZRA_PUBLIC_ORIGIN", "VIZRA_ALLOW_INSECURE_PUBLIC_ORIGIN"},
			Choices: []string{"acme", "internal", "plain-http", "external"}, Default: fixed("acme"),
			Write: func(v string, a map[string]string) map[string]string {
				m := map[string]string{"VIZRA_TLS_MODE": v, "VIZRA_PUBLIC_ORIGIN": "https://" + a["domain"], "VIZRA_ALLOW_INSECURE_PUBLIC_ORIGIN": ""}
				if v == "plain-http" {
					m["VIZRA_PUBLIC_ORIGIN"] = "http://" + a["domain"]
					m["VIZRA_ALLOW_INSECURE_PUBLIC_ORIGIN"] = "1"
				}
				return m
			}},
		{ID: "instance_name", Prompt: "Instance handle (lowercase letters, digits, dashes)", Keys: []string{"VIZRA_SITE_HANDLE"},
			Default: fixed("default"), validate: func(v string) error {
				if !handleRe.MatchString(v) {
					return errors.New("must be lowercase letters, digits and dashes")
				}
				return nil
			}},
		{ID: "release", Prompt: "Platform release tag (vX.Y.Z)", Keys: []string{"VIZRA_CORE_TAG", "VIZRA_USER_TAG", "VIZRA_SEARCH_TAG"},
			validate: func(v string) error {
				if !tagRe.MatchString(v) {
					return errors.New("must be a release tag vX.Y.Z; floating tags are refused")
				}
				return nil
			},
			Write: func(v string, _ map[string]string) map[string]string {
				return map[string]string{"VIZRA_CORE_TAG": v, "VIZRA_USER_TAG": v, "VIZRA_SEARCH_TAG": v}
			}},
		{ID: "postgres", Prompt: "PostgreSQL: managed (bundled) | external", Keys: topoKeys, Write: noKeys, derive: topologyAnswer, Choices: []string{"managed", "external"}, Default: fixed("managed")},
		{ID: "postgres_dsn", Prompt: "External PostgreSQL DSN", Keys: []string{"DATABASE_URL"}, Secret: true, When: is("postgres", "external"), validate: urlWith("postgres", "postgresql")},
		{ID: "postgres_password", Prompt: "Bundled PostgreSQL password (blank = generate)", Keys: []string{"POSTGRES_PASSWORD"}, Secret: true, Mint: true, When: is("postgres", "managed"), validate: minLen(24)},
		{ID: "redis", Prompt: "Redis/Valkey: managed (bundled) | external", Keys: topoKeys, Write: noKeys, derive: topologyAnswer, Choices: []string{"managed", "external"}, Default: fixed("managed")},
		{ID: "redis_url", Prompt: "External Redis URL", Keys: []string{"VIZRA_CACHE_URL"}, Secret: true, When: is("redis", "external"), validate: urlWith("redis", "rediss")},
		// Analytics (ClickHouse) and IPFS: the components are not built yet;
		// the values are the ones the meta compose overlays define today.
		{ID: "analytics", Prompt: "Analytics (ClickHouse): off | managed | external", Keys: topoKeys, Write: noKeys, derive: topologyAnswer, Choices: modes, Default: fixed("off")},
		{ID: "clickhouse_password", Prompt: "ClickHouse password (blank = generate)", Keys: []string{"CLICKHOUSE_PASSWORD"}, Secret: true, Mint: true, When: is("analytics", "managed"), validate: minLen(24)},
		{ID: "ipfs", Prompt: "IPFS pinning: off | managed | external (public media only)", Keys: topoKeys, Write: noKeys, derive: topologyAnswer, Choices: modes, Default: fixed("off")},
		{ID: "search", Prompt: "Search: off | managed | external", Keys: []string{"VIZRA_SEARCH_MODE"}, Choices: modes, Default: fixed("off")},
		{ID: "session_secret", Prompt: "Session secret (blank = generate)", Keys: []string{"VIZRA_SESSION_SECRET"}, Secret: true, Mint: true, validate: minLen(32)},
		{ID: "mfa_kek", Prompt: "MFA key-encryption key (blank = generate)", Keys: []string{"VIZRA_MFA_KEY_KEK"}, Secret: true, Mint: true, validate: minLen(32)},
		{ID: "search_hmac", Prompt: "Search HMAC key (blank = generate)", Keys: []string{"SEARCH_HMAC_KEY"}, Secret: true, Mint: true, validate: minLen(32)},
	}
}

func has(k string) func(map[string]string) bool {
	return func(a map[string]string) bool { return a[k] != "" }
}
func optional(f func(string) error) func(string) error {
	return func(v string) error {
		if v == "" {
			return nil
		}
		return f(v)
	}
}

// Lookup returns the question with id, or a zero Question.
func Lookup(id string) Question {
	for _, q := range Questions() {
		if q.ID == id {
			return q
		}
	}
	return Question{}
}

func mint() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
