package setup

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite edge golden files")

func edgeEnv(mode string) map[string]string {
	return map[string]string{"VIZRA_TLS_MODE": mode, "VIZRA_DOMAIN": "photos.example.org"}
}

func TestRenderEdgeGolden(t *testing.T) {
	for _, mode := range []string{"acme", "internal", "plain-http", "external"} {
		t.Run(mode, func(t *testing.T) {
			name, body, err := RenderEdge(edgeEnv(mode))
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"external": "nginx.example.conf"}[mode]
			if want == "" {
				want = "Caddyfile.local"
			}
			if name != want {
				t.Fatalf("file %q, want %q", name, want)
			}
			g := filepath.Join("testdata", "edge", mode+".golden")
			if *updateGolden {
				_ = os.WriteFile(g, []byte(body), 0o644)
			}
			b, err := os.ReadFile(g)
			if err != nil || string(b) != body {
				t.Fatalf("golden mismatch for %s (err %v):\n%s", mode, err, body)
			}
		})
	}
	if _, _, err := RenderEdge(edgeEnv("bogus")); err == nil {
		t.Fatal("unknown TLS mode accepted")
	}
	if _, _, err := RenderEdge(map[string]string{"VIZRA_TLS_MODE": "acme", "VIZRA_DOMAIN": "x.org\n:80 {"}); err == nil {
		t.Fatal("domain with newline accepted (Caddyfile injection)")
	}
}

func TestWriteEdgeOverwriteRules(t *testing.T) {
	dir := t.TempDir()
	p, err := WriteEdge(dir, edgeEnv("acme"), false)
	if err != nil || filepath.Base(p) != "Caddyfile.local" {
		t.Fatalf("first write: %v %s", err, p)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	// Untouched generated file: regenerated freely on a TLS change.
	if _, err := WriteEdge(dir, edgeEnv("internal"), false); err != nil {
		t.Fatalf("regenerate untouched: %v", err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "tls internal") {
		t.Fatal("not regenerated")
	}
	// Operator edit: refused without yes, overwritten with yes.
	if err := os.WriteFile(p, append(b, []byte("# my edit\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteEdge(dir, edgeEnv("acme"), false); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("operator-edited file overwritten without --yes: %v", err)
	}
	if b2, _ := os.ReadFile(p); !strings.Contains(string(b2), "# my edit") {
		t.Fatal("operator edit lost")
	}
	if _, err := WriteEdge(dir, edgeEnv("acme"), true); err != nil {
		t.Fatalf("--yes overwrite: %v", err)
	}
}
