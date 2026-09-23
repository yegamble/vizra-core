//go:build unix

package testtmp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// deadPID is the PID of a process that has exited and been reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.ProcessState.Pid()
	if alive(pid) {
		t.Fatalf("pid %d is alive again already; cannot construct a dead owner", pid)
	}
	return pid
}

func mkdir(t *testing.T, parent, name string) string {
	t.Helper()
	p := filepath.Join(parent, name)
	if err := os.MkdirAll(filepath.Join(p, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// Sweep removes a root whose process is gone, and nothing else: not a live
// process's root (a concurrent run), not another package's, not a name it does
// not own.
func TestSweepRemovesOnlyRootsOfDeadProcessesOfThisPackage(t *testing.T) {
	parent := t.TempDir()
	dead := deadPID(t)
	gone := mkdir(t, parent, fmt.Sprintf("%sfixtures-%d-123", Prefix, dead))
	live := mkdir(t, parent, fmt.Sprintf("%sfixtures-%d-456", Prefix, os.Getpid()))
	other := mkdir(t, parent, fmt.Sprintf("%sintegration-%d-789", Prefix, dead))
	foreign := mkdir(t, parent, "vizra-fixtures-shared-42")
	garbled := mkdir(t, parent, Prefix+"fixtures-notapid-1")

	removed := Sweep(parent, "fixtures")
	if len(removed) != 1 || removed[0] != gone {
		t.Fatalf("Sweep removed %v; want exactly %s", removed, gone)
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Fatalf("the dead process's root is still there: %v", err)
	}
	for _, keep := range []string{live, other, foreign, garbled} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("Sweep removed %s, which it does not own: %v", keep, err)
		}
	}
}

func TestAliveTellsALiveProcessFromADeadOne(t *testing.T) {
	if !alive(os.Getpid()) {
		t.Fatal("this process is reported dead")
	}
	if alive(deadPID(t)) {
		t.Fatal("a reaped process is reported alive")
	}
	if !alive(1) {
		t.Fatal("pid 1 (owned by root: EPERM, not ESRCH) is reported dead; a foreign live process's root would be swept")
	}
}
