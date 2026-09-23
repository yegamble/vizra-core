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

// Every one of these is a way to neuter a required lane. Since sweep B1 round 2
// the control is DEFAULT-DENY on the SHAPE of the two kinds of step that carry
// the gate, not a blacklist of shell spellings: a verifier found thirteen
// spellings the flag-parsing version could not see (PR#9 VERIFY, § 3b), and a
// blacklist over arbitrary shell cannot be exhaustive.
//
//	check 8b  a step mentioning `make` must be BYTE-EQUAL to a body in
//	          .github/pinned-steps.yml, carry no key but name/run/id, and be
//	          IMMEDIATELY preceded by the anchor
//	check 8c  the lane must actually RUN its required invocations — the
//	          positive half, which closes indirection (`$M -i ci`) without
//	          parsing any shell, because it does not care what replaced them
//	check 9   the direct test steps are pinned the same way
//	check 10  a lane that checks out records which tree it stood in
func TestCIRequiredGuardFixtures(t *testing.T) {
	cases := []struct {
		dir      string
		wantFail bool
		wantText string
	}{
		{dir: "good", wantFail: false},
		{dir: "floor-deleted", wantFail: true, wantText: "is missing"},
		{dir: "fixtures-floor-deleted", wantFail: true, wantText: "'fixtures' is missing"},
		{dir: "floor-commented", wantFail: true, wantText: "commented out"},
		{dir: "missing-job", wantFail: true, wantText: "matches no job"},
		{dir: "coe-bare", wantFail: true, wantText: "continue-on-error"},
		{dir: "coe-quoted", wantFail: true, wantText: "continue-on-error"},
		{dir: "coe-capitalised", wantFail: true, wantText: "continue-on-error"},
		{dir: "coe-expression", wantFail: true, wantText: "continue-on-error"},
		{dir: "coe-underscore", wantFail: true, wantText: "continue-on-error"},
		{dir: "coe-step", wantFail: true, wantText: "carries ['continue-on-error']"},
		{dir: "not-on-pull-request", wantFail: true, wantText: "pull_request"},
		{dir: "bad-runner", wantFail: true, wantText: "runner"},
		{dir: "unpinned-action", wantFail: true, wantText: "pinned"},
		{dir: "anchor-missing", wantFail: true, wantText: "not immediately preceded by"},
		{dir: "anchor-after-make", wantFail: true, wantText: "not immediately preceded by"},
		{dir: "anchor-conditional", wantFail: true, wantText: "the anchor step before 'make ci' carries ['if']"},
		{dir: "anchor-continue-on-error", wantFail: true, wantText: "continue-on-error"},
		{dir: "anchor-not-adjacent", wantFail: true, wantText: "not immediately preceded by"},
		{dir: "needs-leg-unanchored", wantFail: true, wantText: "needs:cache-matrix-leg"},
		{dir: "make-in-comment-needs-anchor", wantFail: true, wantText: "with no make-integrity-guard step before it"},
		{dir: "defaults-shell-workflow", wantFail: true, wantText: "defaults.run"},
		{dir: "defaults-shell-job", wantFail: true, wantText: "defaults.run"},
		{dir: "job-container", wantFail: true, wantText: "container:"},
		{dir: "service-image-unpinned", wantFail: true, wantText: "is not digest-pinned"},
		{dir: "job-env-makeflags", wantFail: true, wantText: "job-level env sets makeflags"},
		{dir: "job-env-makefiles", wantFail: true, wantText: "job-level env sets makefiles"},
		{dir: "job-env-path", wantFail: true, wantText: "job-level env sets path"},
		{dir: "job-env-bash-env", wantFail: true, wantText: "job-level env sets bash_env"},
		{dir: "workflow-env-makeflags", wantFail: true, wantText: "workflow-level env sets gnumakeflags"},
		{dir: "make-flag-override", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "make-var-override", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "make-var-override-after-target", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "make-env-prefix", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "make-env-command", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "make-flag-long-abbrev", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "make-flag-cluster", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "make-file-elsewhere", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "make-directory", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "make-wrapped-shell", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "make-jobs-optarg-swallow", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "make-load-optarg-swallow", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "make-export-makeflags", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "make-export-gnumakeflags", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "make-path-shadow", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "make-shell-function", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "make-backtick", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "make-multiline-chain", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "make-line-continuation", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "untokenisable-run", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "make-step-conditional", wantFail: true, wantText: "carries ['if']"},
		{dir: "make-step-working-directory", wantFail: true, wantText: "carries ['working-directory']"},
		{dir: "make-step-custom-shell", wantFail: true, wantText: "carries ['shell']"},
		{dir: "make-step-env", wantFail: true, wantText: "carries ['env']"},
		{dir: "make-step-timeout", wantFail: true, wantText: "carries ['timeout-minutes']"},
		{dir: "make-indirect-variable", wantFail: true, wantText: "does not run required invocation(s) ['make ci']"},
		{dir: "make-indirect-default", wantFail: true, wantText: "does not run required invocation(s) ['make ci']"},
		{dir: "no-direct-test-lane", wantFail: true, wantText: "runs the unit suite directly from a pinned body"},
		{dir: "no-direct-integration-lane", wantFail: true, wantText: "runs the integration suite directly from a pinned body"},
		{dir: "direct-lane-without-report", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "direct-lane-without-exit-rc", wantFail: true, wantText: "not byte-equal to any entry"},
		{dir: "direct-lane-conditional", wantFail: true, wantText: "direct test step"},
		{dir: "no-provenance", wantFail: true, wantText: "has no scripts/provenance.sh step"},
		{dir: "provenance-conditional", wantFail: true, wantText: "provenance step conditional"},
		{dir: "required-not-floor", wantFail: true, wantText: "'extra-lane'"},
		// Fix round 2, R-1: the anchor is PINNED, not recognised by substring.
		// Each of these satisfied round 2's "immediately preceded by the anchor"
		// while the write reached make and not the guard (PR#9 re-verification,
		// N1–N5). Each is refused as a look-alike AND fails adjacency.
		{dir: "anchor-compound-github-env", wantFail: true, wantText: "names make-integrity-guard but its `run:` is not byte-equal to the pinned anchor"},
		{dir: "anchor-then-lookalike", wantFail: true, wantText: "names make-integrity-guard but its `run:` is not byte-equal to the pinned anchor"},
		{dir: "anchor-fake-noop", wantFail: true, wantText: "names make-integrity-guard but its `run:` is not byte-equal to the pinned anchor"},
		{dir: "anchor-compound-github-path", wantFail: true, wantText: "names make-integrity-guard but its `run:` is not byte-equal to the pinned anchor"},
		{dir: "anchor-compound-makelevel", wantFail: true, wantText: "names make-integrity-guard but its `run:` is not byte-equal to the pinned anchor"},
		// The round-2 anchor form, without --workflow, would run the lenient mode.
		{dir: "anchor-without-workflow-flag", wantFail: true, wantText: "not byte-equal to the pinned anchor"},
		// R-2: make's own recipe variables are refused at job and workflow level.
		{dir: "job-env-makelevel", wantFail: true, wantText: "job-level env sets makelevel"},
		{dir: "workflow-env-makeoverrides", wantFail: true, wantText: "workflow-level env sets makeoverrides"},
		// R-4: a duplicate key is refused, so the guard never reads a different
		// value than the one a reviewer sees first.
		{dir: "duplicate-run-key", wantFail: true, wantText: "duplicate key 'run'"},
		// Found while fixing round 2: `GO ?= go` means the ENVIRONMENT wins, so
		// `GO=true` turns `make test-race` from exit 2 into exit 0.
		{dir: "job-env-makefile-override", wantFail: true, wantText: "which the makefile takes from the environment"},
	}
	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			// Each fixture is independent and only READ, and each spawns a python or
			// shell process; serially they doubled this package's time under -race.
			t.Parallel()
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
		// Checks 8b, 8c and 10, reported rather than merely not-failed. A check
		// that silently stopped running prints nothing at all.
		"are byte-equal to a pinned body, carry no key but name/run/id, and each is immediately preceded by the anchor",
		"runs all 3 required invocation(s) for 'build-test'",
		"runs scripts/provenance.sh after checking out",
		// Check 11 (sweep B5), against the REAL pin and the real Makefile.
		"pinned-makefiles.yml pins 1 makefile(s) (Makefile), covers the Makefile, and every digest matches the tree",
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
		dir        string
		wantFail   bool
		notInvoked bool
		wantText   string
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

		// Sweep B5 — THE DIGEST PIN (chair ruling, tick 132, on the 2026-09-23 desk
		// review, FINDING 4). make EVALUATES a makefile while reading it, so every
		// fixture above carries its own .github/pinned-makefiles.yml: the checks
		// above run only on pinned bytes. Each case below is refused BEFORE make is
		// started, and says so; makefiledigest_test.go proves that line honest with
		// a process recorder.
		{dir: "pin-missing", wantFail: true, notInvoked: true, wantText: "pinned-makefiles.yml does not exist"},
		{dir: "pin-empty", wantFail: true, notInvoked: true, wantText: "pins no file"},
		{dir: "pin-malformed", wantFail: true, notInvoked: true, wantText: "not a `  <path>: <64 lowercase hex sha256>` entry"},
		{dir: "pin-without-makefile", wantFail: true, notInvoked: true, wantText: "does not pin `Makefile`"},
		{dir: "digest-mismatch", wantFail: true, notInvoked: true, wantText: "Makefile: sha256"},
		{dir: "include-unpinned", wantFail: true, notInvoked: true, wantText: "make would read inc.mk, which has no entry"},
		{dir: "include-computed", wantFail: true, notInvoked: true, wantText: "names a file make COMPUTES"},
		{dir: "include-outside", wantFail: true, notInvoked: true, wantText: "reads a file outside the repository"},
		{dir: "eval-in-pinned-bytes", wantFail: true, notInvoked: true, wantText: "can manufacture an include"},
		{dir: "stale-pin-entry", wantFail: true, notInvoked: true, wantText: "pins old.mk, which make would NOT read"},
		{dir: "gnumakefile-present", wantFail: true, notInvoked: true, wantText: "reads GNUmakefile INSTEAD of Makefile"},
		{dir: "makefile-symlink", wantFail: true, notInvoked: true, wantText: "is not a regular file"},
		// The positive control for includes: pinned includes are read, and make's
		// own MAKEFILE_LIST is corroborated against the static reading.
		{dir: "include-pinned-good", wantFail: false, wantText: "MAKEFILE_LIST ['Makefile', 'a.mk', 'b.mk'] is exactly the pinned set"},
	}
	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			// Each fixture is independent and only READ, and each spawns a python or
			// shell process; serially they doubled this package's time under -race.
			t.Parallel()
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
			if tc.notInvoked && !strings.Contains(out, "make was NOT invoked (0 make process(es) started)") {
				t.Fatalf("the refusal must come BEFORE make is started, and say so:\n%s", out)
			}
		})
	}
}

// The anchor's OWN environment, as the workflow invokes it (`--workflow`) and as
// `make ci-guard` invokes it (no flag). Round 2 chose the mode from MAKELEVEL's
// mere presence, which an earlier step can set through $GITHUB_ENV; the lenient
// path then passed -ki, n and --ign, and GNU Make 4.3 made a failing recipe exit 0
// under each (PR#9 re-verification, FINDING R-2). The mode is now chosen by the
// invocation, and the lenient path is an allowlist of what make itself exports —
// MEASURED on 4.3 and 3.81, recorded in make-integrity-guard.py.
func TestMakeIntegrityGuardEnvironment(t *testing.T) {
	cases := []struct {
		name     string
		workflow bool
		env      []string
		wantFail bool
		wantText string
	}{
		// --workflow (the pinned anchor): strict.
		{"workflow/control", true, nil, false, ""},
		{"workflow/MAKELEVEL=1 MAKEFLAGS=-ki", true, []string{"MAKELEVEL=1", "MAKEFLAGS=-ki"}, true, "makelevel"},
		{"workflow/MAKELEVEL=1 MAKEFLAGS=n", true, []string{"MAKELEVEL=1", "MAKEFLAGS=n"}, true, "makelevel"},
		{"workflow/MAKELEVEL= (empty) MAKEFLAGS=--ign", true, []string{"MAKELEVEL=", "MAKEFLAGS=--ign"}, true, "makelevel=''"},
		{"workflow/MAKELEVEL=1 GNUMAKEFLAGS=-ki", true, []string{"MAKELEVEL=1", "GNUMAKEFLAGS=-ki"}, true, "gnumakeflags"},
		{"workflow/MAKEFLAGS=-i", true, []string{"MAKEFLAGS=-i"}, true, "must be unset"},
		{"workflow/MAKEFLAGS=--no-such-flag", true, []string{"MAKEFLAGS=--no-such-flag"}, true, "must be unset"},
		{"workflow/MAKEFLAGS present but empty", true, []string{"MAKEFLAGS="}, true, "must be unset"},
		{"workflow/MAKE_RESTARTS", true, []string{"MAKE_RESTARTS=1"}, true, "make_restarts"},
		{"workflow/MAKEOVERRIDES", true, []string{"MAKEOVERRIDES=SHELL=/usr/bin/true"}, true, "makeoverrides"},
		{"workflow/MAKEFILES", true, []string{"MAKEFILES=/tmp/x.mk"}, true, "makefiles"},
		{"workflow/BASH_ENV", true, []string{"BASH_ENV=/tmp/fn.sh"}, true, "bash_env"},
		{"workflow/SHELL=/usr/bin/true", true, []string{"SHELL=/usr/bin/true"}, true, "not a usable shell"},
		// `?=` variables: the environment decides what the recipe runs.
		{"workflow/GO=true", true, []string{"GO=true"}, true, "the environment sets go='true'"},
		{"workflow/GOFLAGS", true, []string{"GOFLAGS=-run=^$"}, true, "goflags"},
		{"workflow/SQLC=true", true, []string{"SQLC=true"}, true, "sqlc='true'"},
		// no flag (`make ci-guard` parity): an ALLOWLIST of make's own exports.
		{"local/control", false, nil, false, ""},
		{"local/plain make", false, []string{"MAKELEVEL=1", "MAKEFLAGS="}, false, ""},
		{"local/make -j2 (4.3)", false, []string{"MAKELEVEL=1", "MAKEFLAGS= -j2 --jobserver-auth=3,4", "MFLAGS=-j2 --jobserver-auth=3,4"}, false, ""},
		{"local/make -j2 (3.81)", false, []string{"MAKELEVEL=1", "MAKEFLAGS= --jobserver-fds=3,4 -j", "MFLAGS=- --jobserver-fds=3,4 -j"}, false, ""},
		{"local/make -s -w", false, []string{"MAKELEVEL=2", "MAKEFLAGS=sw", "MFLAGS=-s -w"}, false, ""},
		{"local/MAKEFLAGS=-ki", false, []string{"MAKELEVEL=1", "MAKEFLAGS=-ki"}, true, "['-ki'] is not a flag make itself"},
		{"local/MAKEFLAGS=n", false, []string{"MAKELEVEL=1", "MAKEFLAGS=n"}, true, "['n'] is not a flag make itself"},
		{"local/MAKEFLAGS=--ign", false, []string{"MAKELEVEL=", "MAKEFLAGS=--ign"}, true, "['--ign'] is not a flag make itself"},
		{"local/GNUMAKEFLAGS=-ki", false, []string{"MAKELEVEL=1", "GNUMAKEFLAGS=-ki"}, true, "['-ki'] is not a flag make itself"},
		{"local/MAKEFLAGS=e", false, []string{"MAKELEVEL=1", "MAKEFLAGS=e"}, true, "['e'] is not a flag make itself"},
		// Locally a developer may pin their own go; this mode is not a control.
		{"local/GO=go", false, []string{"GO=go"}, false, ""},
	}
	root := repoRoot(t)
	strip := map[string]bool{"MAKEFLAGS": true, "GNUMAKEFLAGS": true, "MFLAGS": true, "MAKELEVEL": true,
		"MAKE_RESTARTS": true, "MAKEOVERRIDES": true, "MAKECMDGOALS": true, "MAKEFILES": true,
		"BASH_ENV": true, "ENV": true,
		"GO": true, "SQLC": true, "GOFLAGS": true, "RELEASE": true, "COMMIT": true, "BUILT_AT": true}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Start from a CLEAN environment: `make ci` runs this package under make,
			// so the inherited MAKELEVEL/MAKEFLAGS must not leak into a strict case.
			env := []string{}
			for _, kv := range os.Environ() {
				if !strip[strings.SplitN(kv, "=", 2)[0]] {
					env = append(env, kv)
				}
			}
			env = append(env, tc.env...)
			// The environment checks do not depend on which targets are resolved,
			// so one target keeps 30 runs of the resolver cheap under -race.
			args := []string{"--targets", "ci"}
			if tc.workflow {
				args = append(args, "--workflow")
			}
			cmd := exec.Command(filepath.Join(root, "scripts", "make-integrity-guard.sh"), args...)
			cmd.Dir = root
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			code := 0
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatalf("make-integrity-guard.sh: %v\n%s", err, out)
			}
			if (code != 0) != tc.wantFail {
				t.Fatalf("exit %d, want failed=%v\n%s", code, tc.wantFail, out)
			}
			if tc.wantText != "" && !strings.Contains(strings.ToLower(string(out)), tc.wantText) {
				t.Fatalf("the refusal does not mention %q:\n%s", tc.wantText, out)
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
		// Sweep B5: the digest pin was checked before make ran, and make's own
		// MAKEFILE_LIST agreed with the static reading of the pinned bytes.
		"make runs only on REVIEWED bytes",
		"MAKEFILE_LIST ['Makefile'] is exactly the pinned set",
		"only on the pinned bytes of Makefile",
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

		// PR#9 VERIFY, FINDING 7. A whole-suite floor does not detect ONE package
		// disappearing: with 1047 unit tests against a floor of 900, twelve of
		// fourteen packages fit inside the headroom. Here the emptied package's
		// tests are inside the suite headroom too, so the suite floor is met and
		// only the PER-PACKAGE floor makes it red.
		{dir: "unit-package-emptied", suite: "unit", floors: "floors-two-packages.json",
			wantFail: true, wantText: "internal/other executed 0 test(s)"},
		// And a package that RAN with no floor at all: nobody would notice its
		// absence next time, so it is refused until a floor is recorded for it.
		{dir: "package-with-no-floor", suite: "unit", wantFail: true,
			wantText: "executed tests with no recorded floor"},
	}
	for _, tc := range cases {
		name := tc.dir + "/" + tc.suite
		if tc.floors != "" {
			name += "/" + tc.floors
		}
		t.Run(name, func(t *testing.T) {
			// Each fixture is independent and only READ, and each spawns a python or
			// shell process; serially they doubled this package's time under -race.
			t.Parallel()
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
	for _, suite := range []string{"unit", "integration"} {
		if len(doc.Suites[suite].MinPackageTests) < 10 {
			t.Errorf("suite %q records per-package floors for only %d package(s). A whole-suite "+
				"floor does not detect ONE package disappearing — twelve of fourteen unit packages "+
				"fit inside the headroom (PR#9 VERIFY, FINDING 7) — so EVERY package carries one.",
				suite, len(doc.Suites[suite].MinPackageTests))
		}
		for pkg, floor := range doc.Suites[suite].MinPackageTests {
			if floor < 1 {
				t.Errorf("suite %q: package %q has a floor of %d; a floor of zero is not a floor",
					suite, pkg, floor)
			}
		}
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
		// PR#9 VERIFY, FINDING 5. `broken` fails EVERY probe including the middle
		// dpkg one, which had no outer `|| true`, so main's inline shape exits 125
		// against it — not 0, as my first D6 transcript claimed. This stub fails
		// only probes 1 and 3, the two whose results that shape swallowed, and is
		// what actually reproduces main's EXIT=0. The NEW script is red against it.
		{stub: "broken-probes-1-3", wantFail: true, wantText: "the container did not run"},
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
			// Each fixture is independent and only READ, and each spawns a python or
			// shell process; serially they doubled this package's time under -race.
			t.Parallel()
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
