package fixtures

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Generating the whole corpus is expensive — tens of seconds under the race
// detector, which a REQUIRED lane runs — so the suite generates it ONCE and
// shares it. The tests that must have an independent run (determinism, and the
// manifest comparison) say so and pay for their own.
var (
	sharedOnce  sync.Once
	sharedDir   string
	sharedFiles map[string]FileInfo
	sharedErr   error
)

func sharedCorpus(t *testing.T) (string, map[string]FileInfo) {
	t.Helper()
	sharedOnce.Do(func() {
		sharedDir, sharedErr = os.MkdirTemp("", "vizra-fixtures-shared-")
		if sharedErr != nil {
			return
		}
		sharedFiles, sharedErr = Generate(sharedDir)
	})
	if sharedErr != nil {
		t.Fatalf("generating the shared corpus: %v", sharedErr)
	}
	return sharedDir, sharedFiles
}

func TestMain(m *testing.M) {
	code := m.Run()
	if sharedDir != "" {
		_ = os.RemoveAll(sharedDir)
	}
	os.Exit(code)
}

// repoRoot is the repository root from this package's directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTheCorpusIsTheTwelveOfADR009 is the scope check. ADR-009 names twelve
// fixtures and says none is dropped; a corpus that quietly lost one would make
// every "verified against the corpus" claim about a smaller corpus.
func TestTheCorpusIsTheTwelveOfADR009(t *testing.T) {
	c := Corpus()
	if len(c) != 12 {
		t.Fatalf("the corpus has %d fixtures, ADR-009 names 12", len(c))
	}
	seenItem := map[int]string{}
	seenPath := map[string]bool{}
	for i, s := range c {
		if s.ADR009 != i+1 {
			t.Errorf("%s is ADR-009 item %d but sits at position %d; the manifest order is the ADR order", s.ID, s.ADR009, i+1)
		}
		if prev, ok := seenItem[s.ADR009]; ok {
			t.Errorf("ADR-009 item %d is claimed by both %s and %s", s.ADR009, prev, s.ID)
		}
		seenItem[s.ADR009] = s.ID
		if seenPath[s.Path] {
			t.Errorf("duplicate path %s", s.Path)
		}
		seenPath[s.Path] = true
		if s.Purpose == "" || len(s.Asserts) == 0 {
			t.Errorf("%s has no purpose or no assertions; a fixture whose only property is its hash is not a fixture", s.ID)
		}
		if s.License == "" {
			t.Errorf("%s has no licence position recorded; the ledger's negative case is that a fixture without provenance is rejected", s.ID)
		}
	}
}

// TestEveryFixtureIsWhatItClaimsToBe generates the corpus and runs every
// fixture's property assertions. This is the check that stops the corpus being
// twelve files with correct hashes and wrong contents.
func TestEveryFixtureIsWhatItClaimsToBe(t *testing.T) {
	dir, _ := sharedCorpus(t)
	if err := Verify(dir); err != nil {
		t.Fatal(err)
	}
	for _, s := range Corpus() {
		st, err := os.Stat(filepath.Join(dir, s.Path))
		if err != nil {
			t.Fatalf("%s: %v", s.ID, err)
		}
		if st.Size() == 0 {
			t.Errorf("%s is empty", s.ID)
		}
	}
}

// TestGenerationIsDeterministicWithinAProcess is the cheap half of the
// determinism claim: two runs of the generator in the same process must agree.
// The expensive half — two runs on two architectures — is the CI lane plus the
// local transcript in docs/evidence/fixtures/.
func TestGenerationIsDeterministicWithinAProcess(t *testing.T) {
	// The shared corpus is one run; this is a second, independent one.
	_, fa := sharedCorpus(t)
	fb, err := Generate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for path, ia := range fa {
		ib, ok := fb[path]
		if !ok {
			t.Errorf("%s was produced by one run and not the other", path)
			continue
		}
		if ia.SHA256 != ib.SHA256 {
			t.Errorf("%s: two runs of the generator produced different bytes (%s vs %s)", path, ia.SHA256, ib.SHA256)
		}
	}
}

// TestCommittedCodecSourceIsGeneratorOutput closes the provenance chain for the
// two fixtures that a real codec had to produce: the image those encoders were
// given must be this generator's own output, not a photograph.
func TestCommittedCodecSourceIsGeneratorOutput(t *testing.T) {
	want, err := buildCodecSourcePNG()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, codecSourcePNG) {
		t.Fatalf("internal/fixtures/codec/source.png is not what the generator produces today "+
			"(committed %d bytes, generated %d bytes). The codec fixtures' provenance chain is broken: "+
			"re-pin with `fixturegen repin-codec` or restore the source.", len(codecSourcePNG), len(want))
	}
}

// TestGeneratorSourceListCoversThePackage stops a corpus-affecting file being
// added to the generator without being added to the digest, which would make
// "the generator changed and the manifest did not" undetectable.
func TestGeneratorSourceListCoversThePackage(t *testing.T) {
	root := repoRoot(t)
	var found []string
	for _, dir := range []string{"internal/fixtures", "cmd/fixturegen"} {
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			n := e.Name()
			if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
				continue
			}
			found = append(found, dir+"/"+n)
		}
	}
	sort.Strings(found)
	want := append([]string(nil), GeneratorSourceFiles...)
	sort.Strings(want)
	if strings.Join(found, "\n") != strings.Join(want, "\n") {
		t.Fatalf("GeneratorSourceFiles does not cover the generator.\n on disk: %v\n recorded: %v\n"+
			"A file that influences the corpus and is not in the digest can change the bytes without turning the lane red.", found, want)
	}
}

// TestCommittedManifestMatchesTheGenerator is the lane itself, run as a unit
// test so that `make ci` cannot be green while the corpus has drifted.
func TestCommittedManifestMatchesTheGenerator(t *testing.T) {
	probs, err := VerifyAgainstManifest(repoRoot(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(probs) > 0 {
		var sb strings.Builder
		for _, p := range probs {
			fmt.Fprintf(&sb, "\n  %s: %s", p.Kind, p.Detail)
		}
		t.Fatalf("the committed manifest does not describe what the generator produces:%s", sb.String())
	}
}

// TestManifestDetectsEveryClassOfDrift is the red half of the manifest check.
// Each case is one of the demonstrations VZ-ISSUE-001 asks for, run as a test
// so that it keeps being true after this PR.
func TestManifestDetectsEveryClassOfDrift(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(t *testing.T, root string)
		wantKind string
	}{
		{
			name: "a changed fixture on disk",
			mutate: func(t *testing.T, root string) {
				// Generate the corpus, then change one byte of one file.
				if _, err := Generate(filepath.Join(root, filepath.FromSlash(OutputDir))); err != nil {
					t.Fatal(err)
				}
				p := filepath.Join(root, filepath.FromSlash(OutputDir), "png-alpha.png")
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				b[len(b)/2] ^= 0x01
				if err := os.WriteFile(p, b, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantKind: "fixture-on-disk",
		},
		{
			name: "the generator changed and the manifest did not",
			mutate: func(t *testing.T, root string) {
				p := filepath.Join(root, "internal", "fixtures", "raster.go")
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				b = append(b, []byte("\n// a change to the generator\n")...)
				if err := os.WriteFile(p, b, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantKind: "generator-source",
		},
		{
			name: "a pinned tool version moved without a re-pin",
			mutate: func(t *testing.T, root string) {
				replaceInFile(t, filepath.Join(root, filepath.FromSlash(ManifestPath)),
					`"go": "`+GoVersion()+`"`, `"go": "go1.99.0"`)
			},
			wantKind: "toolchain-pin",
		},
		{
			name: "the go.mod language version moved without a re-pin",
			mutate: func(t *testing.T, root string) {
				// The real thing, not a simulation: this directive sets the
				// GODEBUG compatibility defaults the standard library encoders
				// run under, and changing it changes the deflate output.
				replaceInFile(t, filepath.Join(root, "go.mod"), "go 1.26.0", "go 1.27.0")
			},
			wantKind: "toolchain-pin",
		},
		{
			name: "a committed codec input changed",
			mutate: func(t *testing.T, root string) {
				p := filepath.Join(root, "internal", "fixtures", "codec", "webm-short.webm")
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				b[len(b)-1] ^= 0xFF
				if err := os.WriteFile(p, b, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantKind: "codec-input",
		},
		{
			// Verifier finding V-2. Dropping a fixture and re-pinning used to
			// leave `fixtures-verify` printing `ok — 11 fixtures`, because the
			// manifest and the generator were only ever compared to each other
			// and both had shrunk. Here the manifest loses an entry; what makes
			// the case sharp is that it is the MANIFEST that moved, which is the
			// half `make fixtures-manifest` rewrites for you.
			name: "a fixture was dropped from the manifest and the set is no longer the twelve",
			mutate: func(t *testing.T, root string) {
				m, err := ReadManifest(root)
				if err != nil {
					t.Fatal(err)
				}
				dropped := m.Fixtures[len(m.Fixtures)-1]
				m.Fixtures = m.Fixtures[:len(m.Fixtures)-1]
				if err := WriteManifest(root, m); err != nil {
					t.Fatal(err)
				}
				t.Logf("dropped %s (ADR-009 item %d) from the manifest", dropped.Path, dropped.ADR009)
			},
			wantKind: "declared-set",
		},
		{
			// Verifier finding V-3. Step 4 walked the manifest and looked each
			// entry up on disk, so a file on disk with no entry was never
			// visited at all.
			name: "an undeclared extra file in the corpus directory",
			mutate: func(t *testing.T, root string) {
				out := filepath.Join(root, filepath.FromSlash(OutputDir))
				if _, err := Generate(out); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(out, "rogue-photograph.jpg"), []byte("not a fixture"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantKind: "undeclared-fixture",
		},
		{
			// The NIT the verifier raised against its own recommendation: a
			// directory walk that goes red for .DS_Store is a check people route
			// around. This case asserts the exemption is exactly dotfiles — it
			// is paired with the case above, which proves the walk still bites.
			name: "a dotfile in the corpus directory is NOT reported",
			mutate: func(t *testing.T, root string) {
				out := filepath.Join(root, filepath.FromSlash(OutputDir))
				if _, err := Generate(out); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(out, ".DS_Store"), []byte("editor turd"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantKind: "", // expect NO problems at all
		},
		{
			name: "a fixture hash in the manifest was edited",
			mutate: func(t *testing.T, root string) {
				m, err := ReadManifest(root)
				if err != nil {
					t.Fatal(err)
				}
				replaceInFile(t, filepath.Join(root, filepath.FromSlash(ManifestPath)),
					m.Fixtures[0].SHA256, strings.Repeat("0", 64))
			},
			wantKind: "fixture-bytes",
		},
	}

	// Green once, on an unmutated copy: if the copied tree did not verify clean
	// to begin with, every red below would be meaningless. Doing it once rather
	// than per case keeps the regeneration count — and `make ci` — bounded.
	if probs, err := VerifyAgainstManifest(copyRepoSlice(t), t.TempDir()); err != nil {
		t.Fatal(err)
	} else if len(probs) != 0 {
		t.Fatalf("the copied tree is already failing before any mutation: %v", probs)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := copyRepoSlice(t)
			tc.mutate(t, root)
			probs, err := VerifyAgainstManifest(root, t.TempDir())
			if err != nil {
				t.Fatalf("verify returned an error instead of a named problem: %v", err)
			}
			var kinds []string
			for _, p := range probs {
				kinds = append(kinds, p.Kind)
			}
			if tc.wantKind == "" {
				if len(probs) != 0 {
					t.Fatalf("%q must NOT be reported, but the verifier reported %v.\n%v", tc.name, kinds, probs)
				}
				return
			}
			if !contains(kinds, tc.wantKind) {
				t.Fatalf("after %q the verifier reported %v, want a %q problem", tc.name, kinds, tc.wantKind)
			}
		})
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func replaceInFile(t *testing.T, path, old, new string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(old)) {
		t.Fatalf("%s does not contain %q, so the mutation would be a no-op", path, old)
	}
	if err := os.WriteFile(path, bytes.Replace(b, []byte(old), []byte(new), 1), 0o644); err != nil {
		t.Fatal(err)
	}
}

// copyRepoSlice makes a throwaway copy of just the files the manifest check
// reads, so a drift case can mutate them without touching the real tree.
func copyRepoSlice(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	dst := t.TempDir()
	var want []string
	want = append(want, GeneratorSourceFiles...)
	want = append(want, ManifestPath, "go.mod")
	for _, o := range CodecInputs() {
		want = append(want, o.Path)
	}
	for _, rel := range want {
		src := filepath.Join(root, filepath.FromSlash(rel))
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

// TestManifestRecordsWhatTheLedgerRequires: VZ-FOUND-007's success case is
// "Fixture manifest lists sha256, dimensions, expected decode outcome and
// license per file", and its negative case is "A fixture without provenance is
// rejected by the manifest check".
func TestManifestRecordsWhatTheLedgerRequires(t *testing.T) {
	m, err := ReadManifest(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Fixtures) != 12 {
		t.Fatalf("the manifest lists %d fixtures, want 12", len(m.Fixtures))
	}
	for _, e := range m.Fixtures {
		if len(e.SHA256) != 64 {
			t.Errorf("%s: no sha256", e.ID)
		}
		if e.Width <= 0 || e.Height <= 0 {
			t.Errorf("%s: no dimensions", e.ID)
		}
		if e.Decode == "" {
			t.Errorf("%s: no expected decode outcome", e.ID)
		}
		if e.License == "" {
			t.Errorf("%s: no licence", e.ID)
		}
		if e.Bytes <= 0 {
			t.Errorf("%s: no size", e.ID)
		}
	}
	if len(m.CodecInputs) == 0 {
		t.Error("no codec input provenance recorded")
	}
	for _, o := range m.CodecInputs {
		if o.Tool == "" || o.ToolVersion == "" || len(o.Argv) == 0 || len(o.SHA256) != 64 {
			t.Errorf("%s: incomplete provenance (tool=%q version=%q argv=%v)", o.Path, o.Tool, o.ToolVersion, o.Argv)
		}
	}
}

// TestTheCorpusStaysSmall. The corpus is regenerated in CI and in every
// developer checkout; ADR-009 keeps it to twelve small files precisely so that
// it can be.
func TestTheCorpusStaysSmall(t *testing.T) {
	m, err := ReadManifest(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	var total int
	for _, e := range m.Fixtures {
		total += e.Bytes
	}
	if total > 16<<20 {
		t.Fatalf("the corpus is %d bytes; keep it under 16 MiB", total)
	}
	var committed int
	for _, o := range m.CodecInputs {
		committed += o.Bytes
	}
	if committed > 256<<10 {
		t.Fatalf("the committed codec inputs are %d bytes; keep them under 256 KiB", committed)
	}
	t.Logf("corpus %d bytes generated, %d bytes committed as codec inputs", total, committed)
}

// TestLoadCorpusIsNotInTheCorrectnessManifest keeps the two corpora apart.
func TestLoadCorpusIsNotInTheCorrectnessManifest(t *testing.T) {
	m, err := ReadManifest(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range m.Fixtures {
		if strings.HasPrefix(e.Path, "load-") {
			t.Errorf("%s looks like a load-corpus file and is in the correctness manifest", e.Path)
		}
	}
	// And the load-corpus generator must actually produce something usable.
	b, err := LoadCorpusImage(640, 480, 7, 85)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 1024 {
		t.Fatalf("a load-corpus image is %d bytes", len(b))
	}
	b2, err := LoadCorpusImage(640, 480, 7, 85)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(b) != sha256.Sum256(b2) {
		t.Error("the load-corpus generator is not deterministic for a fixed seed")
	}
	_ = hex.EncodeToString(nil)
}

// readFile is a small helper shared with mutation_test.go.
func readFile(dir, name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(dir, name))
}

// ---------------------------------------------------------------------------
// Verifier finding V-2, the anchor itself.
// ---------------------------------------------------------------------------

// ADR009Count must be twelve. The literal is written here a SECOND time on
// purpose: TestTheCorpusIsTheTwelveOfADR009 also carries its own literal, and
// V-2 exists because two values that move together anchor nothing. A test that
// read the constant it is checking would pass for any value.
func TestADR009CountIsTwelve(t *testing.T) {
	if ADR009Count != 12 {
		t.Fatalf("ADR009Count is %d; ADR-009 names 12 fixtures and says none is dropped", ADR009Count)
	}
}

// checkDeclaredSet is what makes `fixturegen verify`'s "ok" mean twelve. These
// are the exact shapes the verifier demonstrated printing `ok`.
func TestTheDeclaredSetCheckRefusesEveryShapeThatIsNotTheTwelve(t *testing.T) {
	// A manifest that agrees with the real generator, as a starting point.
	base, err := ReadManifest(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}

	clone := func() *Manifest {
		m := *base
		m.Fixtures = append([]Entry(nil), base.Fixtures...)
		return &m
	}

	if probs := checkDeclaredSet(clone()); len(probs) != 0 {
		t.Fatalf("the real manifest is already failing the declared-set check: %v", probs)
	}

	cases := []struct {
		name     string
		mutate   func(m *Manifest)
		wantText string
	}{
		{
			// The verifier's exact reproduction: `ok — 11 fixtures`.
			name:     "eleven fixtures",
			mutate:   func(m *Manifest) { m.Fixtures = m.Fixtures[:len(m.Fixtures)-1] },
			wantText: "11 fixture(s); ADR-009 names 12",
		},
		{
			// And its other one: `ok — 0 fixtures`.
			name:     "no fixtures at all",
			mutate:   func(m *Manifest) { m.Fixtures = nil },
			wantText: "0 fixture(s); ADR-009 names 12",
		},
		{
			name: "thirteen fixtures",
			mutate: func(m *Manifest) {
				extra := m.Fixtures[0]
				extra.Path = "extra.bin"
				m.Fixtures = append(m.Fixtures, extra)
			},
			wantText: "13 fixture(s); ADR-009 names 12",
		},
		{
			name: "twelve entries, but one ADR-009 item claimed twice and another missing",
			mutate: func(m *Manifest) {
				m.Fixtures[11].ADR009 = m.Fixtures[10].ADR009
			},
			wantText: "is claimed 2 times",
		},
		{
			name:     "an item number outside 1..12",
			mutate:   func(m *Manifest) { m.Fixtures[3].ADR009 = 99 },
			wantText: "outside 1..12",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := clone()
			tc.mutate(m)
			probs := checkDeclaredSet(m)
			if len(probs) == 0 {
				t.Fatalf("%q produced NO problem; `fixtures-verify` would print ok for it", tc.name)
			}
			var joined string
			for _, p := range probs {
				if p.Kind != "declared-set" {
					t.Errorf("problem kind %q, want declared-set", p.Kind)
				}
				joined += p.Detail + "\n"
			}
			if !strings.Contains(joined, tc.wantText) {
				t.Fatalf("the refusal does not say %q, so a developer would not know what is wrong:\n%s", tc.wantText, joined)
			}
		})
	}
}
