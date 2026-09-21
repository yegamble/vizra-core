package jobs

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// The worker's readiness signal.
//
// WHY IT IS NOT "the process exists". The worker has no API listener, so the
// compose tree probed it with `vizra version` — a command that exits 0 whether
// or not the claim loop ever started, whether or not PostgreSQL is reachable,
// and whether or not a single job has moved since boot (meta PR #4,
// `vizra-infrastructure` seat, FINDING 3). Anything downstream that gates on
// that probe is gating on nothing.
//
// What this reports instead, per site:
//
//	database    PostgreSQL pinged AT PROBE TIME. Not remembered from the last
//	            tick: a loop spinning on a dead pool is exactly the wedged state
//	            worth catching, and it has a perfectly fresh tick.
//	claim_loop  the claim loop's last recorded progress against a staleness
//	            bound. `not_started` until the loop's first iteration, so the
//	            window between the listener opening and the worker actually
//	            working is NOT reported ready. A container runtime's
//	            `--start-period` is what covers that window; a probe that lies
//	            through it is not.
//
// Both must hold, for every site, or the endpoint answers 503.
//
// THE STALENESS BOUND, and why saturation counts as progress. The claim loop
// waits on a semaphore bounded by Options.Concurrency. With every slot busy it
// makes NO database round trip, and a job may legitimately run for
// Options.Timeout (5 minutes by default). If a busy worker recorded no
// progress, the bound would have to exceed 5 minutes to avoid calling a healthy
// worker stalled — and a five-minute bound cannot report a wedged loop inside a
// deploy window. So the loop records progress when it is saturated too, and the
// wait for a slot is bounded (see claimLoop) so that recording actually
// happens. The bound is then derived from the POLL interval, which is the
// longest gap between iterations of a loop that is working normally.
// ---------------------------------------------------------------------------

// Component and claim-loop verdict words. They are the strings the JSON body
// carries, and an operator greps them.
const (
	ComponentOK          = "ok"
	ComponentUnavailable = "unavailable"

	ClaimLoopOK         = "ok"
	ClaimLoopStalled    = "stalled"
	ClaimLoopNotStarted = "not_started"
)

// healthProbeTimeout bounds the database ping the handler performs. It is well
// under the container healthcheck timeout so a database that never answers
// produces a 503 the probe can read, rather than a killed probe with no
// message. It is also the floor of the useful staleness bound.
const healthProbeTimeout = 1500 * time.Millisecond

// HealthStaleBound is the staleness bound for the claim loop, derived from the
// poll interval.
//
// jobTimeout is accepted and DELIBERATELY IGNORED. The signature carries it so
// that the decision is visible where the bound is computed rather than only in
// a comment: saturation is recorded as progress precisely so that the bound
// does not have to be wider than the longest job.
//
// Five poll intervals, with a 15s floor. With the default 1s poll the bound is
// 15s: the longest legitimate gap between progress records is one poll interval
// plus one claim round trip, so 15s absorbs scheduler and GC jitter and a slow
// query while still reporting a wedged loop within 15s — inside one Docker
// healthcheck interval, and well inside interval×retries.
func HealthStaleBound(pollInterval, jobTimeout time.Duration) time.Duration {
	_ = jobTimeout
	b := 5 * pollInterval
	if b < 15*time.Second {
		b = 15 * time.Second
	}
	return b
}

// Pinger is the one thing the readiness check needs from a connection pool.
// *pgxpool.Pool satisfies it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// SiteHealth is one site's line in the report.
type SiteHealth struct {
	Site            string `json:"site"`
	Database        string `json:"database"`
	DatabaseDetail  string `json:"database_detail,omitempty"`
	ClaimLoop       string `json:"claim_loop"`
	LastProgressAge string `json:"last_progress_age,omitempty"`
	Saturated       bool   `json:"saturated"`
}

// HealthReport is the body of the worker's /readyz.
type HealthReport struct {
	Ready      bool         `json:"ready"`
	Status     string       `json:"status"`
	CheckedAt  time.Time    `json:"checked_at"`
	StaleAfter string       `json:"stale_after"`
	Sites      []SiteHealth `json:"sites"`
}

type siteProgress struct {
	lastProgress time.Time
	lastClaimOK  time.Time
	saturated    bool
}

// Health records what the claim loops are doing. Every method is safe for
// concurrent use: one goroutine per site writes, the HTTP handler reads.
type Health struct {
	mu         sync.Mutex
	sites      map[string]*siteProgress
	order      []string
	staleAfter time.Duration
	now        func() time.Time
}

// NewHealth builds the tracker for a known set of sites. A site the resolver
// knows but whose loop has not run yet reports `not_started`, which is not
// ready — an absent check is never a pass.
func NewHealth(sites []string, staleAfter time.Duration, now func() time.Time) *Health {
	if now == nil {
		now = time.Now
	}
	h := &Health{
		sites:      make(map[string]*siteProgress, len(sites)),
		order:      append([]string(nil), sites...),
		staleAfter: staleAfter,
		now:        now,
	}
	sort.Strings(h.order)
	for _, s := range h.order {
		h.sites[s] = &siteProgress{}
	}
	return h
}

func (h *Health) touch(site string, fn func(*siteProgress)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.sites[site]
	if !ok {
		p = &siteProgress{}
		h.sites[site] = p
		h.order = append(h.order, site)
		sort.Strings(h.order)
	}
	p.lastProgress = h.now()
	fn(p)
}

// LoopAlive records that the claim loop completed an iteration and is about to
// talk to the database. It is progress, but not evidence the database answered.
func (h *Health) LoopAlive(site string) {
	h.touch(site, func(p *siteProgress) { p.saturated = false })
}

// ClaimOK records a SUCCESSFUL claim round trip — rows or no rows, both are a
// completed conversation with PostgreSQL.
func (h *Health) ClaimOK(site string) {
	h.touch(site, func(p *siteProgress) {
		p.lastClaimOK = h.now()
		p.saturated = false
	})
}

// ClaimFailed records that the loop is alive and the claim did not succeed. The
// loop is still making progress; the database verdict comes from the probe-time
// ping, so nothing is inferred from the error here.
func (h *Health) ClaimFailed(site string) {
	h.touch(site, func(p *siteProgress) { p.saturated = false })
}

// Saturated records that every slot is busy. Progress, without a database round
// trip. See the note at the top of this file.
func (h *Health) Saturated(site string) {
	h.touch(site, func(p *siteProgress) { p.saturated = true })
}

// Report evaluates readiness now. pingers maps site handle to that site's pool.
// A site with no pinger is NOT ready: a check that cannot run is not a pass.
func (h *Health) Report(ctx context.Context, pingers map[string]Pinger) HealthReport {
	h.mu.Lock()
	now := h.now()
	stale := h.staleAfter
	order := append([]string(nil), h.order...)
	snapshot := make(map[string]siteProgress, len(h.sites))
	for k, v := range h.sites {
		snapshot[k] = *v
	}
	h.mu.Unlock()

	rep := HealthReport{
		CheckedAt:  now.UTC(),
		StaleAfter: stale.String(),
		Ready:      true,
	}
	if len(order) == 0 {
		// A worker that knows no site is not doing anything, whatever else is
		// true of it.
		rep.Ready = false
	}

	for _, s := range order {
		p := snapshot[s]
		line := SiteHealth{Site: s, Saturated: p.saturated}

		switch {
		case p.lastProgress.IsZero():
			line.ClaimLoop = ClaimLoopNotStarted
			rep.Ready = false
		case now.Sub(p.lastProgress) > stale:
			line.ClaimLoop = ClaimLoopStalled
			line.LastProgressAge = now.Sub(p.lastProgress).Round(time.Millisecond).String()
			rep.Ready = false
		default:
			line.ClaimLoop = ClaimLoopOK
			line.LastProgressAge = now.Sub(p.lastProgress).Round(time.Millisecond).String()
		}

		pinger, ok := pingers[s]
		switch {
		case !ok:
			line.Database = ComponentUnavailable
			line.DatabaseDetail = "no connection pool for this site"
			rep.Ready = false
		default:
			pctx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
			err := pinger.Ping(pctx)
			cancel()
			if err != nil {
				line.Database = ComponentUnavailable
				// safeError, not err.Error(): a pgx connection error quotes the
				// DSN it failed on, and this body is read by a probe whose
				// output lands in the container health log.
				line.DatabaseDetail = safeError(err.Error())
				rep.Ready = false
			} else {
				line.Database = ComponentOK
			}
		}

		rep.Sites = append(rep.Sites, line)
	}

	rep.Status = ComponentOK
	if !rep.Ready {
		rep.Status = ComponentUnavailable
	}
	return rep
}

// HealthHandler serves the report. 200 only when every site is ready; 503
// otherwise, with the same body either way so an operator can see WHICH site
// and WHICH component failed without a second request.
func HealthHandler(h *Health, pingers map[string]Pinger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rep := h.Report(r.Context(), pingers)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		code := http.StatusOK
		if !rep.Ready {
			code = http.StatusServiceUnavailable
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(rep)
	})
}

// LivenessHandler answers 200 while the process is running. It is deliberately
// separate from readiness and deliberately checks nothing: a liveness probe
// that pings PostgreSQL restarts a healthy worker during a database blip,
// which is the same decision internal/httpapi/probes.go makes for the api.
func LivenessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
}
