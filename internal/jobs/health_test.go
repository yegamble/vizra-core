package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yegamble/vizra-core/internal/jobs"
)

// clock is a hand-wound clock: a staleness bound tested with time.Sleep is a
// flake generator, and one tested with a frozen clock proves nothing about the
// bound moving.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

const bound = 15 * time.Second

func newHealth(t *testing.T, c *clock, sites ...string) *jobs.Health {
	t.Helper()
	return jobs.NewHealth(sites, bound, c.now)
}

func report(t *testing.T, h *jobs.Health, pingers map[string]jobs.Pinger) jobs.HealthReport {
	t.Helper()
	return h.Report(t.Context(), pingers)
}

func okPingers(sites ...string) map[string]jobs.Pinger {
	m := map[string]jobs.Pinger{}
	for _, s := range sites {
		m[s] = fakePinger{}
	}
	return m
}

// ---------------------------------------------------------------------------
// The thing this endpoint exists to refuse: "the process is up".
// ---------------------------------------------------------------------------

func TestAWorkerWhoseClaimLoopNeverStartedIsNotReady(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	h := newHealth(t, c, "default")

	r := report(t, h, okPingers("default"))
	if r.Ready {
		t.Fatal("a worker whose claim loop has never run reported READY. The metrics " +
			"listener answering is not the worker working — that is the `vizra version` " +
			"failure this endpoint replaces.")
	}
	if got := r.Sites[0].ClaimLoop; got != jobs.ClaimLoopNotStarted {
		t.Errorf("claim_loop = %q, want %q", got, jobs.ClaimLoopNotStarted)
	}
}

func TestAClaimLoopStalledPastTheBoundIsNotReady(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	h := newHealth(t, c, "default")
	h.ClaimOK("default")

	// Inside the bound: ready.
	c.add(bound - time.Millisecond)
	if r := report(t, h, okPingers("default")); !r.Ready {
		t.Fatalf("a loop that ticked %s ago is inside the %s bound but reported not ready: %+v",
			bound-time.Millisecond, bound, r.Sites)
	}
	// Past it: not ready, by name.
	c.add(2 * time.Millisecond)
	r := report(t, h, okPingers("default"))
	if r.Ready {
		t.Fatalf("a claim loop last alive %s ago reported READY against a %s bound", bound+time.Millisecond, bound)
	}
	if got := r.Sites[0].ClaimLoop; got != jobs.ClaimLoopStalled {
		t.Errorf("claim_loop = %q, want %q", got, jobs.ClaimLoopStalled)
	}
}

// A worker with every slot busy is HEALTHY, and it makes no database round trip
// while it waits. If saturation did not count as progress, the staleness bound
// would have to exceed Options.Timeout (5 minutes by default) and the probe
// would be useless. This is the reason the claim loop's semaphore wait is
// bounded rather than blocking.
func TestASaturatedWorkerStaysReadyForLongerThanAJobCanRun(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	h := newHealth(t, c, "default")
	h.ClaimOK("default")

	// Ten minutes — twice the default per-job timeout — of nothing but
	// saturation ticks.
	for i := 0; i < 600; i++ {
		c.add(time.Second)
		h.Saturated("default")
	}
	r := report(t, h, okPingers("default"))
	if !r.Ready {
		t.Fatalf("a busy worker was reported not ready: %+v", r.Sites)
	}
	if !r.Sites[0].Saturated {
		t.Error("the report does not say the worker is saturated; an operator reading " +
			"a slow queue needs to see that every slot is busy")
	}
}

// ---------------------------------------------------------------------------
// PostgreSQL is measured AT PROBE TIME, not remembered from the last tick. A
// loop that is ticking happily against a cached connection while the database
// is gone is exactly the wedged state this must catch.
// ---------------------------------------------------------------------------

func TestAnUnreachableDatabaseIsNotReadyEvenWithAFreshTick(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	h := newHealth(t, c, "default")
	h.ClaimOK("default")

	r := report(t, h, map[string]jobs.Pinger{
		"default": fakePinger{err: errors.New("dial tcp 10.0.0.5:5432: connect: connection refused")},
	})
	if r.Ready {
		t.Fatal("an unreachable database reported READY")
	}
	if got := r.Sites[0].Database; got != jobs.ComponentUnavailable {
		t.Errorf("database = %q, want %q", got, jobs.ComponentUnavailable)
	}
}

func TestAMissingPingerIsNotReadyRatherThanAssumedHealthy(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	h := newHealth(t, c, "default")
	h.ClaimOK("default")

	if r := report(t, h, map[string]jobs.Pinger{}); r.Ready {
		t.Fatal("a site with no pool to ping reported READY. An absent check is not a pass.")
	}
}

func TestEverySiteMustBeReadyForTheWorkerToBeReady(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	h := newHealth(t, c, "alpha", "beta")
	h.ClaimOK("alpha")
	h.ClaimOK("beta")

	r := report(t, h, map[string]jobs.Pinger{
		"alpha": fakePinger{},
		"beta":  fakePinger{err: errors.New("down")},
	})
	if r.Ready {
		t.Fatal("one healthy site out of two reported the whole worker READY")
	}
	if len(r.Sites) != 2 {
		t.Fatalf("the report covers %d sites, want 2", len(r.Sites))
	}
}

// ---------------------------------------------------------------------------
// The endpoint is read by a probe whose output lands in the container health
// log. A DSN in a ping error must not travel there.
// ---------------------------------------------------------------------------

func TestTheReportNeverCarriesACredential(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	h := newHealth(t, c, "default")
	h.ClaimOK("default")

	r := report(t, h, map[string]jobs.Pinger{
		"default": fakePinger{err: errors.New(
			"failed to connect to `host=db user=vizra database=vizra`: " +
				"postgres://vizra:sup3rs3cr3t@db:5432/vizra?sslmode=disable")},
	})
	blob, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"sup3rs3cr3t", "postgres://vizra:sup3rs3cr3t"} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("the readiness report carries a credential: %s", blob)
		}
	}
}

// ---------------------------------------------------------------------------
// The HTTP surface.
// ---------------------------------------------------------------------------

func TestTheHandlerAnswers503WhenTheWorkerIsNotReady(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	h := newHealth(t, c, "default")

	srv := httptest.NewServer(jobs.HealthHandler(h, okPingers("default")))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("HTTP %d, want 503", resp.StatusCode)
	}

	h.ClaimOK("default")
	resp2, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d after a tick, want 200", resp2.StatusCode)
	}
	var body jobs.HealthReport
	if err := json.NewDecoder(resp2.Body).Decode(&body); err != nil {
		t.Fatalf("the readiness body is not JSON: %v", err)
	}
	if body.StaleAfter == "" {
		t.Error("the body does not state the staleness bound it judged against")
	}
}

// The handler must bound its own work: a ping that never returns must not pin
// the probe past the container healthcheck timeout.
func TestTheHandlerBoundsAHangingPing(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	h := newHealth(t, c, "default")
	h.ClaimOK("default")

	srv := httptest.NewServer(jobs.HealthHandler(h, map[string]jobs.Pinger{"default": hangingPinger{}}))
	t.Cleanup(srv.Close)

	start := time.Now()
	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("a hanging ping held the readiness answer for %s", d)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("HTTP %d, want 503 — a database that never answers is not reachable", resp.StatusCode)
	}
}

type hangingPinger struct{}

func (hangingPinger) Ping(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// ---------------------------------------------------------------------------
// The bound itself.
// ---------------------------------------------------------------------------

// It must NOT be derived from Options.Timeout. A bound that has to exceed the
// longest job (5 minutes) cannot report a wedged loop inside a deploy window.
func TestTheStalenessBoundIsTightAndDoesNotScaleWithTheJobTimeout(t *testing.T) {
	short := jobs.HealthStaleBound(time.Second, 5*time.Minute)
	long := jobs.HealthStaleBound(time.Second, 60*time.Minute)
	if short != long {
		t.Errorf("the bound moved with Options.Timeout (%s vs %s); saturation counts as "+
			"progress precisely so it does not have to", short, long)
	}
	if short > 30*time.Second {
		t.Errorf("the bound is %s — too wide to report a wedged claim loop inside a deploy window", short)
	}
	if short < 5*time.Second {
		t.Errorf("the bound is %s — tight enough to flap on ordinary scheduler jitter", short)
	}
	// It does follow a slow poll interval, or a worker configured to poll
	// rarely would be called stalled for polling as configured.
	if slow := jobs.HealthStaleBound(30*time.Second, 5*time.Minute); slow <= short {
		t.Errorf("a 30s poll interval gave bound %s, not more than the 1s interval's %s", slow, short)
	}
}
