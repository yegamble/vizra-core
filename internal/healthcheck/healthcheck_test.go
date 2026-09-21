package healthcheck_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yegamble/vizra-core/internal/healthcheck"
)

// env builds a lookup over a literal map, so a test never depends on the
// ambient environment of the machine running it.
func env(kv map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := kv[k]
		return v, ok
	}
}

type capture struct{ out, err strings.Builder }

func run(t *testing.T, args []string, kv map[string]string) (int, string, string) {
	t.Helper()
	var c capture
	code := healthcheck.Run(t.Context(), args, env(kv), &c.out, &c.err)
	return code, c.out.String(), c.err.String()
}

// ---------------------------------------------------------------------------
// The address comes from the keys the SERVICES already read. A probe that has
// its own address key can be pointed at a different process than the one it
// claims to be probing.
// ---------------------------------------------------------------------------

func TestTheProbeReadsTheSameAddressKeysTheServicesRead(t *testing.T) {
	for _, tc := range []struct{ target, key string }{
		{"api", "VIZRA_LISTEN_ADDR"},
		{"worker", "VIZRA_METRICS_ADDR"},
	} {
		tgt, ok := healthcheck.TargetByName(tc.target)
		if !ok {
			t.Fatalf("no target %q", tc.target)
		}
		if tgt.AddrKey != tc.key {
			t.Errorf("target %q reads %q, want %q — the probe must not introduce "+
				"an address key of its own", tc.target, tgt.AddrKey, tc.key)
		}
	}
}

func TestABindAddressBecomesALoopbackDialAddress(t *testing.T) {
	for _, tc := range []struct{ bind, want string }{
		{":8080", "127.0.0.1:8080"},
		{"0.0.0.0:8080", "127.0.0.1:8080"},
		{"[::]:8080", "127.0.0.1:8080"},
		{"127.0.0.1:9090", "127.0.0.1:9090"},
		{"localhost:9090", "localhost:9090"},
	} {
		got, err := healthcheck.DialAddr(tc.bind)
		if err != nil {
			t.Errorf("DialAddr(%q): %v", tc.bind, err)
			continue
		}
		if got != tc.want {
			t.Errorf("DialAddr(%q) = %q, want %q", tc.bind, got, tc.want)
		}
	}
	if _, err := healthcheck.DialAddr("8080"); err == nil {
		t.Error("DialAddr(\"8080\") returned no error; an address with no port is a usage error")
	}
}

// ---------------------------------------------------------------------------
// Exit codes. 0 is READY and nothing else.
// ---------------------------------------------------------------------------

func readyzServer(t *testing.T, code int, body any) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestAReadyAnswerExitsZero(t *testing.T) {
	s := readyzServer(t, http.StatusOK, map[string]any{"status": "ok"})
	code, out, errs := run(t, []string{"api"}, map[string]string{"VIZRA_LISTEN_ADDR": s.Listener.Addr().String()})
	if code != healthcheck.ExitReady {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, healthcheck.ExitReady, errs)
	}
	if !strings.Contains(out, "ready") {
		t.Errorf("stdout does not say the service is ready: %q", out)
	}
}

// ADR-002 §Probes: /readyz 503s on PostgreSQL ONLY. `degraded` is a 200 and the
// instance keeps serving reads. The probe keeps that semantics rather than
// inventing a stricter one, and it SAYS degraded so the operator sees it in
// `docker inspect`'s health log.
func TestADegradedButServingApiExitsZeroAndSaysSo(t *testing.T) {
	s := readyzServer(t, http.StatusOK, map[string]any{
		"status": "degraded",
		"components": []map[string]string{
			{"name": "cache", "status": "degraded", "detail": "cache unreachable"},
		},
	})
	code, out, errs := run(t, []string{"api"}, map[string]string{"VIZRA_LISTEN_ADDR": s.Listener.Addr().String()})
	if code != healthcheck.ExitReady {
		t.Fatalf("exit %d, want %d — a degraded instance still serves reads; pulling it "+
			"from rotation on a cache blip takes the site down (stderr: %s)",
			code, healthcheck.ExitReady, errs)
	}
	if !strings.Contains(out, "degraded") {
		t.Errorf("the probe hid the degraded status from the health log: %q", out)
	}
}

func TestANonSuccessStatusExitsNonZero(t *testing.T) {
	for _, code := range []int{http.StatusServiceUnavailable, http.StatusInternalServerError,
		http.StatusNotFound, http.StatusMovedPermanently} {
		s := readyzServer(t, code, map[string]any{"status": "unavailable"})
		got, _, errs := run(t, []string{"api"}, map[string]string{"VIZRA_LISTEN_ADDR": s.Listener.Addr().String()})
		if got != healthcheck.ExitNotReady {
			t.Errorf("HTTP %d gave exit %d, want %d", code, got, healthcheck.ExitNotReady)
		}
		if errs == "" {
			t.Errorf("HTTP %d produced no message on stderr", code)
		}
	}
}

func TestConnectionRefusedExitsNonZero(t *testing.T) {
	// Bind, record the address, close: nothing is listening there now.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	code, _, errs := run(t, []string{"api", "--timeout=2s"}, map[string]string{"VIZRA_LISTEN_ADDR": addr})
	if code != healthcheck.ExitNotReady {
		t.Fatalf("exit %d, want %d", code, healthcheck.ExitNotReady)
	}
	if !strings.Contains(errs, "NOT READY") {
		t.Errorf("stderr does not report a failure: %q", errs)
	}
}

// The probe is BOUNDED and has no retries of its own: Docker's --interval and
// --retries already supply them, and a probe that retries inside its own
// timeout turns one slow answer into a healthcheck timeout.
func TestATimeoutIsEnforcedAndTheProbeNeverRetries(t *testing.T) {
	var hits atomic.Int32
	done := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-done // never answers within the deadline
	}))
	t.Cleanup(func() { close(done); s.Close() })

	start := time.Now()
	code, _, errs := run(t, []string{"api", "--timeout=300ms"},
		map[string]string{"VIZRA_LISTEN_ADDR": s.Listener.Addr().String()})
	elapsed := time.Since(start)

	if code != healthcheck.ExitNotReady {
		t.Fatalf("exit %d, want %d", code, healthcheck.ExitNotReady)
	}
	if elapsed > 3*time.Second {
		t.Errorf("the probe took %s against a 300ms deadline; it is not bounded", elapsed)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("the server saw %d requests; the probe must make exactly one (no retries)", n)
	}
	if !strings.Contains(errs, "timed out") {
		t.Errorf("stderr does not name the timeout: %q", errs)
	}
}

// ---------------------------------------------------------------------------
// Usage. Docker reserves exit code 2, so a usage error must not use it.
// ---------------------------------------------------------------------------

func TestUsageErrorsDoNotCollideWithDockersReservedCode(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"nonsense"},
		{"api", "--nosuchflag"},
		{"api", "extra"},
	} {
		code, _, errs := run(t, args, map[string]string{})
		if code != healthcheck.ExitUsage {
			t.Errorf("args %v gave exit %d, want %d", args, code, healthcheck.ExitUsage)
		}
		if code == 2 {
			t.Errorf("args %v used exit code 2, which Docker reserves", args)
		}
		if errs == "" {
			t.Errorf("args %v produced no usage message", args)
		}
	}
}

func TestHelpExitsZeroAndNamesBothTargets(t *testing.T) {
	code, out, _ := run(t, []string{"--help"}, map[string]string{})
	if code != healthcheck.ExitReady {
		t.Fatalf("--help exited %d, want 0", code)
	}
	for _, want := range []string{"api", "worker", "--timeout", "Exit codes"} {
		if !strings.Contains(out, want) {
			t.Errorf("--help does not mention %q:\n%s", want, out)
		}
	}
}

// ---------------------------------------------------------------------------
// Nothing secret is ever printed. The probe is run by a container runtime that
// records its output in the health log, which `docker inspect` shows to anyone
// who can reach the daemon.
// ---------------------------------------------------------------------------

func TestTheProbeNeverPrintsASecretItCanSee(t *testing.T) {
	const dsn = "postgres://vizra:sup3rs3cr3t@db:5432/vizra"
	const key = "0123456789abcdef0123456789abcdef"

	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	_ = l.Close()

	_, out, errs := run(t, []string{"api", "--timeout=1s"}, map[string]string{
		"VIZRA_LISTEN_ADDR":    addr,
		"DATABASE_URL":         dsn,
		"SEARCH_HMAC_KEY":      key,
		"VIZRA_SESSION_SECRET": key,
	})
	for _, secret := range []string{dsn, "sup3rs3cr3t", key} {
		if strings.Contains(out+errs, secret) {
			t.Errorf("the probe printed a secret:\nstdout: %s\nstderr: %s", out, errs)
		}
	}
}

// ---------------------------------------------------------------------------
// The default deadline must sit well under the container healthcheck timeout
// the compose file uses (3s), or the runtime kills the probe before it can
// report and every answer becomes "unhealthy" for the wrong reason.
// ---------------------------------------------------------------------------

func TestTheDefaultDeadlineFitsInsideTheContainerHealthcheckTimeout(t *testing.T) {
	if healthcheck.DefaultTimeout <= 0 {
		t.Fatal("DefaultTimeout is not set")
	}
	const dockerTimeout = 3 * time.Second
	if healthcheck.DefaultTimeout >= dockerTimeout {
		t.Fatalf("DefaultTimeout is %s, which is not well under the %s container "+
			"healthcheck timeout", healthcheck.DefaultTimeout, dockerTimeout)
	}
}

// A caller's cancellation reaches the request rather than being ignored.
func TestTheCallersContextIsPropagated(t *testing.T) {
	block := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	t.Cleanup(func() { close(block); s.Close() })

	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()

	var c capture
	start := time.Now()
	code := healthcheck.Run(ctx, []string{"api", "--timeout=30s"},
		env(map[string]string{"VIZRA_LISTEN_ADDR": s.Listener.Addr().String()}), &c.out, &c.err)
	if code != healthcheck.ExitNotReady {
		t.Fatalf("exit %d, want %d", code, healthcheck.ExitNotReady)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("cancelling the caller's context took %s to reach the request; it is not propagated", d)
	}
}
