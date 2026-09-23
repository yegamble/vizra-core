//go:build integration

package integration

import (
	"os"
	"testing"

	"github.com/yegamble/vizra-core/internal/testtmp"
)

// TestMain runs the package inside ONE temporary root (internal/testtmp):
// every os.MkdirTemp and t.TempDir here — and the ~74 MB of entry-point
// binaries healthcheck_test.go builds — lands in it, it is removed when the run
// ends, and a run killed before that is swept by the next run of this package
// (sentinel S-0001: before this, every run left a `vizra-healthcheck-bin-*`
// directory behind).
func TestMain(m *testing.M) {
	os.Exit(testtmp.Run(m, "integration"))
}
