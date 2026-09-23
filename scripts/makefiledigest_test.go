package scripts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Sweep B5 — the anchor never runs make on unreviewed Makefile bytes.
//
// The workflow anchor reads the Makefile database with `make -pn`, and GNU Make
// EVALUATES a makefile while reading it: `$(shell …)`, `$(file …)`, `!=`, `+`
// and `$(MAKE)` recipe lines, makefile-remake rules, `.SECONDEXPANSION`
// prerequisites. One Makefile line could therefore run code DURING the anchor
// step and write the next step's environment after the anchor had passed
// (docs/evidence/warroom/2026-09-23-anchor-preflight-DESK-REVIEW-security.md,
// FINDING 4). The chair ruled the control is a committed digest of the bytes,
// .github/pinned-makefiles.yml, checked BEFORE make is invoked (tick 132).
//
// Every demonstration here is a BYTE MUTATION of a temporary copy of this
// repository's REAL Makefile and pin. None constructs a payload.
//
// "make was not invoked" is not taken from the guard's own output: the guard
// runs in-process under scripts/testdata/spawn-recorder.py, which records every
// process it starts, with its argv and environment, before starting it.
// ---------------------------------------------------------------------------

type spawn struct {
	Argv      []string `json:"argv"`
	EnvNames  []string `json:"env_names"`
	EnvValues []string `json:"env_values"`
}

type recording struct {
	Calls                  []spawn `json:"calls"`
	Exit                   int     `json:"exit"`
	MakeInvocationsCounted int     `json:"make_invocations_counted"`
}

func (r recording) makeCalls() int {
	n := 0
	for _, c := range r.Calls {
		if len(c.Argv) > 0 {
			if b := filepath.Base(c.Argv[0]); b == "make" || b == "gmake" {
				n++
			}
		}
	}
	return n
}

// guardEnv is the test process's environment without the variables the
// anchor refuses or reports: `make ci` runs this package under make, and its
// MAKELEVEL/MAKEFLAGS must not leak into a strict run.
func guardEnv(extra ...string) []string {
	strip := map[string]bool{"MAKEFLAGS": true, "GNUMAKEFLAGS": true, "MFLAGS": true, "MAKELEVEL": true,
		"MAKE_RESTARTS": true, "MAKEOVERRIDES": true, "MAKECMDGOALS": true, "MAKEFILES": true,
		"BASH_ENV": true, "ENV": true,
		"GO": true, "SQLC": true, "GOFLAGS": true, "RELEASE": true, "COMMIT": true, "BUILT_AT": true}
	env := []string{}
	for _, kv := range os.Environ() {
		if !strip[strings.SplitN(kv, "=", 2)[0]] {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// runRecorded runs the anchor against root under the spawn recorder.
func runRecorded(t *testing.T, root string, workflow bool, extraEnv ...string) (string, recording) {
	t.Helper()
	repo := repoRoot(t)
	recFile := filepath.Join(t.TempDir(), "record.json")
	args := []string{filepath.Join(repo, "scripts", "testdata", "spawn-recorder.py"), recFile,
		"--root", root, "--targets", "ci"}
	if workflow {
		args = append(args, "--workflow")
	}
	cmd := exec.Command("python3", args...)
	cmd.Dir = repo
	cmd.Env = guardEnv(extraEnv...)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("spawn-recorder: %v\n%s", err, out)
	}
	raw, rerr := os.ReadFile(recFile)
	if rerr != nil {
		t.Fatalf("the recorder wrote no record (%v); nothing about make's invocation is known:\n%s", rerr, out)
	}
	var rec recording
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("record: %v", err)
	}
	if code := cmd.ProcessState.ExitCode(); code != rec.Exit {
		t.Fatalf("recorder exit %d disagrees with the guard's own %d", code, rec.Exit)
	}
	return string(out), rec
}

// treeDigest is relpath -> sha256 for every regular file under dir.
func treeDigest(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		sum := sha256.Sum256(b)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sha256File(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// replaceOnce edits exactly one occurrence of old with new in path. It REFUSES
// a mutation that would not change the bytes: a demonstration whose mutation
// silently did not apply proves nothing, and would read as a pass.
func replaceOnce(path, old, new string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if old == new {
		return fmt.Errorf("refusing a no-op mutation of %s: old == new", path)
	}
	if strings.Count(string(b), old) != 1 {
		return fmt.Errorf("refusing to mutate %s: %q occurs %d times, not exactly once",
			path, old, strings.Count(string(b), old))
	}
	return os.WriteFile(path, []byte(strings.Replace(string(b), old, new, 1)), 0o644)
}

func appendBytes(path, extra string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(extra); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// repin rewrites dir's pin so it pins exactly files, with their CURRENT bytes —
// what a reviewer's paired pin update does.
func repin(dir string, files ...string) error {
	var sb strings.Builder
	sb.WriteString("makefiles:\n")
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		fmt.Fprintf(&sb, "  %s: %s\n", f, hex.EncodeToString(sum[:]))
	}
	return os.WriteFile(filepath.Join(dir, ".github", "pinned-makefiles.yml"), []byte(sb.String()), 0o644)
}

// copyRealTree copies this repository's real Makefile and pin into a temp dir.
func copyRealTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo := repoRoot(t)
	if err := os.MkdirAll(filepath.Join(dir, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"Makefile", filepath.Join(".github", "pinned-makefiles.yml")} {
		b, err := os.ReadFile(filepath.Join(repo, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// applyMutation applies m and PROVES it changed the tree. It returns the files
// whose digest changed (before -> after), or an error if nothing changed.
func applyMutation(t *testing.T, dir string, m func(dir string) error) (map[string][2]string, error) {
	t.Helper()
	before := treeDigest(t, dir)
	if err := m(dir); err != nil {
		return nil, err
	}
	after := treeDigest(t, dir)
	changed := map[string][2]string{}
	for f, h := range after {
		if b, ok := before[f]; !ok {
			changed[f] = [2]string{"(absent)", h}
		} else if b != h {
			changed[f] = [2]string{b, h}
		}
	}
	for f, h := range before {
		if _, ok := after[f]; !ok {
			changed[f] = [2]string{h, "(deleted)"}
		}
	}
	if len(changed) == 0 {
		return nil, errors.New("the mutation did not change a single byte of the tree; refusing to report " +
			"a demonstration that demonstrates nothing")
	}
	return changed, nil
}

type snapshot map[string][]byte

func takeSnapshot(t *testing.T, dir string) snapshot {
	t.Helper()
	s := snapshot{}
	for rel := range treeDigest(t, dir) {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		s[rel] = b
	}
	return s
}

// restore puts every byte back and PROVES the tree is byte-identical to s.
func restore(t *testing.T, dir string, s snapshot, want map[string]string) {
	t.Helper()
	for rel := range treeDigest(t, dir) {
		if _, ok := s[rel]; !ok {
			if err := os.Remove(filepath.Join(dir, rel)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for rel, b := range s {
		if err := os.WriteFile(filepath.Join(dir, rel), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := treeDigest(t, dir)
	if len(got) != len(want) {
		t.Fatalf("restore: %d files, want %d", len(got), len(want))
	}
	for rel, h := range want {
		if got[rel] != h {
			t.Fatalf("restore is NOT byte-identical: %s is %s, was %s", rel, got[rel], h)
		}
	}
}

func assertGreen(t *testing.T, label, out string, rec recording) {
	t.Helper()
	if rec.Exit != 0 {
		t.Fatalf("%s: exit %d, want 0 — without a green control every red below proves nothing:\n%s", label, rec.Exit, out)
	}
	if rec.makeCalls() == 0 || rec.makeCalls() != rec.MakeInvocationsCounted {
		t.Fatalf("%s: the recorder saw %d make process(es) and the guard counted %d; the control must "+
			"show make DOES run on pinned bytes, or 'not invoked' below means nothing:\n%s",
			label, rec.makeCalls(), rec.MakeInvocationsCounted, out)
	}
	t.Logf("%s: exit 0, make started %d time(s) (recorded)", label, rec.makeCalls())
}

func assertRefusedBeforeMake(t *testing.T, label, out string, rec recording, wantText string) {
	t.Helper()
	if rec.Exit != 1 {
		t.Fatalf("%s: exit %d, want 1:\n%s", label, rec.Exit, out)
	}
	if !strings.Contains(out, wantText) {
		t.Fatalf("%s: the refusal does not name %q:\n%s", label, wantText, out)
	}
	if n := rec.makeCalls(); n != 0 {
		t.Fatalf("%s: make was STARTED %d time(s) although a pre-flight check had refused:\n%s", label, n, out)
	}
	if !strings.Contains(out, "make was NOT invoked (0 make process(es) started)") {
		t.Fatalf("%s: the guard did not say make was not invoked:\n%s", label, out)
	}
	var fails []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "FAIL") {
			fails = append(fails, strings.TrimSpace(l))
		}
	}
	t.Logf("%s: exit 1; processes started: %d, of which make: 0 (recorded)\n    %s",
		label, len(rec.Calls), strings.Join(fails, "\n    "))
}

func TestMakefileDigestMutations(t *testing.T) {
	cases := []struct {
		name string
		// setup arranges the tree BEFORE the control run. Whatever it builds must
		// be green, or the red that follows proves nothing.
		setup    func(dir string) error
		mutate   func(dir string) error
		wantText string
	}{
		{
			name: "one byte of the Makefile",
			mutate: func(dir string) error {
				return replaceOnce(filepath.Join(dir, "Makefile"), "`make ci` is the contract.", "`make ci` is the Contract.")
			},
			wantText: "Makefile: sha256",
		},
		{
			name: "an extra include line, pin not updated",
			mutate: func(dir string) error {
				if err := os.WriteFile(filepath.Join(dir, "extra.mk"), []byte("# extra\n"), 0o644); err != nil {
					return err
				}
				return appendBytes(filepath.Join(dir, "Makefile"), "include extra.mk\n")
			},
			wantText: "Makefile: sha256",
		},
		{
			name: "an extra include line, Makefile re-pinned but the included file not",
			mutate: func(dir string) error {
				if err := os.WriteFile(filepath.Join(dir, "extra.mk"), []byte("# extra\n"), 0o644); err != nil {
					return err
				}
				if err := appendBytes(filepath.Join(dir, "Makefile"), "include extra.mk\n"); err != nil {
					return err
				}
				return repin(dir, "Makefile")
			},
			wantText: "make would read extra.mk, which has no entry in .github/pinned-makefiles.yml",
		},
		{
			name: "an included file's bytes",
			setup: func(dir string) error {
				if err := os.WriteFile(filepath.Join(dir, "inc.mk"), []byte("# inc.mk: pinned\nINC_VALUE := 1\n"), 0o644); err != nil {
					return err
				}
				if err := appendBytes(filepath.Join(dir, "Makefile"), "include inc.mk\n"); err != nil {
					return err
				}
				return repin(dir, "Makefile", "inc.mk")
			},
			mutate: func(dir string) error {
				return replaceOnce(filepath.Join(dir, "inc.mk"), "INC_VALUE := 1", "INC_VALUE := 2")
			},
			wantText: "inc.mk: sha256",
		},
		{
			name: "a pin entry deleted (the Makefile's)",
			mutate: func(dir string) error {
				p := filepath.Join(dir, ".github", "pinned-makefiles.yml")
				b, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				line := regexp.MustCompile(`(?m)^  Makefile: [0-9a-f]{64}\n`).Find(b)
				if line == nil {
					return errors.New("the pin has no Makefile entry to delete")
				}
				return replaceOnce(p, string(line), "")
			},
			wantText: "pins no file",
		},
		{
			name: "a pin entry deleted (an included file's)",
			setup: func(dir string) error {
				if err := os.WriteFile(filepath.Join(dir, "inc.mk"), []byte("# inc.mk: pinned\n"), 0o644); err != nil {
					return err
				}
				if err := appendBytes(filepath.Join(dir, "Makefile"), "include inc.mk\n"); err != nil {
					return err
				}
				return repin(dir, "Makefile", "inc.mk")
			},
			mutate: func(dir string) error {
				p := filepath.Join(dir, ".github", "pinned-makefiles.yml")
				b, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				line := regexp.MustCompile(`(?m)^  inc\.mk: [0-9a-f]{64}\n`).Find(b)
				if line == nil {
					return errors.New("the pin has no inc.mk entry to delete")
				}
				return replaceOnce(p, string(line), "")
			},
			wantText: "make would read inc.mk, which has no entry in .github/pinned-makefiles.yml",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := copyRealTree(t)
			if tc.setup != nil {
				if err := tc.setup(dir); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}
			out, rec := runRecorded(t, dir, true)
			assertGreen(t, "control (pinned bytes, --workflow)", out, rec)

			snap := takeSnapshot(t, dir)
			original := treeDigest(t, dir)
			changed, err := applyMutation(t, dir, tc.mutate)
			if err != nil {
				t.Fatalf("mutation not applied: %v", err)
			}
			keys := make([]string, 0, len(changed))
			for f := range changed {
				keys = append(keys, f)
			}
			sort.Strings(keys)
			for _, f := range keys {
				t.Logf("mutated %s: sha256 %s -> %s", f, changed[f][0], changed[f][1])
			}

			for _, workflow := range []bool{true, false} {
				mode := "lenient"
				if workflow {
					mode = "--workflow"
				}
				out, rec := runRecorded(t, dir, workflow)
				assertRefusedBeforeMake(t, "mutated, "+mode, out, rec, tc.wantText)
			}

			restore(t, dir, snap, original)
			t.Logf("restored: every file byte-identical to the control tree (%d file(s))", len(original))
			out, rec = runRecorded(t, dir, true)
			assertGreen(t, "restored (--workflow)", out, rec)
		})
	}
}

// The helpers above REFUSE a mutation that did not apply. Without this, a
// needle that stopped matching after an unrelated Makefile edit would make a
// demonstration pass by demonstrating nothing.
func TestDigestHarnessRefusesAnUnappliedMutation(t *testing.T) {
	dir := copyRealTree(t)
	mk := filepath.Join(dir, "Makefile")
	if err := replaceOnce(mk, "this text is not in the Makefile", "anything"); err == nil {
		t.Fatal("replaceOnce accepted a needle that does not occur")
	}
	if err := replaceOnce(mk, "`make ci` is the contract.", "`make ci` is the contract."); err == nil {
		t.Fatal("replaceOnce accepted old == new")
	}
	if _, err := applyMutation(t, dir, func(string) error { return nil }); err == nil {
		t.Fatal("applyMutation accepted a mutation that changed no byte")
	}
	// And the tree is untouched by the refusals.
	if got, want := sha256File(t, mk), sha256File(t, filepath.Join(repoRoot(t), "Makefile")); got != want {
		t.Fatalf("a refused mutation still changed the Makefile: %s != %s", got, want)
	}
}

// Every process the anchor starts — make AND the bash it asks what `make` is —
// is handed an environment without the runner's command-file variables, and a
// real child started with that environment cannot see them.
func TestTheAnchorsSubprocessesCannotSeeTheRunnerCommandFiles(t *testing.T) {
	cmdDir := filepath.Join(t.TempDir(), "_temp", "_runner_file_commands")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	named := map[string]string{
		"GITHUB_ENV":          "set_env_b5",
		"GITHUB_PATH":         "add_path_b5",
		"GITHUB_OUTPUT":       "set_output_b5",
		"GITHUB_STATE":        "save_state_b5",
		"GITHUB_STEP_SUMMARY": "step_summary_b5",
		// Not one of the five: a command file the runner might add later, under
		// a name nothing here lists. It lives in the same directory.
		"GITHUB_SOME_FUTURE_COMMAND": "future_b5",
	}
	var extra []string
	for k, f := range named {
		p := filepath.Join(cmdDir, f)
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		extra = append(extra, k+"="+p)
	}
	// The control: an ordinary variable must SURVIVE, or "not visible" would be
	// satisfied by an empty environment.
	extra = append(extra, "VIZRA_B5_CONTROL=still-here")

	check := func(label string, names, values []string) {
		t.Helper()
		seen := map[string]bool{}
		for _, n := range names {
			seen[n] = true
		}
		for k := range named {
			if seen[k] {
				t.Errorf("%s: %s is visible to the child", label, k)
			}
		}
		for _, v := range values {
			if strings.Contains(v, "_runner_file_commands") {
				t.Errorf("%s: a value pointing into the runner's command-file directory is visible: %q", label, v)
			}
		}
		if !seen["VIZRA_B5_CONTROL"] {
			t.Errorf("%s: the control variable is missing too — the scrub dropped everything, which proves nothing", label)
		}
	}

	out, rec := runRecorded(t, repoRoot(t), true, extra...)
	if rec.Exit != 0 {
		t.Fatalf("the anchor fails on the real tree with command files set:\n%s", out)
	}
	kinds := map[string]int{}
	for _, c := range rec.Calls {
		kinds[filepath.Base(c.Argv[0])]++
		check("spawn "+strings.Join(c.Argv, " "), c.EnvNames, c.EnvValues)
	}
	if kinds["make"] == 0 || kinds["bash"] == 0 {
		t.Fatalf("expected the anchor to start both make and bash; recorded %v", kinds)
	}
	t.Logf("recorded %d process(es) %v; none was handed a runner command-file variable", len(rec.Calls), kinds)

	// A REAL child, started with the guard's own clean_env(): what it can see.
	recFile := filepath.Join(t.TempDir(), "child.json")
	cmd := exec.Command("python3", filepath.Join(repoRoot(t), "scripts", "testdata", "spawn-recorder.py"),
		"--child-env", recFile)
	cmd.Env = guardEnv(extra...)
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child-env probe: %v\n%s", err, o)
	}
	raw, err := os.ReadFile(recFile)
	if err != nil {
		t.Fatal(err)
	}
	var child struct {
		Names  []string `json:"child_env_names"`
		Values []string `json:"child_env_values"`
	}
	if err := json.Unmarshal(raw, &child); err != nil {
		t.Fatal(err)
	}
	check("child `env`", child.Names, child.Values)
	t.Logf("a real child (`env`) printed %d variable(s); none of the %d command-file variables", len(child.Names), len(named))
}

// The recorder sees processes started through the subprocess module. That is a
// complete record only while the guard starts processes no other way.
func TestTheSpawnRecorderSeesEveryWayTheGuardStartsAProcess(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "make-integrity-guard.py"))
	if err != nil {
		t.Fatal(err)
	}
	// makefile_pin.py is imported by the guard and runs in its process, so it
	// is held to the same rule — and it must start no process at all.
	pin, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "makefile_pin.py"))
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"make-integrity-guard.py": string(src), "makefile_pin.py": string(pin)} {
		for _, forbidden := range []string{"os.system", "os.popen", "os.exec", "os.spawn", "os.posix_spawn",
			"os.fork", "import pty", "import ctypes", "multiprocessing", "asyncio"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s uses %q, which spawn-recorder.py does not intercept; "+
					"'make was not invoked' would no longer be proved by it", name, forbidden)
			}
		}
	}
	if regexp.MustCompile(`(?m)^\s*(import|from)\s+subprocess\b`).Match(pin) {
		t.Error("makefile_pin.py imports subprocess; it must decide everything WITHOUT starting a process")
	}
	if !strings.Contains(string(src), "subprocess.run(") {
		t.Error("the guard no longer starts processes through subprocess.run; re-check the recorder")
	}
}

// ci-required-guard check 11: the pin exists, has its one shape, is non-empty,
// pins the Makefile, and matches. Run against the real workflows and manifest,
// with only the pin pointed at a fixture.
func TestCIRequiredGuardMakefilePin(t *testing.T) {
	cases := []struct {
		pin      string
		wantFail bool
		wantText string
	}{
		{pin: "good", wantFail: false, wantText: "covers the Makefile, and every digest matches the tree"},
		{pin: "missing", wantFail: true, wantText: "pinned-makefiles.yml is missing"},
		{pin: "empty", wantFail: true, wantText: "pins no file"},
		{pin: "quoted-key", wantFail: true, wantText: "is not in its one accepted shape"},
		{pin: "no-makefile-entry", wantFail: true, wantText: "does not pin `Makefile`"},
		// THE acceptance case: a Makefile edit without the paired pin update.
		{pin: "stale", wantFail: true, wantText: "Makefile changed without the paired update to pinned-makefiles.yml"},
		{pin: "pinned-file-missing", wantFail: true, wantText: "inc.mk: pinned, but not a file"},
		// Fix round 1 (PR#10 VERIFY FINDING 3): check 11 now calls the anchor's
		// own makefile_pin.verify_pin, so it refuses whatever the anchor refuses.
		// Each of these was exit 0 from check 11 at 62d16aa (transcript P1).
		{pin: "gnumakefile-present", wantFail: true, wantText: "GNUmakefile exists beside the Makefile"},
		{pin: "makefile-symlink", wantFail: true, wantText: "Makefile is not a regular file"},
		{pin: "stale-entry", wantFail: true, wantText: "pins old.mk, which make would NOT read"},
		{pin: "include-unpinned", wantFail: true, wantText: "make would read inc.mk, which has no entry"},
		{pin: "include-computed", wantFail: true, wantText: "names a file make COMPUTES"},
		// Fix round 2 NIT: a directory where the pin should be is named as one.
		{pin: "pin-is-directory", wantFail: true, wantText: "is a directory, not a file"},
	}
	for _, tc := range cases {
		t.Run(tc.pin, func(t *testing.T) {
			t.Parallel()
			out, code := run(t, "ci-required-guard.sh", "--makefile-pins",
				filepath.Join("scripts", "testdata", "makefilepin", tc.pin, ".github", "pinned-makefiles.yml"))
			if (code != 0) != tc.wantFail {
				t.Fatalf("exit %d, want failed=%v\n%s", code, tc.wantFail, out)
			}
			if !strings.Contains(out, tc.wantText) {
				t.Fatalf("the output does not say %q:\n%s", tc.wantText, out)
			}
		})
	}
}

// Every fixture directory for the anchor and for check 11 must be named by a
// case IN ITS OWN TABLE. An orphaned fixture looks like coverage in a diff and
// proves nothing.
func TestEveryMakeGuardAndPinFixtureIsExercised(t *testing.T) {
	root := repoRoot(t)
	body := func(file, fn string) string {
		src, err := os.ReadFile(filepath.Join(root, "scripts", file))
		if err != nil {
			t.Fatal(err)
		}
		s := string(src)
		i := strings.Index(s, "func "+fn+"(")
		if i < 0 {
			t.Fatalf("%s has no %s", file, fn)
		}
		j := strings.Index(s[i+1:], "\nfunc ")
		if j < 0 {
			return s[i:]
		}
		return s[i : i+1+j]
	}
	for _, set := range []struct{ dir, file, fn, field string }{
		{"makeguard", "scripts_test.go", "TestMakeIntegrityGuardFixtures", "dir"},
		{"makefilepin", "makefiledigest_test.go", "TestCIRequiredGuardMakefilePin", "pin"},
	} {
		entries, err := os.ReadDir(filepath.Join(root, "scripts", "testdata", set.dir))
		if err != nil {
			t.Fatal(err)
		}
		table := body(set.file, set.fn)
		for _, e := range entries {
			if e.IsDir() && !strings.Contains(table, set.field+`: "`+e.Name()+`"`) {
				t.Errorf("scripts/testdata/%s/%s is a fixture %s does not name", set.dir, e.Name(), set.fn)
			}
		}
	}
}

// A makefile can be REMADE from a file nobody pinned, by make's own builtin
// implicit rules, with no line in any makefile saying so — and make does it
// even under -n. MEASURED on GNU Make 3.81 and 4.3: with a newer sibling
// `Makefile.sh` (builtin rule `%: %.sh`), `make -pn ci` executed
// `cat Makefile.sh >Makefile` and re-read the result. The digest check alone
// cannot see it: the bytes are still the pinned bytes when the anchor looks.
//
// Fix round 1 (PR#10 VERIFY, FINDING 1): -q applies in make's remake phase ONLY
// to makefiles that are command-line goals. The first probe ran `make -q
// Makefile`, then `make -q a.mk`, … — so in the first one a pinned INCLUDE was
// not a goal, and make really ran `cat a.mk.sh >a.mk`, re-read it, and said
// "up to date". The probe is now ONE `make -q` naming every pinned makefile;
// the `included` case below is red against the per-file loop (transcript C7).
//
// Each sibling is a byte-identical copy of its makefile plus one comment line —
// a byte mutation, not a payload. Asserted: the anchor refuses in both modes,
// every pinned file is byte-identical afterwards (nothing was remade), and make
// was started exactly ONCE, as `make -q <every pinned makefile>`.
func TestAMakefileMakeWouldRemakeIsRefusedWithoutRunningARecipe(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(dir string) error // arranges a tree whose pin lists every file make reads
		target string                 // the pinned makefile that gets a newer sibling
		pinned []string               // every pinned makefile, in the order make reads them
	}{
		{name: "the Makefile", target: "Makefile", pinned: []string{"Makefile"}},
		{
			name: "an included makefile",
			setup: func(dir string) error {
				if err := os.WriteFile(filepath.Join(dir, "a.mk"), []byte("# a.mk: pinned\nA_VALUE := a\n"), 0o644); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(dir, "b.mk"), []byte("# b.mk: pinned\nB_VALUE := b\n"), 0o644); err != nil {
					return err
				}
				if err := appendBytes(filepath.Join(dir, "Makefile"), "include a.mk\nsinclude b.mk\n"); err != nil {
					return err
				}
				return repin(dir, "Makefile", "a.mk", "b.mk")
			},
			target: "a.mk",
			pinned: []string{"Makefile", "a.mk", "b.mk"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := copyRealTree(t)
			if tc.setup != nil {
				if err := tc.setup(dir); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}
			out, rec := runRecorded(t, dir, true)
			assertGreen(t, "control (no sibling)", out, rec)

			before := map[string]string{}
			for _, f := range tc.pinned {
				before[f] = sha256File(t, filepath.Join(dir, f))
			}
			target := filepath.Join(dir, tc.target)
			b, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			sibling := target + ".sh"
			if err := os.WriteFile(sibling, append(b, []byte("# a sibling copy, one comment longer\n")...), 0o644); err != nil {
				t.Fatal(err)
			}
			// Make the sibling unambiguously NEWER, as a checkout that writes it
			// after the makefile would.
			past := time.Now().Add(-time.Hour)
			if err := os.Chtimes(target, past, past); err != nil {
				t.Fatal(err)
			}
			t.Logf("added %s.sh (sha256 %s), newer than %s (sha256 %s)",
				tc.target, sha256File(t, sibling), tc.target, before[tc.target])

			want := append([]string{"make", "-q"}, tc.pinned...)
			for _, workflow := range []bool{true, false} {
				out, rec := runRecorded(t, dir, workflow)
				if rec.Exit != 1 || !strings.Contains(out, "make would REMAKE one of "+strings.Join(tc.pinned, ", ")) {
					t.Fatalf("workflow=%v: exit %d; want the remake refused by name:\n%s", workflow, rec.Exit, out)
				}
				for _, f := range tc.pinned {
					if got := sha256File(t, filepath.Join(dir, f)); got != before[f] {
						t.Fatalf("workflow=%v: %s was REWRITTEN during the anchor (%s -> %s):\n%s",
							workflow, f, before[f], got, out)
					}
				}
				var makes [][]string
				for _, c := range rec.Calls {
					if filepath.Base(c.Argv[0]) == "make" {
						makes = append(makes, c.Argv)
					}
				}
				if len(makes) != 1 || strings.Join(makes[0], " ") != strings.Join(want, " ") {
					t.Fatalf("workflow=%v: make was started as %v; want exactly ONE process, %v — every "+
						"pinned makefile a goal, so -q applies to each, and nothing after it:\n%s",
						workflow, makes, want, out)
				}
				t.Logf("workflow=%v: exit 1, every pinned file byte-identical afterwards, make started once: %v",
					workflow, makes[0])
			}

			if err := os.Remove(sibling); err != nil {
				t.Fatal(err)
			}
			out, rec = runRecorded(t, dir, true)
			assertGreen(t, "sibling removed", out, rec)
		})
	}
}

// Before sweep B5 a failed environment check was REPORTED and make was run
// anyway — with BASH_ENV set, the Makefile's own reviewed `$(shell …)` calls
// ran through a bash that sourced it. Now any failed pre-flight stops the
// anchor before make. The rows are ones the anchor already refused; what is new
// is that make is never started for them (recorded, not taken from the output).
func TestAFailedEnvironmentCheckStopsTheAnchorBeforeMake(t *testing.T) {
	cases := []struct {
		name     string
		workflow bool
		env      []string
		wantText string
	}{
		{"workflow/MAKEFLAGS=-i", true, []string{"MAKEFLAGS=-i"}, "must be UNSET"},
		{"workflow/BASH_ENV", true, []string{"BASH_ENV=/nonexistent/b5.sh"}, "BASH_ENV"},
		{"workflow/MAKEFILES", true, []string{"MAKEFILES=/nonexistent/b5.mk"}, "MAKEFILES"},
		{"workflow/GO=true", true, []string{"GO=true"}, "the environment sets GO='true'"},
		{"local/MAKEFILES", false, []string{"MAKEFILES=/nonexistent/b5.mk"}, "MAKEFILES"},
		{"local/MAKEFLAGS=-ki", false, []string{"MAKELEVEL=1", "MAKEFLAGS=-ki"}, "is not a flag make itself"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, rec := runRecorded(t, repoRoot(t), tc.workflow, tc.env...)
			assertRefusedBeforeMake(t, tc.name, out, rec, tc.wantText)
		})
	}
}

// Fix round 2 (PR#10 re-verification, R1-F1, and the chair's ruling): the
// directives and assignments the TEXT checks refuse are refused in EVERY
// spelling make accepts — any operator, `override`/`export`/`private`
// modifiers, whitespace variants, `define`, target- and pattern-specific
// forms — before make is started (recorded, not read from the output). Each
// row is the `good` fixture plus the lines shown, re-pinned: reviewed-bytes
// shape, no payload. The control rows must PASS, with make started, or the
// refusals prove nothing.
func TestEveryRefusedSpellingIsRefusedBeforeMake(t *testing.T) {
	good, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "testdata", "makeguard", "good", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	const (
		rp  = "names `.RECIPEPREFIX`"
		se  = "names `.SECONDEXPANSION`"
		tsa = "target- or pattern-specific assignment of"
		def = "with `define`"
	)
	cases := []struct {
		name, extra, wantText string
		// mustNot: text the refusal must NOT contain — a refusal for the wrong
		// reason names the wrong construct (#11 cross-check X-1).
		mustNot []string
	}{
		{"recipeprefix :=", ".RECIPEPREFIX := >\n", rp, nil},
		{"recipeprefix = no spaces", ".RECIPEPREFIX=>\n", rp, nil},
		{"recipeprefix +=", ".RECIPEPREFIX   +=   >\n", rp, nil},
		{"recipeprefix ?=", ".RECIPEPREFIX ?= >\n", rp, nil},
		{"recipeprefix ::=", ".RECIPEPREFIX ::= >\n", rp, nil},
		{"recipeprefix !=", ".RECIPEPREFIX != printf '>'\n", rp, nil},
		{"recipeprefix override", "override .RECIPEPREFIX = >\n", rp, nil},
		{"recipeprefix export", "export .RECIPEPREFIX := >\n", rp, nil},
		{"recipeprefix override export", "override export .RECIPEPREFIX := >\n", rp, nil},
		{"recipeprefix private", "private .RECIPEPREFIX := >\n", rp, nil},
		{"recipeprefix leading spaces", "   .RECIPEPREFIX := >\n", rp, nil},
		{"recipeprefix trailing comment", ".RECIPEPREFIX := > # a comment\n", rp, nil},
		{"recipeprefix define", "define .RECIPEPREFIX\n>\nendef\n", rp, nil},
		{"recipeprefix target-specific", "ci: .RECIPEPREFIX := >\n", rp, nil},
		{"recipeprefix line continuation", ".RECIPEPREFIX := \\\n>\n", rp, nil},
		{"secondexpansion", ".SECONDEXPANSION:\n", se, nil},
		{"secondexpansion spaced", ".SECONDEXPANSION :\n", se, nil},
		{"secondexpansion double colon", ".SECONDEXPANSION::\n", se, nil},
		{"secondexpansion among targets", ".PHONY .SECONDEXPANSION:\n", se, nil},
		{"pattern-specific SHELL", "%: SHELL := /usr/bin/true\n", tsa + " SHELL", nil},
		{"target-specific SHELL", "ci: SHELL := /usr/bin/true\n", tsa + " SHELL", nil},
		{"pattern-specific MAKEFLAGS", "%: MAKEFLAGS += -i\n", tsa + " MAKEFLAGS", nil},
		{"pattern-specific override SHELL", "%: override SHELL = /usr/bin/true\n", tsa + " SHELL", nil},
		{"pattern-specific private .SHELLFLAGS", "%: private .SHELLFLAGS := -c\n", tsa + " .SHELLFLAGS", nil},
		{"prefix-pattern export GNUMAKEFLAGS", "test-%: export GNUMAKEFLAGS := -k\n", tsa + " GNUMAKEFLAGS", nil},
		{"pattern-specific computed name", "NAME := SHELL\n%: $(NAME) := /usr/bin/true\n", tsa + " a variable whose NAME make computes", nil},
		{"define SHELL", "define SHELL\n/usr/bin/true\nendef\n", "assigns SHELL " + def, nil},
		{"override define SHELL", "override define SHELL\n/usr/bin/true\nendef\n", "assigns SHELL " + def, nil},
		{"define MAKEFLAGS", "define MAKEFLAGS\n-i\nendef\n", "assigns MAKEFLAGS " + def, nil},
		{"private SHELL", "private SHELL := /usr/bin/true\n", "sets SHELL to something other than the approved value", nil},
		// Slice B5b (R2-F1): .IGNORE / .DEFAULT / .EXTRA_PREREQS in every form.
		{"ignore bare", ".IGNORE:\n", "names `.IGNORE`", nil},
		{"ignore per-target", ".IGNORE: ci\n", "names `.IGNORE`", nil},
		{"ignore double colon", ".IGNORE::\n", "names `.IGNORE`", nil},
		{"ignore among targets", ".PHONY .IGNORE: ci\n", "names `.IGNORE`", nil},
		{"default rule", ".DEFAULT:\n\t@true\n", "names `.DEFAULT`", nil},
		{"default one-line recipe", ".DEFAULT: ; @true\n", "names `.DEFAULT`", nil},
		{"extra-prereqs :=", ".EXTRA_PREREQS := Makefile\n", "names `.EXTRA_PREREQS`", nil},
		{"extra-prereqs target-specific", "ci: .EXTRA_PREREQS := Makefile\n", "names `.EXTRA_PREREQS`", nil},
		{"extra-prereqs override", "override .EXTRA_PREREQS += Makefile\n", "names `.EXTRA_PREREQS`", nil},
		// Slice B5b (R2-F2).
		{"pattern rule", "%.done:\n\t@echo pattern\n", "is a PATTERN rule", nil},
		{"pattern rule double colon", "%.done::\n\t@echo pattern\n", "is a PATTERN rule", nil},
		{"sub-make ${MAKE}", "ci-sub:\n\t${MAKE} other\n.PHONY: ci-sub\nci: ci-sub\n", "starts a sub-make", nil},
		// Controls: a pattern-specific assignment of an ordinary variable, a
		// `.DEFAULT_GOAL` (which `.DEFAULT` is a prefix of), and a comment-free
		// tree, are NOT refused.
		// #11 fix round 1 (cross-check X-1): the recipe forms the TAB-keyed,
		// one-target-per-line reading did not scan, each refused by name for
		// what it is — not incidentally, through junk "prerequisites".
		{"inline recipe on the gate target", "ci: ; -true\n", "is a rule with an INLINE `;` recipe", []string{"has no explicit rule"}},
		{"inline recipe, no space", "ci:;-true\n", "is a rule with an INLINE `;` recipe", []string{"has no explicit rule"}},
		{"inline recipe glued to a computed token", "EMPTY :=\nci: $(EMPTY); -./run-the-real-tests.sh\n", "is a rule with an INLINE `;` recipe", []string{"has no explicit rule"}},
		{"inline recipe on a closure prerequisite", "ci: lane\n.PHONY: lane\nlane: ; -./run-the-real-tests.sh\n", "is a rule with an INLINE `;` recipe", []string{"has no explicit rule"}},
		{"multi-target rule naming a closure target", "other ci:\n\t-./run-the-real-tests.sh\n", "is a MULTI-TARGET rule (other, ci)", []string{"is not defined in any makefile"}},
		{"multi-target rule, gate target first", "ci other:\n\t-./run-the-real-tests.sh\n", "is a MULTI-TARGET rule (ci, other)", []string{"is not defined in any makefile"}},
		{"multi-target recipe beside a recipe-less rule", "ci: lane\n.PHONY: lane\nlane:\nother lane:\n\t-./run-the-real-tests.sh\n", "is a MULTI-TARGET rule (other, lane)", []string{"has no explicit rule"}},
		{"grouped targets", "a b &:\n\t@true\n", "is a MULTI-TARGET rule", nil},
		{"static pattern, two targets", "a b: %.x: %.y\n", "is a MULTI-TARGET rule", nil},
		// B5c (search desk review M-1, M-5).
		{"computed prerequisite", "LANE := lane\nci: $(LANE)\n", "has a prerequisite make COMPUTES: $(LANE)", nil},
		{"computed order-only prerequisite", "LANE := lane\nci: | $(LANE)\n", "has a prerequisite make COMPUTES: $(LANE)", nil},
		{"posix", ".POSIX:\n", "names `.POSIX`", nil},
		{"control: .DEFAULT_GOAL", ".DEFAULT_GOAL := ci\n", "", nil},
		{"control: pattern-specific ordinary variable", "%: FOO := bar\n", "", nil},
		{"control: nothing added", "", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, ".github"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "Makefile"), append(append([]byte{}, good...), tc.extra...), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := repin(dir, "Makefile"); err != nil {
				t.Fatal(err)
			}
			out, rec := runRecorded(t, dir, true)
			if tc.wantText == "" {
				assertGreen(t, "control", out, rec)
				return
			}
			assertRefusedBeforeMake(t, tc.name, out, rec, tc.wantText)
			for _, bad := range tc.mustNot {
				if strings.Contains(out, bad) {
					t.Fatalf("%s: refused for the WRONG reason — the output says %q:\n%s", tc.name, bad, out)
				}
			}
		})
	}
}

// Fix round 2 NIT: a pinned file that cannot be READ is refused by name — in
// the anchor (make not started) and in check 11 — never by a traceback.
func TestAnUnreadablePinnedFileIsRefusedByName(t *testing.T) {
	if os.Geteuid() == 0 {
		// root reads a mode-000 file anyway, so the case cannot be constructed.
		t.Fatal("this test must not run as root: chmod 000 would not make the file unreadable")
	}
	dir := copyRealTree(t)
	inc := filepath.Join(dir, "inc.mk")
	if err := os.WriteFile(inc, []byte("# inc.mk: pinned\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := appendBytes(filepath.Join(dir, "Makefile"), "include inc.mk\n"); err != nil {
		t.Fatal(err)
	}
	if err := repin(dir, "Makefile", "inc.mk"); err != nil {
		t.Fatal(err)
	}
	out, rec := runRecorded(t, dir, true)
	assertGreen(t, "control (readable)", out, rec)
	if err := os.Chmod(inc, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(inc, 0o644) })

	out, rec = runRecorded(t, dir, true)
	assertRefusedBeforeMake(t, "anchor, inc.mk mode 000", out, rec, "inc.mk is pinned but cannot be read")
	if strings.Contains(out, "Traceback") {
		t.Fatalf("the anchor refused with a traceback, not by name:\n%s", out)
	}
	gout, code := run(t, "ci-required-guard.sh", "--makefile-pins", filepath.Join(dir, ".github", "pinned-makefiles.yml"))
	if code != 1 || !strings.Contains(gout, "inc.mk is pinned but cannot be read") || strings.Contains(gout, "Traceback") {
		t.Fatalf("check 11: exit %d; want exit 1 naming inc.mk, no traceback:\n%s", code, gout)
	}
}
