package config

import (
	"encoding/json"
	"os"
	"slices"
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
	if !ve.Has("VIZRA_SEARCH_URL") || !ve.Has("SEARCH_HMAC_KEY") {
		t.Fatalf("expected both VIZRA_SEARCH_URL and SEARCH_HMAC_KEY problems, got %v", ve.Problems)
	}

	env["VIZRA_SEARCH_URL"] = "http://search:8081"
	env["SEARCH_HMAC_KEY"] = placeholderSecret(32)
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
		env["SEARCH_HMAC_KEY"] = published

		_, err := LoadFrom(lookupOf(env))
		ve, ok := AsValidationError(err)
		if !ok || !ve.Has("SEARCH_HMAC_KEY") {
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
			"DATABASE_URL":         "postgres://localhost:5432/vizra",
			"VIZRA_SEARCH_MODE":    "managed",
			"VIZRA_SEARCH_URL":     "http://search:8081",
			"SEARCH_HMAC_KEY":      published,
			"VIZRA_SESSION_SECRET": published,
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

// ---------------------------------------------------------------------------
// The naming ruling (chair, 2026-09-20): the core<->search shared secret is
// SEARCH_HMAC_KEY, the name api/search-internal.openapi.yaml already uses and
// vizra-search already reads.
// ---------------------------------------------------------------------------

// The contract's name is the name core reads. If this ever regresses, the two
// services read the same shared secret under two spellings again and the
// deployment templates have to paper over it.
func TestTheSearchSecretIsTheNameTheContractUses(t *testing.T) {
	const want = "SEARCH_HMAC_KEY"

	var found *Key
	for i := range Registry {
		if Registry[i].Name == want {
			found = &Registry[i]
		}
		if Registry[i].Name == "VIZRA_SEARCH_HMAC_KEY" {
			t.Fatalf("the registry still reads VIZRA_SEARCH_HMAC_KEY; the contract names this secret %s", want)
		}
	}
	if found == nil {
		t.Fatalf("the registry does not read %s", want)
	}
	if !found.Secret {
		t.Errorf("%s is not marked Secret; it would be eligible for logging and doctor output", want)
	}

	// And the loader must really read THAT name — a registry entry nothing
	// reads would satisfy the check above and nothing else.
	env := validProduction()
	env["VIZRA_SEARCH_MODE"] = "managed"
	env["VIZRA_SEARCH_URL"] = "http://search:8081"
	env[want] = placeholderSecret(40)
	cfg, err := LoadFrom(lookupOf(env))
	if err != nil {
		t.Fatalf("production with %s set was refused: %v", want, err)
	}
	if cfg.SearchHMACKey != placeholderSecret(40) {
		t.Fatalf("LoadFrom did not read %s into Config.SearchHMACKey", want)
	}
}

// The name the CONTRACT file uses and the name the loader reads must be the
// same string, checked against the contract's own bytes rather than against a
// constant in this package — otherwise both could drift together.
func TestTheContractAndTheLoaderNameTheSameVariable(t *testing.T) {
	raw, err := os.ReadFile("../../api/search-internal.openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "SEARCH_HMAC_KEY") {
		t.Fatal("api/search-internal.openapi.yaml does not name SEARCH_HMAC_KEY at all; " +
			"this test can no longer tell whether core agrees with the contract")
	}
	if strings.Contains(text, "VIZRA_SEARCH_HMAC_KEY") {
		t.Error("api/search-internal.openapi.yaml names VIZRA_SEARCH_HMAC_KEY; " +
			"the contract is the authority and core follows it, not the other way round")
	}

	var registryNames []string
	for _, k := range AllKeys() {
		registryNames = append(registryNames, k.Name)
	}
	if !slices.Contains(registryNames, "SEARCH_HMAC_KEY") {
		t.Errorf("the contract names SEARCH_HMAC_KEY but the config registry does not read it: %v", registryNames)
	}
}

// The core of the ruling: NO compatibility alias, and a leftover old name is a
// production BOOT REFUSAL naming the variable — not a silent ignore that leaves
// the operator believing a key is configured.
func TestProductionRefusesARetiredKeyName(t *testing.T) {
	const old = "VIZRA_SEARCH_HMAC_KEY"
	const cur = "SEARCH_HMAC_KEY"

	t.Run("refused by name, with the replacement in the message", func(t *testing.T) {
		env := validProduction()
		env[old] = placeholderSecret(40)

		_, err := LoadFrom(lookupOf(env))
		ve, ok := AsValidationError(err)
		if !ok {
			t.Fatalf("production with a leftover %s was ACCEPTED. The operator's file looks configured "+
				"and the process booted with no search key at all. err = %v", old, err)
		}
		if !ve.Has(old) {
			t.Fatalf("the refusal does not name %s; problems: %v", old, ve.Problems)
		}
		var msg string
		for _, p := range ve.Problems {
			if p.Key == old {
				msg = p.Message
			}
		}
		if !strings.Contains(msg, cur) {
			t.Errorf("the refusal for %s does not name its replacement %s: %q", old, cur, msg)
		}
		if strings.Contains(err.Error(), placeholderSecret(40)) {
			t.Errorf("the refusal echoed the secret: %v", err)
		}
	})

	// There is NO alias. Setting only the old name must not configure search;
	// if it did, the refusal above would be the only thing standing between an
	// operator and an alias nobody decided to ship.
	t.Run("the old name is not an alias", func(t *testing.T) {
		env := map[string]string{
			"VIZRA_MODE":        "development", // development, so the refusal above is not what fails
			"DATABASE_URL":      "postgres://localhost:5432/vizra",
			"VIZRA_SEARCH_MODE": "managed",
			"VIZRA_SEARCH_URL":  "http://search:8081",
			old:                 placeholderSecret(40),
		}
		_, err := LoadFrom(lookupOf(env))
		ve, ok := AsValidationError(err)
		if !ok {
			t.Fatalf("managed search configured with ONLY %s loaded successfully; that is a compatibility "+
				"alias, and the ruling says there is none. err = %v", old, err)
		}
		if !ve.Has(cur) {
			t.Fatalf("expected a %s problem (no key configured), got %v", cur, ve.Problems)
		}
	})

	// A completely empty `KEY=` is tolerated — the template ships the name as a
	// tombstone, and an empty value cannot make anyone believe a key is set.
	t.Run("a completely empty value is tolerated", func(t *testing.T) {
		env := validProduction()
		env[old] = ""
		if _, err := LoadFrom(lookupOf(env)); err != nil {
			t.Fatalf("production refused an EMPTY %s; the template carries it as a tombstone: %v", old, err)
		}
	})

	// Whitespace is not empty. `KEY= ` is ambiguous and the fail-secure reading
	// of an ambiguous env file is that the value is set.
	t.Run("whitespace is refused, not trimmed away", func(t *testing.T) {
		env := validProduction()
		env[old] = "  "
		_, err := LoadFrom(lookupOf(env))
		ve, ok := AsValidationError(err)
		if !ok || !ve.Has(old) {
			t.Fatalf("production accepted %s set to whitespace; got %v", old, err)
		}
	})

	// Every retired name, not only the one that prompted the rule, so adding a
	// retirement without a refusal cannot pass.
	for _, r := range RetiredKeys {
		t.Run("every retired name: "+r.Name, func(t *testing.T) {
			env := validProduction()
			env[r.Name] = placeholderSecret(40)
			_, err := LoadFrom(lookupOf(env))
			ve, ok := AsValidationError(err)
			if !ok || !ve.Has(r.Name) {
				t.Fatalf("production accepted the retired name %s; got %v", r.Name, err)
			}
			if r.ReplacedBy == "" {
				t.Errorf("%s declares no replacement, so the refusal cannot tell an operator what to do instead", r.Name)
			}
			for _, k := range AllKeys() {
				if k.Name == r.Name {
					t.Errorf("%s is both RETIRED and in AllKeys(); a name cannot be read and refused at once", r.Name)
				}
			}
		})
	}
}

// sentinel S-0010: an owner-claim TTL the database cannot store is refused at
// config load, not at the first mint. The interval is truncated to microseconds
// on the way to PostgreSQL, so 500ns became 0 and every mint then failed
// owner_claim_tokens_ttl (expires_at > minted_at) with a raw 23514; and a TTL of
// a few seconds is accepted but unusable, since the operator has to copy the
// token out of a terminal and submit a form inside it.
func TestTheOwnerClaimTTLHasALowerBound(t *testing.T) {
	for _, v := range []string{"500ns", "1us", "1s", "59s"} {
		t.Run("refuses "+v, func(t *testing.T) {
			requireProblem(t, "VIZRA_OWNER_CLAIM_TTL", v, "VIZRA_OWNER_CLAIM_TTL")
		})
	}
	for _, v := range []string{"1m", "15m", "1h", "24h"} {
		t.Run("accepts "+v, func(t *testing.T) {
			env := validProduction()
			env["VIZRA_OWNER_CLAIM_TTL"] = v
			cfg, err := LoadFrom(lookupOf(env))
			if err != nil {
				t.Fatalf("VIZRA_OWNER_CLAIM_TTL=%s was refused: %v", v, err)
			}
			if cfg.OwnerClaimTTL.String() == "" {
				t.Fatal("no TTL loaded")
			}
		})
	}
}
