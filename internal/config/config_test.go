package config

import (
	"strings"
	"testing"
)

// validProduction is a minimal environment that must boot in production mode.
// Every negative test below mutates exactly one key of this map, so a test that
// goes red names the single thing that broke.
func validProduction() map[string]string {
	return map[string]string{
		"VIZRA_MODE":           "production",
		"VIZRA_PUBLIC_ORIGIN":  "https://photos.example.org",
		"DATABASE_URL":         "postgres://vizra:pw@db:5432/vizra?sslmode=require",
		"VIZRA_CACHE_URL":      "redis://cache:6379/0",
		"VIZRA_SESSION_SECRET": "Kv8Qn2Rt6Wp1Zx5Ym9Bc3Fd7Gh0Jl4Nq", // 32 bytes, not a published default
		"VIZRA_MFA_KEY_KEK":    "Pz3Xw7Ru1Ty5Vb9Nm2Ck6Hj0Ls4Df8Ga", // 32 bytes
	}
}

func lookupOf(m map[string]string) Lookup {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestValidProductionEnvironmentLoads(t *testing.T) {
	cfg, err := LoadFrom(lookupOf(validProduction()))
	if err != nil {
		t.Fatalf("expected the baseline production environment to load, got: %v", err)
	}
	if cfg.Mode != ModeProduction {
		t.Fatalf("Mode = %q, want production", cfg.Mode)
	}
	if cfg.SearchMode != SearchOff {
		t.Fatalf("SearchMode = %q, want off (the M0 default, ADR-002 Q-001)", cfg.SearchMode)
	}
	if cfg.QueueAgeThreshold.Minutes() != 15 {
		t.Fatalf("QueueAgeThreshold = %v, want 15m (Q-028)", cfg.QueueAgeThreshold)
	}
}

// requireProblem asserts that mutating one key of the valid production
// environment is refused, and refused *for that key*.
func requireProblem(t *testing.T, key, value, wantKey string) {
	t.Helper()
	env := validProduction()
	if value == "" {
		delete(env, key)
	} else {
		env[key] = value
	}
	_, err := LoadFrom(lookupOf(env))
	if err == nil {
		t.Fatalf("production accepted %s=%q; it must refuse it", key, redactForTest(key, value))
	}
	ve, ok := AsValidationError(err)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}
	if !ve.Has(wantKey) {
		t.Fatalf("refusal did not name %s. Problems: %v", wantKey, ve.Problems)
	}
	// A refusal must never echo the secret it refused.
	if value != "" && isSecretKey(key) && strings.Contains(err.Error(), value) {
		t.Fatalf("the refusal message leaked the value of %s", key)
	}
}

func isSecretKey(name string) bool {
	for _, k := range AllKeys() {
		if k.Name == name {
			return k.Secret
		}
	}
	return false
}

func redactForTest(key, value string) string {
	if isSecretKey(key) {
		return "<redacted>"
	}
	return value
}

// D4: production boot refuses dev secrets.
func TestProductionRefusesDevSecrets(t *testing.T) {
	for _, v := range []string{
		"dev",
		"devsecret",
		"changeme",
		"change-me-change-me-change-me-ch", // 32 bytes, still a published default
		"insecure-development-session-secret-0123456789", // long, still a development value
		"vizra-dev-secret-vizra-dev-secret",
		"test-test-test-test-test-test-te",
	} {
		t.Run(v, func(t *testing.T) {
			requireProblem(t, "VIZRA_SESSION_SECRET", v, "VIZRA_SESSION_SECRET")
		})
	}
}

func TestProductionRefusesShortSecrets(t *testing.T) {
	requireProblem(t, "VIZRA_SESSION_SECRET", "Kv8Qn2Rt6Wp1Zx5Ym9Bc3Fd7Gh0Jl4N", "VIZRA_SESSION_SECRET") // 31 bytes
	requireProblem(t, "VIZRA_MFA_KEY_KEK", "Pz3Xw7Ru1Ty5Vb9Nm2Ck6Hj0Ls4Df8G", "VIZRA_MFA_KEY_KEK")       // 31 bytes
}

// ADR-003: an unset MFA KEK is a boot refusal, never a warning.
func TestProductionRefusesMissingSecrets(t *testing.T) {
	requireProblem(t, "VIZRA_MFA_KEY_KEK", "", "VIZRA_MFA_KEY_KEK")
	requireProblem(t, "VIZRA_SESSION_SECRET", "", "VIZRA_SESSION_SECRET")
	requireProblem(t, "DATABASE_URL", "", "DATABASE_URL")
}

func TestProductionRefusesWildcardCORS(t *testing.T) {
	requireProblem(t, "VIZRA_CORS_ALLOWED_ORIGINS", "*", "VIZRA_CORS_ALLOWED_ORIGINS")
	requireProblem(t, "VIZRA_CORS_ALLOWED_ORIGINS", "https://a.example,*", "VIZRA_CORS_ALLOWED_ORIGINS")
}

func TestProductionRefusesPlainHTTPOriginUnlessExplicitlyAllowed(t *testing.T) {
	requireProblem(t, "VIZRA_PUBLIC_ORIGIN", "http://photos.example.org", "VIZRA_PUBLIC_ORIGIN")

	env := validProduction()
	env["VIZRA_PUBLIC_ORIGIN"] = "http://photos.example.org"
	env["VIZRA_ALLOW_INSECURE_PUBLIC_ORIGIN"] = "true"
	if _, err := LoadFrom(lookupOf(env)); err != nil {
		t.Fatalf("an explicitly allowed plain-http origin must load: %v", err)
	}
}

func TestProductionRefusesPlainHTTPCORSOrigin(t *testing.T) {
	requireProblem(t, "VIZRA_CORS_ALLOWED_ORIGINS", "http://a.example", "VIZRA_CORS_ALLOWED_ORIGINS")
}

// Enumerates the escape-hatch registry itself, so a hatch added to keys.go
// without a refusal turns this red rather than shipping unguarded.
func TestEveryEscapeHatchIsRefusedInProduction(t *testing.T) {
	if len(EscapeHatches) == 0 {
		t.Fatal("the escape-hatch registry is empty; the production refusal has nothing to enforce")
	}
	for _, h := range EscapeHatches {
		t.Run(h.Name, func(t *testing.T) {
			requireProblem(t, h.Name, "true", h.Name)
			// A falsey value is not a refusal: an operator may leave the key
			// present and set to 0 in a shared template.
			env := validProduction()
			env[h.Name] = "false"
			if _, err := LoadFrom(lookupOf(env)); err != nil {
				t.Fatalf("%s=false must not refuse boot: %v", h.Name, err)
			}
		})
	}
}

func TestEscapeHatchesAreAllowedInDevelopment(t *testing.T) {
	env := map[string]string{
		"DATABASE_URL": "postgres://localhost:5432/vizra",
	}
	for _, h := range EscapeHatches {
		env[h.Name] = "true"
	}
	if _, err := LoadFrom(lookupOf(env)); err != nil {
		t.Fatalf("development must tolerate escape hatches: %v", err)
	}
}

// validate() collects every problem rather than returning the first.
func TestValidateCollectsEveryProblem(t *testing.T) {
	env := validProduction()
	env["VIZRA_SESSION_SECRET"] = "dev"
	env["VIZRA_MFA_KEY_KEK"] = ""
	env["VIZRA_CORS_ALLOWED_ORIGINS"] = "*"
	env["VIZRA_PUBLIC_ORIGIN"] = "http://photos.example.org"
	env["VIZRA_DEV_DISABLE_AUTH"] = "1"

	_, err := LoadFrom(lookupOf(env))
	ve, ok := AsValidationError(err)
	if !ok {
		t.Fatalf("expected *ValidationError, got %v", err)
	}
	for _, want := range []string{
		"VIZRA_SESSION_SECRET", "VIZRA_MFA_KEY_KEK",
		"VIZRA_CORS_ALLOWED_ORIGINS", "VIZRA_PUBLIC_ORIGIN", "VIZRA_DEV_DISABLE_AUTH",
	} {
		if !ve.Has(want) {
			t.Errorf("collected problems omit %s; got %v", want, ve.Problems)
		}
	}
	if len(ve.Problems) < 5 {
		t.Errorf("expected at least 5 collected problems, got %d: %v", len(ve.Problems), ve.Problems)
	}
}

func TestSearchModeRequiresURLAndKey(t *testing.T) {
	env := validProduction()
	env["VIZRA_SEARCH_MODE"] = "managed"
	_, err := LoadFrom(lookupOf(env))
	ve, ok := AsValidationError(err)
	if !ok {
		t.Fatalf("expected refusal for managed search with no URL or key, got %v", err)
	}
	if !ve.Has("VIZRA_SEARCH_URL") || !ve.Has("VIZRA_SEARCH_HMAC_KEY") {
		t.Fatalf("expected both VIZRA_SEARCH_URL and VIZRA_SEARCH_HMAC_KEY problems, got %v", ve.Problems)
	}

	env["VIZRA_SEARCH_URL"] = "http://search:8081"
	env["VIZRA_SEARCH_HMAC_KEY"] = "Ar4Lo8Cq2Ei6Uk0Wn3Sv7Yb1Md5Pt9Xz"
	if _, err := LoadFrom(lookupOf(env)); err != nil {
		t.Fatalf("managed search with URL and key must load: %v", err)
	}
}

func TestJobTimeoutMustExceedLease(t *testing.T) {
	env := validProduction()
	env["VIZRA_JOB_LEASE"] = "60s"
	env["VIZRA_JOB_TIMEOUT"] = "30s"
	_, err := LoadFrom(lookupOf(env))
	ve, ok := AsValidationError(err)
	if !ok || !ve.Has("VIZRA_JOB_TIMEOUT") {
		t.Fatalf("a job timeout below the lease must be refused; got %v", err)
	}
}

// CheckEnv is the seam setup and doctor use; it must be exactly LoadFrom.
func TestCheckEnvIsTheSameValidatorAsBoot(t *testing.T) {
	bad := validProduction()
	bad["VIZRA_SESSION_SECRET"] = "changeme"
	if err := CheckEnv(bad); err == nil {
		t.Fatal("CheckEnv accepted an environment boot would refuse")
	}
	if err := CheckEnv(validProduction()); err != nil {
		t.Fatalf("CheckEnv refused an environment boot accepts: %v", err)
	}
}

func TestRegistryHasNoDuplicateKeys(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range AllKeys() {
		if seen[k.Name] {
			t.Errorf("duplicate configuration key %s", k.Name)
		}
		seen[k.Name] = true
	}
}
