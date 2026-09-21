package scripts_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
		{dir: "no-direct-test-lane", wantFail: true, wantText: "every test invocation goes through"},

		{dir: "bad-runner", wantFail: true, wantText: "runner"},
		{dir: "unpinned-action", wantFail: true, wantText: "pinned"},
		{dir: "missing-job", wantFail: true, wantText: "matches no job"},
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
		"runs the suite directly, without make",
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
