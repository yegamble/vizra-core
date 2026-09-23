//go:build integration && unix

package integration

import (
	"strings"
	"testing"

	"github.com/yegamble/vizra-core/internal/testtmp"
)

// TestTheIntegrationTestsLeaveNoTemporaryEntry is sentinel S-0001. It runs
// THIS package's test binary as a child with a TMPDIR only this test owns, so
// nothing another process writes to the shared temporary directory can make it
// pass or fail: a normal run of TestTheEntryPointsBuild (which builds the
// ~74 MB of binaries) must leave no `vizra-*` entry behind.
//
// On main at 3994893 this is red: binaries() created `vizra-healthcheck-bin-*`
// with os.MkdirTemp and nothing ever removed it. That a KILLED run's root is
// swept by the next run is the same internal/testtmp code this package's
// TestMain calls, demonstrated in internal/fixtures
// (TestTheFixturesTestsLeaveNoTemporaryEntry) and internal/testtmp; it is not
// repeated here, because each child run here rebuilds the entry points.
func TestTheIntegrationTestsLeaveNoTemporaryEntry(t *testing.T) {
	bin := testtmp.BuildTestBinary(t, "integration")
	tmp := t.TempDir()

	code, out := testtmp.RunChild(t, bin, tmp, "^TestTheEntryPointsBuild$")
	if code != 0 || !strings.Contains(out, "PASS") {
		t.Fatalf("the child run failed (exit %d), so its temporary files prove nothing:\n%s", code, out)
	}
	if left := testtmp.VizraEntries(t, tmp); len(left) != 0 {
		t.Fatalf("a normal run of the integration tests left %v in its TMPDIR", left)
	}
}
