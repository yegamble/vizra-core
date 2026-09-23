//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTheEntryPointsBuild builds the three shipped entry points the way every
// process-level test here does (binaries), and checks each is an executable
// file. It needs no database, so the leak test in tmpleak_test.go can run it
// as a child.
func TestTheEntryPointsBuild(t *testing.T) {
	dir := binaries(t)
	for _, name := range []string{"vizra", "vizra-api", "vizra-worker"} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s was not built: %v", name, err)
		}
		if !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s is not an executable file (mode %v)", name, st.Mode())
		}
	}
}
