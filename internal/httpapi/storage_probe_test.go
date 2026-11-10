package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yegamble/vizra-core/internal/site"
)

// VZ-FOUND-005: /readyz is 200 only when the storage root is reachable. These
// tests run the REAL check against the real filesystem.

func storageServer(t *testing.T, root string, mutate func(*Deps)) *Server {
	t.Helper()
	return newProbeServer(t, func(d *Deps) {
		d.Config.StorageRoot = root
		if mutate != nil {
			mutate(d)
		}
	})
}

func assertStorageUnavailable(t *testing.T, s *Server, root string) {
	t.Helper()
	code, body := get(t, s, "/readyz")
	if code != http.StatusServiceUnavailable || body["status"] != "unavailable" {
		t.Fatalf("/readyz = %d %v, want 503 unavailable", code, body["status"])
	}
	if got := componentStatus(t, body, "storage"); got != "unavailable" {
		t.Fatalf("storage component = %q, want unavailable", got)
	}
	// The other components are still reported truthfully.
	if got := componentStatus(t, body, "database"); got != "ok" {
		t.Fatalf("database component = %q, want ok", got)
	}
	// The body never carries the path: a root can sit under a directory whose
	// name is nobody else's business.
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), root) || strings.Contains(string(raw), filepath.Base(root)) {
		t.Fatalf("readiness body leaks the storage path: %s", raw)
	}
}

func TestReadyzStorageRootMissingIs503(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent-media-root")
	assertStorageUnavailable(t, storageServer(t, root, nil), root)
}

func TestReadyzStorageRootNotADirectoryIs503(t *testing.T) {
	root := filepath.Join(t.TempDir(), "media-root-file")
	if err := os.WriteFile(root, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertStorageUnavailable(t, storageServer(t, root, nil), root)
}

// unwritableDir returns a directory this process cannot create a file in. For
// an ordinary uid that is a 0555 directory. Root ignores permission bits, so
// there it is /proc, which refuses file creation for every uid — the test runs
// either way instead of skipping.
func unwritableDir(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		if runtime.GOOS != "linux" {
			t.Fatal("running as root on a platform with no known unwritable directory; run this test as an ordinary user")
		}
		return "/proc"
	}
	root := filepath.Join(t.TempDir(), "readonly-media-root")
	if err := os.Mkdir(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	return root
}

func TestReadyzStorageRootNotWritableIs503(t *testing.T) {
	root := unwritableDir(t)
	// Precondition, so the test cannot pass for the wrong reason.
	if f, err := os.CreateTemp(root, "precondition-*"); err == nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		t.Fatalf("precondition: %s is writable by this process", root)
	}
	assertStorageUnavailable(t, storageServer(t, root, nil), root)
}

// Recovery without a restart: the SAME server goes 503 -> 200 once the root
// appears and the 2 s readiness cache has expired.
func TestReadyzStorageRecoversWithoutRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "late-media-root")
	var mu sync.Mutex
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s := storageServer(t, root, func(d *Deps) {
		d.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	})
	assertStorageUnavailable(t, s, root)

	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	now = now.Add(3 * time.Second)
	mu.Unlock()

	code, body := get(t, s, "/readyz")
	if code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("/readyz after the root returned = %d %v, want 200 ok", code, body["status"])
	}
	if got := componentStatus(t, body, "storage"); got != "ok" {
		t.Fatalf("storage component after recovery = %q, want ok", got)
	}
	// The probe leaves nothing behind in the media root.
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("the probe left %d entries in the root (err %v)", len(entries), err)
	}
}

// A filesystem call cannot be cancelled, so a hung mount must not hang the
// probe: the check answers `unavailable` at the deadline, and a check that is
// still stuck is not stacked with another one.
func TestStorageProbeIsBoundedByTheContext(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	p := &storageProbe{check: func() error { calls.Add(1); <-release; return nil }}

	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		start := time.Now()
		err := p.Check(ctx)
		cancel()
		if err == nil {
			t.Fatalf("call %d: a hung check reported ok", i)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("call %d: the probe took %s; it is not bounded", i, d)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d filesystem checks were started while the first was still stuck, want 1", n)
	}
}

// With no root configured the check fails closed rather than passing vacuously.
func TestReadyzStorageRootUnconfiguredIs503(t *testing.T) {
	cfg := probeConfig()
	s := New(Deps{
		Config:          cfg,
		Resolver:        site.NewResolver(cfg),
		PingDatabase:    func(context.Context) error { return nil },
		PingCache:       func(context.Context) error { return nil },
		InstanceClaimed: func(context.Context) (bool, error) { return true, nil },
	})
	code, body := get(t, s, "/readyz")
	if code != http.StatusServiceUnavailable || componentStatus(t, body, "storage") != "unavailable" {
		t.Fatalf("/readyz with no storage root = %d %+v, want 503 with storage unavailable", code, body)
	}
}
