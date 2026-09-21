// Package healthcheck is the `vizra healthcheck` probe: the thing a container
// runtime runs to decide whether this container is ready.
//
// WHY IT EXISTS. The compose tree's api and worker probes were
// `["CMD", "/usr/local/bin/vizra", "version"]` — a CLI that prints build info
// and exits 0 without opening a socket or touching PostgreSQL. An api wedged
// with PostgreSQL unreachable reported `healthy`, and a `service_healthy` edge
// gated the frontend on it (meta PR #4, `vizra-infrastructure` seat, FINDING 3).
// A probe that returns healthy while the service is dead is worse than no
// probe, because everything downstream reads the same green.
//
// So the rule this package exists to keep is: THE PROBE CANNOT PASS WHILE THE
// SERVICE IT PROBES IS BROKEN. It therefore talks to the service's own listener
// over the loopback interface and reads the service's own readiness verdict.
//
// THREE THINGS IT DELIBERATELY DOES NOT DO.
//
//  1. It does not retry. Docker's `--interval` and `--retries` already supply
//     retries, and a probe that retries inside its own `--timeout` turns one
//     slow answer into a killed probe, which the runtime reports as a failure
//     with no message at all.
//  2. It does not load the full configuration. `config.Load` refuses a
//     production process with, say, no `VIZRA_MFA_KEY_KEK`; a probe that
//     refuses for that reason reports the api unhealthy when the api is fine.
//     It reads exactly the two address keys the services themselves bind to,
//     with the registry's own defaults, and nothing else.
//  3. It does not decide readiness itself. `/readyz` is the service's verdict;
//     duplicating the rule here is how the probe and the service drift apart.
//
// `degraded` (ADR-002 § Probes). Core's `/readyz` returns 503 for exactly one
// condition — PostgreSQL unreachable — and 200 `degraded` for a cache that is
// down, a search backend that is unreachable and a queue past its age
// threshold, so a degraded instance keeps serving reads instead of being pulled
// from rotation and taking the site down with it. This probe keeps that
// semantics exactly: ANY 2xx is exit 0. It does not invent a stricter rule. It
// does print the reported status, so `degraded` is visible in the runtime's
// health log rather than being flattened into "healthy".
package healthcheck

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/yegamble/vizra-core/internal/config"
)

// Exit codes. Documented here because an operator reads them out of
// `docker inspect`'s health log with no other context.
//
// There is deliberately ONE failure code. Docker treats every non-zero exit
// from a healthcheck as "unhealthy" and RESERVES exit code 2 ("do not use"), so
// splitting "refused" from "non-2xx" across codes would buy nothing a runtime
// can act on while risking the reserved value. The reason is in the message
// instead, where the health log actually shows it. The usage code is 64
// (sysexits' EX_USAGE) precisely to stay clear of 2.
const (
	// ExitReady — the service answered and is ready (2xx, including `degraded`).
	ExitReady = 0
	// ExitNotReady — connection refused, timed out, or a non-2xx answer.
	ExitNotReady = 1
	// ExitUsage — the command line was wrong. Never a verdict about the service.
	ExitUsage = 64
)

// DefaultTimeout bounds the whole probe. It must sit WELL under the container
// healthcheck timeout (3s in the compose tree), or the runtime kills the probe
// before it can report and every answer becomes "unhealthy" for the wrong
// reason — which is indistinguishable, in the health log, from a real outage.
const DefaultTimeout = 2 * time.Second

// Target is one probeable process.
type Target struct {
	// Name is the subcommand argument.
	Name string
	// AddrKey is the configuration key THE SERVICE ITSELF binds. The probe
	// introduces no address key of its own: a probe with a separate key can be
	// pointed at a different process than the one it claims to be probing, and
	// then it is green about something nobody is using.
	AddrKey string
	// Path is the readiness endpoint on that listener.
	Path string
	// What is a one-line description for `--help`.
	What string
}

// Targets is the probeable set.
//
// The worker has no API listener, so its readiness lives on the metrics
// listener it already runs (`VIZRA_METRICS_ADDR`, loopback by default). See
// internal/jobs.HealthHandler for what that endpoint actually measures — it is
// the claim loop's progress and PostgreSQL's reachability, not the presence of
// a process.
var Targets = []Target{
	{
		Name:    "api",
		AddrKey: "VIZRA_LISTEN_ADDR",
		Path:    "/readyz",
		What:    "the public API listener's readiness endpoint",
	},
	{
		Name:    "worker",
		AddrKey: "VIZRA_METRICS_ADDR",
		Path:    "/readyz",
		What:    "the worker's readiness endpoint on its metrics listener",
	},
}

// TargetByName looks one up.
func TargetByName(name string) (Target, bool) {
	for _, t := range Targets {
		if t.Name == name {
			return t, true
		}
	}
	return Target{}, false
}

// DialAddr turns a BIND address into an address that can be dialled from inside
// the same container.
//
// `:8080` and `0.0.0.0:8080` mean "every interface" to a listener and mean
// nothing to a dialler — `Dial("tcp", ":8080")` happens to work on Linux but
// not portably, and `0.0.0.0` is not a destination. The probe always talks to
// the loopback interface, which is also the only interface it is entitled to
// assume is reachable from inside the container.
func DialAddr(bind string) (string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(bind))
	if err != nil {
		return "", fmt.Errorf("%q is not a host:port address: %w", bind, err)
	}
	if port == "" {
		return "", fmt.Errorf("%q has no port", bind)
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}

const usage = `vizra healthcheck — probe a local Vizra process for readiness

Usage:
  vizra healthcheck api     [--timeout D] [--addr HOST:PORT]
  vizra healthcheck worker  [--timeout D] [--addr HOST:PORT]

  api     %s
          address from VIZRA_LISTEN_ADDR (default %q)
  worker  %s
          address from VIZRA_METRICS_ADDR (default %q)

Flags:
  --timeout D     Deadline for the whole probe. Default %s. Keep it well under
                  the container healthcheck timeout.
  --addr HOST:PORT
                  Override the address. Diagnostics only; the default comes
                  from the key the service itself binds.

The probe makes EXACTLY ONE request and never retries: a container runtime's
--interval and --retries already supply retries.

Exit codes:
  0   ready. A 2xx answer, including a ` + "`degraded`" + ` one — core's /readyz returns
      503 for an unreachable PostgreSQL only, and keeps serving reads while the
      cache or search backend is down (ADR-002). The reported status is printed.
  1   NOT ready: connection refused, timed out, or a non-2xx answer.
  64  usage error. Never a verdict about the service. (Docker reserves 2.)
`

func writeUsage(w io.Writer) {
	api, _ := TargetByName("api")
	worker, _ := TargetByName("worker")
	fmt.Fprintf(w, usage,
		api.What, config.DefaultFor(api.AddrKey),
		worker.What, config.DefaultFor(worker.AddrKey),
		DefaultTimeout)
}

// Run executes one probe and returns the process exit code. lookup is the
// environment (os.LookupEnv in the binary, a literal map in tests).
func Run(ctx context.Context, args []string, lookup func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "vizra healthcheck: no target given")
		writeUsage(stderr)
		return ExitUsage
	}
	switch args[0] {
	case "-h", "--help", "help":
		writeUsage(stdout)
		return ExitReady
	}

	target, ok := TargetByName(args[0])
	if !ok {
		names := make([]string, 0, len(Targets))
		for _, t := range Targets {
			names = append(names, t.Name)
		}
		fmt.Fprintf(stderr, "vizra healthcheck: unknown target %q; known targets are %s\n",
			args[0], strings.Join(names, " and "))
		writeUsage(stderr)
		return ExitUsage
	}

	fs := flag.NewFlagSet("vizra healthcheck "+target.Name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { writeUsage(stderr) }
	timeout := fs.Duration("timeout", DefaultTimeout, "deadline for the whole probe")
	addrFlag := fs.String("addr", "", "override the address (diagnostics only)")
	if err := fs.Parse(args[1:]); err != nil {
		return ExitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "vizra healthcheck %s: unexpected argument %q\n", target.Name, fs.Arg(0))
		return ExitUsage
	}
	if *timeout <= 0 {
		fmt.Fprintf(stderr, "vizra healthcheck %s: --timeout must be positive\n", target.Name)
		return ExitUsage
	}

	bind := *addrFlag
	if bind == "" {
		if v, ok := lookup(target.AddrKey); ok && strings.TrimSpace(v) != "" {
			bind = v
		} else {
			bind = config.DefaultFor(target.AddrKey)
		}
	}
	addr, err := DialAddr(bind)
	if err != nil {
		fmt.Fprintf(stderr, "vizra healthcheck %s: %s is not usable: %v\n", target.Name, target.AddrKey, err)
		return ExitUsage
	}

	// The caller's context is the parent, so a cancelled caller cancels the
	// request in flight rather than being ignored until the deadline.
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	status, code, elapsed, err := probe(ctx, addr, target.Path)
	if err != nil {
		fmt.Fprintf(stderr, "vizra healthcheck %s: NOT READY — %s (%s, %s)\n",
			target.Name, classify(ctx, err), addr+target.Path, elapsed.Round(time.Millisecond))
		return ExitNotReady
	}
	if code < 200 || code > 299 {
		fmt.Fprintf(stderr, "vizra healthcheck %s: NOT READY — HTTP %d%s (%s, %s)\n",
			target.Name, code, statusSuffix(status), addr+target.Path, elapsed.Round(time.Millisecond))
		return ExitNotReady
	}
	fmt.Fprintf(stdout, "vizra healthcheck %s: ready — HTTP %d%s (%s, %s)\n",
		target.Name, code, statusSuffix(status), addr+target.Path, elapsed.Round(time.Millisecond))
	return ExitReady
}

func statusSuffix(status string) string {
	if status == "" {
		return ""
	}
	return " " + status
}

// probe makes exactly one request.
//
// The response body is read under a cap: a readiness body is a few hundred
// bytes, and a probe that streams an unbounded body from a confused process is
// a memory bug in the healthcheck.
const maxBody = 64 << 10

func probe(ctx context.Context, addr, path string) (status string, code int, elapsed time.Duration, err error) {
	start := time.Now()
	u := (&url.URL{Scheme: "http", Host: addr, Path: path}).String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", 0, time.Since(start), err
	}
	req.Header.Set("User-Agent", "vizra-healthcheck")

	client := &http.Client{
		// A redirect is not a ready answer, and following one would let a
		// confused process send the probe somewhere else entirely.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{}).DialContext,
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: 0, // the context is the single deadline
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, time.Since(start), err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	elapsed = time.Since(start)

	// The status word is best effort: a non-JSON body is not itself a failure,
	// the HTTP code is the verdict.
	var parsed struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		status = sanitiseStatus(parsed.Status)
	}
	return status, resp.StatusCode, elapsed, nil
}

// sanitiseStatus bounds and cleans the one field of the answer that is echoed
// into the container health log. The endpoint is local and ours, but the health
// log is read by `docker inspect`, so nothing arbitrary is copied into it.
func sanitiseStatus(s string) string {
	if len(s) > 32 {
		s = s[:32]
	}
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || r == '-' {
			return r
		}
		return -1
	}, s)
}

// classify turns a transport error into a message an operator can act on,
// WITHOUT echoing the wrapped *url.Error — which carries the full request URL —
// or any other value the probe happens to have in hand. The address is printed
// separately by the caller because the caller knows it is not a secret.
func classify(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return "the probe timed out before the service answered"
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return "the probe was cancelled"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if urlErr.Timeout() {
			return "the probe timed out before the service answered"
		}
		err = urlErr.Err
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return "the probe timed out before the service answered"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if errors.Is(opErr.Err, context.DeadlineExceeded) {
			return "the probe timed out before the service answered"
		}
		return fmt.Sprintf("%s: %v", opErr.Op, opErr.Err)
	}
	return err.Error()
}
