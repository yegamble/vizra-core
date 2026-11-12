package setup

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yegamble/vizra-core/internal/config"
)

func base() map[string]string {
	return map[string]string{
		"domain": "photos.example.org", "tls": "acme", "instance_name": "main",
		"release": "v0.1.0", "postgres": "managed", "redis": "managed",
		"analytics": "off", "ipfs": "off", "search": "off",
	}
}

func resolve(t *testing.T, existing, ans map[string]string) (map[string]string, error) {
	t.Helper()
	return Resolve(existing, ans, nil, nil)
}

// Tests keyed by question id: one valid and one invalid answer each.
func TestQuestionsValidAndInvalid(t *testing.T) {
	cases := map[string][2]string{
		"domain":              {"photos.example.org", "http://x/"},
		"tls":                 {"internal", "letsencrypt"},
		"instance_name":       {"main-site", "Main Site!"},
		"release":             {"v1.2.3", "latest"},
		"postgres":            {"external", "yes"},
		"postgres_dsn":        {"postgres://u:p@db:5432/v?sslmode=require", ""},
		"redis":               {"external", "maybe"},
		"redis_url":           {"rediss://cache:6380/0", "http://cache"},
		"analytics":           {"managed", "on"},
		"ipfs":                {"external", "public"},
		"search":              {"managed", "elastic"},
		"session_secret":      {strings.Repeat("a", 32), "short"},
		"mfa_kek":             {strings.Repeat("b", 32), ""},
		"search_hmac":         {strings.Repeat("c", 32), "x"},
		"postgres_password":   {strings.Repeat("d", 24), "p"},
		"clickhouse_password": {strings.Repeat("e", 24), ""},
	}
	seen := map[string]bool{}
	for _, q := range Questions() {
		seen[q.ID] = true
		c, ok := cases[q.ID]
		if !ok {
			t.Errorf("question %q has no valid/invalid case", q.ID)
			continue
		}
		if err := q.Validate(c[0]); err != nil {
			t.Errorf("%s: valid %q refused: %v", q.ID, c[0], err)
		}
		if err := q.Validate(c[1]); err == nil {
			t.Errorf("%s: invalid %q accepted", q.ID, c[1])
		}
	}
	for id := range cases {
		if !seen[id] {
			t.Errorf("case for unknown question %q", id)
		}
	}
}

// S1: every key a question writes lands on a template key: the meta
// production template (checked-in snapshot) or core's config registry, or is
// listed in PendingTemplateKeys (keys the meta template must still add).
func TestS1EveryAnswerLandsOnATemplateKey(t *testing.T) {
	known := map[string]bool{}
	b, err := os.ReadFile("testdata/meta-production-env-keys.txt")
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		known[strings.TrimSpace(sc.Text())] = true
	}
	for _, k := range config.Registry {
		known[k.Name] = true
	}
	for _, k := range ComposeKeys {
		known[k] = true
	}
	pending := map[string]bool{}
	for _, k := range PendingTemplateKeys {
		if known[k] {
			t.Errorf("%s is pending but already in a template: drop it from PendingTemplateKeys", k)
		}
		pending[k] = true
	}
	for _, q := range Questions() {
		if len(q.Keys) == 0 {
			t.Errorf("%s writes no key", q.ID)
		}
		for _, k := range q.Keys {
			if !known[k] && !pending[k] {
				t.Errorf("%s writes %s, which no template has", q.ID, k)
			}
		}
	}
}

// N1: external datastore with no DSN is refused.
func TestN1ExternalWithoutDSNRefused(t *testing.T) {
	for _, id := range []string{"postgres", "redis"} {
		a := base()
		a[id] = "external"
		if _, err := resolve(t, nil, a); err == nil || !strings.Contains(err.Error(), id) {
			t.Errorf("%s=external without DSN: err=%v, want refusal", id, err)
		}
	}
	dir := t.TempDir()
	a := base()
	a["postgres"] = "external"
	path := filepath.Join(dir, "env", "production.env")
	if err := Run(Options{Path: path, Answers: a}); err == nil {
		t.Fatal("Run accepted external postgres with no DSN")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("refused run still wrote a file")
	}
}

// Secrets are minted, never blank, and differ per run.
func TestSecretsMintedNeverBlank(t *testing.T) {
	a := base()
	a["postgres"] = "managed"
	env1, err := resolve(t, nil, a)
	if err != nil {
		t.Fatal(err)
	}
	env2, _ := resolve(t, nil, a)
	for _, k := range []string{"VIZRA_SESSION_SECRET", "VIZRA_MFA_KEY_KEK", "SEARCH_HMAC_KEY", "POSTGRES_PASSWORD"} {
		if len(env1[k]) < 32 {
			t.Errorf("%s minted as %q", k, env1[k])
		}
		if env1[k] == env2[k] {
			t.Errorf("%s is not random", k)
		}
	}
	if env1["VIZRA_PUBLIC_ORIGIN"] != "https://photos.example.org" {
		t.Errorf("origin = %q", env1["VIZRA_PUBLIC_ORIGIN"])
	}
}

// P1: indirections; 0600; no secret on any output.
func TestP1IndirectionsModeAndNoLeak(t *testing.T) {
	dir := t.TempDir()
	sf := filepath.Join(dir, "kek")
	os.WriteFile(sf, []byte(strings.Repeat("k", 40)+"\n"), 0o600)
	t.Setenv("VIZRA_SETUP_SESSION_SECRET", strings.Repeat("s", 40))
	a := base()
	a["mfa_kek"] = "@" + sf
	a["search_hmac"] = "-"
	var out bytes.Buffer
	path := filepath.Join(dir, "env", "production.env")
	err := Run(Options{Path: path, Answers: a, Stdin: strings.NewReader(strings.Repeat("h", 40) + "\n"), Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	env, _ := ReadEnvFile(path)
	if env["VIZRA_MFA_KEY_KEK"] != strings.Repeat("k", 40) || env["SEARCH_HMAC_KEY"] != strings.Repeat("h", 40) || env["VIZRA_SESSION_SECRET"] != strings.Repeat("s", 40) {
		t.Fatalf("indirections not applied: %v", env)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	for _, q := range Questions() {
		if !q.Secret {
			continue
		}
		for _, k := range q.Keys {
			if v := env[k]; v != "" && strings.Contains(out.String(), v) {
				t.Errorf("secret %s printed", k)
			}
		}
	}
	// broader permissions on an existing file are refused
	os.Chmod(path, 0o644)
	if err := Run(Options{Path: path, Answers: a, Yes: true, Out: &out}); err == nil {
		t.Fatal("0644 existing file accepted")
	}
}

// P1: the interactive reader never echoes a secret.
func TestP1InteractiveSecretNotEchoed(t *testing.T) {
	var prompts bytes.Buffer
	secret := strings.Repeat("z", 40)
	ask := &LineAsker{In: bufio.NewReader(strings.NewReader("")), Out: &prompts,
		ReadSecret: func() (string, error) { return secret, nil }}
	got, err := ask.Ask(Lookup("mfa_kek"), "")
	if err != nil || got != secret {
		t.Fatalf("got %q %v", got, err)
	}
	if strings.Contains(prompts.String(), secret) {
		t.Fatal("secret echoed")
	}
	// blank secret → minted, never blank
	ask.ReadSecret = func() (string, error) { return "", nil }
	env, err := Resolve(nil, base(), ask, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(env["VIZRA_MFA_KEY_KEK"]) < 32 {
		t.Fatal("blank interactive secret not minted")
	}
}

// R1: re-run keeps every value, byte-identical; rewrite refused without --yes.
func TestR1RerunPreservesAndRefusesRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env", "production.env")
	a := base()
	if err := Run(Options{Path: path, Answers: a}); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	if err := Run(Options{Path: path, Answers: a}); err != nil {
		t.Fatalf("identical re-run: %v", err)
	}
	second, _ := os.ReadFile(path)
	if !bytes.Equal(first, second) {
		t.Fatal("re-run changed the file")
	}
	// an existing hand-added key survives
	os.WriteFile(path, append(second, []byte("CUSTOM_KEY=keep\n")...), 0o600)
	if err := Run(Options{Path: path, Answers: a}); err != nil {
		t.Fatal(err)
	}
	b2 := base()
	b2["search"] = "external" // existing value wins over a new answer
	if err := Run(Options{Path: path, Answers: b2}); err != nil {
		t.Fatal(err)
	}
	env, _ := ReadEnvFile(path)
	if env["CUSTOM_KEY"] != "keep" || env["VIZRA_SEARCH_MODE"] != "off" {
		t.Fatalf("values not preserved: %v", env)
	}
	// a change (missing key to fill) without --yes is refused
	delete(env, "VIZRA_SITE_HANDLE")
	os.WriteFile(path, []byte(Render(env)), 0o600)
	before, _ := os.ReadFile(path)
	if err := Run(Options{Path: path, Answers: a}); err == nil {
		t.Fatal("rewrite without --yes accepted")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("refused run changed the file")
	}
	if err := Run(Options{Path: path, Answers: a, Yes: true}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckNamesBadKeys(t *testing.T) {
	env, _ := resolve(t, nil, base())
	if errs := Check(env); len(errs) != 0 {
		t.Fatalf("clean env: %v", errs)
	}
	env["VIZRA_TLS_MODE"] = "bogus"
	delete(env, "VIZRA_SESSION_SECRET")
	errs := Check(env)
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, "VIZRA_TLS_MODE") || !strings.Contains(joined, "VIZRA_SESSION_SECRET") {
		t.Fatalf("check output: %v", errs)
	}
}

func TestRotate(t *testing.T) {
	env, _ := resolve(t, nil, base())
	old := env["VIZRA_MFA_KEY_KEK"]
	if _, err := Rotate(env, "mfa_kek", true, false); err == nil {
		t.Fatal("KEK rotation without --yes-i-know accepted")
	}
	if _, err := Rotate(env, "session_secret", false, false); err == nil {
		t.Fatal("rotation without --yes accepted")
	}
	if _, err := Rotate(env, "domain", true, false); err == nil {
		t.Fatal("rotating a non-secret accepted")
	}
	_ = old
	// Data-loss guard: no re-wrap of TOTP enrolments exists, so the KEK, and
	// the database passwords (the env value does not change the server's
	// password), are refused even with --yes --yes-i-know.
	for _, id := range []string{"mfa_kek", "postgres_password", "clickhouse_password"} {
		if _, err := Rotate(env, id, true, true); err == nil {
			t.Fatalf("rotate %s with --yes --yes-i-know accepted", id)
		}
	}
	if _, err := Rotate(env, "mfa_kek", true, true); err == nil || !strings.Contains(err.Error(), "vizra mfa rewrap-kek") {
		t.Fatalf("KEK refusal must name the future command: %v", err)
	}
	for _, id := range []string{"session_secret", "search_hmac"} {
		key := Lookup(id).Keys[0]
		n, err := Rotate(env, id, true, false)
		if err != nil || n[key] == env[key] || len(n[key]) < 32 {
			t.Fatalf("rotate %s: %v", id, err)
		}
	}
}
