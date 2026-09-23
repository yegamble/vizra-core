// Package config is the single seam through which boot, `vizra setup`,
// `vizra doctor` and CI agree on what a valid environment is (ADR-002
// § Configuration ownership). There is exactly one validator: LoadFrom. Setup
// and doctor validate a candidate env file by calling CheckEnv, which runs the
// same code boot runs, so the three can never drift apart.
//
// validate collects every problem instead of returning the first, because an
// operator fixing one variable at a time from a chain of single-error boots is
// how a misconfiguration survives to production.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Mode is the boot mode. Production enables the fail-secure block.
type Mode string

const (
	ModeDevelopment Mode = "development"
	ModeProduction  Mode = "production"
)

// SearchMode selects the core<->search topology (ADR-002, Q-001).
type SearchMode string

const (
	SearchOff      SearchMode = "off"
	SearchManaged  SearchMode = "managed"
	SearchExternal SearchMode = "external"
)

// Config is the validated environment. Secret fields are never included in any
// String, log or doctor output; see Redacted.
type Config struct {
	Mode         Mode
	PublicOrigin string
	ListenAddr   string
	MetricsAddr  string
	SiteHandle   string

	DatabaseURL    string
	CacheURL       string
	CacheNamespace string
	StoragePrefix  string

	SessionSecret string
	MFAKeyKEK     string

	SearchMode    SearchMode
	SearchURL     string
	SearchHMACKey string
	SearchTimeout time.Duration

	CORSAllowedOrigins  []string
	AllowInsecureOrigin bool

	OwnerClaimAnnounce OwnerClaimAnnounce
	OwnerClaimTTL      time.Duration

	QueueAgeThreshold time.Duration
	WorkerConcurrency int
	JobLease          time.Duration
	JobTimeout        time.Duration
	ShutdownGrace     time.Duration
}

// OwnerClaimAnnounce says where an unclaimed instance announces its claim token.
//
// The default is deliberately Off. A token printed to stderr is captured by
// every Docker log driver, shipped to whatever aggregator the operator runs,
// retained for that pipeline's retention period, and pasted into issue trackers
// along with the rest of `docker compose logs`. VZ-INSTALL-003's privacy case —
// "Token never appears in HTTP responses or non-local logs" — is an acceptance
// bullet, so the safe default wins over the more discoverable one, and the
// aggregation-safe path (`vizra claim-token`, whose output goes to the
// operator's terminal rather than the container's log stream) is the primary.
type OwnerClaimAnnounce string

const (
	// OwnerClaimAnnounceOff prints only the command that mints a token.
	OwnerClaimAnnounceOff OwnerClaimAnnounce = "off"
	// OwnerClaimAnnounceStderr prints the token itself, once, on stderr.
	OwnerClaimAnnounceStderr OwnerClaimAnnounce = "stderr"
)

// Lookup is os.LookupEnv, or any equivalent over a candidate env file.
type Lookup func(string) (string, bool)

// Problem is one validation failure. Value is deliberately absent: a problem
// with a secret must not print the secret.
type Problem struct {
	Key     string
	Message string
}

func (p Problem) String() string { return p.Key + ": " + p.Message }

// ValidationError carries every problem found, sorted by key.
type ValidationError struct{ Problems []Problem }

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = p.String()
	}
	return fmt.Sprintf("configuration is invalid (%d problem(s)):\n  - %s",
		len(e.Problems), strings.Join(parts, "\n  - "))
}

// Has reports whether a problem was recorded for key.
func (e *ValidationError) Has(key string) bool {
	for _, p := range e.Problems {
		if p.Key == key {
			return true
		}
	}
	return false
}

// Load reads the process environment.
func Load() (*Config, error) { return LoadFrom(os.LookupEnv) }

// CheckEnv validates a candidate env file, represented as a map. `vizra setup`
// and `vizra doctor` call this so that what they accept is exactly what boot
// accepts.
func CheckEnv(env map[string]string) error {
	_, err := LoadFrom(func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	})
	return err
}

// knownDevSecrets are values that must never reach production, whatever their
// length. A 64-character "changeme-changeme-..." passes a length check and is
// still a published default.
var knownDevSecrets = []string{
	"change", "changeme", "dev", "devsecret", "development", "example",
	"insecure", "password", "placeholder", "please-change-me", "secret",
	"test", "testing", "todo", "vizra", "vizra-dev", "xxx", "0000", "1234",
}

// isPublishedSecret reports an exact match against a value this repository
// publishes. Exact, not substring: these are known strings, not a heuristic.
func isPublishedSecret(v string) bool {
	for _, p := range knownPublishedSecrets {
		if v == p {
			return true
		}
	}
	return false
}

func looksLikeDevSecret(v string) bool {
	l := strings.ToLower(v)
	for _, bad := range knownDevSecrets {
		// Contains, not equality: a padded or repeated development default is
		// still a published value.
		if strings.Contains(l, bad) {
			return true
		}
	}
	return false
}

const minSecretBytes = 32

// knownPublishedSecrets are values this repository PUBLISHES. They are refused
// in production by exact match, at any length, regardless of what the substring
// heuristic thinks.
//
// The heuristic cannot see them: they are 32 bytes and match no English word.
// But the first one is the key in api/search-hmac-testvectors.json — the file
// that documents the very key it is for, with the field named `key_utf8` next
// to a working example. That is where an operator wiring up search will look,
// and a core<->search channel signed with a key anyone can read from the
// repository is a full compromise of that boundary: the contract carries viewer
// identity and an index-events endpoint, and there is no nonce store yet.
//
// Refusing three exact strings costs four lines. The general problem — "you
// cannot denylist the internet" — is unsolvable; this specific one is nearly
// free, because we published these ourselves.
//
// TestProductionRefusesPublishedTestKeys reads the first value FROM the vectors
// file at test time, so editing that file without updating this list is red.
var knownPublishedSecrets = []string{
	// api/search-hmac-testvectors.json, field "key_utf8".
	"Ar4Lo8Cq2Ei6Uk0Wn3Sv7Yb1Md5Pt9Xz",
	// internal/search/search_test.go, testKey.
	"Aa1Bb2Cc3Dd4Aa1Bb2Cc3Dd4Aa1Bb2Cc",
}

// KnownPublishedSecrets returns a copy, so a test can assert its own fixtures
// do not collide with the denylist.
func KnownPublishedSecrets() []string {
	out := make([]string, len(knownPublishedSecrets))
	copy(out, knownPublishedSecrets)
	return out
}

// LoadFrom is the one validator. Boot, setup, doctor and CI all reach the
// fail-secure block through here.
func LoadFrom(lookup Lookup) (*Config, error) {
	get := func(k string) string {
		if v, ok := lookup(k); ok {
			return strings.TrimSpace(v)
		}
		return defaultFor(k)
	}

	var probs []Problem
	bad := func(k, msg string) { probs = append(probs, Problem{Key: k, Message: msg}) }

	c := &Config{
		Mode:           Mode(strings.ToLower(get("VIZRA_MODE"))),
		PublicOrigin:   strings.TrimRight(get("VIZRA_PUBLIC_ORIGIN"), "/"),
		ListenAddr:     get("VIZRA_LISTEN_ADDR"),
		MetricsAddr:    get("VIZRA_METRICS_ADDR"),
		SiteHandle:     get("VIZRA_SITE_HANDLE"),
		DatabaseURL:    get("DATABASE_URL"),
		CacheURL:       get("VIZRA_CACHE_URL"),
		CacheNamespace: get("VIZRA_CACHE_NAMESPACE"),
		StoragePrefix:  get("VIZRA_STORAGE_PREFIX"),
		SessionSecret:  get("VIZRA_SESSION_SECRET"),
		MFAKeyKEK:      get("VIZRA_MFA_KEY_KEK"),
		SearchMode:     SearchMode(strings.ToLower(get("VIZRA_SEARCH_MODE"))),
		SearchURL:      strings.TrimRight(get("VIZRA_SEARCH_URL"), "/"),
		SearchHMACKey:  get("SEARCH_HMAC_KEY"),
	}

	switch c.Mode {
	case ModeDevelopment, ModeProduction:
	default:
		bad("VIZRA_MODE", "must be 'development' or 'production'")
	}
	production := c.Mode == ModeProduction

	switch c.SearchMode {
	case SearchOff, SearchManaged, SearchExternal:
	default:
		bad("VIZRA_SEARCH_MODE", "must be 'off', 'managed' or 'external'")
	}

	c.AllowInsecureOrigin = truthy(get("VIZRA_ALLOW_INSECURE_PUBLIC_ORIGIN"))
	c.CORSAllowedOrigins = splitList(get("VIZRA_CORS_ALLOWED_ORIGINS"))

	c.SearchTimeout = mustDuration(get, bad, "VIZRA_SEARCH_TIMEOUT")
	c.QueueAgeThreshold = mustDuration(get, bad, "VIZRA_QUEUE_AGE_THRESHOLD")
	c.JobLease = mustDuration(get, bad, "VIZRA_JOB_LEASE")
	c.JobTimeout = mustDuration(get, bad, "VIZRA_JOB_TIMEOUT")
	c.ShutdownGrace = mustDuration(get, bad, "VIZRA_SHUTDOWN_GRACE")
	c.OwnerClaimTTL = mustDuration(get, bad, "VIZRA_OWNER_CLAIM_TTL")
	c.OwnerClaimAnnounce = OwnerClaimAnnounce(strings.ToLower(get("VIZRA_OWNER_CLAIM_ANNOUNCE")))
	switch c.OwnerClaimAnnounce {
	case OwnerClaimAnnounceOff, OwnerClaimAnnounceStderr:
	default:
		bad("VIZRA_OWNER_CLAIM_ANNOUNCE", "must be 'off' or 'stderr'")
	}
	c.WorkerConcurrency = int(mustInt64(get, bad, "VIZRA_WORKER_CONCURRENCY"))

	if c.WorkerConcurrency < 1 {
		bad("VIZRA_WORKER_CONCURRENCY", "must be at least 1")
	}
	if c.JobLease > 0 && c.JobTimeout > 0 && c.JobTimeout <= c.JobLease {
		bad("VIZRA_JOB_TIMEOUT", "must exceed VIZRA_JOB_LEASE, or a running job loses its lease before it can finish")
	}
	if c.SiteHandle == "" {
		bad("VIZRA_SITE_HANDLE", "must not be empty")
	}
	if c.CacheNamespace == "" {
		bad("VIZRA_CACHE_NAMESPACE", "must not be empty; it is supplied by the site resolver, never hardcoded")
	}
	if c.StoragePrefix == "" {
		bad("VIZRA_STORAGE_PREFIX", "must not be empty; it is supplied by the site resolver, never a hardcoded root")
	}

	// Public origin.
	if c.PublicOrigin == "" {
		bad("VIZRA_PUBLIC_ORIGIN", "must be set")
	} else if u, err := url.Parse(c.PublicOrigin); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		bad("VIZRA_PUBLIC_ORIGIN", "must be an absolute http or https origin, for example https://photos.example.org")
	} else if u.Path != "" && u.Path != "/" {
		bad("VIZRA_PUBLIC_ORIGIN", "must not carry a path")
	} else if production && u.Scheme == "http" && !c.AllowInsecureOrigin {
		bad("VIZRA_PUBLIC_ORIGIN", "production refuses a plain-http origin; set VIZRA_ALLOW_INSECURE_PUBLIC_ORIGIN=true only behind a trusted TLS terminator")
	} else if n := NormalizeOrigin(c.PublicOrigin); n == "" {
		// Reached only for a shape url.Parse accepted but that cannot be compared
		// with a browser's Origin — in practice a non-ASCII host.
		bad("VIZRA_PUBLIC_ORIGIN", "must be comparable with a browser Origin header; write an internationalised host in its A-label (punycode) form, for example https://xn--80ak6aa92e.example")
	} else {
		// Store the NORMALISED origin: lowercased scheme and host, default port
		// elided, trailing slash and trailing dot stripped. The Origin check is a
		// value comparison, and normalising once at boot is what keeps a single
		// trailing slash in .env from 403-ing every browser claim while curl
		// still works.
		c.PublicOrigin = n
	}

	// DSN and cache URL.
	if c.DatabaseURL == "" {
		bad("DATABASE_URL", "must be set")
	} else if !strings.HasPrefix(c.DatabaseURL, "postgres://") && !strings.HasPrefix(c.DatabaseURL, "postgresql://") {
		bad("DATABASE_URL", "must be a postgres:// or postgresql:// DSN")
	}
	if c.CacheURL == "" {
		bad("VIZRA_CACHE_URL", "must be set")
	} else if !strings.HasPrefix(c.CacheURL, "redis://") && !strings.HasPrefix(c.CacheURL, "rediss://") {
		bad("VIZRA_CACHE_URL", "must be a redis:// or rediss:// URL (Valkey speaks the same scheme)")
	}

	// Search topology.
	if c.SearchMode != SearchOff {
		if c.SearchURL == "" {
			bad("VIZRA_SEARCH_URL", "must be set when VIZRA_SEARCH_MODE is not 'off'")
		} else if u, err := url.Parse(c.SearchURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			bad("VIZRA_SEARCH_URL", "must be an absolute http or https URL")
		}
		switch {
		case len(c.SearchHMACKey) < minSecretBytes:
			bad("SEARCH_HMAC_KEY", fmt.Sprintf("must be at least %d bytes when VIZRA_SEARCH_MODE is not 'off'", minSecretBytes))
		case production && isPublishedSecret(c.SearchHMACKey):
			// Named first and specifically: this is the value an operator is
			// most likely to have copied, and the message has to tell them why
			// it will not do — without echoing it.
			bad("SEARCH_HMAC_KEY",
				"this value is published in this repository (api/search-hmac-testvectors.json is a TEST VECTOR, not a configuration value). "+
					"Generate a real key: openssl rand -base64 32")
		case production && looksLikeDevSecret(c.SearchHMACKey):
			bad("SEARCH_HMAC_KEY", "production refuses a known development value")
		}
	}

	// ---- Fail-secure production block (ADR-002 § Configuration ownership) ----
	if production {
		for _, k := range []struct {
			name string
			val  string
		}{
			{"VIZRA_SESSION_SECRET", c.SessionSecret},
			{"VIZRA_MFA_KEY_KEK", c.MFAKeyKEK},
		} {
			switch {
			case k.val == "":
				// ADR-003: an unset MFA KEK is a boot refusal, never a warning.
				bad(k.name, "must be set in production")
			case isPublishedSecret(k.val):
				// Checked BEFORE the length rule: a published value is refused
				// at any length, and the operator needs the specific reason.
				bad(k.name, "this value is published in this repository and must never be a production secret. "+
					"Generate a real one: openssl rand -base64 32")
			case len(k.val) < minSecretBytes:
				bad(k.name, fmt.Sprintf("must be at least %d bytes in production; got %d", minSecretBytes, len(k.val)))
			case looksLikeDevSecret(k.val):
				bad(k.name, "production refuses a known development value")
			}
		}

		for _, o := range c.CORSAllowedOrigins {
			if o == "*" {
				bad("VIZRA_CORS_ALLOWED_ORIGINS", "production refuses the wildcard origin '*'; list exact origins")
			} else if u, err := url.Parse(o); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				bad("VIZRA_CORS_ALLOWED_ORIGINS", "each entry must be an absolute http or https origin; got "+redactOrigin(o))
			} else if u.Scheme == "http" && !c.AllowInsecureOrigin {
				bad("VIZRA_CORS_ALLOWED_ORIGINS", "production refuses a plain-http CORS origin: "+redactOrigin(o))
			}
		}

		// Every RETIRED name, refused by name.
		//
		// There is no compatibility alias — nothing is deployed — so a leftover
		// old name has no effect at all. Ignoring it silently is the dangerous
		// reading: the operator's file looks configured, and the process booted
		// without the secret. Refused on PRESENCE with any non-empty value,
		// which is the same rule the value-bearing escape hatches use: a
		// completely empty `KEY=` is tolerated so a template may carry the name
		// as a tombstone, and whitespace is refused rather than trimmed away
		// because `KEY= ` is ambiguous and the fail-secure reading of an
		// ambiguous env file is that the value is set. The value is never
		// echoed — it is a secret.
		for _, r := range RetiredKeys {
			raw, ok := lookup(r.Name)
			if !ok || raw == "" {
				continue
			}
			bad(r.Name, fmt.Sprintf(
				"was renamed to %s and is NO LONGER READ. There is no compatibility alias, so this value has "+
					"no effect: %s. Rename the variable — production will not boot believing a key is configured "+
					"when none is.", r.ReplacedBy, r.Why))
		}

		// Every dev escape hatch, refused by name.
		//
		// Two rules, because not every hatch is a boolean. A BOOLEAN hatch is
		// refused when truthy, which preserves the deliberate affordance of a
		// shared template that lists the hatches set to 0. A VALUE-BEARING
		// hatch — VIZRA_DEV_AUTOLOGIN_USER, whose documented value is a
		// username — is refused on PRESENCE with any non-empty value, because
		// no realistic setting of it is "truthy" and testing truthiness meant
		// it was never refused at all.
		for _, h := range EscapeHatches {
			raw, ok := lookup(h.Name)
			if !ok {
				continue
			}
			switch {
			case h.RefuseIfPresent:
				// Only a COMPLETELY empty value is tolerated, so an operator may
				// leave the key in a template with nothing after the `=`.
				// Whitespace is refused rather than trimmed away: `KEY= ` is
				// ambiguous, and the fail-secure reading of an ambiguous env
				// file is that the hatch is set. The value is never echoed — it
				// may name a real account.
				if raw != "" {
					bad(h.Name, "is a development-only escape hatch that carries a value, and must not be present in production")
				}
			case truthy(strings.TrimSpace(raw)):
				bad(h.Name, "is a development-only escape hatch and must not be set in production")
			}
		}
	}

	if len(probs) > 0 {
		sort.Slice(probs, func(i, j int) bool {
			if probs[i].Key != probs[j].Key {
				return probs[i].Key < probs[j].Key
			}
			return probs[i].Message < probs[j].Message
		})
		return nil, &ValidationError{Problems: probs}
	}
	return c, nil
}

// AsValidationError extracts a *ValidationError from err, if it is one.
func AsValidationError(err error) (*ValidationError, bool) {
	var ve *ValidationError
	ok := errors.As(err, &ve)
	return ve, ok
}

func defaultFor(k string) string {
	for _, key := range Registry {
		if key.Name == k {
			return key.Default
		}
	}
	return ""
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	}
	return false
}

func splitList(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func mustDuration(get func(string) string, bad func(string, string), key string) time.Duration {
	raw := get(key)
	d, err := time.ParseDuration(raw)
	if err != nil {
		bad(key, "must be a duration such as 15m or 2s")
		return 0
	}
	if d <= 0 {
		bad(key, "must be greater than zero")
		return 0
	}
	return d
}

func mustInt64(get func(string) string, bad func(string, string), key string) int64 {
	raw := get(key)
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		bad(key, "must be an integer")
		return 0
	}
	return n
}

// redactOrigin keeps the scheme and host of a malformed origin out of a log
// line if it contains userinfo, which is the one place a credential can hide in
// something that looks like a URL.
func redactOrigin(o string) string {
	if u, err := url.Parse(o); err == nil && u.User != nil {
		u.User = url.User("redacted")
		return u.String()
	}
	return o
}
