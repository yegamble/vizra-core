package scripts_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runPython executes a python script from the repository root, the way the
// workflow steps do, and returns combined output and exit code.
func runPython(t *testing.T, name string, args ...string) (string, int) {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command("python3", append([]string{filepath.Join(root, "scripts", name)}, args...)...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("%s: %v\n%s", name, err, out)
	}
	return string(out), code
}

// repoRoot is the absolute path of the repository, so the scripts can be
// executed by absolute path while running with the repository as their working
// directory — they all `cd` relative to their own location.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// run executes a script from the repository root and returns combined output
// and exit code.
func run(t *testing.T, name string, args ...string) (string, int) {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command(filepath.Join(root, "scripts", name), args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return string(out), code
}

// ---------------------------------------------------------------------------
// migrate-lint
// ---------------------------------------------------------------------------

// VZ-FOUND-003's negative case is "A DROP COLUMN in *.up.sql fails
// migrate-lint". That was satisfied by the current text of a regex and by
// nothing that keeps it satisfied: removing COLUMN from the alternation
// survived the whole gate.
func TestMigrateLintFixtures(t *testing.T) {
	cases := []struct {
		dir      string
		wantFail bool
		wantText string
	}{
		{dir: "good", wantFail: false},
		{dir: "commented-out", wantFail: false},
		// A deliberate, REVIEWED exception is still allowed, or the rule becomes
		// one people route around.
		{dir: "allow-destructive", wantFail: false},
		{dir: "no-down-declared", wantFail: false},

		{dir: "drop-column", wantFail: true, wantText: "destructive"},
		{dir: "drop-table", wantFail: true, wantText: "destructive"},
		{dir: "drop-index", wantFail: true, wantText: "destructive"},
		{dir: "drop-constraint", wantFail: true, wantText: "destructive"},
		{dir: "rename", wantFail: true, wantText: "destructive"},
		{dir: "alter-type", wantFail: true, wantText: "destructive"},
		{dir: "truncate", wantFail: true, wantText: "destructive"},

		{dir: "bad-filename", wantFail: true, wantText: "filename"},
		{dir: "sequence-gap", wantFail: true, wantText: "gap"},
		{dir: "missing-down", wantFail: true, wantText: "no-down"},
		{dir: "empty-down", wantFail: true, wantText: "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			out, code := run(t, "migrate-lint.sh", "--dir", filepath.Join("scripts", "testdata", "migratelint", tc.dir))
			failed := code != 0
			if failed != tc.wantFail {
				t.Fatalf("exit %d (failed=%v), want failed=%v.\n%s", code, failed, tc.wantFail, out)
			}
			if tc.wantText != "" && !strings.Contains(strings.ToLower(out), tc.wantText) {
				t.Fatalf("the failure does not mention %q, so an author would not know what to fix:\n%s", tc.wantText, out)
			}
		})
	}
}

// The real migrations must pass, or every fixture above is checking a script
// the repository does not actually run.
func TestMigrateLintPassesOnTheRealMigrations(t *testing.T) {
	out, code := run(t, "migrate-lint.sh")
	if code != 0 {
		t.Fatalf("migrate-lint fails on the repository's own migrations:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// ci-required-guard
// ---------------------------------------------------------------------------

// Every one of these is a way to neuter a required lane while the old
// regex-based guard printed "ok no continue-on-error on any lane".
func TestCIRequiredGuardFixtures(t *testing.T) {
	cases := []struct {
		dir      string
		wantFail bool
		wantText string
	}{
		{dir: "good", wantFail: false},

		// The manifest is editable by the PR it gates.
		{dir: "floor-deleted", wantFail: true, wantText: "missing"},
		// VZ-FOUND-007: deleting the deterministic-corpus lane from the
		// manifest must turn the floor red by name. Without that, a PR that
		// moved the fixture bytes could drop the lane that would have noticed
		// and every later media assertion would still report green.
		{dir: "fixtures-floor-deleted", wantFail: true, wantText: "'fixtures' is missing"},
		{dir: "floor-commented", wantFail: true, wantText: "commented out"},

		// Every spelling of continue-on-error. The regex saw only the first.
		{dir: "coe-bare", wantFail: true, wantText: "continue-on-error"},
		{dir: "coe-quoted", wantFail: true, wantText: "continue-on-error"},
		{dir: "coe-capitalised", wantFail: true, wantText: "continue-on-error"},
		{dir: "coe-expression", wantFail: true, wantText: "continue-on-error"},
		{dir: "coe-underscore", wantFail: true, wantText: "continue-on-error"},
		{dir: "coe-step", wantFail: true, wantText: "continue-on-error"},

		// A floor lane that never runs on a PR gates nothing.
		{dir: "not-on-pull-request", wantFail: true, wantText: "pull_request"},

		// Checks 8 and 9: the out-of-make controls on the make-driven gate.
		// Every required lane here runs through `make`, and ONE line in a
		// Makefile no-ops every recipe, so these assert the anchor step and the
		// direct test lane are present AND armed. Each fixture changes exactly
		// one thing relative to "good".
		{dir: "anchor-missing", wantFail: true, wantText: "with no make-integrity-guard step before it"},
		{dir: "anchor-after-make", wantFail: true, wantText: "after `make` at position"},
		{dir: "anchor-conditional", wantFail: true, wantText: "conditional"},
		{dir: "anchor-continue-on-error", wantFail: true, wantText: "continue-on-error"},
		// An aggregate floor lane whose `needs:` leg runs make with no anchor.
		// This is the shape `cache-matrix` really has: checking only the named
		// job would have printed ok while the job that invokes make was
		// unanchored.
		{dir: "needs-leg-unanchored", wantFail: true, wantText: "needs:cache-matrix-leg"},
		// The Actions analogue of `SHELL := /usr/bin/true`.
		{dir: "defaults-shell-workflow", wantFail: true, wantText: "defaults.run.shell"},
		{dir: "defaults-shell-job", wantFail: true, wantText: "defaults.run.shell"},
		{dir: "no-direct-test-lane", wantFail: true, wantText: "every unit test invocation goes through"},

		{dir: "bad-runner", wantFail: true, wantText: "runner"},
		{dir: "unpinned-action", wantFail: true, wantText: "pinned"},
		{dir: "missing-job", wantFail: true, wantText: "matches no job"},

		// ------------------------------------------------------------------
		// Sweep B1, check 8b: the make step's own WORKFLOW LINE.
		//
		// The anchor runs `make -pn` in its own process and reads its own
		// environment; it cannot see another step's argv or `env:`. A verifier
		// measured four spellings that left BOTH guards exiting 0 while every
		// make-driven lane went silent (PR#6 VERIFY, FINDING 1). Each fixture
		// below changes exactly one thing relative to "good".
		// ------------------------------------------------------------------
		{dir: "make-flag-override", wantFail: true, wantText: "-i/--ignore-errors"},
		{dir: "make-var-override", wantFail: true, wantText: "variable override 'shell=/usr/bin/true'"},
		// make applies a command-line override wherever it sits, so putting it
		// after the target must be refused identically.
		{dir: "make-var-override-after-target", wantFail: true, wantText: "variable override 'shell=/usr/bin/true'"},
		{dir: "make-env-prefix", wantFail: true, wantText: "environment prefix 'makeflags=-i'"},
		{dir: "make-env-command", wantFail: true, wantText: "environment prefix 'gnumakeflags=-i'"},
		// GNU make accepts any unambiguous abbreviation of a long option, so a
		// literal-string check for "--ignore-errors" would miss this.
		{dir: "make-flag-long-abbrev", wantFail: true, wantText: "-i/--ignore-errors"},
		// The harmful letter inside a cluster of harmless ones.
		{dir: "make-flag-cluster", wantFail: true, wantText: "carries -i"},
		{dir: "make-file-elsewhere", wantFail: true, wantText: "different makefile"},
		{dir: "make-directory", wantFail: true, wantText: "changes directory"},
		// The command hidden inside a quoted sub-shell.
		{dir: "make-wrapped-shell", wantFail: true, wantText: "inside the quoted script"},
		// Buried after an `&&` in a multi-line script.
		{dir: "make-multiline-chain", wantFail: true, wantText: "--keep-going"},
		// Split off the command by a line continuation.
		{dir: "make-line-continuation", wantFail: true, wantText: "--dry-run"},
		// The same reach, through `env:` at each of the three levels.
		{dir: "step-env-makeflags", wantFail: true, wantText: "sets makeflags"},
		{dir: "job-env-makeflags", wantFail: true, wantText: "job-level env sets makeflags"},
		{dir: "workflow-env-makeflags", wantFail: true, wantText: "workflow-level env sets gnumakeflags"},
		{dir: "job-env-makefiles", wantFail: true, wantText: "sets makefiles"},
		// A `run:` this guard cannot read is a FAILURE, not a skip: otherwise an
		// unbalanced quote would be a way to hide the argv from check 8b.
		{dir: "untokenisable-run", wantFail: true, wantText: "cannot be tokenised as shell"},
		// The ANCHOR heuristic is deliberately WIDER than the tokeniser: a bare
		// `make` token in a COMMENT still demands the anchor. Over-demanding
		// fails closed; under-demanding does not. This pins that property.
		{dir: "make-in-comment-needs-anchor", wantFail: true, wantText: "with no make-integrity-guard step before it"},

		// ------------------------------------------------------------------
		// Sweep B1: the CHECKED SET is floor ∪ required (PR#6 VERIFY, FINDING 6).
		// ------------------------------------------------------------------
		// A lane that is REQUIRED but absent from FLOOR_LANES used to get one
		// line — "ok … resolves to a job" — while carrying continue-on-error,
		// an unanchored `make -i ci` and no pull_request trigger.
		{dir: "required-not-floor", wantFail: true, wantText: "'extra-lane'"},

		// Sweep B1, check 9: the INTEGRATION suite needs a make-free invocation
		// too (PR#6 VERIFY, FINDING 2).
		{dir: "no-direct-integration-lane", wantFail: true, wantText: "runs the integration suite directly"},

		// Sweep B1, check 10: a lane that checks out says which tree it stood
		// in (PR#6 VERIFY, FINDING 4).
		{dir: "no-provenance", wantFail: true, wantText: "has no scripts/provenance.sh step"},
		{dir: "provenance-conditional", wantFail: true, wantText: "provenance step conditional"},
	}
	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			base := filepath.Join("scripts", "testdata", "guard", tc.dir)
			out, code := run(t, "ci-required-guard.sh",
				"--workflows", filepath.Join(base, "workflows"),
				"--manifest", filepath.Join(base, "required-checks.txt"),
				"--skip-makefile")
			failed := code != 0
			if failed != tc.wantFail {
				t.Fatalf("exit %d (failed=%v), want failed=%v.\n%s", code, failed, tc.wantFail, out)
			}
			if tc.wantText != "" && !strings.Contains(strings.ToLower(out), tc.wantText) {
				t.Fatalf("the failure does not mention %q:\n%s", tc.wantText, out)
			}
		})
	}
}

// The guard must pass on the repository's own .github, or the fixtures are
// checking something the gate does not run.
func TestCIRequiredGuardPassesOnTheRealWorkflows(t *testing.T) {
	out, code := run(t, "ci-required-guard.sh")
	if code != 0 {
		t.Fatalf("the guard fails on the repository's own workflows:\n%s", out)
	}
	// And it must have actually read the Makefile — the check it used to skip.
	if !strings.Contains(out, "PKGS = ./...") {
		t.Fatalf("the guard did not report on the Makefile's test selection:\n%s", out)
	}
	// A check that silently stopped running still prints nothing, so assert the
	// two out-of-make controls were REPORTED ON, not merely not-failed.
	for _, want := range []string{
		"runs the make-integrity-guard anchor",
		// BOTH suites, named separately: from sweep B1 the integration suite
		// needs a make-free invocation too, and a single substring would have
		// been satisfied by the unit lane alone.
		"the unit suite runs directly, without make",
		"the integration suite runs directly, without make",
		// Checks 8b and 10, reported rather than merely not-failed.
		"carry no no-op flag, no variable override and no MAKEFLAGS-family env",
		"runs scripts/provenance.sh after checking out",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the guard did not report %q against the real workflows:\n%s", want, out)
		}
	}
}

// ---------------------------------------------------------------------------
// make-integrity-guard
// ---------------------------------------------------------------------------

// Every required lane in this repository runs through `make`, and a verifier
// measured that ONE line — `SHELL := /usr/bin/true` or `MAKEFLAGS += -i` —
// makes every recipe exit 0 without running, so `ci-required` would be green
// with the whole gate failing underneath it. These fixtures are one crafted
// Makefile per way of doing that.
func TestMakeIntegrityGuardFixtures(t *testing.T) {
	cases := []struct {
		dir      string
		wantFail bool
		wantText string
	}{
		{dir: "good", wantFail: false},

		// The two the verifier actually measured.
		{dir: "shell-override", wantFail: true, wantText: "shell"},
		{dir: "makeflags-ignore", wantFail: true, wantText: "makeflags"},

		// The same two reached through an `include`, which a scan of the root
		// Makefile alone would not see. MAKEFILE_LIST comes from make itself.
		{dir: "included-makeflags", wantFail: true, wantText: "inc.mk"},
		{dir: "included-shell", wantFail: true, wantText: "inc.mk"},

		// Neighbours of the same class.
		{dir: "shell-colon", wantFail: true, wantText: "shell"},
		{dir: "shellflags-neutered", wantFail: true, wantText: "shellflags"},
		{dir: "gnumakeflags", wantFail: true, wantText: "gnumakeflags"},
		{dir: "no-shell-pin", wantFail: true, wantText: "approved assignments"},
		{dir: "oneshell", wantFail: true, wantText: "oneshell"},

		// A `-` prefix is INVISIBLE to `make --dry-run`, which prints the
		// command without it. Only the text reading can see this one, which is
		// why there is a text reading at all.
		{dir: "dash-prefix", wantFail: true, wantText: "prefixed `-`"},
		{dir: "at-dash-prefix", wantFail: true, wantText: "prefixed `-`"},
		{dir: "plus-prefix", wantFail: true, wantText: "prefixed `+`"},

		{dir: "or-true", wantFail: true, wantText: "|| true"},
		{dir: "semicolon-true", wantFail: true, wantText: "; true"},

		// make runs the LAST definition while a reader — and any text-based
		// check — sees the first.
		{dir: "duplicate-target", wantFail: true, wantText: "defined 2 times"},
		{dir: "conditional-target", wantFail: true, wantText: "conditional"},

		{dir: "missing-target", wantFail: true, wantText: "could not be established"},
	}
	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			out, code := run(t, "make-integrity-guard.sh",
				"--root", filepath.Join("scripts", "testdata", "makeguard", tc.dir),
				"--targets", "ci")
			failed := code != 0
			if failed != tc.wantFail {
				t.Fatalf("exit %d (failed=%v), want failed=%v.\n%s", code, failed, tc.wantFail, out)
			}
			if tc.wantText != "" && !strings.Contains(strings.ToLower(out), strings.ToLower(tc.wantText)) {
				t.Fatalf("the failure does not mention %q, so an author would not know what to fix:\n%s", tc.wantText, out)
			}
		})
	}
}

// The guard must pass on THIS repository's own Makefile, or every fixture above
// is checking a program the gate does not actually run against anything real.
//
// This test is also the third reading of the control: the workflow anchor is
// outside make, `make ci-guard` runs it for local parity, and this runs it from
// the ordinary suite — which the `build-test` lane invokes DIRECTLY, without
// make. So a Makefile neutered badly enough to disarm the other two still turns
// this red.
func TestMakeIntegrityGuardPassesOnTheRealMakefile(t *testing.T) {
	out, code := run(t, "make-integrity-guard.sh")
	if code != 0 {
		t.Fatalf("the make-integrity guard fails on this repository's own Makefile:\n%s", out)
	}
	for _, want := range []string{
		"resolves SHELL to the approved",
		"resolves .SHELLFLAGS to the approved",
		"MAKEFLAGS carries nothing beyond",
		"no duplicate-definition override",
		"defined exactly once",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the guard did not report on %q; a check that silently stopped running prints nothing:\n%s", want, out)
		}
	}
	// The closure must really have expanded: `ci` has no recipe of its own, and
	// it is `test-race`'s recipe that runs the tests.
	if !strings.Contains(out, "gate target `test-race` is defined exactly once") {
		t.Errorf("the guard did not reach `test-race` through `ci`'s prerequisites, so the recipe that "+
			"actually runs the tests was never scanned:\n%s", out)
	}
}

// Every fixture directory under scripts/testdata/guard/ must be named by a case
// above. Without this, a check can be removed by ORPHANING its fixture: the
// directory stays in the tree, looks like coverage in a diff, and nothing runs
// it. scripts/imagescan_test.go has the same guard over its own fixtures.
func TestEveryGuardFixtureIsExercised(t *testing.T) {
	root := repoRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, "scripts", "testdata", "guard"))
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(root, "scripts", "scripts_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !strings.Contains(string(src), `dir: "`+e.Name()+`"`) {
			t.Errorf("scripts/testdata/guard/%s is a fixture no test case names, so whatever it "+
				"exists to prove is not being proved", e.Name())
		}
	}
}

// ---------------------------------------------------------------------------
// go-test-report — a `go test` lane that corroborates what it ran
// ---------------------------------------------------------------------------

// `go test ./...` exits 0 when it ran nothing: with every *_test.go moved aside
// it reports `[no test files]` per package and succeeds (measured at 5eb2829,
// PR#6 VERIFY, FINDING 3). And a non-verbose `go test` prints nothing at all
// for a SKIPPED test, so no lane here could corroborate a skip count (PR#7
// VERIFY, § 1). Each fixture below is one way a green lane can mean nothing.
func TestGoTestReportFixtures(t *testing.T) {
	cases := []struct {
		dir      string
		suite    string
		floors   string
		wantFail bool
		wantText string
	}{
		// The control. If this is not 0, every red below proves nothing.
		{dir: "good", suite: "unit", wantFail: false},

		// THE defect this program exists for.
		{dir: "zero-tests", suite: "unit", wantFail: true, wantText: "the recorded floor for 'unit' is 4"},

		// A skip is not a pass (AGENTS.md), and CI could not previously see one.
		{dir: "skipped", suite: "unit", wantFail: true, wantText: "skipped:"},
		// …unless it is named, with a reason, in a reviewed diff.
		{dir: "skipped", suite: "unit", floors: "floors-allow-skip.json", wantFail: false, wantText: "allowed skip"},

		{dir: "failed", suite: "unit", wantFail: true, wantText: "planted failure"},
		// A package that failed to BUILD has no failing test to name it.
		{dir: "package-failed", suite: "unit", wantFail: true, wantText: "failed with no failing test"},
		// The runner failed and the stream does not explain why: not a pass.
		{dir: "unexplained-exit", suite: "unit", wantFail: true, wantText: "no test or package event reports a failure"},
		// A truncated stream is a stream that cannot be judged.
		{dir: "truncated", suite: "unit", wantFail: true, wantText: "are not json events"},
		{dir: "empty", suite: "unit", wantFail: true, wantText: "no `go test -json` events"},

		// The integration suite is a SUPERSET of the unit suite, so a
		// whole-suite floor says little about the integration tests themselves.
		{dir: "integration-good", suite: "integration", wantFail: false},
		{dir: "integration-package-emptied", suite: "integration", wantFail: true,
			wantText: "internal/integration executed 0 test(s)"},
	}
	for _, tc := range cases {
		name := tc.dir + "/" + tc.suite
		if tc.floors != "" {
			name += "/" + tc.floors
		}
		t.Run(name, func(t *testing.T) {
			base := filepath.Join("scripts", "testdata", "gotest")
			floors := tc.floors
			if floors == "" {
				floors = "floors.json"
			}
			out, code := runPython(t, "go-test-report.py",
				"--events", filepath.Join(base, tc.dir, "events.json"),
				"--suite", tc.suite,
				"--floors", filepath.Join(base, floors),
				"--go-exit-file", filepath.Join(base, tc.dir, "go-exit.txt"))
			failed := code != 0
			if failed != tc.wantFail {
				t.Fatalf("exit %d (failed=%v), want failed=%v.\n%s", code, failed, tc.wantFail, out)
			}
			if tc.wantText != "" && !strings.Contains(strings.ToLower(out), tc.wantText) {
				t.Fatalf("the output does not mention %q:\n%s", tc.wantText, out)
			}
		})
	}
}

// A lane that does not record `go test`'s own exit code cannot judge it, so
// leaving the flag off is a failure rather than a silently weaker report.
func TestGoTestReportRefusesToRunWithoutTheGoExitCode(t *testing.T) {
	base := filepath.Join("scripts", "testdata", "gotest")
	out, code := runPython(t, "go-test-report.py",
		"--events", filepath.Join(base, "good", "events.json"),
		"--suite", "unit",
		"--floors", filepath.Join(base, "floors.json"))
	if code == 0 {
		t.Fatalf("exit 0 with no --go-exit-file; `go test`'s own failure would go unread:\n%s", out)
	}
	if !strings.Contains(out, "--go-exit-file was not given") {
		t.Fatalf("the failure does not name the missing input:\n%s", out)
	}
}

// A suite with no recorded floor, or a floor of zero, would pass having run
// nothing — which is the condition this program exists to refuse.
func TestGoTestReportRefusesASuiteWithNoFloor(t *testing.T) {
	base := filepath.Join("scripts", "testdata", "gotest")
	for _, suite := range []string{"nofloor", "not-a-suite"} {
		out, code := runPython(t, "go-test-report.py",
			"--events", filepath.Join(base, "good", "events.json"),
			"--suite", suite,
			"--floors", filepath.Join(base, "floors.json"),
			"--go-exit-file", filepath.Join(base, "good", "go-exit.txt"))
		if code == 0 {
			t.Errorf("suite %q has no usable floor and the report exited 0:\n%s", suite, out)
		}
	}
}

// The floors the REPOSITORY's own lanes are held to must parse, name both
// suites, and carry no skip nobody reviewed. A floor file that drifted out of
// shape would make every lane above check nothing.
func TestTheRepositoryFloorsAreUsable(t *testing.T) {
	root := repoRoot(t)
	blob, err := os.ReadFile(filepath.Join(root, "scripts", "test-floors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Suites map[string]struct {
			MinTests        int               `json:"min_tests"`
			MinPackageTests map[string]int    `json:"min_package_tests"`
			AllowedSkips    map[string]string `json:"allowed_skips"`
		} `json:"suites"`
	}
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatalf("scripts/test-floors.json is not valid JSON: %v", err)
	}
	for _, want := range []string{"unit", "integration"} {
		s, ok := doc.Suites[want]
		if !ok {
			t.Fatalf("scripts/test-floors.json has no floor for the %q suite, so that lane's "+
				"report would refuse to run", want)
		}
		if s.MinTests < 1 {
			t.Errorf("suite %q has min_tests=%d; a floor of zero is not a floor", want, s.MinTests)
		}
		for name, reason := range s.AllowedSkips {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("suite %q allows skip %q with no reason. A skip nobody justified is a "+
					"skipped required test, which is not a pass (AGENTS.md)", want, name)
			}
		}
	}
	// The integration suite is a superset of the unit suite, so without a
	// per-package floor its whole-suite number says nothing about the
	// integration tests themselves.
	if len(doc.Suites["integration"].MinPackageTests) == 0 {
		t.Error("the integration suite has no min_package_tests, so emptying internal/integration " +
			"would still clear its whole-suite floor")
	}
}

// ---------------------------------------------------------------------------
// assert-runtime-image
// ---------------------------------------------------------------------------

// The three image assertions used to be inline in docker-build.yml, and all
// three PASSED when `docker run` itself failed: two carried `|| true` around
// the docker invocation (whose in-container script already ended `true`, so the
// `|| true` was load-bearing ONLY on a docker failure), and the third was
// `if docker run … test -e /src; then`, which takes its else branch — printing
// "no /src" — on any docker failure (PR#7 VERIFY, FINDING 1). The lane reported
// the toolchain absent WITHOUT HAVING LOOKED.
//
// `$DOCKER` is injectable precisely so that case can be demonstrated: there is
// no other way to produce a `docker run` failure on demand in a test.
func TestAssertRuntimeImageFixtures(t *testing.T) {
	cases := []struct {
		stub     string
		wantFail bool
		wantText string
	}{
		// The control: a correct image.
		{stub: "clean", wantFail: false, wantText: "no build tooling on path"},
		// THE finding. A failed `docker run` is a FAILED ASSERTION, never a
		// clean image — nothing looked inside it.
		{stub: "broken", wantFail: true, wantText: "the container did not run"},
		// And the assertions still catch what they were always for.
		{stub: "has-toolchain", wantFail: true, wantText: "carries build tooling"},
		{stub: "has-src", wantFail: true, wantText: "source tree is still in the runtime image"},
		// For `test -e /src`, 0 and 1 are the verdicts; anything else means the
		// command did not run and must not be read as "absent".
		{stub: "weird-exit", wantFail: true, wantText: "the container did not run while checking /src"},
	}
	root := repoRoot(t)
	for _, tc := range cases {
		t.Run(tc.stub, func(t *testing.T) {
			cmd := exec.Command(filepath.Join(root, "scripts", "assert-runtime-image.sh"), "vizra-core:ci")
			cmd.Dir = root
			cmd.Env = append(os.Environ(),
				"DOCKER="+filepath.Join(root, "scripts", "testdata", "fakedocker", tc.stub))
			out, err := cmd.CombinedOutput()
			code := 0
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatalf("assert-runtime-image.sh: %v\n%s", err, out)
			}
			failed := code != 0
			if failed != tc.wantFail {
				t.Fatalf("exit %d (failed=%v), want failed=%v.\n%s", code, failed, tc.wantFail, out)
			}
			if !strings.Contains(strings.ToLower(string(out)), tc.wantText) {
				t.Fatalf("the output does not mention %q:\n%s", tc.wantText, out)
			}
		})
	}
}

// A denylist that is empty, comments-only or missing would make the toolchain
// assertion pass having checked nothing.
func TestAssertRuntimeImageRefusesAVacuousDenylist(t *testing.T) {
	root := repoRoot(t)
	dir := t.TempDir()
	cases := map[string]string{
		"empty.txt":    "",
		"comments.txt": "# only comments here\n#\n",
	}
	for name, body := range cases {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, list := range []string{"empty.txt", "comments.txt", "absent.txt"} {
		cmd := exec.Command(filepath.Join(root, "scripts", "assert-runtime-image.sh"),
			"vizra-core:ci", filepath.Join(dir, list))
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"DOCKER="+filepath.Join(root, "scripts", "testdata", "fakedocker", "clean"))
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("denylist %q was accepted; the toolchain assertion would pass vacuously:\n%s", list, out)
		}
	}
}

// ---------------------------------------------------------------------------
// lint-imports
// ---------------------------------------------------------------------------

func TestLintImportsPassesOnTheRealTree(t *testing.T) {
	out, code := run(t, "lint-imports.sh")
	if code != 0 {
		t.Fatalf("lint-imports fails on the repository's own tree:\n%s", out)
	}
	for _, want := range []string{"Echo", "package-global", "storage root", "decoder"} {
		if !strings.Contains(out, want) {
			t.Errorf("lint-imports did not report on %q; a check that silently stopped running still prints nothing:\n%s", want, out)
		}
	}
}
