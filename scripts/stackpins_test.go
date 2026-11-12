package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// VZ-FOUND-001 / VZ-FOUND-004: check-stack-pins.py must pass on the real tree
// and fail on each controlled mutation of a copy of it.

func copyPinInputs(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	dst := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dst, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := []string{"go.mod", "go.sum"}
	wfs, _ := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	for _, w := range wfs {
		rel, _ := filepath.Rel(root, w)
		files = append(files, rel)
	}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func mutate(t *testing.T, path, old, repl string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), old) {
		t.Fatalf("%s: %q not found; mutation would be vacuous", path, old)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), old, repl, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStackPinsRealTreePasses(t *testing.T) {
	out, code := runPython(t, "check-stack-pins.py")
	if code != 0 {
		t.Fatalf("real tree must satisfy ADR-001 pins, exit %d:\n%s", code, out)
	}
}

func TestStackPinsMutationsFail(t *testing.T) {
	cases := []struct {
		name  string
		apply func(t *testing.T, dir string)
		want  string
	}{
		{"baseline copy passes", func(t *testing.T, dir string) {}, ""},
		{"go.sum deleted", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "go.sum")); err != nil {
				t.Fatal(err)
			}
		}, "go.sum is missing"},
		{"go.sum emptied", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "go.sum"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}, "go.sum is missing or empty"},
		{"toolchain changed", func(t *testing.T, dir string) {
			mutate(t, filepath.Join(dir, "go.mod"), "toolchain go1.27.4", "toolchain go1.27.3")
		}, "`toolchain` line is go1.27.3"},
		{"toolchain removed", func(t *testing.T, dir string) {
			mutate(t, filepath.Join(dir, "go.mod"), "toolchain go1.27.4\n", "")
		}, "`toolchain` line is absent"},
		{"go line changed", func(t *testing.T, dir string) {
			mutate(t, filepath.Join(dir, "go.mod"), "\ngo 1.26.0\n", "\ngo 1.25.0\n")
		}, "`go` line is 1.25.0"},
		{"setup-go go-version 1.26", func(t *testing.T, dir string) {
			mutate(t, filepath.Join(dir, ".github/workflows/govulncheck.yml"), "go-version-file: go.mod", "go-version: '1.26'")
		}, "govulncheck.yml"},
		{"setup-go go-version-file other", func(t *testing.T, dir string) {
			mutate(t, filepath.Join(dir, ".github/workflows/build-test.yml"), "go-version-file: go.mod", "go-version-file: .go-version")
		}, "build-test.yml"},
		{"setup-go version source removed", func(t *testing.T, dir string) {
			mutate(t, filepath.Join(dir, ".github/workflows/fixtures.yml"), "go-version-file: go.mod", "cache: false")
		}, "setup-go reads nothing"},
		{"echo below ADR floor", func(t *testing.T, dir string) {
			mutate(t, filepath.Join(dir, "go.mod"), "github.com/labstack/echo/v5 v5.4.4", "github.com/labstack/echo/v5 v5.4.3")
		}, "echo/v5 is v5.4.3"},
		{"go-redis off ADR minor", func(t *testing.T, dir string) {
			mutate(t, filepath.Join(dir, "go.mod"), "github.com/redis/go-redis/v9 v9.22.0", "github.com/redis/go-redis/v9 v9.21.0")
		}, "go-redis/v9 is v9.21.0"},
		{"govips off ADR pin", func(t *testing.T, dir string) {
			mutate(t, filepath.Join(dir, "go.mod"), "github.com/redis/go-redis/v9 v9.22.0", "github.com/redis/go-redis/v9 v9.22.0\n\tgithub.com/davidbyttow/govips/v2 v2.17.0")
		}, "govips/v2 is v2.17.0"},
		{"sqlc off ADR pin", func(t *testing.T, dir string) {
			mutate(t, filepath.Join(dir, ".github/workflows/build-test.yml"), "sqlc/cmd/sqlc@v1.31.1", "sqlc/cmd/sqlc@v1.30.0")
		}, "sqlc v1.30.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := copyPinInputs(t)
			c.apply(t, dir)
			out, code := runPython(t, "check-stack-pins.py", "--root", dir)
			if c.want == "" {
				if code != 0 {
					t.Fatalf("baseline copy must pass, exit %d:\n%s", code, out)
				}
				return
			}
			if code != 1 || !strings.Contains(out, c.want) {
				t.Fatalf("want exit 1 containing %q, got exit %d:\n%s", c.want, code, out)
			}
		})
	}
}
