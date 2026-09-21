package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yegamble/vizra-core/internal/obs"
	"github.com/yegamble/vizra-core/internal/site"
	"github.com/yegamble/vizra-core/internal/store/sqlcgen"
)

// Handler runs one job. It must be idempotent: delivery is at-least-once
// because a lease can be reclaimed while the handler is still running, so every
// handler is tested by delivering its job twice (ADR-004).
//
// Return a Terminal error to bypass the retry ladder.
type Handler func(ctx context.Context, j Claimed) error

// Claimed is a leased job handed to a handler.
type Claimed struct {
	ID            uuid.UUID
	Kind          Kind
	Payload       []byte
	Attempts      int32
	MaxAttempts   int32
	CorrelationID string
	LeasedUntil   time.Time
}

// DecodePayload unmarshals into v.
func (c Claimed) DecodePayload(v any) error { return json.Unmarshal(c.Payload, v) }

// Options configure the loop.
type Options struct {
	// Lease is how long a claim is held. The heartbeat renews at Lease/3.
	Lease time.Duration
	// Timeout is the per-job wall-clock limit, applied as a context deadline.
	// Any subprocess a handler starts runs in its own process group and is
	// killed when this fires; that is the handler's contract, enforced where
	// the subprocess is started.
	Timeout time.Duration
	// Concurrency bounds how many jobs run at once in this process.
	Concurrency int
	// PollInterval is how long to wait when the queue was empty.
	PollInterval time.Duration
	// SweepInterval is how often the leader reclaims elapsed leases.
	SweepInterval time.Duration
	Logger        *slog.Logger
	// DrainGrace bounds how long Run waits for in-flight jobs after ctx is
	// cancelled. A stuck handler must not hold shutdown past the container's
	// kill deadline.
	DrainGrace time.Duration
	// WorkerID identifies the lease holder. Defaults to hostname+pid+random.
	WorkerID string
}

func (o *Options) applyDefaults() {
	if o.Lease <= 0 {
		o.Lease = 60 * time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Minute
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	if o.PollInterval <= 0 {
		o.PollInterval = time.Second
	}
	if o.SweepInterval <= 0 {
		o.SweepInterval = 2 * time.Minute
	}
	if o.DrainGrace <= 0 {
		o.DrainGrace = 20 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.WorkerID == "" {
		host, _ := os.Hostname()
		o.WorkerID = fmt.Sprintf("%s-%d-%04x", host, os.Getpid(), rand.IntN(0xffff))
	}
}

// leaderLockClassID is the first integer of the session-scoped advisory lock.
// The TWO-integer form is deliberate: golang-migrate takes a single-bigint
// advisory lock, and the two forms occupy different spaces, so a sweep can
// never collide with a migration (ADR-004).
const (
	leaderLockClassID  = 0x565A // 'VZ'
	leaderLockObjectID = 1      // the job sweep

	// outcomeWriteTimeout bounds the detached write that records a job's
	// outcome after ctx is cancelled.
	outcomeWriteTimeout = 10 * time.Second
)

// Worker runs the loop for every site the resolver knows (Q-008 item 5: the
// worker loops the registry and leases from each database).
type Worker struct {
	resolver *site.Resolver
	pools    map[string]*pgxpool.Pool
	handlers map[Kind]Handler
	opts     Options
	metrics  *Metrics
}

// NewWorker builds the loop. pools must have one entry per site handle.
func NewWorker(r *site.Resolver, pools map[string]*pgxpool.Pool, m *Metrics, opts Options) *Worker {
	opts.applyDefaults()
	w := &Worker{
		resolver: r,
		pools:    pools,
		handlers: map[Kind]Handler{},
		opts:     opts,
		metrics:  m,
	}
	w.Register(KindNoop, func(ctx context.Context, j Claimed) error { return nil })
	return w
}

// Register adds a handler. Registering an unknown kind twice is a programming
// error and panics at start-up rather than silently replacing a handler.
func (w *Worker) Register(k Kind, h Handler) {
	if _, dup := w.handlers[k]; dup && k != KindNoop {
		panic("jobs: duplicate handler for kind " + string(k))
	}
	w.handlers[k] = h
}

// Kinds is the registered set, used as the claim filter so a worker never
// leases a job it cannot run and then dead-letters it.
func (w *Worker) Kinds() []string {
	out := make([]string, 0, len(w.handlers))
	for k := range w.handlers {
		out = append(out, string(k))
	}
	return out
}

// Run blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, s := range w.resolver.Sites() {
		pool, ok := w.pools[s.Handle]
		if !ok {
			return fmt.Errorf("jobs: no pool for site %q", s.Handle)
		}
		wg.Add(2)
		go func(s site.Site, pool *pgxpool.Pool) {
			defer wg.Done()
			w.claimLoop(ctx, s, pool)
		}(s, pool)
		go func(s site.Site, pool *pgxpool.Pool) {
			defer wg.Done()
			w.sweepLoop(ctx, s, pool)
		}(s, pool)
	}
	wg.Wait()
	return ctx.Err()
}

func (w *Worker) claimLoop(ctx context.Context, s site.Site, pool *pgxpool.Pool) {
	sem := make(chan struct{}, w.opts.Concurrency)
	var inFlight sync.WaitGroup
	log := w.opts.Logger.With("site", s.Handle, "worker", w.opts.WorkerID)

loop:
	for {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break loop
		}

		claimed, err := w.claim(ctx, pool)
		if err != nil {
			<-sem
			log.Warn("jobs: claim failed", "error", safeError(err.Error()))
			w.sleep(ctx, w.opts.PollInterval)
			continue
		}
		if claimed == nil {
			<-sem
			w.sleep(ctx, w.opts.PollInterval)
			continue
		}

		inFlight.Add(1)
		go func(j Claimed) {
			defer inFlight.Done()
			defer func() { <-sem }()
			w.run(ctx, pool, j, log)
		}(*claimed)
	}
	// Drain: a job already leased by this process is allowed to finish and
	// record its outcome, or the sweep reclaims work that actually succeeded.
	//
	// BOUNDED. A handler can legitimately run for JobTimeout (5 minutes by
	// default), which is longer than a container's kill deadline, so an
	// unbounded wait here means the orchestrator SIGKILLs the process
	// mid-write and the drain achieves nothing. Past the grace period we stop
	// waiting and let the sweep do its job — which is now correct, because the
	// sweep dead-letters an exhausted row instead of requeueing it forever.
	done := make(chan struct{})
	go func() { inFlight.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(w.opts.DrainGrace):
		log.Warn("jobs: drain grace elapsed with jobs still running; their leases will be swept",
			"grace", w.opts.DrainGrace.String())
	}
}

func (w *Worker) claim(ctx context.Context, pool *pgxpool.Pool) (*Claimed, error) {
	q := sqlcgen.New(pool)
	row, err := q.ClaimJob(ctx, sqlcgen.ClaimJobParams{
		LeasedBy:      strPtr(w.opts.WorkerID),
		LeaseDuration: toInterval(w.opts.Lease),
		Kinds:         w.Kinds(),
	})
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return &Claimed{
		ID: row.ID, Kind: Kind(row.Kind), Payload: row.Payload,
		Attempts: row.Attempts, MaxAttempts: row.MaxAttempts,
		CorrelationID: row.CorrelationID, LeasedUntil: fromTimestamptz(row.LeasedUntil),
	}, nil
}

func (w *Worker) run(ctx context.Context, pool *pgxpool.Pool, j Claimed, log *slog.Logger) {
	q := sqlcgen.New(pool)
	log = log.With("job_id", j.ID.String(), "kind", string(j.Kind), "correlation_id", j.CorrelationID)

	// The per-job wall-clock timeout is a context deadline (ADR-004).
	jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.opts.Timeout)
	defer cancel()

	// Recording the OUTCOME must survive SIGTERM, or the drain below is
	// pointless: the handler would run to completion and then every
	// Complete/Retry/DeadLetter/Fail would fail instantly with "context
	// canceled", the row would stay `leased`, and two minutes later the sweep
	// would requeue it — so every rolling deploy would re-deliver every
	// in-flight job, up to Concurrency per site, forever. Idempotent handlers
	// make that survivable rather than corrupting, but it turns a routine
	// restart into guaranteed duplicate external effects: email, federation
	// delivery, IPFS pin.
	//
	// Detached from ctx, and short: a database that is also going away must not
	// hold shutdown open.
	recCtx, recCancel := context.WithTimeout(context.WithoutCancel(ctx), outcomeWriteTimeout)
	defer recCancel()

	// Heartbeat at lease/3. It is conditional on still holding the lease: if the
	// sweep took the job, renewal fails and the handler's context is cancelled,
	// so two workers cannot both believe they own it.
	hbCtx, stopHB := context.WithCancel(jobCtx)
	defer stopHB()
	go w.heartbeat(hbCtx, q, j, cancel, log)

	h, ok := w.handlers[j.Kind]
	if !ok {
		// Claim filters by registered kind, so this means the filter and the
		// registry disagreed. Terminal: retrying would loop forever.
		w.finishFailed(recCtx, q, j, fmt.Sprintf("no handler registered for kind %s", j.Kind), log)
		return
	}

	err := h(jobCtx, j)
	stopHB()

	switch {
	case err == nil:
		n, cerr := q.CompleteJob(recCtx, sqlcgen.CompleteJobParams{ID: j.ID, LeasedBy: strPtr(w.opts.WorkerID)})
		if cerr != nil {
			log.Error("jobs: recording success failed", "error", safeError(cerr.Error()))
			return
		}
		if n == 0 {
			// The lease was lost mid-run. The job will be re-run by whoever holds
			// it now; that is why handlers must be idempotent.
			log.Warn("jobs: lease was lost before completion could be recorded; the job will be redelivered")
		}
	case IsTerminal(err):
		w.finishFailed(recCtx, q, j, err.Error(), log)
	case j.Attempts >= j.MaxAttempts:
		n, derr := q.DeadLetterJob(recCtx, sqlcgen.DeadLetterJobParams{
			ID: j.ID, LeasedBy: strPtr(w.opts.WorkerID), LastError: strPtr(safeError(err.Error())),
		})
		if derr != nil || n == 0 {
			log.Warn("jobs: recording dead-letter failed", "rows", n, "error", safeError(errText(derr)))
		}
		// safeError, not err.Error(): the SAME path last_error already takes.
		// Passing the raw handler error made this line safe only because
		// cmd/api and cmd/worker install obs.NewLogger with slog.SetDefault —
		// any other caller that builds a Worker with its own logger logged the
		// credential in the clear (verifier FINDING V-2). Redaction belongs at
		// the call site, not in the process wiring.
		log.Error("jobs: exhausted attempts", "attempts", j.Attempts, "error", safeError(err.Error()))
	default:
		backoff := Backoff(j.Attempts) + jitter(Backoff(j.Attempts))
		n, rerr := q.RetryJob(recCtx, sqlcgen.RetryJobParams{
			ID: j.ID, LeasedBy: strPtr(w.opts.WorkerID),
			Backoff: toInterval(backoff), LastError: strPtr(safeError(err.Error())),
		})
		if rerr != nil || n == 0 {
			log.Warn("jobs: scheduling retry failed", "rows", n, "error", safeError(errText(rerr)))
		}
		// safeError for the same reason as the dead-letter line above. The
		// verifier named the other two; this third site passes the same raw
		// handler error and would have been the one left leaking.
		log.Warn("jobs: retrying", "attempt", j.Attempts, "backoff", backoff.String(),
			"error", safeError(err.Error()))
	}
}

func (w *Worker) finishFailed(ctx context.Context, q *sqlcgen.Queries, j Claimed, msg string, log *slog.Logger) {
	if _, err := q.FailJob(ctx, sqlcgen.FailJobParams{
		ID: j.ID, LeasedBy: strPtr(w.opts.WorkerID), LastError: strPtr(safeError(msg)),
	}); err != nil {
		log.Error("jobs: recording terminal failure failed", "error", safeError(err.Error()))
	}
	// safeError: msg is the raw handler error, exactly as it reaches
	// last_error above (verifier FINDING V-2).
	log.Error("jobs: terminal failure", "error", safeError(msg))
}

func (w *Worker) heartbeat(ctx context.Context, q *sqlcgen.Queries, j Claimed, cancelJob context.CancelFunc, log *slog.Logger) {
	t := time.NewTicker(w.opts.Lease / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, err := q.HeartbeatJob(ctx, sqlcgen.HeartbeatJobParams{
				ID: j.ID, LeasedBy: strPtr(w.opts.WorkerID), LeaseDuration: toInterval(w.opts.Lease),
			})
			if err != nil {
				if isNoRows(err) {
					// Somebody else owns this job now. Stop working on it: two
					// workers writing the same effect is the failure this exists
					// to prevent.
					log.Warn("jobs: lease lost; abandoning the job")
					cancelJob()
					return
				}
				log.Warn("jobs: heartbeat failed", "error", safeError(err.Error()))
			}
		}
	}
}

// sweepLoop reclaims elapsed leases. It is LEADER-GATED: only the process
// holding the advisory lock sweeps, so N replicas do not each requeue the
// others' work. There is deliberately no boot blanket requeue.
func (w *Worker) sweepLoop(ctx context.Context, s site.Site, pool *pgxpool.Pool) {
	log := w.opts.Logger.With("site", s.Handle, "worker", w.opts.WorkerID)
	t := time.NewTicker(w.opts.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.sweepOnce(ctx, pool, log); err != nil {
				log.Warn("jobs: sweep failed", "error", safeError(err.Error()))
			}
			if w.metrics != nil {
				if _, err := Collect(ctx, pool, w.metrics); err != nil {
					log.Warn("jobs: metric collection failed", "error", safeError(err.Error()))
				}
			}
		}
	}
}

func (w *Worker) sweepOnce(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	// The advisory lock is SESSION-scoped, so it must be taken and released on
	// the same connection. Acquire one for the duration.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	q := sqlcgen.New(conn)
	got, err := q.TryAdvisoryLock(ctx, sqlcgen.TryAdvisoryLockParams{
		ClassID: leaderLockClassID, ObjectID: leaderLockObjectID,
	})
	if err != nil {
		return err
	}
	if !got {
		return nil // another replica is the leader
	}
	defer func() {
		_, _ = q.AdvisoryUnlock(context.WithoutCancel(ctx), sqlcgen.AdvisoryUnlockParams{
			ClassID: leaderLockClassID, ObjectID: leaderLockObjectID,
		})
	}()

	rows, err := q.SweepExpiredLeases(ctx)
	if err != nil {
		return err
	}
	var requeued, dead int
	for _, r := range rows {
		if r.State == "dead" {
			dead++
			// Named individually: a job that died because its worker kept
			// stopping is an operator signal, not a statistic.
			log.Error("jobs: dead-lettered after repeated lease loss",
				"job_id", r.ID.String(), "kind", r.Kind, "attempts", r.Attempts)
			continue
		}
		requeued++
	}
	if requeued > 0 || dead > 0 {
		log.Warn("jobs: swept elapsed leases", "requeued", requeued, "dead_lettered", dead)
	}
	return nil
}

func (w *Worker) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	// +/- 10%, so a thundering herd of retries spreads out.
	return time.Duration(rand.Int64N(int64(d/5))) - d/10
}

// safeError is what may be written to jobs.last_error.
//
// REDACT FIRST, THEN TRUNCATE. ADR-002 says secrets do not enter a queryable
// table, and last_error is both queryable and rendered in /admin/jobs and in
// any pg_dump an operator shares. A Go HTTP error is a *url.Error that formats
// as `Post "https://user:pw@host/path": ...`, so an M1 handler doing an S3 put,
// a federation delivery or a webhook call will produce exactly that text.
//
// The order matters: truncating first could cut a credential in half and store
// the first 2000 bytes of it, which is still a leak. Redacting first means a
// secret straddling the boundary is replaced before the cut is made.
//
// A CHECK constraint cannot express this, so the control belongs here — which
// is the same argument the logger already made for obs.Redact.
func safeError(s string) string { return truncate(obs.Redact(s)) }

// truncate bounds what a handler's error text can write into a row. The
// database also enforces this (jobs_last_error_bounded, 4096), so this is the
// friendly half of a bound that is real either way.
//
// IT MUST NOT CUT A RUNE IN HALF. last_error is a `text` column and PostgreSQL
// rejects an invalid UTF-8 byte sequence outright —
//
//	ERROR: invalid byte sequence for encoding "UTF8": 0xe6 0xe2 0x80
//
// — so a mid-rune slice does not merely mangle the message: the whole write
// FAILS, the row stays `leased` with its cause unrecorded, and the sweep
// re-runs the job having lost the one piece of evidence an operator needed.
//
// A multibyte error message is ordinary, not exotic: a filename, a photo
// title, a remote server's error body, or the em dash in our own text below.
func truncate(s string) string {
	const max = 2000
	if len(s) <= max {
		return s
	}
	// Back off to a rune START. Continuation bytes are 0b10xxxxxx, so this
	// takes at most three steps, and s[:cut] then ends exactly on a boundary.
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	out := s[:cut]
	// Belt and braces. If the INPUT was already invalid UTF-8 — a subprocess
	// dumping raw bytes into stderr, say — the arithmetic above cannot fix it,
	// and the column would still reject the write.
	if !utf8.ValidString(out) {
		out = strings.ToValidUTF8(out, "")
	}
	return out + "… (truncated)"
}

// isNoRows distinguishes "the claim found nothing" from a real database error.
// pgx returns ErrNoRows from QueryRow.Scan; sqlc propagates it unwrapped.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// errText renders an error for a log field, including the "no error" case.
// Logging only a row count when the write failed says "rows=0" and nothing
// about why, which is how an outcome-write failure stays invisible.
func errText(err error) string {
	if err == nil {
		return "none (the statement matched no row)"
	}
	return err.Error()
}
