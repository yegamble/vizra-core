package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// image-scan-verdict.py
//
// A vulnerability scan is the one lane whose GREEN looks exactly like its NOT
// HAVING RUN. Every case below is a way an image-scan lane can report success
// while nothing was scanned, and each one is a separate fixture so a change
// that removes one check turns exactly one named test red rather than the
// suite. See scripts/image-scan-verdict.py.
//
// The exit-code vocabulary is load-bearing and is asserted, not merely
// "non-zero": 1 is "there are findings" and 3 is "THERE WAS NO VALID SCAN".
// Collapsing them is how a lane that never ran gets read as a clean bill of
// health by the next person to look at it.
// ---------------------------------------------------------------------------

const (
	verdictClean       = 0
	verdictFindings    = 1
	verdictNoValidScan = 3
)

func verdict(t *testing.T, fixture string, extra ...string) (string, int) {
	t.Helper()
	root := repoRoot(t)
	dir := filepath.Join(root, "scripts", "testdata", "imagescan", fixture)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("fixture %q does not exist: %v", fixture, err)
	}
	args := append([]string{
		filepath.Join(root, "scripts", "image-scan-verdict.py"),
		"--report", filepath.Join(dir, "trivy-image.json"),
		"--scanner-exit-code-file", filepath.Join(dir, "trivy-exit-code.txt"),
		"--image-ref", "vizra-core:scan",
	}, extra...)
	cmd := exec.Command("python3", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running image-scan-verdict.py: %v\n%s", err, out)
	}
	return string(out), code
}

// The control: a real, complete report of the right image with nothing at or
// above the failing severities. If this is not 0, every red below proves
// nothing.
func TestImageScanVerdictPassesAValidCleanScan(t *testing.T) {
	out, code := verdict(t, "good")
	if code != verdictClean {
		t.Fatalf("a valid clean scan exited %d, want %d:\n%s", code, verdictClean, out)
	}
	for _, want := range []string{
		"the scanner exited 0",
		"Trivy schema",
		"the report describes vizra-core:scan",
		"identified the OS: debian",
		"result section(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the verdict did not report on %q; a check that silently stopped "+
				"running prints nothing:\n%s", want, out)
		}
	}
}

func TestImageScanVerdictFailsOnFindings(t *testing.T) {
	out, code := verdict(t, "findings")
	if code != verdictFindings {
		t.Fatalf("a CRITICAL finding exited %d, want %d:\n%s", code, verdictFindings, out)
	}
	if !strings.Contains(out, "CVE-2026-9999") {
		t.Errorf("the verdict does not name the finding:\n%s", out)
	}
}

// The severity threshold is a parameter, and lowering it must change the
// verdict — otherwise the counting is decorative.
func TestImageScanVerdictHonoursTheSeverityThreshold(t *testing.T) {
	if _, code := verdict(t, "good", "--fail-on", "LOW,MEDIUM,HIGH,CRITICAL"); code != verdictFindings {
		t.Errorf("the clean fixture carries LOW and MEDIUM findings but --fail-on LOW "+
			"exited %d; the threshold is not being applied", code)
	}
	if _, code := verdict(t, "findings", "--fail-on", "CRITICAL"); code != verdictFindings {
		t.Errorf("a CRITICAL finding survived --fail-on CRITICAL, exit %d", code)
	}
}

// An EMPTY --fail-on names no failing severity, so every finding is compared
// against an empty set and the one script whose whole thesis is "a scan lane
// must not pass vacuously" passed vacuously over a CRITICAL. Measured at
// 5eb2829 against this repository's own `findings` fixture: `--fail-on ”` and
// `--fail-on ','` both exited 0, printing "found nothing at or above []"
// (PR#7 VERIFY, FINDING 2). It is reachable by a one-character edit to
// image-scan.yml.
//
// It reuses the existing `findings` fixture, so TestEveryImageScanFixtureIsExercised
// stays satisfied without a new fixture directory.
func TestImageScanVerdictRefusesAThresholdThatCannotFail(t *testing.T) {
	for _, arg := range []string{"", ",", " , ", ",,"} {
		out, code := verdict(t, "findings", "--fail-on", arg)
		if code != verdictNoValidScan {
			t.Errorf("--fail-on %q over a report carrying a CRITICAL exited %d, want %d "+
				"(THERE WAS NO VALID SCAN). A threshold that cannot fail is a vacuous pass, "+
				"not a clean bill of health.\n%s", arg, code, verdictNoValidScan, out)
		}
		if !strings.Contains(out, "--fail-on is empty") {
			t.Errorf("--fail-on %q: the refusal does not name the reason:\n%s", arg, out)
		}
	}
	// An unrecognised severity is still refused, and still distinct from empty.
	if out, code := verdict(t, "findings", "--fail-on", "HIGH,SEVERE"); code != verdictNoValidScan {
		t.Errorf("--fail-on HIGH,SEVERE exited %d, want %d\n%s", code, verdictNoValidScan, out)
	}
	// And the real threshold the lane uses still reports FINDINGS, not exit 3 —
	// or the refusal above would have broken the control it protects.
	if out, code := verdict(t, "findings", "--fail-on", "HIGH,CRITICAL"); code != verdictFindings {
		t.Errorf("the lane's own --fail-on HIGH,CRITICAL exited %d, want %d\n%s", code, verdictFindings, out)
	}
}

// ---------------------------------------------------------------------------
// Every way the lane can pass vacuously. Each is exit 3, NOT exit 1: "nothing
// was scanned" must never be reported in the vocabulary of "nothing was found".
// ---------------------------------------------------------------------------

func TestImageScanVerdictRefusesEveryVacuousPass(t *testing.T) {
	cases := []struct {
		fixture string
		because string
		says    string
	}{
		{"scanner-error", "trivy exited 1: a scanner that errored reports no findings", "THE SCANNER FAILED"},
		{"scanner-crashed", "trivy was killed (137)", "THE SCANNER FAILED"},
		{"missing-exit-code", "nothing recorded the scanner's exit code, so nothing can judge it", "does not exist"},
		{"blank-exit-code", "the recorded exit code is blank", "was not recorded"},
		{"missing-report", "the scanner produced no report", "does not exist"},
		{"empty-report", "the report is empty", "is empty"},
		{"malformed-report", "the report is not JSON", "not valid JSON"},
		{"no-schema", "the file was not written by trivy", "no SchemaVersion"},
		{"wrong-image", "the report is about a different image — a stale file from an earlier run", "not 'vizra-core:scan'"},
		{"wrong-artifact-type", "a filesystem scan does not cover the layers that ship", "not a container image"},
		{"no-os-detected", "with no OS family trivy reports zero OS vulnerabilities and exits 0", "did not identify the image's operating system"},
		{"null-results", "the scanner analysed nothing", "Results is null"},
		{"empty-results", "no analysed target", "result section(s), fewer than"},
		{"no-os-pkgs", "the distribution package layer was never covered", "Class 'os-pkgs'"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			out, code := verdict(t, tc.fixture)
			if code != verdictNoValidScan {
				t.Fatalf("%s: exited %d, want %d (%s)\n%s", tc.fixture, code, verdictNoValidScan, tc.because, out)
			}
			if !strings.Contains(out, tc.says) {
				t.Errorf("%s: the message does not name the reason %q:\n%s", tc.fixture, tc.says, out)
			}
			if !strings.Contains(out, "NOT a statement that the image is clean") {
				t.Errorf("%s: the verdict does not say it is not a clean bill of health:\n%s", tc.fixture, out)
			}
		})
	}
}

// Every fixture directory must be exercised. A fixture that stops being read is
// a check that stopped running, and the suite would stay green.
func TestEveryImageScanFixtureIsExercised(t *testing.T) {
	root := repoRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, "scripts", "testdata", "imagescan"))
	if err != nil {
		t.Fatal(err)
	}
	named := map[string]bool{"good": true, "findings": true}
	for _, tc := range []string{
		"scanner-error", "scanner-crashed", "missing-exit-code", "blank-exit-code",
		"missing-report", "empty-report", "malformed-report", "no-schema",
		"wrong-image", "wrong-artifact-type", "no-os-detected",
		"null-results", "empty-results", "no-os-pkgs",
	} {
		named[tc] = true
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !named[e.Name()] {
			t.Errorf("fixture %q exists but no test reads it", e.Name())
		}
	}
	if len(named) != countDirs(entries) {
		t.Errorf("%d fixtures are named by tests but %d exist on disk", len(named), countDirs(entries))
	}
}

func countDirs(entries []os.DirEntry) int {
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			n++
		}
	}
	return n
}
