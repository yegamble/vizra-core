package config

// Key is one configuration key with its single documented home. VZ-FOUND-006
// requires every key `Load` reads to have exactly one home; this registry is
// that home, and `make config-template-check` asserts `.env.example` matches it
// exactly, so a key added to the code without a template entry fails CI.
type Key struct {
	Name string
	// Secret marks a value that must never be logged, echoed by doctor, or
	// written to a queryable table (ADR-002 § Logging and redaction).
	Secret bool
	// RequiredInProduction marks a key whose absence is a production boot refusal.
	RequiredInProduction bool
	// RefuseIfPresent marks an escape hatch whose value is NOT a boolean, so
	// production must refuse it on PRESENCE with any non-empty value rather
	// than on truthiness.
	RefuseIfPresent bool
	// Default is the development default. Empty means "no default".
	Default string
	Doc     string
}

// EscapeHatch is a development-only switch. Production refuses to boot when any
// of these is set to a truthy value, by name, with the name in the error
// (ADR-002 § Configuration ownership). Adding a new hatch means adding it here;
// `TestEveryEscapeHatchIsRefusedInProduction` enumerates this slice, so a hatch
// that is not refused cannot be added without turning the suite red.
var EscapeHatches = []Key{
	{Name: "VIZRA_DEV_DISABLE_AUTH", Doc: "Skips authentication entirely. Development only."},
	{Name: "VIZRA_DEV_ALLOW_ANY_ORIGIN", Doc: "Disables the CSRF same-origin check. Development only."},
	{Name: "VIZRA_DEV_SKIP_MIGRATIONS", Doc: "Boots without applying migrations. Development only."},
	// Its value is a USERNAME, so no realistic setting of it is "truthy".
	// Refused on PRESENCE — see the production block in config.go.
	{Name: "VIZRA_DEV_AUTOLOGIN_USER", RefuseIfPresent: true, Doc: "Signs every request in as this user. Development only."},
	{Name: "VIZRA_DEV_FAKE_SEARCH", Doc: "Returns canned search results. Development only."},
	{Name: "VIZRA_DEV_INSECURE_COOKIES", Doc: "Drops the Secure attribute from session cookies. Development only."},
	{Name: "VIZRA_DEV_TRUST_ANY_HMAC", Doc: "Accepts any internal HMAC signature. Development only."},
}

// Registry is every key read by LoadFrom, in template order.
var Registry = []Key{
	{Name: "VIZRA_MODE", Default: "development", Doc: "development | production. Production enables the fail-secure checks."},
	{Name: "VIZRA_PUBLIC_ORIGIN", RequiredInProduction: true, Default: "http://localhost:8080", Doc: "Scheme, host and optional port the instance is reached at. The CSRF same-origin check compares against this."},
	{Name: "VIZRA_LISTEN_ADDR", Default: ":8080", Doc: "Address the public API server binds."},
	{Name: "VIZRA_METRICS_ADDR", Default: "127.0.0.1:9090", Doc: "Address the Prometheus listener binds. Never the same as VIZRA_LISTEN_ADDR: metrics are not part of the public API contract."},
	{Name: "VIZRA_SITE_HANDLE", Default: "default", Doc: "Handle of the single site row in core (ADR-007). One entry; the seam exists so tenancy is not precluded."},
	{Name: "DATABASE_URL", Secret: true, RequiredInProduction: true, Doc: "PostgreSQL DSN. The only DSN source in core (ADR-007 § The DSN source)."},
	{Name: "VIZRA_CACHE_URL", Secret: true, RequiredInProduction: true, Default: "redis://127.0.0.1:6379/0", Doc: "RESP server URL (Valkey managed, or any RESP >= 7.2 server). ADR-001 Q-004."},
	{Name: "VIZRA_CACHE_NAMESPACE", Default: "default", Doc: "Cache key namespace. Supplied by the site resolver; never hardcoded (Q-008 item 4)."},
	{Name: "VIZRA_STORAGE_PREFIX", Default: "default", Doc: "Storage key prefix. Supplied by the site resolver; never a hardcoded root (Q-008 item 3)."},
	{Name: "VIZRA_SESSION_SECRET", Secret: true, RequiredInProduction: true, Doc: "At least 32 bytes. Production refuses a known development value."},
	{Name: "VIZRA_MFA_KEY_KEK", Secret: true, RequiredInProduction: true, Doc: "Key-encryption key wrapping TOTP secrets. Unset in production is a boot refusal, never a warning (ADR-003)."},
	{Name: "VIZRA_SEARCH_MODE", Default: "off", Doc: "off | managed | external. Default off until M3 (ADR-002, Q-001)."},
	{Name: "VIZRA_SEARCH_URL", Doc: "Base URL of vizra-search. Required when VIZRA_SEARCH_MODE is not off."},
	// NOT VIZRA_-prefixed, and deliberately so. The canonical contract
	// api/search-internal.openapi.yaml names this secret `SEARCH_HMAC_KEY`, and
	// vizra-search reads it under that name. A variable the contract names keeps
	// that name in every service (AGENTS.md § Contract ownership); a per-service
	// spelling of one shared secret is a deployment template papering over a
	// disagreement, which is how one side ends up signing with a key the other
	// never loaded.
	{Name: "SEARCH_HMAC_KEY", Secret: true, Doc: "Shared secret for the internal search contract. At least 32 bytes. Required when VIZRA_SEARCH_MODE is not off. Named by the contract (api/search-internal.openapi.yaml), so it is NOT VIZRA_-prefixed."},
	{Name: "VIZRA_SEARCH_TIMEOUT", Default: "2s", Doc: "Per-request timeout for internal search calls."},
	{Name: "VIZRA_CORS_ALLOWED_ORIGINS", Doc: "Comma-separated exact origins. Production refuses '*'."},
	{Name: "VIZRA_ALLOW_INSECURE_PUBLIC_ORIGIN", Doc: "Set to true to allow a plain-http VIZRA_PUBLIC_ORIGIN in production. Explicit by design (ADR-002)."},
	{Name: "VIZRA_QUEUE_AGE_THRESHOLD", Default: "15m", Doc: "Oldest queued job age above which readiness reports degraded and doctor FAILs (Q-028)."},
	{Name: "VIZRA_WORKER_CONCURRENCY", Default: "4", Doc: "Maximum jobs a worker process runs at once."},
	{Name: "VIZRA_JOB_LEASE", Default: "60s", Doc: "Lease duration. The heartbeat renews at lease/3."},
	{Name: "VIZRA_JOB_TIMEOUT", Default: "5m", Doc: "Per-job wall-clock timeout, applied as a context deadline."},
	{Name: "VIZRA_SHUTDOWN_GRACE", Default: "20s", Doc: "Time allowed for in-flight requests after SIGTERM."},
	{Name: "VIZRA_OWNER_CLAIM_ANNOUNCE", Default: "off", Doc: "Where an unclaimed instance announces its owner-claim token: 'off' (default) prints only the command that mints one; 'stderr' WRITES A LIVE CREDENTIAL to the container log, which every log driver captures, ships and retains. Use 'stderr' only on a single host with no log aggregation."},
	{Name: "VIZRA_OWNER_CLAIM_TTL", Default: "1h", Doc: "How long an owner-claim token stays redeemable, at least 1m. It is minted on demand by `vizra claim-token`, so this only has to cover one claim attempt."},
}

// RetiredKey is a name this repository USED TO read and no longer does.
//
// Nothing is deployed, so there is no compatibility alias: the old name is not
// read, and its value has no effect. That is exactly why it has to be refused
// rather than ignored. An operator who carried the old name forward has a file
// that LOOKS configured — the secret is right there, spelled the way last
// week's template spelled it — while the process booted with no key at all. A
// silent ignore turns that into a running production instance whose operator
// believes a key is set.
//
// Production refuses these by name, with the replacement in the message. See
// the production block in config.go, and TestProductionRefusesARetiredKeyName.
type RetiredKey struct {
	Name       string
	ReplacedBy string
	Why        string
}

// RetiredKeys is every name production refuses because it was renamed.
var RetiredKeys = []RetiredKey{
	{
		Name:       "VIZRA_SEARCH_HMAC_KEY",
		ReplacedBy: "SEARCH_HMAC_KEY",
		Why: "the canonical contract api/search-internal.openapi.yaml names this secret SEARCH_HMAC_KEY " +
			"and vizra-search reads it under that name; core read a different spelling of the same shared secret",
	},
}

// DefaultFor returns the registry default for name, or "" when the key has no
// default or does not exist.
//
// It exists so a tool that legitimately reads ONE key — `vizra healthcheck`
// needs the address its service binds and nothing else — gets the SAME default
// the service would have got, without calling Load. Load is fail-secure: in
// production it refuses a process with no VIZRA_MFA_KEY_KEK, which is correct
// for a service and wrong for a probe, whose answer would then be "unhealthy"
// for a reason that has nothing to do with the service's health.
//
// This is not a second home for a key. The registry above is still the single
// home; this only reads it.
func DefaultFor(name string) string {
	for _, k := range Registry {
		if k.Name == name {
			return k.Default
		}
	}
	return ""
}

// AllKeys returns the registry followed by the escape hatches: the complete set
// of names LoadFrom consults. Retired names are NOT here — LoadFrom does not
// read them; it refuses them.
func AllKeys() []Key {
	out := make([]Key, 0, len(Registry)+len(EscapeHatches))
	out = append(out, Registry...)
	out = append(out, EscapeHatches...)
	return out
}
