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
	{Name: "VIZRA_SEARCH_HMAC_KEY", Secret: true, Doc: "Shared secret for the internal search contract. At least 32 bytes. Required when VIZRA_SEARCH_MODE is not off."},
	{Name: "VIZRA_SEARCH_TIMEOUT", Default: "2s", Doc: "Per-request timeout for internal search calls."},
	{Name: "VIZRA_CORS_ALLOWED_ORIGINS", Doc: "Comma-separated exact origins. Production refuses '*'."},
	{Name: "VIZRA_ALLOW_INSECURE_PUBLIC_ORIGIN", Doc: "Set to true to allow a plain-http VIZRA_PUBLIC_ORIGIN in production. Explicit by design (ADR-002)."},
	{Name: "VIZRA_QUEUE_AGE_THRESHOLD", Default: "15m", Doc: "Oldest queued job age above which readiness reports degraded and doctor FAILs (Q-028)."},
	{Name: "VIZRA_WORKER_CONCURRENCY", Default: "4", Doc: "Maximum jobs a worker process runs at once."},
	{Name: "VIZRA_JOB_LEASE", Default: "60s", Doc: "Lease duration. The heartbeat renews at lease/3."},
	{Name: "VIZRA_JOB_TIMEOUT", Default: "5m", Doc: "Per-job wall-clock timeout, applied as a context deadline."},
	{Name: "VIZRA_SHUTDOWN_GRACE", Default: "20s", Doc: "Time allowed for in-flight requests after SIGTERM."},
}

// AllKeys returns the registry followed by the escape hatches: the complete set
// of names LoadFrom consults.
func AllKeys() []Key {
	out := make([]Key, 0, len(Registry)+len(EscapeHatches))
	out = append(out, Registry...)
	out = append(out, EscapeHatches...)
	return out
}
