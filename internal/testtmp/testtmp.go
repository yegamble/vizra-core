//go:build unix

// Package testtmp gives one test binary ONE temporary root, so nothing it
// writes to the temporary directory outlives it — including when it dies
// without running any cleanup.
//
// Why a package: `t.TempDir()` and `t.Cleanup` run when a test ENDS. A test
// binary killed by `go test`'s `-timeout` (a panic that exits the process) or
// by a signal runs no cleanup at all, so every `t.TempDir()`, every
// `os.MkdirTemp` and every binary a test built stays behind. Sentinel S-0016
// and S-0001: `internal/fixtures` left its shared corpus behind on timeout, and
// `internal/integration` left a ~74 MB directory of built binaries behind on
// EVERY run.
//
// Run, called from a package's TestMain:
//
//  1. removes the roots of earlier runs of the same package whose process is
//     no longer alive (the name carries the PID; a live process's root is never
//     touched, so concurrent runs are safe);
//  2. creates `vizra-test-<name>-<pid>-*` under the temporary directory and
//     points TMPDIR at it, so os.TempDir(), os.MkdirTemp("", …) and t.TempDir()
//     in that binary — and the processes it starts — all land inside it;
//  3. runs the tests, then removes the root.
//
// What it cannot do: remove a root while its own process is being killed. That
// root is removed by the NEXT run of the same package (step 1).
package testtmp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Prefix is the name every root starts with.
const Prefix = "vizra-test-"

// Run is the body of a TestMain: it returns m.Run()'s exit code.
func Run(m *testing.M, name string) int {
	parent := os.TempDir()
	Sweep(parent, name)
	root, err := os.MkdirTemp(parent, fmt.Sprintf("%s%s-%d-", Prefix, name, os.Getpid()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "testtmp: cannot create the temporary root for %s: %v\n", name, err)
		return 1
	}
	prev, had := os.LookupEnv("TMPDIR")
	if err := os.Setenv("TMPDIR", root); err != nil {
		fmt.Fprintf(os.Stderr, "testtmp: cannot point TMPDIR at %s: %v\n", root, err)
		_ = os.RemoveAll(root)
		return 1
	}
	code := m.Run()
	if had {
		_ = os.Setenv("TMPDIR", prev)
	} else {
		_ = os.Unsetenv("TMPDIR")
	}
	if err := os.RemoveAll(root); err != nil {
		fmt.Fprintf(os.Stderr, "testtmp: could not remove %s: %v\n", root, err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// Sweep removes, from parent, every root of package name whose owning process
// is no longer alive, and returns the paths it removed.
func Sweep(parent, name string) []string {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil
	}
	stem := Prefix + name + "-"
	var removed []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), stem) {
			continue
		}
		rest := strings.TrimPrefix(e.Name(), stem)
		pidText, _, ok := strings.Cut(rest, "-")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidText)
		if err != nil || pid <= 0 || alive(pid) {
			continue
		}
		p := filepath.Join(parent, e.Name())
		if os.RemoveAll(p) == nil {
			removed = append(removed, p)
		}
	}
	return removed
}

// alive reports whether a process with this PID exists. Only ESRCH means it
// does not: EPERM is a live process owned by someone else.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}
