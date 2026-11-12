//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// VZ-FOUND-005 against the SHIPPED vizra-api process and REAL PostgreSQL:
//
//   - stopping PostgreSQL turns /readyz non-200 within the declared interval;
//   - the probe returns to 200 when PostgreSQL comes back, WITHOUT restarting
//     the api (the same PID serves every request in the test);
//   - a migration ledger behind the binary's embedded latest fails readiness
//     closed with the `schema` component named, and recovers when it is current.
//
// The declared interval is readyzInterval: the 2 s readiness cache (ADR-002 §
// Probes) plus the 3 s compute timeout, rounded up to 10 s. Both the
// transition to non-200 and the recovery are asserted to happen inside it.
// ---------------------------------------------------------------------------

const readyzInterval = 10 * time.Second

// restartableProxy is "PostgreSQL stopped / started" as a real TCP event on a
// fixed address the api's DSN points at. stop closes the listener and every
// established connection; start listens on the same address again.
type restartableProxy struct {
	addr, upstream string

	mu    sync.Mutex
	ln    net.Listener
	conns []net.Conn
}

func newRestartableProxy(t *testing.T, upstream string) *restartableProxy {
	t.Helper()
	p := &restartableProxy{addr: freePort(t), upstream: upstream}
	p.start(t)
	t.Cleanup(p.stop)
	return p
}

func (p *restartableProxy) start(t *testing.T) {
	t.Helper()
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ { // the port may sit briefly in close-wait
		if ln, err = net.Listen("tcp", p.addr); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("proxy relisten on %s: %v", p.addr, err)
	}
	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.DialTimeout("tcp", p.upstream, 5*time.Second)
			if err != nil {
				_ = c.Close()
				continue
			}
			p.mu.Lock()
			if p.ln != ln {
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
	}()
}

func (p *restartableProxy) stop() {
	p.mu.Lock()
	ln, conns := p.ln, p.conns
	p.ln, p.conns = nil, nil
	p.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
}

type readyzReply struct {
	code int
	body struct {
		Status     string `json:"status"`
		Components []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"components"`
	}
}

func (r readyzReply) component(name string) string {
	for _, c := range r.body.Components {
		if c.Name == name {
			return c.Status
		}
	}
	return ""
}

func getReadyz(t *testing.T, listen string) (readyzReply, error) {
	t.Helper()
	var r readyzReply
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + listen + "/readyz")
	if err != nil {
		return r, err
	}
	defer func() { _ = resp.Body.Close() }()
	r.code = resp.StatusCode
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&r.body)
	return r, err
}

// awaitReadyz polls /readyz until want(reply) holds, and fails if that takes
// longer than readyzInterval. It returns the reply and the time it took.
func awaitReadyz(t *testing.T, svc *service, listen, what string, want func(readyzReply) bool) (readyzReply, time.Duration) {
	t.Helper()
	start := time.Now()
	var last readyzReply
	var lastErr error
	for time.Since(start) < readyzInterval {
		last, lastErr = getReadyz(t, listen)
		if lastErr == nil && want(last) {
			took := time.Since(start)
			t.Logf("%s: /readyz %d %s after %s", what, last.code, last.body.Status, took.Round(time.Millisecond))
			return last, took
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s: not reached within the declared %s (last %d %+v, err %v)\napi log:\n%s",
		what, readyzInterval, last.code, last.body, lastErr, svc.log.String())
	return last, 0
}

func startAPI(t *testing.T, dsn string) (*service, string) {
	t.Helper()
	listen := freePort(t)
	svc := startService(t, "vizra-api", map[string]string{
		"VIZRA_MODE":         "development",
		"DATABASE_URL":       dsn,
		"VIZRA_CACHE_URL":    mustEnv(t, "VIZRA_TEST_CACHE_URL"),
		"VIZRA_LISTEN_ADDR":  listen,
		"VIZRA_STORAGE_ROOT": t.TempDir(),
		"VIZRA_METRICS_ADDR": freePort(t),
	})
	return svc, listen
}

func assertSameProcess(t *testing.T, svc *service, pid int) {
	t.Helper()
	if svc.cmd.Process.Pid != pid || svc.cmd.ProcessState != nil {
		t.Fatalf("the api was restarted or exited (pid %d -> %d, state %v)", pid, svc.cmd.Process.Pid, svc.cmd.ProcessState)
	}
}

func TestReadyzGoesNon200WhenPostgresStopsAndRecoversWithoutRestart(t *testing.T) {
	freshDatabase(t)
	direct := mustEnv(t, "VIZRA_TEST_DATABASE_URL")
	u, err := url.Parse(direct)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newRestartableProxy(t, u.Host)
	svc, listen := startAPI(t, proxiedDSN(t, direct, proxy.addr))
	pid := svc.cmd.Process.Pid

	// Boot can take longer than the probe interval; readiness is asserted
	// against the interval from here on.
	deadline := time.Now().Add(30 * time.Second)
	for {
		r, err := getReadyz(t, listen)
		if err == nil && r.code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the api never became ready: %v %+v\n%s", err, r, svc.log.String())
		}
		time.Sleep(250 * time.Millisecond)
	}

	for round := 1; round <= 2; round++ { // twice: recovery is repeatable
		proxy.stop()
		down, _ := awaitReadyz(t, svc, listen, "PostgreSQL stopped", func(r readyzReply) bool {
			return r.code != http.StatusOK
		})
		if down.code != http.StatusServiceUnavailable || down.component("database") != "unavailable" {
			t.Fatalf("PostgreSQL stopped: /readyz %d database=%q, want 503 database=unavailable",
				down.code, down.component("database"))
		}
		assertSameProcess(t, svc, pid)

		proxy.start(t)
		up, _ := awaitReadyz(t, svc, listen, "PostgreSQL returned", func(r readyzReply) bool {
			return r.code == http.StatusOK
		})
		if up.component("database") != "ok" || up.component("schema") != "ok" {
			t.Fatalf("after recovery database=%q schema=%q, want ok/ok", up.component("database"), up.component("schema"))
		}
		assertSameProcess(t, svc, pid)
	}
}

func TestReadyzFailsClosedWhenTheSchemaIsBehind(t *testing.T) {
	_, _, pool := freshDatabase(t)
	svc, listen := startAPI(t, mustEnv(t, "VIZRA_TEST_DATABASE_URL"))
	pid := svc.cmd.Process.Pid

	deadline := time.Now().Add(30 * time.Second)
	for {
		r, err := getReadyz(t, listen)
		if err == nil && r.code == http.StatusOK && r.component("schema") == "ok" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the api never became ready with a current schema: %v %+v\n%s", err, r, svc.log.String())
		}
		time.Sleep(250 * time.Millisecond)
	}

	// Behind: the ledger records one migration fewer than the binary embeds.
	embedded := mustEmbedded(t)
	if _, err := pool.Exec(t.Context(), `UPDATE schema_migrations SET version = $1`, embedded-1); err != nil {
		t.Fatal(err)
	}
	behind, _ := awaitReadyz(t, svc, listen, "schema behind", func(r readyzReply) bool {
		return r.code != http.StatusOK
	})
	if behind.code != http.StatusServiceUnavailable || behind.component("schema") != "unavailable" ||
		behind.component("database") != "ok" {
		t.Fatalf("schema behind: /readyz %d schema=%q database=%q, want 503 schema=unavailable database=ok",
			behind.code, behind.component("schema"), behind.component("database"))
	}

	// /schemaz stays 200 and says "behind" with the versions.
	resp, err := http.Get("http://" + listen + "/schemaz")
	if err != nil {
		t.Fatal(err)
	}
	var sch struct {
		Applied  int64  `json:"applied_version"`
		Embedded int64  `json:"embedded_version"`
		State    string `json:"state"`
	}
	err = json.NewDecoder(resp.Body).Decode(&sch)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || sch.State != "behind" ||
		sch.Applied != embedded-1 || sch.Embedded != embedded {
		t.Fatalf("/schemaz = %d %+v (%v), want 200 behind %d/%d", resp.StatusCode, sch, err, embedded-1, embedded)
	}

	// Current again -> 200, same process.
	if _, err := pool.Exec(t.Context(), `UPDATE schema_migrations SET version = $1`, embedded); err != nil {
		t.Fatal(err)
	}
	awaitReadyz(t, svc, listen, "schema current again", func(r readyzReply) bool {
		return r.code == http.StatusOK && r.component("schema") == "ok"
	})
	assertSameProcess(t, svc, pid)
}
