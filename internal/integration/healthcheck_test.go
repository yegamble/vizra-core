//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yegamble/vizra-core/internal/healthcheck"
	"github.com/yegamble/vizra-core/internal/jobs"
)

// ---------------------------------------------------------------------------
// `vizra healthcheck` against REAL processes and REAL PostgreSQL.
//
// The property under test is the one meta PR #4's infrastructure seat found
// missing (FINDING 3): THE PROBE CANNOT PASS WHILE THE SERVICE IT PROBES IS
// BROKEN. Everything here therefore runs the shipped binaries — `vizra`,
// `vizra-api`, `vizra-worker` — as separate processes and reads their exit
// codes, rather than calling a function that happens to share a package with
// the probe. A test that calls the probe in-process cannot tell the difference
// between `os.Exit(0)` and a correct verdict.
//
// "PostgreSQL stopped" is a real TCP-level stop: the pool reaches PostgreSQL
// through a proxy this test owns, and stopping it closes every established
// connection and refuses new ones. That is what an operator's database going
// away looks like, and unlike `docker stop` it is deterministic and leaves no
// container behind.
// ---------------------------------------------------------------------------

// buildBinaries compiles the three entry points once per package run.
var (
	buildOnce sync.Once
	binDir    string
	buildErr  error
)

func binaries(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		// Under the package's testtmp root (main_test.go): removed when the run
		// ends, and swept by the next run if this one is killed. Before that,
		// nothing removed it — ~74 MB per run (sentinel S-0001).
		dir, err := os.MkdirTemp("", "vizra-healthcheck-bin-")
		if err != nil {
			buildErr = err
			return
		}
		binDir = dir
		for _, pkg := range []string{"./cmd/vizra", "./cmd/api", "./cmd/worker"} {
			out := filepath.Join(dir, strings.TrimPrefix(pkg, "./cmd/"))
			if pkg == "./cmd/api" {
				out = filepath.Join(dir, "vizra-api")
			}
			if pkg == "./cmd/worker" {
				out = filepath.Join(dir, "vizra-worker")
			}
			cmd := exec.Command("go", "build", "-o", out, pkg)
			cmd.Dir = ".."
			cmd.Dir = repoRoot(t)
			if b, err := cmd.CombinedOutput(); err != nil {
				buildErr = fmt.Errorf("building %s: %v\n%s", pkg, err, b)
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatalf("this lane is BLOCKED, not skipped: %v", buildErr)
	}
	return binDir
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// .../internal/integration -> repository root
	return filepath.Dir(filepath.Dir(wd))
}

// ---------------------------------------------------------------------------
// A TCP proxy, so "the database went away" is a real event this test controls.
// ---------------------------------------------------------------------------

type tcpProxy struct {
	ln       net.Listener
	upstream string

	mu     sync.Mutex
	conns  []net.Conn
	closed bool
}

func newTCPProxy(t *testing.T, upstream string) *tcpProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &tcpProxy{ln: ln, upstream: upstream}
	go p.serve()
	t.Cleanup(p.stop)
	return p
}

func (p *tcpProxy) addr() string { return p.ln.Addr().String() }

func (p *tcpProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		up, err := net.DialTimeout("tcp", p.upstream, 5*time.Second)
		if err != nil {
			_ = c.Close()
			continue
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = c.Close()
			_ = up.Close()
			return
		}
		p.conns = append(p.conns, c, up)
		p.mu.Unlock()
		go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
		go func() { _, _ = io.Copy(c, up); _ = c.Close() }()
	}
}

// stop is "PostgreSQL stopped": no new connections, and every established one
// is torn down. Idempotent, so a test may call it and Cleanup may call it again.
func (p *tcpProxy) stop() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	conns := p.conns
	p.conns = nil
	p.mu.Unlock()

	_ = p.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
}

// proxiedDSN rewrites the test DSN to reach PostgreSQL through the proxy.
func proxiedDSN(t *testing.T, dsn, proxyAddr string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parsing the test DSN: %v", err)
	}
	u.Host = proxyAddr
	return u.String()
}

// ---------------------------------------------------------------------------
// Running the probe.
// ---------------------------------------------------------------------------

type probeResult struct {
	code    int
	stdout  string
	stderr  string
	elapsed time.Duration
}

// runProbe executes the SHIPPED `vizra healthcheck` binary. env carries the
// address key the probe is required to read — the same key the service binds —
// so the test exercises the configured path, not a --addr override.
func runProbe(t *testing.T, target string, env map[string]string, args ...string) probeResult {
	t.Helper()
	cmd := exec.Command(filepath.Join(binaries(t), "vizra"), append([]string{"healthcheck", target}, args...)...)
	cmd.Env = append(os.Environ(), envPairs(env)...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running the probe: %v", err)
		}
		code = ee.ExitCode()
	}
	r := probeResult{code: code, stdout: out.String(), stderr: errb.String(), elapsed: elapsed}
	t.Logf("vizra healthcheck %s %v -> exit %d in %s\n  stdout: %s  stderr: %s",
		target, args, r.code, r.elapsed.Round(time.Millisecond), r.stdout, r.stderr)
	return r
}

func envPairs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// startProcess runs one of the service binaries and returns its captured
// output. It is killed at the end of the test.
type service struct {
	cmd *exec.Cmd
	log *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func startService(t *testing.T, bin string, env map[string]string) *service {
	t.Helper()
	cmd := exec.Command(filepath.Join(binaries(t), bin))
	cmd.Env = append(os.Environ(), envPairs(env)...)
	buf := &syncBuffer{}
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", bin, err)
	}
	svc := &service{cmd: cmd, log: buf}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("%s log:\n%s", bin, buf.String())
		}
	})
	return svc
}

// waitReady polls the probe until it exits 0, or fails with the service log.
func waitReady(t *testing.T, svc *service, target string, env map[string]string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	var last probeResult
	for time.Now().Before(deadline) {
		last = runProbe(t, target, env)
		if last.code == healthcheck.ExitReady {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s never became ready within %s (last exit %d, stderr %q)\nservice log:\n%s",
		target, within, last.code, last.stderr, svc.log.String())
}

// waitNotReady polls until the probe exits non-zero, which is what "within the
// deadline" means for a readiness endpoint with a 2s single-flight cache.
func waitNotReady(t *testing.T, target string, env map[string]string, within time.Duration) probeResult {
	t.Helper()
	deadline := time.Now().Add(within)
	var last probeResult
	for time.Now().Before(deadline) {
		last = runProbe(t, target, env)
		if last.code != healthcheck.ExitReady {
			return last
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s still reported READY %s after the service was broken. A probe that "+
		"cannot fail while the service is broken is the defect this exists to close.",
		target, within)
	return last
}

// ---------------------------------------------------------------------------
// api
// ---------------------------------------------------------------------------

func TestHealthcheckApiIsZeroWhenReadyAndNonZeroWhenPostgresStops(t *testing.T) {
	cfg, _, _ := freshDatabase(t)
	_ = cfg

	direct := mustEnv(t, "VIZRA_TEST_DATABASE_URL")
	u, err := url.Parse(direct)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newTCPProxy(t, u.Host)

	listen := freePort(t)
	env := map[string]string{
		"VIZRA_MODE":         "development",
		"DATABASE_URL":       proxiedDSN(t, direct, proxy.addr()),
		"VIZRA_CACHE_URL":    mustEnv(t, "VIZRA_TEST_CACHE_URL"),
		"VIZRA_LISTEN_ADDR":  listen,
		"VIZRA_METRICS_ADDR": freePort(t),
	}
	svc := startService(t, "vizra-api", env)

	// HEALTHY -> 0.
	waitReady(t, svc, "api", env, 30*time.Second)

	// PostgreSQL stopped -> non-zero, within the deadline. /readyz caches its
	// verdict for 2s (ADR-002), so "within the deadline" is what is asserted,
	// not "on the next call".
	proxy.stop()
	got := waitNotReady(t, "api", env, 20*time.Second)
	if got.code != healthcheck.ExitNotReady {
		t.Fatalf("exit %d, want %d", got.code, healthcheck.ExitNotReady)
	}
	if !strings.Contains(got.stderr, "NOT READY") {
		t.Errorf("stderr does not report the failure: %q", got.stderr)
	}
	// It must be a 503 from a LIVE listener, not a refused connection: the api
	// process is still up and still serving — that is the whole point.
	if !strings.Contains(got.stderr, "503") {
		t.Errorf("the api answered something other than 503 with PostgreSQL gone: %q", got.stderr)
	}
}

func TestHealthcheckApiIsNonZeroWhenTheListenerIsAbsent(t *testing.T) {
	binaries(t)
	env := map[string]string{"VIZRA_LISTEN_ADDR": freePort(t)} // nothing is listening there

	start := time.Now()
	got := runProbe(t, "api", env, "--timeout=2s")
	if got.code != healthcheck.ExitNotReady {
		t.Fatalf("exit %d, want %d", got.code, healthcheck.ExitNotReady)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("the probe took %s against an absent listener; it is not bounded", d)
	}
	if !strings.Contains(got.stderr, "NOT READY") {
		t.Errorf("stderr does not report the failure: %q", got.stderr)
	}
}

// ---------------------------------------------------------------------------
// worker
// ---------------------------------------------------------------------------

func TestHealthcheckWorkerIsZeroWhenTheLoopIsRunningAndNonZeroWhenPostgresStops(t *testing.T) {
	freshDatabase(t)

	direct := mustEnv(t, "VIZRA_TEST_DATABASE_URL")
	u, err := url.Parse(direct)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newTCPProxy(t, u.Host)

	metrics := freePort(t)
	env := map[string]string{
		"VIZRA_MODE":         "development",
		"DATABASE_URL":       proxiedDSN(t, direct, proxy.addr()),
		"VIZRA_CACHE_URL":    mustEnv(t, "VIZRA_TEST_CACHE_URL"),
		"VIZRA_METRICS_ADDR": metrics,
	}
	svc := startService(t, "vizra-worker", env)

	waitReady(t, svc, "worker", env, 30*time.Second)

	proxy.stop()
	got := waitNotReady(t, "worker", env, 20*time.Second)
	if got.code != healthcheck.ExitNotReady {
		t.Fatalf("exit %d, want %d", got.code, healthcheck.ExitNotReady)
	}
}

// The `vizra version` probe this replaces exits 0 for a worker whose claim loop
// has stopped. This is the isolated proof that the new one does not: PostgreSQL
// stays up and pingable throughout, and only the loop stops.
func TestHealthcheckWorkerIsNonZeroWhenTheClaimLoopStallsWhilePostgresIsFine(t *testing.T) {
	binaries(t)
	_, resolver, pool := freshDatabase(t)

	const staleAfter = 1500 * time.Millisecond
	w := jobs.NewWorker(resolver, map[string]*pgxpool.Pool{"default": pool}, nil, jobs.Options{
		Lease: 5 * time.Second, Timeout: 10 * time.Second, Concurrency: 2,
		PollInterval: 50 * time.Millisecond, SweepInterval: time.Hour,
		WorkerID: "stall-test", HealthStaleAfter: staleAfter,
	})

	// The same wiring cmd/worker builds: the readiness handler on the metrics
	// listener, with the real pool as the pinger.
	addr := freePort(t)
	mux := http.NewServeMux()
	mux.Handle("/readyz", jobs.HealthHandler(w.Health(), map[string]jobs.Pinger{"default": pool}))
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("binding the worker health listener: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		sctx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Shutdown(sctx)
	})

	ctx, cancel := context.WithCancel(t.Context())
	loopDone := make(chan struct{})
	go func() { defer close(loopDone); _ = w.Run(ctx) }()

	env := map[string]string{"VIZRA_METRICS_ADDR": addr}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if runProbe(t, "worker", env).code == healthcheck.ExitReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the worker never reported ready with a live claim loop")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Stop ONLY the loop. The listener stays up and PostgreSQL stays up.
	cancel()
	<-loopDone

	// The pool is still fine — prove it, so the failure below cannot be blamed
	// on the database.
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatalf("PostgreSQL is not reachable, so this test proves nothing: %v", err)
	}

	time.Sleep(staleAfter + 500*time.Millisecond)
	got := runProbe(t, "worker", env)
	if got.code != healthcheck.ExitNotReady {
		t.Fatalf("a worker whose claim loop has stopped reported exit %d. That is exactly "+
			"the `vizra version` failure this replaces.", got.code)
	}

	// And the body names the reason, so an operator is not left guessing.
	body := fetchReadyz(t, addr)
	if body.Sites[0].ClaimLoop != jobs.ClaimLoopStalled {
		t.Errorf("claim_loop = %q, want %q; body = %+v", body.Sites[0].ClaimLoop, jobs.ClaimLoopStalled, body)
	}
	if body.Sites[0].Database != jobs.ComponentOK {
		t.Errorf("database = %q, want %q — PostgreSQL was up the whole time",
			body.Sites[0].Database, jobs.ComponentOK)
	}
}

func fetchReadyz(t *testing.T, addr string) jobs.HealthReport {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/readyz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("reading /readyz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	var rep jobs.HealthReport
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatalf("/readyz is not JSON: %v (%s)", err, body)
	}
	if len(rep.Sites) == 0 {
		t.Fatal("/readyz reported no sites")
	}
	return rep
}

// ---------------------------------------------------------------------------
// The exit-code contract the container runtime reads.
// ---------------------------------------------------------------------------

func TestHealthcheckUsageErrorsAreDistinctFromAVerdict(t *testing.T) {
	binaries(t)
	for _, args := range [][]string{{"nonsense"}, {"api", "--nosuchflag"}} {
		cmd := exec.Command(filepath.Join(binaries(t), "vizra"), append([]string{"healthcheck"}, args...)...)
		var errb strings.Builder
		cmd.Stderr = &errb
		err := cmd.Run()
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("args %v: expected a non-zero exit, got %v", args, err)
		}
		if ee.ExitCode() != healthcheck.ExitUsage {
			t.Errorf("args %v gave exit %d, want %d (a usage mistake must not read as a "+
				"verdict about the service, and must not use Docker's reserved 2)",
				args, ee.ExitCode(), healthcheck.ExitUsage)
		}
	}
}
