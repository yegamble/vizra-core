//go:build unix

package fixtures

import (
	"strings"
	"testing"
	"time"

	"github.com/yegamble/vizra-core/internal/testtmp"
)

// TestTheFixturesTestsLeaveNoTemporaryEntry is sentinel S-0016's leak half. It
// runs THIS package's test binary as a child with a TMPDIR only this test
// owns, so nothing another process writes to the shared temporary directory
// can make it pass or fail:
//
//  1. a normal run of a test that generates the shared corpus leaves no
//     `vizra-*` entry behind;
//  2. a run KILLED mid-generation (SIGKILL: what go test's -timeout amounts
//     to — no deferred call, no t.Cleanup, no TestMain epilogue runs) may leave
//     one, and the next run of this package must remove it.
//
// On main at 3994893 step 2 is red: the killed run's
// `vizra-fixtures-shared-*` corpus stays behind for ever.
func TestTheFixturesTestsLeaveNoTemporaryEntry(t *testing.T) {
	bin := testtmp.BuildTestBinary(t)
	tmp := t.TempDir()

	code, out := testtmp.RunChild(t, bin, tmp, "^TestEveryFixtureIsWhatItClaimsToBe$")
	if code != 0 || !strings.Contains(out, "PASS") {
		t.Fatalf("the child run failed (exit %d), so its temporary files prove nothing:\n%s", code, out)
	}
	if left := testtmp.VizraEntries(t, tmp); len(left) != 0 {
		t.Fatalf("a normal run of the fixtures tests left %v in its TMPDIR", left)
	}

	seen := testtmp.KillChildWhen(t, bin, tmp, "^TestEveryFixtureIsWhatItClaimsToBe$", "vizra-fixtures-shared-", 2*time.Minute)
	t.Logf("killed the child once %s existed", strings.TrimPrefix(seen, tmp))
	if left := testtmp.VizraEntries(t, tmp); len(left) == 0 {
		t.Fatal("the killed run left nothing behind, so this step demonstrates nothing about the sweep")
	} else {
		t.Logf("the killed run left %v", left)
	}

	code, out = testtmp.RunChild(t, bin, tmp, "^$")
	if code != 0 {
		t.Fatalf("the follow-up run failed (exit %d):\n%s", code, out)
	}
	if left := testtmp.VizraEntries(t, tmp); len(left) != 0 {
		t.Fatalf("the next run of this package did not remove what a killed run left: %v", left)
	}
}
