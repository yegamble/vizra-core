//go:build unix

package testtmp

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The helpers below drive a package's OWN test binary as a child process with
// a TMPDIR the calling test owns, so a leak check counts only what that child
// left behind — never another process's files in the shared temporary
// directory.

// BuildTestBinary compiles the test binary of the package in the current
// directory (a test's working directory is its package directory) into a
// directory the calling test owns, and returns its path. It is built without
// -race: the property under test is what the binary leaves on disk, not its
// data races, and the build is several times faster.
func BuildTestBinary(t *testing.T, tags ...string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "pkg.test")
	args := []string{"test", "-c", "-o", bin}
	if len(tags) > 0 {
		args = append(args, "-tags="+strings.Join(tags, ","))
	}
	args = append(args, ".")
	cmd := exec.Command("go", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building this package's test binary: %v\n%s", err, out)
	}
	return bin
}

// ChildEnv is the current environment with TMPDIR set to tmp.
func ChildEnv(tmp string) []string {
	env := []string{"TMPDIR=" + tmp}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TMPDIR=") {
			env = append(env, kv)
		}
	}
	return env
}

// RunChild runs bin with -test.run=run and TMPDIR=tmp, and returns its exit
// code and combined output.
func RunChild(t *testing.T, bin, tmp, run string) (int, string) {
	t.Helper()
	cmd := exec.Command(bin, "-test.run="+run, "-test.count=1")
	cmd.Env = ChildEnv(tmp)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("running %s: %v", bin, err)
	}
	return cmd.ProcessState.ExitCode(), string(out)
}

// KillChildWhen starts bin with -test.run=run and TMPDIR=tmp, waits until an
// entry whose base name starts with marker exists anywhere under tmp, and then
// kills it and everything it started with SIGKILL — the way an OOM kill or a
// CI job timeout ends a test binary: no deferred call, no t.Cleanup and no
// TestMain epilogue runs (go test's own -timeout is a panic that exits the
// binary: no cleanup either). It fails the test if the marker never appears
// within wait.
func KillChildWhen(t *testing.T, bin, tmp, run, marker string, wait time.Duration) string {
	t.Helper()
	cmd := exec.Command(bin, "-test.run="+run, "-test.count=1")
	cmd.Env = ChildEnv(tmp)
	// Its own process group, so the kill below also reaches what the child
	// started (the `go build` of the entry points). Killing only the test
	// binary left that build running as an orphan, still writing into the
	// directory the next run had swept — measured on the first run of this
	// test in the integration lane.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", bin, err)
	}
	deadline := time.Now().Add(wait)
	var seen string
	for seen == "" && time.Now().Before(deadline) {
		seen = find(tmp, marker)
		if seen == "" {
			time.Sleep(20 * time.Millisecond)
		}
	}
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	_ = cmd.Wait()
	// Every member of the group must be gone before the caller looks at tmp.
	for gone := time.Now().Add(30 * time.Second); ; {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(gone) {
			t.Fatalf("processes of the killed child's group %d are still alive after 30s", pgid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if seen == "" {
		t.Fatalf("no %s* entry appeared under the child's TMPDIR within %s; the kill proves nothing", marker, wait)
	}
	return seen
}

func find(root, marker string) string {
	var hit string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || hit != "" {
			return nil
		}
		if p != root && strings.HasPrefix(d.Name(), marker) {
			hit = p
			return fs.SkipAll
		}
		return nil
	})
	return hit
}

// VizraEntries lists the top-level entries of dir whose names start with
// "vizra-": what a run of a Vizra test binary left in its TMPDIR.
func VizraEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "vizra-") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}
