package config

import (
	"bufio"
	"os"
	"sort"
	"strings"
	"testing"
)

// VZ-FOUND-006: "every config key has one documented home (env template +
// compose consumer)" and its negative case, "a key added to config without
// template/compose consumer fails CI".
//
// This is the template half. The compose half arrives with VZ-ISSUE-002, which
// owns the compose topology; when it does, the same assertion runs against the
// rendered compose model. Saying that here is deliberate: the ledger entry is
// not satisfied yet, and a test that quietly covered half of it would let the
// entry be marked done.
func TestEveryKeyHasATemplateEntry(t *testing.T) {
	inTemplate := templateKeys(t)

	var missing []string
	for _, k := range AllKeys() {
		if !inTemplate[k.Name] {
			missing = append(missing, k.Name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("internal/config reads %d key(s) that .env.example does not document:\n  %s\n\n"+
			"Every key has one documented home. Add it to .env.example.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

func TestTemplateHasNoKeyNothingReads(t *testing.T) {
	known := map[string]bool{}
	for _, k := range AllKeys() {
		known[k.Name] = true
	}
	var extra []string
	for name := range templateKeys(t) {
		if !known[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf(".env.example documents %d key(s) nothing reads:\n  %s\n\n"+
			"A documented key that has no effect is worse than an undocumented one: "+
			"an operator sets it and believes something changed.",
			len(extra), strings.Join(extra, "\n  "))
	}
}

// The template must not ship a value production would accept. An operator who
// copies .env.example to .env and flips VIZRA_MODE=production must be refused,
// not silently running on a published secret.
func TestTemplateSecretsAreRefusedInProduction(t *testing.T) {
	env := templateValues(t)
	env["VIZRA_MODE"] = "production"

	err := CheckEnv(env)
	if err == nil {
		t.Fatal(".env.example with VIZRA_MODE=production was ACCEPTED. " +
			"The template ships values production must refuse.")
	}
	ve, ok := AsValidationError(err)
	if !ok {
		t.Fatalf("expected *ValidationError, got %v", err)
	}
	for _, k := range []string{"VIZRA_SESSION_SECRET", "VIZRA_MFA_KEY_KEK"} {
		if !ve.Has(k) {
			t.Errorf("the template's %s was not refused in production; problems: %v", k, ve.Problems)
		}
	}
}

// The template as shipped must boot in development, or the first thing a new
// contributor meets is a broken default.
func TestTemplateBootsInDevelopment(t *testing.T) {
	env := templateValues(t)
	env["DATABASE_URL"] = "postgres://vizra:vizra@127.0.0.1:5432/vizra?sslmode=disable"
	if err := CheckEnv(env); err != nil {
		t.Fatalf(".env.example does not boot in development mode: %v", err)
	}
}

// Escape hatches must be present in the template but COMMENTED OUT: an operator
// should be able to see what exists without having it enabled.
func TestEscapeHatchesAreCommentedOutInTheTemplate(t *testing.T) {
	raw, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	live := templateValues(t)
	for _, h := range EscapeHatches {
		if !strings.Contains(string(raw), h.Name) {
			t.Errorf(".env.example does not mention the escape hatch %s", h.Name)
		}
		if _, active := live[h.Name]; active {
			t.Errorf(".env.example sets %s as a live value; escape hatches must be commented out", h.Name)
		}
	}
}

// ---------------------------------------------------------------------------

// templateKeys returns every key named in .env.example, live or commented.
func templateKeys(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for name := range templateValues(t) {
		out[name] = true
	}
	for name := range commentedTemplateKeys(t) {
		out[name] = true
	}
	return out
}

func scanTemplate(t *testing.T, fn func(line string)) {
	t.Helper()
	f, err := os.Open("../../.env.example")
	if err != nil {
		t.Fatalf(".env.example is missing: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fn(sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
}

// templateValues returns the LIVE assignments.
func templateValues(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	scanTemplate(t, func(line string) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			return
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	})
	return out
}

// commentedTemplateKeys returns keys that appear as `# KEY=value`.
func commentedTemplateKeys(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	scanTemplate(t, func(line string) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#") {
			return
		}
		body := strings.TrimSpace(strings.TrimPrefix(line, "#"))
		k, _, ok := strings.Cut(body, "=")
		if !ok {
			return
		}
		k = strings.TrimSpace(k)
		if k != "" && k == strings.ToUpper(k) && !strings.Contains(k, " ") {
			out[k] = true
		}
	})
	return out
}
