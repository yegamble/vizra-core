package config

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

// validProduction is a minimal environment that must boot in production mode.
// Every negative test below mutates exactly one key of this map, so a test that
// goes red names the single thing that broke.
// placeholderSecret builds a value of exactly n bytes that is long enough for
// production and contains none of the known development substrings.
//
// It is BUILT rather than written as a literal, deliberately. A 32-character
// random-looking string in a source file is indistinguishable from a leaked
// credential to a scanner and to a reviewer, and the test needs neither
// randomness nor secrecy — only length and the absence of a banned substring.
func placeholderSecret(n int) string {
	return strings.Repeat("Vz9Kp4Mw2Ng7", (n/12)+1)[:n]
}

func validProduction() map[string]string {
	return map[string]string{
		"VIZRA_MODE":           "production",
		"VIZRA_PUBLIC_ORIGIN":  "https://photos.example.org",
		"DATABASE_URL":         "postgres://vizra@db:5432/vizra?sslmode=require",
		"VIZRA_CACHE_URL":      "redis://cache:6379/0",
		"VIZRA_SESSION_SECRET": placeholderSecret(32),
		"VIZRA_MFA_KEY_KEK":    placeholderSecret(32),
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
	// One byte below the floor: the boundary is where an off-by-one lives.
	requireProblem(t, "VIZRA_SESSION_SECRET", placeholderSecret(31), "VIZRA_SESSION_SECRET")
	requireProblem(t, "VIZRA_MFA_KEY_KEK", placeholderSecret(31), "VIZRA_MFA_KEY_KEK")
	// And exactly at the floor it is accepted, so the test is not passing for
	// the wrong reason.
	env := validProduction()
	env["VIZRA_SESSION_SECRET"] = placeholderSecret(32)
	if _, err := LoadFrom(lookupOf(env)); err != nil {
		t.Fatalf("a 32-byte secret was refused: %v", err)
	}
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
			if h.RefuseIfPresent {
				// A value-bearing hatch is refused on PRESENCE, so there is no
				// tolerated value. TestValueBearingEscapeHatchIsRefusedWhenPresent
				// covers it.
				return
			}
			// For a BOOLEAN hatch a falsey value is not a refusal: an operator
			// may leave the key present and set to 0 in a shared template.
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
	env["VIZRA_SEARCH_HMAC_KEY"] = placeholderSecret(32)
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

// ---------------------------------------------------------------------------
// Security Finding 2 — the key this repository PUBLISHES is not a production secret
// ---------------------------------------------------------------------------

// api/search-hmac-testvectors.json is where an operator wiring up search WILL
// look: it is the file that documents the very key it is for, and the field is
// named `key_utf8` next to a working example. The substring denylist cannot see
// it — it is 32 bytes and matches no English word — so production used to
// accept it, and a core<->search channel signed with a key anyone can read from
// the repository is a full compromise of that boundary.
//
// The value is READ FROM THE VECTORS FILE at test time rather than duplicated
// here, so editing the vectors file without updating the refusal turns this
// red instead of silently un-covering it.
func publishedHMACKey(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../api/search-hmac-testvectors.json")
	if err != nil {
		t.Fatalf("the vectors file is missing: %v", err)
	}
	var vf struct {
		KeyUTF8 string `json:"key_utf8"`
	}
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatalf("parsing the vectors file: %v", err)
	}
	if vf.KeyUTF8 == "" {
		t.Fatal("the vectors file has no key_utf8")
	}
	return vf.KeyUTF8
}

func TestProductionRefusesPublishedTestKeys(t *testing.T) {
	published := publishedHMACKey(t)

	t.Run("as the search HMAC key", func(t *testing.T) {
		env := validProduction()
		env["VIZRA_SEARCH_MODE"] = "managed"
		env["VIZRA_SEARCH_URL"] = "http://search:8081"
		env["VIZRA_SEARCH_HMAC_KEY"] = published

		_, err := LoadFrom(lookupOf(env))
		ve, ok := AsValidationError(err)
		if !ok || !ve.Has("VIZRA_SEARCH_HMAC_KEY") {
			t.Fatalf("production ACCEPTED the key published in api/search-hmac-testvectors.json. "+
				"An operator who copies it out of that file gets a channel signed with a key anyone "+
				"can read from the repository. err = %v", err)
		}
		if strings.Contains(err.Error(), published) {
			t.Fatalf("the refusal echoed the key: %v", err)
		}
	})

	// The same value must be refused wherever a secret is expected, not only in
	// the field it happens to belong to.
	for _, key := range []string{"VIZRA_SESSION_SECRET", "VIZRA_MFA_KEY_KEK"} {
		t.Run("as "+key, func(t *testing.T) {
			requireProblem(t, key, published, key)
		})
	}

	// Every in-repo test key, not only the published one.
	for _, key := range []string{"VIZRA_SESSION_SECRET", "VIZRA_MFA_KEY_KEK"} {
		t.Run("the search package test key as "+key, func(t *testing.T) {
			requireProblem(t, key, strings.Repeat("Aa1Bb2Cc3Dd4", 3)[:32], key)
		})
	}

	// Development must still accept them, or the vectors stop being usable.
	t.Run("development still accepts them", func(t *testing.T) {
		env := map[string]string{
			"DATABASE_URL":          "postgres://localhost:5432/vizra",
			"VIZRA_SEARCH_MODE":     "managed",
			"VIZRA_SEARCH_URL":      "http://search:8081",
			"VIZRA_SEARCH_HMAC_KEY": published,
			"VIZRA_SESSION_SECRET":  published,
		}
		if err := CheckEnv(env); err != nil {
			t.Fatalf("development refused the test vectors' key; the vectors must stay usable: %v", err)
		}
	})
}

// The baseline this suite uses must not itself be on the denylist, or every
// other test in the file would be passing for the wrong reason.
func TestTheTestBaselineIsNotAPublishedSecret(t *testing.T) {
	for _, n := range []int{31, 32, 40} {
		v := placeholderSecret(n)
		for _, published := range KnownPublishedSecrets() {
			if v == published {
				t.Fatalf("placeholderSecret(%d) collides with a published secret; "+
					"validProduction() would be refused and every negative test would pass vacuously", n)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Security Finding 3 — a hatch that carries a VALUE is never truthy
// ---------------------------------------------------------------------------

// VIZRA_DEV_AUTOLOGIN_USER's documented value is a USERNAME. The production
// refusal tested truthiness, so no realistic setting of it was ever refused —
// and the old test actively cemented the hole by asserting that a non-truthy
// value MUST boot.
//
// The consumer arrives in M1 with sessions. VIZRA_DEV_AUTOLOGIN_USER=owner in a
// production env file would then sign every request in as the site owner, and
// authz would correctly allow everything.
func TestValueBearingEscapeHatchIsRefusedWhenPresent(t *testing.T) {
	for _, v := range []string{"alice", "owner", "0", "false", "off", "no", "1", "true", " ", "-"} {
		t.Run(strconv.Quote(v), func(t *testing.T) {
			requireProblem(t, "VIZRA_DEV_AUTOLOGIN_USER", v, "VIZRA_DEV_AUTOLOGIN_USER")
		})
	}
	// Present but empty is not "set": an operator may leave the key in a
	// template with no value.
	env := validProduction()
	env["VIZRA_DEV_AUTOLOGIN_USER"] = ""
	if _, err := LoadFrom(lookupOf(env)); err != nil {
		t.Fatalf("an empty value must not refuse boot: %v", err)
	}
}

// Drives every hatch from a per-hatch value table rather than the literal
// "true", so a hatch whose realistic value is not a boolean cannot slip through
// again.
func TestEveryEscapeHatchIsRefusedForItsRealisticValues(t *testing.T) {
	// The values an operator would actually write for each hatch.
	realistic := map[string][]string{
		"VIZRA_DEV_AUTOLOGIN_USER": {"alice", "owner", "0", "false"},
	}
	const booleanDefault = "true"

	for _, h := range EscapeHatches {
		values, ok := realistic[h.Name]
		if !ok {
			values = []string{booleanDefault, "1", "yes", "on"}
		}
		for _, v := range values {
			t.Run(h.Name+"="+strconv.Quote(v), func(t *testing.T) {
				requireProblem(t, h.Name, v, h.Name)
			})
		}
		if h.RefuseIfPresent {
			continue
		}
		// A BOOLEAN hatch keeps the deliberate affordance: a shared template may
		// list it set to a falsey value.
		t.Run(h.Name+"=false tolerated", func(t *testing.T) {
			env := validProduction()
			env[h.Name] = "false"
			if _, err := LoadFrom(lookupOf(env)); err != nil {
				t.Fatalf("%s=false must not refuse boot: %v", h.Name, err)
			}
		})
	}
}
