//go:build integration

// Package integration holds the tests that need real PostgreSQL and a real
// RESP server. They are behind a build tag so `make test-race` stays fast, and
// `make test-integration` REFUSES to run rather than skipping when the services
// are absent — a skipped required lane is not a pass (AGENTS.md).
//
// The cache tests run twice in CI, against pinned Valkey and against Redis
// 7.2.x, because go-redis publishes no Valkey support statement and Valkey
// publishes no formal protocol-compatibility guarantee (ADR-001 Q-004). The
// matrix is permanent.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yegamble/vizra-core/internal/cache"
	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/db"
	"github.com/yegamble/vizra-core/internal/httpapi"
	"github.com/yegamble/vizra-core/internal/jobs"
	"github.com/yegamble/vizra-core/internal/migrate"
	"github.com/yegamble/vizra-core/internal/search"
	"github.com/yegamble/vizra-core/internal/site"
	"github.com/yegamble/vizra-core/internal/store/sqlcgen"
)

// mustEnv fails rather than skips. A skipped required lane is not a pass.
func mustEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("%s is not set. This lane is BLOCKED, not skipped: it cannot report a pass "+
			"for something it did not run.", key)
	}
	return v
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.LoadFrom(func(k string) (string, bool) {
		switch k {
		case "DATABASE_URL":
			return mustEnv(t, "VIZRA_TEST_DATABASE_URL"), true
		case "VIZRA_CACHE_URL":
			return mustEnv(t, "VIZRA_TEST_CACHE_URL"), true
		case "VIZRA_MODE":
			return "development", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("test configuration is invalid: %v", err)
	}
	return cfg
}

// freshDatabase drops and recreates the public schema, then migrates. Every
// test that needs a schema calls it, so no test depends on another's leftovers.
func freshDatabase(t *testing.T) (*config.Config, *site.Resolver, *pgxpool.Pool) {
	t.Helper()
	cfg := testConfig(t)
	resolver := site.NewResolver(cfg)
	ctx := t.Context()

	pools, err := db.Open(ctx, resolver)
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	t.Cleanup(pools.Close)
	pool := pools.Default()

	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("resetting the test schema: %v", err)
	}

	applied, version, err := migrate.Apply(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("migrating an empty database: %v", err)
	}
	if !applied {
		t.Fatal("migrate reported no change on a freshly dropped schema")
	}
	embedded, err := migrate.EmbeddedVersion()
	if err != nil {
		t.Fatal(err)
	}
	if version != embedded {
		t.Fatalf("migrated to version %d, binary embeds %d", version, embedded)
	}
	return cfg, resolver, pool
}

// ---------------------------------------------------------------------------
// Migrations
// ---------------------------------------------------------------------------

// VZ-FOUND-003 success case: `migrate up` from the release binary applies
// cleanly on an empty database.
func TestMigrateUpOnEmptyDatabase(t *testing.T) {
	_, _, pool := freshDatabase(t)
	ctx := t.Context()

	embedded, _ := migrate.EmbeddedVersion()
	st := migrate.Probe(ctx, pool, embedded)
	if st.State != migrate.StateCurrent {
		t.Fatalf("schema state = %s after migrating, want current (%+v)", st.State, st)
	}
	if st.Dirty {
		t.Fatal("the ledger is dirty after a clean migration")
	}

	// The seeded rows ADR-005 and ADR-007 require must exist exactly once.
	q := sqlcgen.New(pool)
	n, err := q.CountSites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("sites has %d rows, want exactly 1 (ADR-007: exactly one row in core)", n)
	}
	s, err := q.GetDefaultSite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Handle != "default" {
		t.Fatalf("default site handle = %q, want %q", s.Handle, "default")
	}
	if s.PrivacyMode != "public" {
		t.Fatalf("default site privacy_mode = %q, want public", s.PrivacyMode)
	}

	loc, err := q.GetDefaultStorageLocation(ctx)
	if err != nil {
		t.Fatalf("no default storage_locations row (ADR-005 requires one `local` default): %v", err)
	}
	if loc.Handle != "local" || loc.Kind != "local" || !loc.IsDefault {
		t.Fatalf("default storage location = %+v, want the local default", loc)
	}
}

// Migrating twice must be a no-op, or a redeploy of the same tag would fail.
func TestMigrateIsIdempotent(t *testing.T) {
	cfg, _, _ := freshDatabase(t)
	applied, _, err := migrate.Apply(t.Context(), cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("second migrate run: %v", err)
	}
	if applied {
		t.Fatal("the second migrate run applied changes; migrating an already-current database must be a no-op")
	}
}

// The database enforces the invariants rather than trusting application
// discipline (AGENTS.md: database constraints for concurrency invariants).
func TestSchemaConstraintsAreEnforced(t *testing.T) {
	_, _, pool := freshDatabase(t)
	ctx := t.Context()

	t.Run("only one default storage location", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO storage_locations (id, handle, kind, is_default) VALUES ($1,'second','s3',true)`,
			uuid.New())
		if err == nil {
			t.Fatal("a second default storage location was accepted; uploads would have two destinations")
		}
	})

	t.Run("a read-only location cannot be the default", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO storage_locations (id, handle, kind, is_default, read_only) VALUES ($1,'ro','s3',false,true)`,
			uuid.New())
		if err != nil {
			t.Fatalf("a non-default read-only location must be allowed: %v", err)
		}
		_, err = pool.Exec(ctx, `UPDATE storage_locations SET is_default = true WHERE handle = 'ro'`)
		if err == nil {
			t.Fatal("a read-only location became the default; new objects would have nowhere to go")
		}
	})

	t.Run("a job state outside the enum is rejected", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO jobs (id, kind, payload, state, priority, attempts, max_attempts, run_after, correlation_id, created_at, updated_at)
			 VALUES ($1,'noop','{}','invented',100,0,5,now(),'c',now(),now())`, uuid.New())
		if err == nil {
			t.Fatal("a job with an unknown state was accepted")
		}
	})

	t.Run("a leased job must name its holder", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO jobs (id, kind, payload, state, priority, attempts, max_attempts, run_after, correlation_id, created_at, updated_at)
			 VALUES ($1,'noop','{}','leased',100,1,5,now(),'c',now(),now())`, uuid.New())
		if err == nil {
			t.Fatal("a leased job with no leased_by/leased_until was accepted; the sweep could not tell it from a corrupt row")
		}
	})

	t.Run("a system audit event cannot claim a user", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO audit_events (id, actor_kind, actor_user_id, action, subject_type)
			 VALUES ($1,'system',$2,'x','y')`, uuid.New(), uuid.New())
		if err == nil {
			t.Fatal("a system actor with a user id was accepted")
		}
		_, err = pool.Exec(ctx,
			`INSERT INTO audit_events (id, actor_kind, action, subject_type) VALUES ($1,'user','x','y')`, uuid.New())
		if err == nil {
			t.Fatal("a user actor with no user id was accepted")
		}
	})
}

// ---------------------------------------------------------------------------
// Jobs (ADR-004)
// ---------------------------------------------------------------------------

func enqueue(t *testing.T, pool *pgxpool.Pool, j jobs.NewJob) jobs.Enqueued {
	t.Helper()
	ctx := t.Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out, err := jobs.Enqueue(ctx, tx, j)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return out
}

// The ADR-004 invariant, end to end: a job enqueued in a transaction that is
// ROLLED BACK must not exist. That is what "a committed mutation can never lose
// its side effect" buys — and its mirror, "an abandoned mutation never gains
// one".
func TestEnqueueIsRolledBackWithItsTransaction(t *testing.T) {
	_, _, pool := freshDatabase(t)
	ctx := t.Context()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out, err := jobs.Enqueue(ctx, tx, jobs.NewJob{Kind: jobs.KindNoop, CorrelationID: "rollback"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := sqlcgen.New(pool).GetJob(ctx, out.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the job survived its rolled-back transaction (err = %v)", err)
	}
}

// Idempotency: a second enqueue while one is live returns the in-flight job
// rather than creating a duplicate; once the first has finished, a new one is
// allowed.
func TestEnqueueIdempotency(t *testing.T) {
	_, _, pool := freshDatabase(t)
	ctx := t.Context()

	first := enqueue(t, pool, jobs.NewJob{Kind: jobs.KindNoop, IdempotencyKey: "asset-42", CorrelationID: "c1"})
	second := enqueue(t, pool, jobs.NewJob{Kind: jobs.KindNoop, IdempotencyKey: "asset-42", CorrelationID: "c2"})

	if second.ID != first.ID {
		t.Fatalf("a duplicate enqueue created a second job (%s and %s)", first.ID, second.ID)
	}
	if !second.AlreadyInFlight {
		t.Fatal("the duplicate enqueue did not report AlreadyInFlight")
	}

	// The unique index is PARTIAL over live states: once the job is terminal,
	// the same logical work may legitimately be enqueued again.
	if _, err := pool.Exec(ctx,
		`UPDATE jobs SET state='succeeded', finished_at=now() WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	third := enqueue(t, pool, jobs.NewJob{Kind: jobs.KindNoop, IdempotencyKey: "asset-42", CorrelationID: "c3"})
	if third.ID == first.ID || third.AlreadyInFlight {
		t.Fatal("a finished job still blocked a legitimate re-enqueue")
	}
}

// PostgreSQL is the SINGLE CLOCK AUTHORITY for job eligibility.
//
// `run_after` was stamped from the APPLICATION host clock when the caller did
// not ask for a delay, while ClaimJob tests `run_after <= now()` against the
// DATABASE clock. Every other timestamp in a job's life — created_at,
// leased_until, RetryJob's run_after, the sweep's `leased_until < now()`,
// finished_at — is assigned by the database, so this was the only place in the
// system where two clocks were compared.
//
// The consequence, measured on this machine at 415a6d1 with the database ~1 ms
// behind the host: a job whose intent is "run now" was not eligible until the
// skew had elapsed, and a claim issued inside that window got pgx.ErrNoRows on
// a row that was sitting right there, queued and unleased. In the integration
// suite that made TestHeartbeatRequiresStillHoldingTheLease and
// TestSweepReclaimsOnlyElapsedLeases fail 3 runs in 20 (verifier FINDING V-1).
// In production the API and the database are different containers on different
// hosts and ordinary NTP skew is milliseconds to seconds, so the same window
// hides freshly enqueued work from the claim loop for real.
//
// The rule this test pins: when the caller does NOT specify RunAfter, the
// database supplies it, and it is therefore the same instant as created_at. An
// explicit RunAfter is a deliberate wall-clock decision by the caller and is
// stored verbatim.
func TestRunAfterComesFromTheDatabaseClockNotTheApplicationHost(t *testing.T) {
	_, _, pool := freshDatabase(t)
	ctx := t.Context()
	q := sqlcgen.New(pool)

	// 1. No RunAfter: the database assigns it, in the same transaction and
	//    therefore from the same now() as created_at.
	j := enqueue(t, pool, jobs.NewJob{Kind: jobs.KindNoop, CorrelationID: "db-clock"})

	var sameInstant bool
	var runAfter, createdAt time.Time
	var skewMicros int64
	if err := pool.QueryRow(ctx,
		`SELECT run_after = created_at, run_after, created_at,
		        (EXTRACT(EPOCH FROM (run_after - created_at)) * 1e6)::bigint
		   FROM jobs WHERE id = $1`, j.ID,
	).Scan(&sameInstant, &runAfter, &createdAt, &skewMicros); err != nil {
		t.Fatal(err)
	}
	if !sameInstant {
		t.Fatalf("run_after (%s) is not created_at (%s): they differ by %d µs, so run_after was "+
			"read from a clock OTHER than the one ClaimJob compares it against. A job that is "+
			"meant to run now is then not claimable for the duration of that difference.",
			runAfter.UTC().Format(time.RFC3339Nano), createdAt.UTC().Format(time.RFC3339Nano), skewMicros)
	}

	// 2. And the consequence that matters: it is claimable IMMEDIATELY, with no
	//    settling time, on the very next statement.
	if _, err := q.ClaimJob(ctx, sqlcgen.ClaimJobParams{
		LeasedBy: ptr("worker-now"), LeaseDuration: interval(time.Minute),
		Kinds: []string{string(jobs.KindNoop)},
	}); err != nil {
		t.Fatalf("a job enqueued to run now was not claimable on the next statement: %v", err)
	}

	// 3. An explicit RunAfter is the caller's decision and must survive intact —
	//    otherwise "the database assigns now()" would have quietly become
	//    "the database always assigns now()" and every scheduled job would fire
	//    at once.
	future := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Microsecond)
	scheduled := enqueue(t, pool, jobs.NewJob{
		Kind: jobs.KindNoop, CorrelationID: "scheduled", RunAfter: future,
	})
	var storedFuture time.Time
	var eligible bool
	if err := pool.QueryRow(ctx,
		`SELECT run_after, run_after <= now() FROM jobs WHERE id = $1`, scheduled.ID,
	).Scan(&storedFuture, &eligible); err != nil {
		t.Fatal(err)
	}
	if !storedFuture.UTC().Equal(future) {
		t.Fatalf("an explicit RunAfter was not stored verbatim: stored %s, asked for %s",
			storedFuture.UTC().Format(time.RFC3339Nano), future.Format(time.RFC3339Nano))
	}
	if eligible {
		t.Fatal("a job scheduled two hours out is already eligible; the delay was discarded")
	}
	// The value the caller gets back is the value the database holds.
	if !scheduled.RunAfter.UTC().Equal(future) {
		t.Fatalf("Enqueue returned run_after %s, the row holds %s",
			scheduled.RunAfter.UTC().Format(time.RFC3339Nano), future.Format(time.RFC3339Nano))
	}
	// And a claim must not take it.
	if _, err := q.ClaimJob(ctx, sqlcgen.ClaimJobParams{
		LeasedBy: ptr("worker-now"), LeaseDuration: interval(time.Minute),
		Kinds: []string{string(jobs.KindNoop)},
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a claim took a job scheduled two hours out (err = %v)", err)
	}

	// 4. A RunAfter in the PAST is equally deliberate — a retry ladder writes
	//    one — and must not be pushed forward to now().
	past := time.Now().Add(-90 * time.Minute).UTC().Truncate(time.Microsecond)
	backdated := enqueue(t, pool, jobs.NewJob{
		Kind: jobs.KindNoop, CorrelationID: "backdated", RunAfter: past,
	})
	var storedPast time.Time
	if err := pool.QueryRow(ctx,
		`SELECT run_after FROM jobs WHERE id = $1`, backdated.ID).Scan(&storedPast); err != nil {
		t.Fatal(err)
	}
	if !storedPast.UTC().Equal(past) {
		t.Fatalf("an explicit past RunAfter was rewritten to %s, asked for %s",
			storedPast.UTC().Format(time.RFC3339Nano), past.Format(time.RFC3339Nano))
	}
}

// Two concurrent claims must never take the same row (FOR UPDATE SKIP LOCKED).
func TestConcurrentClaimsNeverTakeTheSameJob(t *testing.T) {
	_, resolver, pool := freshDatabase(t)
	ctx := t.Context()

	const n = 20
	for i := range n {
		enqueue(t, pool, jobs.NewJob{Kind: jobs.KindNoop, CorrelationID: fmt.Sprintf("c%d", i)})
	}

	var mu sync.Mutex
	seen := map[uuid.UUID]int{}
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			q := sqlcgen.New(pool)
			for {
				row, err := q.ClaimJob(ctx, sqlcgen.ClaimJobParams{
					LeasedBy:      ptr(fmt.Sprintf("worker-%d", w)),
					LeaseDuration: interval(time.Minute),
					Kinds:         []string{string(jobs.KindNoop)},
				})
				if err != nil {
					return // no rows left
				}
				mu.Lock()
				seen[row.ID]++
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	_ = resolver

	if len(seen) != n {
		t.Fatalf("claimed %d distinct jobs, want %d", len(seen), n)
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("job %s was claimed %d times; SKIP LOCKED did not hold", id, count)
		}
	}
}

// A heartbeat must only renew a lease the caller still holds. Without that,
// a worker whose lease was swept could keep extending it and two workers would
// both believe they own the job.
func TestHeartbeatRequiresStillHoldingTheLease(t *testing.T) {
	_, _, pool := freshDatabase(t)
	ctx := t.Context()
	q := sqlcgen.New(pool)

	enqueue(t, pool, jobs.NewJob{Kind: jobs.KindNoop, CorrelationID: "c"})
	claimed, err := q.ClaimJob(ctx, sqlcgen.ClaimJobParams{
		LeasedBy: ptr("worker-a"), LeaseDuration: interval(time.Minute),
		Kinds: []string{string(jobs.KindNoop)},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := q.HeartbeatJob(ctx, sqlcgen.HeartbeatJobParams{
		ID: claimed.ID, LeasedBy: ptr("worker-a"), LeaseDuration: interval(time.Minute),
	}); err != nil {
		t.Fatalf("the lease holder could not renew: %v", err)
	}
	if _, err := q.HeartbeatJob(ctx, sqlcgen.HeartbeatJobParams{
		ID: claimed.ID, LeasedBy: ptr("worker-b"), LeaseDuration: interval(time.Minute),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("worker-b renewed a lease it does not hold (err = %v)", err)
	}
	// The same rule for completion: a worker that lost its lease must not be
	// able to record success over the new holder's work.
	rows, err := q.CompleteJob(ctx, sqlcgen.CompleteJobParams{ID: claimed.ID, LeasedBy: ptr("worker-b")})
	if err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatal("worker-b completed a job it does not hold")
	}
}

// The sweep reclaims ONLY elapsed leases, and there is no boot blanket requeue.
func TestSweepReclaimsOnlyElapsedLeases(t *testing.T) {
	_, _, pool := freshDatabase(t)
	ctx := t.Context()
	q := sqlcgen.New(pool)

	enqueue(t, pool, jobs.NewJob{Kind: jobs.KindNoop, CorrelationID: "live"})
	enqueue(t, pool, jobs.NewJob{Kind: jobs.KindNoop, CorrelationID: "stale"})

	live, err := q.ClaimJob(ctx, sqlcgen.ClaimJobParams{
		LeasedBy: ptr("w"), LeaseDuration: interval(time.Hour), Kinds: []string{string(jobs.KindNoop)},
	})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := q.ClaimJob(ctx, sqlcgen.ClaimJobParams{
		LeasedBy: ptr("w"), LeaseDuration: interval(time.Hour), Kinds: []string{string(jobs.KindNoop)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET leased_until = now() - interval '1 minute' WHERE id=$1`, stale.ID); err != nil {
		t.Fatal(err)
	}

	reclaimed, err := q.SweepExpiredLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 1 || reclaimed[0].ID != stale.ID {
		t.Fatalf("the sweep reclaimed %+v, want only the elapsed lease %s", reclaimed, stale.ID)
	}
	after, err := q.GetJob(ctx, live.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "leased" {
		t.Fatalf("the live lease was reclaimed; state = %s", after.State)
	}
}

// The full loop: a worker claims, runs and completes. And the handler is
// delivered its job twice, which is the ADR-004 idempotency test.
func TestWorkerRunsAndCompletesAJob(t *testing.T) {
	_, resolver, pool := freshDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	var mu sync.Mutex
	deliveries := map[string]int{}
	done := make(chan struct{}, 2)

	w := jobs.NewWorker(resolver, map[string]*pgxpool.Pool{"default": pool}, nil, jobs.Options{
		Lease: 5 * time.Second, Timeout: 10 * time.Second, Concurrency: 2,
		PollInterval: 50 * time.Millisecond, SweepInterval: time.Second,
		WorkerID: "test-worker",
	})
	w.Register("counting", func(ctx context.Context, j jobs.Claimed) error {
		var p struct {
			Key string `json:"key"`
		}
		if err := j.DecodePayload(&p); err != nil {
			return jobs.Terminal{Err: err}
		}
		mu.Lock()
		deliveries[p.Key]++
		mu.Unlock()
		done <- struct{}{}
		return nil
	})

	startWorker(t, ctx, cancel, w)

	a := enqueue(t, pool, jobs.NewJob{Kind: "counting", Payload: map[string]string{"key": "k"}, CorrelationID: "c1"})
	b := enqueue(t, pool, jobs.NewJob{Kind: "counting", Payload: map[string]string{"key": "k"}, CorrelationID: "c2"})

	for range 2 {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("the worker did not run both jobs within the timeout")
		}
	}

	// The handler saw the same logical work twice; an idempotent handler must
	// tolerate that, which is why every handler is tested this way.
	mu.Lock()
	got := deliveries["k"]
	mu.Unlock()
	if got != 2 {
		t.Fatalf("handler ran %d times, want 2", got)
	}

	waitFor(t, ctx, func() bool {
		q := sqlcgen.New(pool)
		ja, err1 := q.GetJob(ctx, a.ID)
		jb, err2 := q.GetJob(ctx, b.ID)
		return err1 == nil && err2 == nil && ja.State == "succeeded" && jb.State == "succeeded"
	}, "both jobs to reach succeeded")
}

// A handler that keeps failing walks the ladder and then dead-letters; a
// terminal error bypasses the ladder entirely.
func TestRetryLadderAndTerminalFailure(t *testing.T) {
	_, resolver, pool := freshDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	w := jobs.NewWorker(resolver, map[string]*pgxpool.Pool{"default": pool}, nil, jobs.Options{
		Lease: 5 * time.Second, Timeout: 10 * time.Second, Concurrency: 2,
		PollInterval: 50 * time.Millisecond, SweepInterval: time.Hour, WorkerID: "test-worker",
	})
	w.Register("always-retryable", func(context.Context, jobs.Claimed) error {
		return errors.New("transient")
	})
	w.Register("always-terminal", func(context.Context, jobs.Claimed) error {
		return jobs.Terminal{Err: errors.New("malformed payload")}
	})
	startWorker(t, ctx, cancel, w)

	retryable := enqueue(t, pool, jobs.NewJob{Kind: "always-retryable", MaxAttempts: 1, CorrelationID: "c1"})
	terminal := enqueue(t, pool, jobs.NewJob{Kind: "always-terminal", MaxAttempts: 5, CorrelationID: "c2"})

	q := sqlcgen.New(pool)
	waitFor(t, ctx, func() bool {
		j, err := q.GetJob(ctx, retryable.ID)
		return err == nil && j.State == "dead"
	}, "the exhausted job to be dead-lettered")

	waitFor(t, ctx, func() bool {
		j, err := q.GetJob(ctx, terminal.ID)
		return err == nil && j.State == "failed"
	}, "the terminal job to fail without retrying")

	j, err := q.GetJob(ctx, terminal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Attempts != 1 {
		t.Fatalf("the terminal job was attempted %d times; a terminal error must bypass the ladder", j.Attempts)
	}
	if j.LastError == nil || !strings.Contains(*j.LastError, "malformed payload") {
		t.Fatalf("last_error = %v, want the terminal cause", j.LastError)
	}
}

// ---------------------------------------------------------------------------
// Cache (ADR-001 Q-004) — this file runs against Valkey and Redis 7.2 in CI.
// ---------------------------------------------------------------------------

func TestCacheFlavourAndVersionAreIdentified(t *testing.T) {
	cfg := testConfig(t)
	c, err := cache.Open(cfg.CacheURL, cfg.CacheNamespace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Ping(t.Context()); err != nil {
		t.Fatalf("the cache is unreachable: %v", err)
	}
	info, err := c.Identify(t.Context())
	if err != nil {
		t.Fatalf("INFO server did not identify the cache: %v", err)
	}
	if info.Flavour != cache.FlavourValkey && info.Flavour != cache.FlavourRedis {
		t.Fatalf("flavour = %q; doctor must be able to name the server it found", info.Flavour)
	}
	if info.Version == "" {
		t.Fatal("no version reported")
	}
	t.Logf("cache identified as %s", info.String())

	// The expected flavour is asserted when CI says which image this is, so a
	// matrix leg that silently ran against the wrong server is caught.
	if want := os.Getenv("VIZRA_TEST_CACHE_FLAVOUR"); want != "" && string(info.Flavour) != want {
		t.Fatalf("this matrix leg expected %s but connected to %s", want, info.Flavour)
	}
}

// The rate limiter must work on whichever server answered, since Vizra uses
// only the Redis 7.2 / Valkey 7.2 command set.
func TestRateLimiterOnTheConnectedServer(t *testing.T) {
	cfg := testConfig(t)
	c, err := cache.Open(cfg.CacheURL, cfg.CacheNamespace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := t.Context()
	if err := c.FlushAll(ctx); err != nil {
		t.Fatal(err)
	}

	l := cache.NewFallbackLimiter(c)
	key := "test-" + uuid.NewString()
	for i := range 3 {
		allowed, _ := l.Allow(ctx, key, 3, time.Minute)
		if !allowed {
			t.Fatalf("request %d of 3 was refused under a limit of 3", i+1)
		}
	}
	if allowed, _ := l.Allow(ctx, key, 3, time.Minute); allowed {
		t.Fatal("the 4th request was allowed under a limit of 3")
	}
	if l.Degraded() {
		t.Fatal("the limiter reports degraded while the cache is up")
	}
}

// ---------------------------------------------------------------------------
// D6: the golden path passes after FLUSHALL (ADR-001 Q-004)
// ---------------------------------------------------------------------------

// Eviction is configured as allkeys-lru, so the cache may lose ANY key at any
// time. This asserts the claim that makes that safe: nothing durable lives in
// the cache, so emptying it entirely must change no outcome.
func TestGoldenPathPassesAfterFlushAll(t *testing.T) {
	cfg, resolver, pool := freshDatabase(t)
	ctx := t.Context()

	cacheClient, err := cache.Open(cfg.CacheURL, cfg.CacheNamespace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cacheClient.Close() }()

	limiter := cache.NewFallbackLimiter(cacheClient)
	srv := httpapi.New(httpapi.Deps{
		Config:                cfg,
		Resolver:              resolver,
		Pools:                 poolsOf(t, resolver),
		Cache:                 cacheClient,
		Limiter:               limiter,
		Search:                search.NewService(search.NewSQL(), nil, nil),
		EmbeddedSchemaVersion: mustEmbedded(t),
		QueueSnapshot: func(ctx context.Context) (jobs.Snapshot, error) {
			return jobs.Collect(ctx, pool, nil)
		},
		// The single-flight cache would otherwise serve a stale readiness from
		// before the flush, which would make this test prove nothing.
		Now: time.Now,
	})

	goldenPath := func(stage string) {
		t.Helper()

		// 1. liveness
		expect(t, srv, "/healthz", http.StatusOK, func(body map[string]any) error {
			if body["status"] != "ok" {
				return fmt.Errorf("status = %v", body["status"])
			}
			return nil
		}, stage)

		// 2. readiness, with every component named
		expect(t, srv, "/readyz", http.StatusOK, func(body map[string]any) error {
			if body["status"] != "ok" {
				return fmt.Errorf("status = %v, components = %v", body["status"], body["components"])
			}
			return nil
		}, stage)

		// 3. schema ledger
		expect(t, srv, "/schemaz", http.StatusOK, func(body map[string]any) error {
			if body["state"] != "current" {
				return fmt.Errorf("state = %v", body["state"])
			}
			return nil
		}, stage)

		// 4. build identity
		expect(t, srv, "/version", http.StatusOK, func(body map[string]any) error {
			if body["go_version"] == "" {
				return errors.New("no go_version")
			}
			return nil
		}, stage)

		// 5. a durable job survives, is claimed, and completes — the part that
		//    must NOT be in the cache.
		j := enqueue(t, pool, jobs.NewJob{Kind: jobs.KindNoop, CorrelationID: "golden-" + stage})
		row, err := sqlcgen.New(pool).GetJob(ctx, j.ID)
		if err != nil {
			t.Fatalf("[%s] the enqueued job is not in the database: %v", stage, err)
		}
		if row.State != "queued" {
			t.Fatalf("[%s] job state = %s, want queued", stage, row.State)
		}

		// 6. rate limiting answers
		if allowed, _ := limiter.Allow(ctx, "golden-"+stage, 5, time.Minute); !allowed {
			t.Fatalf("[%s] the first request under a limit of 5 was refused", stage)
		}
	}

	goldenPath("before FLUSHALL")

	// Fill the cache so the flush has something to destroy, then empty it.
	for i := range 50 {
		if allowed, _ := limiter.Allow(ctx, fmt.Sprintf("warm-%d", i), 100, time.Minute); !allowed {
			t.Fatalf("warming the cache: request %d refused", i)
		}
	}
	if err := cacheClient.FlushAll(ctx); err != nil {
		t.Fatalf("FLUSHALL: %v", err)
	}
	t.Log("FLUSHALL issued; the cache is now empty")

	// Past the readiness TTL, so the second pass recomputes rather than
	// replaying the cached answer.
	time.Sleep(2100 * time.Millisecond)

	goldenPath("after FLUSHALL")

	// And the durable state is untouched: the jobs both passes enqueued are
	// still there.
	var n int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE correlation_id LIKE 'golden-%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("%d golden-path jobs survived FLUSHALL, want 2: durable work must not live in the cache", n)
	}
}

// Readiness must report `degraded`, not `ok` and not 503, when the cache is
// unreachable — with the API still serving.
func TestReadinessDegradesWhenTheCacheIsUnreachable(t *testing.T) {
	cfg, resolver, pool := freshDatabase(t)

	// A port nothing listens on: a real unreachable cache, not a mock.
	dead, err := cache.Open("redis://127.0.0.1:1/0", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dead.Close() }()

	srv := httpapi.New(httpapi.Deps{
		Config:                cfg,
		Resolver:              resolver,
		Pools:                 poolsOf(t, resolver),
		Cache:                 dead,
		Limiter:               cache.NewFallbackLimiter(dead),
		Search:                search.NewService(search.NewSQL(), nil, nil),
		EmbeddedSchemaVersion: mustEmbedded(t),
		QueueSnapshot: func(ctx context.Context) (jobs.Snapshot, error) {
			return jobs.Collect(ctx, pool, nil)
		},
	})

	expect(t, srv, "/readyz", http.StatusOK, func(body map[string]any) error {
		if body["status"] != "degraded" {
			return fmt.Errorf("status = %v, want degraded", body["status"])
		}
		return nil
	}, "cache unreachable")
}

// ---------------------------------------------------------------------------

func expect(t *testing.T, srv *httpapi.Server, path string, wantCode int, check func(map[string]any) error, stage string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != wantCode {
		t.Fatalf("[%s] %s = %d, want %d. Body: %s", stage, path, rec.Code, wantCode, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("[%s] %s returned a non-JSON body: %q", stage, path, rec.Body.String())
	}
	if err := check(body); err != nil {
		t.Fatalf("[%s] %s: %v. Body: %s", stage, path, err, rec.Body.String())
	}
}

func poolsOf(t *testing.T, r *site.Resolver) *db.Pools {
	t.Helper()
	p, err := db.Open(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func mustEmbedded(t *testing.T) int64 {
	t.Helper()
	v, err := migrate.EmbeddedVersion()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func waitFor(t *testing.T, ctx context.Context, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			t.Fatalf("context cancelled waiting for %s", what)
		}
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func ptr(s string) *string { return &s }

func interval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}

// ---------------------------------------------------------------------------
// Backend Finding 1 — a crash-looping job must not wedge the queue
// ---------------------------------------------------------------------------

// Three individually-correct decisions used to combine into a total queue
// outage: `attempts` is incremented at CLAIM time, the sweep requeued an
// elapsed lease without touching `attempts`, and jobs_attempts_bounded makes
// exceeding max_attempts a hard error.
//
// After max_attempts worker deaths the row could no longer be claimed, and
// because the claim subselect orders by (priority, run_after) it was re-selected
// on every poll — so the claim raised a constraint violation each cycle, the
// worker logged a transient failure and slept, and every other job for that site
// was never claimed. One poisoned job, one operator with psql.
func TestACrashLoopingJobDeadLettersAndDoesNotBlockTheQueue(t *testing.T) {
	_, _, pool := freshDatabase(t)
	ctx := t.Context()
	q := sqlcgen.New(pool)

	// The poison is enqueued FIRST so it sits at the head of the claim order.
	poison := enqueue(t, pool, jobs.NewJob{Kind: jobs.KindNoop, MaxAttempts: 2, CorrelationID: "poison"})
	healthy := enqueue(t, pool, jobs.NewJob{Kind: jobs.KindNoop, MaxAttempts: 5, CorrelationID: "healthy"})

	// Simulate a worker that dies mid-job: claim, never record an outcome, let
	// the lease elapse, sweep. Repeat past max_attempts.
	for i := range 3 {
		row, err := q.ClaimJob(ctx, sqlcgen.ClaimJobParams{
			LeasedBy:      ptr("dying-worker"),
			LeaseDuration: interval(time.Minute),
			Kinds:         []string{string(jobs.KindNoop)},
		})
		if err != nil {
			// The bug was a constraint violation here, on cycle 3.
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("cycle %d: ClaimJob failed: %v\n"+
					"A claim must never raise jobs_attempts_bounded: the exhausted row is "+
					"then permanently at the head of the claim order and no other job is ever claimed.", i+1, err)
			}
			// No claimable row left is a legitimate outcome once the poison is dead.
			continue
		}
		// The worker dies: the lease elapses with no outcome recorded.
		if _, err := pool.Exec(ctx,
			`UPDATE jobs SET leased_until = now() - interval '1 minute' WHERE id = $1`, row.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := q.SweepExpiredLeases(ctx); err != nil {
			t.Fatalf("cycle %d: sweep failed: %v", i+1, err)
		}
	}

	p, err := q.GetJob(ctx, poison.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != "dead" {
		t.Fatalf("the poison job is %q, want dead. A job whose worker dies max_attempts times must be "+
			"dead-lettered by the sweep — the retry path never sees it, because no handler ever returned "+
			"an error.", p.State)
	}
	if p.LastError == nil || !strings.Contains(*p.LastError, "lease elapsed") {
		t.Fatalf("last_error = %v, want it to name lease exhaustion", p.LastError)
	}
	if !p.FinishedAt.Valid {
		t.Fatal("a dead job has no finished_at; retention cannot find it")
	}

	// The whole point: the job behind it is claimable.
	h, err := q.ClaimJob(ctx, sqlcgen.ClaimJobParams{
		LeasedBy: ptr("healthy-worker"), LeaseDuration: interval(time.Minute),
		Kinds: []string{string(jobs.KindNoop)},
	})
	if err != nil {
		t.Fatalf("the job enqueued behind the poison could not be claimed: %v", err)
	}
	if h.ID != healthy.ID {
		t.Fatalf("claimed %s, want the healthy job %s", h.ID, healthy.ID)
	}
	if _, err := q.CompleteJob(ctx, sqlcgen.CompleteJobParams{ID: h.ID, LeasedBy: ptr("healthy-worker")}); err != nil {
		t.Fatal(err)
	}

	// And the depth gauge is not stuck.
	snap, err := jobs.Collect(ctx, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Depth) != 0 {
		t.Fatalf("live depth is %v after the crash loop; it must drain", snap.Depth)
	}
}

// ---------------------------------------------------------------------------
// Backend Finding 3 — the claim plan must not sort the backlog
// ---------------------------------------------------------------------------

// Asserts the PROPERTY (no Sort node), not a timing, so it cannot be flaky in
// CI. With the index ordered (state, run_after, priority) while the claim
// orders by (priority, run_after), every claim sorted the whole eligible
// backlog and spilled to disk — and the cost grew with backlog depth, which is
// exactly when the queue is deepest.
func TestClaimPlanDoesNotSortTheBacklog(t *testing.T) {
	_, _, pool := freshDatabase(t)
	ctx := t.Context()

	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id, kind, payload, state, priority, attempts, max_attempts, run_after, correlation_id, created_at, updated_at)
		SELECT gen_random_uuid(), 'noop', '{}'::jsonb, 'queued',
		       (100 + (i % 5))::smallint, 0, 5, now() - (i || ' seconds')::interval,
		       'bulk-' || i, now(), now()
		FROM generate_series(1, 10000) AS i`); err != nil {
		t.Fatalf("seeding 10k queued rows: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE jobs`); err != nil {
		t.Fatal(err)
	}

	var plan string
	err := pool.QueryRow(ctx, `
		EXPLAIN (FORMAT JSON)
		SELECT id FROM jobs
		WHERE state = 'queued'
		  AND run_after <= now()
		  AND attempts < max_attempts
		  AND kind = ANY(ARRAY['noop']::text[])
		ORDER BY priority, run_after
		LIMIT 1`).Scan(&plan)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	t.Logf("claim plan:\n%s", plan)

	if strings.Contains(plan, `"Sort`) {
		t.Fatalf("the claim plan contains a Sort node at 10k queued rows.\n"+
			"Every claim would sort the whole eligible backlog and spill to disk.\n%s", plan)
	}
	if !strings.Contains(plan, "jobs_claim") {
		t.Fatalf("the claim plan does not use jobs_claim:\n%s", plan)
	}
}

// The partial predicate is load-bearing: the index must not carry terminal rows.
func TestClaimIndexExcludesTerminalRows(t *testing.T) {
	_, _, pool := freshDatabase(t)
	var def string
	if err := pool.QueryRow(t.Context(),
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'jobs_claim'`).Scan(&def); err != nil {
		t.Fatalf("jobs_claim does not exist: %v", err)
	}
	t.Logf("jobs_claim = %s", def)
	if !strings.Contains(def, "WHERE") || !strings.Contains(def, "queued") {
		t.Fatalf("jobs_claim is not partial on state='queued'; it would index every "+
			"succeeded, failed and dead row for the whole retention window:\n%s", def)
	}
}

// ---------------------------------------------------------------------------
// Backend Finding 4 — the bound is in the DATABASE, not only in the caller
// ---------------------------------------------------------------------------

func TestEnqueueRefusesAnUnboundedPayload(t *testing.T) {
	_, _, pool := freshDatabase(t)
	ctx := t.Context()

	t.Run("the Go error", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		_, err = jobs.Enqueue(ctx, tx, jobs.NewJob{
			Kind: jobs.KindNoop, CorrelationID: "c1",
			Payload: map[string]string{"blob": strings.Repeat("x", jobs.MaxPayloadBytes)},
		})
		if !errors.Is(err, jobs.ErrPayloadTooLarge) {
			t.Fatalf("err = %v, want ErrPayloadTooLarge", err)
		}
		// And no row was written.
		var n int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%d row(s) written despite the refusal", n)
		}
	})

	// The half that proves the bound is not only in this caller. A future
	// writer — a migration script, another service, psql — hits the same wall.
	t.Run("a raw INSERT is rejected by the database", func(t *testing.T) {
		_, err := pool.Exec(ctx, `
			INSERT INTO jobs (id, kind, payload, state, priority, attempts, max_attempts, run_after, correlation_id, created_at, updated_at)
			VALUES (gen_random_uuid(), 'noop', jsonb_build_object('blob', repeat('x', 100000)),
			        'queued', 100, 0, 5, now(), 'c1', now(), now())`)
		if err == nil {
			t.Fatal("the database accepted a 100 KB payload; jobs_payload_bounded is not enforcing")
		}
		if !strings.Contains(err.Error(), "jobs_payload_bounded") {
			t.Fatalf("rejected for the wrong reason: %v", err)
		}
	})

	t.Run("an oversized kind and correlation_id are rejected", func(t *testing.T) {
		_, err := pool.Exec(ctx, `
			INSERT INTO jobs (id, kind, payload, state, priority, attempts, max_attempts, run_after, correlation_id, created_at, updated_at)
			VALUES (gen_random_uuid(), repeat('k', 200), '{}'::jsonb, 'queued', 100, 0, 5, now(), 'c1', now(), now())`)
		if err == nil || !strings.Contains(err.Error(), "jobs_kind_bounded") {
			t.Fatalf("a 200-character kind: %v", err)
		}
		_, err = pool.Exec(ctx, `
			INSERT INTO jobs (id, kind, payload, state, priority, attempts, max_attempts, run_after, correlation_id, created_at, updated_at)
			VALUES (gen_random_uuid(), 'noop', '{}'::jsonb, 'queued', 100, 0, 5, now(), repeat('c', 500), now(), now())`)
		if err == nil || !strings.Contains(err.Error(), "jobs_correlation_bounded") {
			t.Fatalf("a 500-character correlation_id: %v", err)
		}
	})

	// A payload comfortably under the limit still works, so the bound does not
	// break the normal path.
	t.Run("a normal payload is accepted", func(t *testing.T) {
		out := enqueue(t, pool, jobs.NewJob{
			Kind: jobs.KindNoop, CorrelationID: "c-ok",
			Payload: map[string]any{"asset_id": "abc", "version": 3},
		})
		if out.ID.String() == "" {
			t.Fatal("a normal enqueue failed")
		}
	})
}

// ---------------------------------------------------------------------------
// Backend Finding 7 — a graceful shutdown must RECORD the outcome
// ---------------------------------------------------------------------------

// Before the fix the handler ran to completion on SIGTERM (correct) and then
// every outcome write failed instantly with "context canceled": the row stayed
// `leased`, the sweep requeued it two minutes later, and it ran AGAIN. Every
// rolling deploy re-delivered every in-flight job.
func TestGracefulShutdownRecordsTheOutcomeOfAnInFlightJob(t *testing.T) {
	_, resolver, pool := freshDatabase(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	release := make(chan struct{})
	started := make(chan struct{}, 1)

	w := jobs.NewWorker(resolver, map[string]*pgxpool.Pool{"default": pool}, nil, jobs.Options{
		Lease: 30 * time.Second, Timeout: 30 * time.Second, Concurrency: 1,
		PollInterval: 50 * time.Millisecond, SweepInterval: time.Hour,
		DrainGrace: 15 * time.Second, WorkerID: "shutdown-worker",
	})
	w.Register("blocking", func(hctx context.Context, j jobs.Claimed) error {
		started <- struct{}{}
		<-release
		return nil
	})

	stopped := make(chan struct{})
	go func() { _ = w.Run(ctx); close(stopped) }()

	j := enqueue(t, pool, jobs.NewJob{Kind: "blocking", CorrelationID: "drain"})
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("the handler never started")
	}

	// SIGTERM arrives while the job is in flight.
	cancel()
	time.Sleep(200 * time.Millisecond)
	close(release)

	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("the worker did not stop within the drain grace")
	}

	q := sqlcgen.New(pool)
	row, err := q.GetJob(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "succeeded" {
		t.Fatalf("after a graceful shutdown the job is %q, want succeeded.\n"+
			"A drain that runs the handler and then cannot record the result is worse than no drain: "+
			"it pays the shutdown latency AND redelivers.", row.State)
	}

	// And the sweep has nothing to reclaim, so the next deploy does not
	// re-deliver it.
	reclaimed, err := q.SweepExpiredLeases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 0 {
		t.Fatalf("the sweep reclaimed %d row(s) after a graceful restart; it must reclaim none", len(reclaimed))
	}
}

// ---------------------------------------------------------------------------
// Backend Finding 8 — an actual ladder walk, and worker-level crash recovery
// ---------------------------------------------------------------------------

// The previous retry test enqueued with MaxAttempts 1, so RetryJob's
// `attempts < max_attempts` guard was false on the first failure and the job
// went straight to dead — the backoff path was never executed against a
// database despite the test being named for it.
func TestRetryLadderActuallyWalksTheLadder(t *testing.T) {
	_, resolver, pool := freshDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	var mu sync.Mutex
	calls := 0
	w := jobs.NewWorker(resolver, map[string]*pgxpool.Pool{"default": pool}, nil, jobs.Options{
		Lease: 10 * time.Second, Timeout: 10 * time.Second, Concurrency: 1,
		PollInterval: 50 * time.Millisecond, SweepInterval: time.Hour,
		WorkerID: "ladder-worker",
	})
	w.Register("flaky", func(context.Context, jobs.Claimed) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return errors.New("transient")
	})
	startWorker(t, ctx, cancel, w)

	j := enqueue(t, pool, jobs.NewJob{Kind: "flaky", MaxAttempts: 3, CorrelationID: "ladder"})
	q := sqlcgen.New(pool)

	// First failure: back to queued, attempts 1, run_after pushed out along the
	// ladder — which is the assertion the old test could not make.
	waitFor(t, ctx, func() bool {
		row, err := q.GetJob(ctx, j.ID)
		return err == nil && row.State == "queued" && row.Attempts == 1
	}, "the first failure to requeue with attempts=1")

	row, err := q.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !row.RunAfter.Time.After(time.Now().Add(10 * time.Second)) {
		t.Fatalf("run_after = %v; the first ladder step is 30s, so it must be pushed well into the future",
			row.RunAfter.Time)
	}
	if row.LastError == nil || !strings.Contains(*row.LastError, "transient") {
		t.Fatalf("last_error = %v, want the handler's cause", row.LastError)
	}

	// Drive the remaining attempts without waiting out the real backoff.
	for range 2 {
		if _, err := pool.Exec(ctx, `UPDATE jobs SET run_after = now() WHERE id = $1 AND state = 'queued'`, j.ID); err != nil {
			t.Fatal(err)
		}
		time.Sleep(400 * time.Millisecond)
	}
	waitFor(t, ctx, func() bool {
		row, err := q.GetJob(ctx, j.ID)
		return err == nil && row.State == "dead"
	}, "the exhausted job to be dead-lettered")

	mu.Lock()
	got := calls
	mu.Unlock()
	if got < 3 {
		t.Fatalf("the handler ran %d time(s); MaxAttempts 3 must walk the ladder, not skip it", got)
	}
}

// A worker-level crash recovery: the sweep STATEMENT was tested, but never that
// a job whose worker died is subsequently re-claimed and completed by a worker.
func TestAJobWhoseWorkerDiedIsReclaimedAndCompleted(t *testing.T) {
	_, resolver, pool := freshDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	q := sqlcgen.New(pool)

	j := enqueue(t, pool, jobs.NewJob{Kind: jobs.KindNoop, MaxAttempts: 5, CorrelationID: "recover"})

	// A worker claims it and dies without recording anything.
	if _, err := q.ClaimJob(ctx, sqlcgen.ClaimJobParams{
		LeasedBy: ptr("dead-worker"), LeaseDuration: interval(time.Minute),
		Kinds: []string{string(jobs.KindNoop)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET leased_until = now() - interval '1 minute' WHERE id = $1`, j.ID); err != nil {
		t.Fatal(err)
	}

	// A live worker, with a fast sweep, recovers it end to end.
	w := jobs.NewWorker(resolver, map[string]*pgxpool.Pool{"default": pool}, nil, jobs.Options{
		Lease: 10 * time.Second, Timeout: 10 * time.Second, Concurrency: 1,
		PollInterval: 50 * time.Millisecond, SweepInterval: 300 * time.Millisecond,
		WorkerID: "recovery-worker",
	})
	startWorker(t, ctx, cancel, w)

	waitFor(t, ctx, func() bool {
		row, err := q.GetJob(ctx, j.ID)
		return err == nil && row.State == "succeeded"
	}, "the abandoned job to be swept, re-claimed and completed")
}

// ---------------------------------------------------------------------------
// Backend Finding 5 and 6 — schema invariants
// ---------------------------------------------------------------------------

func TestSitesIsASingleton(t *testing.T) {
	_, _, pool := freshDatabase(t)
	ctx := t.Context()

	_, err := pool.Exec(ctx,
		`INSERT INTO sites (id, handle, base_url) VALUES ($1, 'a-test', 'https://a.example')`, uuid.New())
	if err == nil {
		t.Fatal("a second site row was accepted.\n" +
			"privacy_mode is step (1) of the frozen precedence matrix, so a second row makes the " +
			"answer depend on which handle sorts first — a private site with handle 'z-main' would " +
			"lose to an accidental 'a-test' row defaulting to 'public'.")
	}
	if !strings.Contains(err.Error(), "sites_singleton") {
		t.Fatalf("rejected for the wrong reason: %v", err)
	}

	// And the read is still correct with the one row.
	s, err := sqlcgen.New(pool).GetDefaultSite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Handle != "default" {
		t.Fatalf("GetDefaultSite returned %q", s.Handle)
	}
}

func TestAuditEventsRefusesAFullIPAddress(t *testing.T) {
	_, _, pool := freshDatabase(t)
	ctx := t.Context()

	insert := func(v any) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO audit_events (id, actor_kind, action, subject_type, ip_prefix)
			 VALUES ($1, 'system', 'test', 'site', $2)`, uuid.New(), v)
		return err
	}

	t.Run("a full address is refused", func(t *testing.T) {
		for _, addr := range []string{
			"203.0.113.47",
			"203.0.113.47/32",
			"192.168.1.1",
			"2001:db8::1",
			"2001:db8:0:0:0:0:0:dead",
		} {
			if err := insert(addr); err == nil {
				t.Errorf("ip_prefix accepted the full address %q; the migration promises a truncated "+
					"prefix only, and the first M1 caller that passes c.RealIP() would store this", addr)
			} else if !strings.Contains(err.Error(), "audit_events_ip_prefix_shape") {
				t.Errorf("%q rejected for the wrong reason: %v", addr, err)
			}
		}
	})

	// Two seats found the same hole independently: the IPv6 branch's group
	// repetition was unbounded, so a /96 or /112 was accepted while the header
	// promised /48 or /64. The whole point of the column is that it CANNOT hold
	// something more specific than a prefix, and this CHECK freezes on merge.
	t.Run("a prefix more specific than /64 is refused", func(t *testing.T) {
		for _, addr := range []string{
			"2001:db8:1234:5678:9abc:def0::",      // /96
			"2001:db8:1234:5678:9abc:def0:1234::", // /112
			"a:b:c:d:e:f:1::",                     // seven groups, short labels
			"2001:db8:1234:5678:9abc:def0::/48",   // a /48 suffix does not make the value a /48
			"999.999.999.0",                       // not an address at all
		} {
			if err := insert(addr); err == nil {
				t.Errorf("ip_prefix accepted %q, which is more specific than /64 (or is not an address). "+
					"The column must refuse it: the header promises /24 and /48-or-/64 only.", addr)
			} else if !strings.Contains(err.Error(), "audit_events_ip_prefix_shape") {
				t.Errorf("%q rejected for the wrong reason: %v", addr, err)
			}
		}
	})

	// Lowercase is the frozen rule: Go's netip emits lowercase, and a canonical
	// column is what keeps prefix EQUALITY honest — "2001:DB8::" and
	// "2001:db8::" are the same network but different text.
	t.Run("uppercase hex is refused", func(t *testing.T) {
		for _, addr := range []string{"2001:DB8::", "FE80::", "2001:Db8:85A3::"} {
			if err := insert(addr); err == nil {
				t.Errorf("ip_prefix accepted %q; the column is canonical lowercase", addr)
			}
		}
	})

	t.Run("a truncated prefix is accepted", func(t *testing.T) {
		for _, prefix := range []string{
			"203.0.113.0", "203.0.113.0/24", "10.0.0.0",
			"2001:db8::", "2001:db8::/48", "2001:db8::/64",
			"2001:db8:85a3:1::",       // four groups, the /64 shape
			"2001:db8:1234:5678::/64", //
			"2001:0db8:0000::",        // zero-padded labels are still lowercase hex
			"fe80::",                  // link-local, one group
		} {
			if err := insert(prefix); err != nil {
				t.Errorf("ip_prefix refused the truncated prefix %q: %v", prefix, err)
			}
		}
	})

	t.Run("null is accepted", func(t *testing.T) {
		if err := insert(nil); err != nil {
			t.Errorf(`"no IP available" must be representable: %v`, err)
		}
	})
}

// ---------------------------------------------------------------------------
// last_error must actually reach the column, multibyte and all
// ---------------------------------------------------------------------------

// The unit test proves truncate produces valid UTF-8. This proves the whole
// path: a handler returns a >2 KiB error containing multibyte runes and a
// credential, and the row ends up terminal with last_error STORED.
//
// Before truncate was rune-safe, PostgreSQL rejected the write with
// "invalid byte sequence for encoding UTF8", the worker logged "recording
// terminal failure failed", and the row stayed `leased` with its cause lost.
func TestAMultibyteErrorIsStoredInLastError(t *testing.T) {
	_, resolver, pool := freshDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// The misalignment is DELIBERATE, not left to luck. truncate cuts at byte
	// 2000, so the head is padded to exactly 1999 bytes and a 3-byte rune
	// starts at 2000 — byte 2000 is then its second byte, and a byte-index
	// slice is guaranteed to split it. Without this the test would pass or fail
	// depending on where the repeats happened to land.
	const head = "処理に失敗しました — fetching https://b.example/o?"
	pad := strings.Repeat("a", 1999-len(head))
	const secret = "X-Amz-Signature=SuperSecretSignatureMaterial0123456789"
	msg := head + pad + strings.Repeat("日", 700) + "&" + secret
	if len(head)+len(pad) != 1999 {
		t.Fatalf("the fixture is misbuilt: head+pad is %d bytes, want 1999", len(head)+len(pad))
	}

	var workerLog safeBuffer
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("worker log:\n%s", workerLog.String())
		}
	})

	w := jobs.NewWorker(resolver, map[string]*pgxpool.Pool{"default": pool}, nil, jobs.Options{
		Lease: 10 * time.Second, Timeout: 10 * time.Second, Concurrency: 1,
		PollInterval: 50 * time.Millisecond, SweepInterval: time.Hour,
		WorkerID: "utf8-worker",
		// Captured rather than discarded: when this test fails, the reason is in
		// the worker's log ("invalid byte sequence for encoding UTF8"), and a
		// test that hides its own diagnosis wastes the next reader's hour.
		//
		// Captured rather than printed, because the raw handler error carries
		// the fixture credential and the worker's logger in a test is not the
		// redacting one cmd/api and cmd/worker install with slog.SetDefault.
		Logger: slog.New(slog.NewTextHandler(&workerLog, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	// A PLAIN error, not a Terminal one: Terminal's Error() prefixes
	// "terminal: ", which would shift the byte offsets the fixture depends on.
	// With MaxAttempts 1 the first failure exhausts the ladder and the worker
	// dead-letters it, which writes last_error by the same path.
	w.Register("multibyte-fail", func(context.Context, jobs.Claimed) error {
		return errors.New(msg)
	})
	startWorker(t, ctx, cancel, w)

	j := enqueue(t, pool, jobs.NewJob{Kind: "multibyte-fail", MaxAttempts: 1, CorrelationID: "utf8"})
	q := sqlcgen.New(pool)
	waitFor(t, ctx, func() bool {
		row, err := q.GetJob(ctx, j.ID)
		return err == nil && (row.State == "dead" || row.State == "failed")
	}, "the job to reach a terminal state")

	row, err := q.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.LastError == nil || *row.LastError == "" {
		t.Fatal("last_error is empty. The write was rejected, so the job's real cause was never " +
			"recorded and an operator has nothing to read.")
	}
	stored := *row.LastError
	if !utf8.ValidString(stored) {
		t.Fatalf("the stored value is not valid UTF-8; PostgreSQL rejects such a write outright, "+
			"so in production the row would stay leased with its cause lost.\nlast 16 bytes: % x",
			stored[max(0, len(stored)-16):])
	}
	// Terminal{} prefixes "terminal: ", so match on the message itself.
	if !strings.Contains(stored, "処理に失敗しました") {
		t.Fatalf("the beginning of the message was lost: %q", firstRunes(stored, 30))
	}
	if strings.Contains(stored, "SuperSecretSignatureMaterial") {
		t.Fatalf("the credential was stored: %s", stored)
	}
	if !strings.Contains(stored, "truncated") {
		t.Fatalf("a truncated message must say so: %q", lastRunes(stored, 30))
	}
	// And the database's own bound still holds.
	if len(stored) > 4096 {
		t.Fatalf("last_error is %d bytes; jobs_last_error_bounded caps it at 4096", len(stored))
	}
	t.Logf("stored %d bytes, valid UTF-8, redacted, message preserved", len(stored))
}

// ---------------------------------------------------------------------------
// Verifier FINDING V-2 — redaction must live at the call site, not in the wiring
// ---------------------------------------------------------------------------

// The worker must not depend on WHICH slog handler it was built with to keep
// credentials out of its own log.
//
// `last_error` has always been safe: it goes through safeError, which is
// truncate(obs.Redact(s)). The log lines carrying the same handler error did
// not — they passed err.Error() raw and relied entirely on cmd/api and
// cmd/worker installing obs.NewLogger with slog.SetDefault. Any other caller
// that builds a Worker with its own logger — an admin tool, a one-off
// migration command, a test — logged the credential in the clear. The
// protection belonged at the call site.
//
// This test builds a Worker with a PLAIN slog.TextHandler, deliberately NOT the
// redacting one, and drives every branch that logs a handler error:
//
//	dead-letter  ("jobs: exhausted attempts")  — MaxAttempts 1, plain error
//	terminal     ("jobs: terminal failure")    — jobs.Terminal
//	retry        ("jobs: retrying")            — MaxAttempts 3, plain error
//
// The value classes are the ones internal/obs/log_test.go pins, restricted to
// the ones that can appear inside free error text (the secret-NAMED-attribute
// classes are a handler concern and do not arise here, where the key is
// "error").
func TestAWorkerWithAPlainHandlerLogsNoCredentials(t *testing.T) {
	_, resolver, pool := freshDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// Same construction as internal/obs/log_test.go, so the two stay in step.
	var (
		fakeToken  = "ey" + strings.Repeat("Jh", 9) + "ZyI6MQ"
		fakeAPIKey = "vzk_" + strings.Repeat("Nn4Pp7", 5)
	)
	secrets := map[string]string{
		"DSN password":                       "hunter2",
		"cache URL password":                 "s3cr3tpw",
		"presigned URL signature":            "abc123def456",
		"bearer token":                       fakeToken,
		"Vizra API key":                      fakeAPIKey,
		"signature material in a second URL": "SuperSecretSignatureMaterial0123456789",
		"presigned URL credential":           "AKIAIOSFODNN7EXAMPLE",
	}
	// One error text carrying every class, shaped the way a real handler error
	// carries them: inside URLs and tool output. Each credential sits where its
	// class actually occurs — obs.Redact's presigned rule is anchored on the
	// query separator, so X-Amz-Credential is written as a query parameter and
	// not as a bare word. (A BARE `X-Amz-Credential=…` outside a URL is not
	// covered by obs.Redact today; that is a pattern-coverage question in the
	// same family as verifier FINDING V-3(b) and is reported to the chair
	// rather than fixed here, where the subject is the CALL SITE.)
	failure := "upload failed: dialling postgres://vizra:hunter2@db:5432/vizra; " +
		"cache rediss://default:s3cr3tpw@cache:6379/0; " +
		"GET https://bucket.s3.example/obj?X-Amz-Signature=abc123def456" +
		"&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE&X-Amz-Expires=900 ; " +
		"retry with https://b.example/o?X-Amz-Signature=SuperSecretSignatureMaterial0123456789 ; " +
		"upstream said Authorization: Bearer " + fakeToken + " ; " +
		"caller key " + fakeAPIKey

	var workerLog safeBuffer
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("worker log:\n%s", workerLog.String())
		}
	})

	w := jobs.NewWorker(resolver, map[string]*pgxpool.Pool{"default": pool}, nil, jobs.Options{
		Lease: 10 * time.Second, Timeout: 10 * time.Second, Concurrency: 2,
		PollInterval: 50 * time.Millisecond, SweepInterval: time.Hour,
		WorkerID: "plain-handler-worker",
		// DELIBERATELY the plain handler. obs.NewLogger would redact at the
		// handler and prove nothing about the call sites.
		Logger: slog.New(slog.NewTextHandler(&workerLog, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	w.Register("leaky-exhausted", func(context.Context, jobs.Claimed) error {
		return errors.New(failure)
	})
	w.Register("leaky-terminal", func(context.Context, jobs.Claimed) error {
		return jobs.Terminal{Err: errors.New(failure)}
	})
	w.Register("leaky-retry", func(context.Context, jobs.Claimed) error {
		return errors.New(failure)
	})
	startWorker(t, ctx, cancel, w)

	exhausted := enqueue(t, pool, jobs.NewJob{Kind: "leaky-exhausted", MaxAttempts: 1, CorrelationID: "x"})
	terminal := enqueue(t, pool, jobs.NewJob{Kind: "leaky-terminal", MaxAttempts: 5, CorrelationID: "t"})
	retrying := enqueue(t, pool, jobs.NewJob{Kind: "leaky-retry", MaxAttempts: 3, CorrelationID: "r"})

	// WAIT ON THE OBSERVABLE THIS TEST ACTUALLY READS — the log buffer.
	//
	// The first version of this waited on `c.Attempts >= 1` for the retry job.
	// That is a PROXY, and it becomes true too early: ClaimJob increments
	// `attempts` in the same UPDATE that sets state='leased' — its own comment
	// says "`attempts` counts CLAIMS, not failures: it is incremented here, at
	// claim time" — so the wait was satisfied the instant the job was claimed,
	// before the handler returned, before RetryJob ran, and before
	// `jobs: retrying` had been written to the buffer the assertions below
	// read. The result was a 1-in-30 red on a lane this very slice exists to
	// make deterministic (verifier FINDING 1, round 1: 2 of 120 full runs).
	//
	// The rule, which is the actual lesson: a wait must be on the thing the
	// assertion reads, never on a state change that happens to precede it.
	// The database conditions are kept because they are still necessary — they
	// just are not sufficient.
	lines := []string{
		"jobs: exhausted attempts",
		"jobs: terminal failure",
		"jobs: retrying",
	}
	q := sqlcgen.New(pool)
	waitFor(t, ctx, func() bool {
		a, err1 := q.GetJob(ctx, exhausted.ID)
		b, err2 := q.GetJob(ctx, terminal.ID)
		c, err3 := q.GetJob(ctx, retrying.ID)
		if err1 != nil || err2 != nil || err3 != nil ||
			a.State != "dead" || b.State != "failed" || c.Attempts < 1 {
			return false
		}
		logged := workerLog.String()
		for _, line := range lines {
			if !strings.Contains(logged, line) {
				return false
			}
		}
		return true
	}, "all three branches to have written their log line")

	// Kept deliberately after the wait: it turns a timeout into a named
	// message saying WHICH branch never logged, instead of a bare deadline.
	// This test would otherwise be able to pass by logging nothing at all.
	out := workerLog.String()
	for _, line := range lines {
		if !strings.Contains(out, line) {
			t.Fatalf("the worker never logged %q, so this test proved nothing about it:\n%s", line, out)
		}
	}

	for name, secret := range secrets {
		if strings.Contains(out, secret) {
			t.Fatalf("the worker's own log leaked the %s (%q) with a plain handler installed. "+
				"Redaction must happen at the call site, not in whichever handler the process "+
				"happened to wire up.\n%s", name, secret, out)
		}
	}
	// The line must still be USEFUL: an operator needs the cause, not a blank.
	if !strings.Contains(out, "upload failed") {
		t.Fatalf("redaction removed the diagnosis as well as the secret:\n%s", out)
	}
	t.Logf("%d log bytes across three branches, all seven credential classes absent", len(out))
}

// startWorker runs w and GUARANTEES its goroutine is gone before the test
// returns.
//
// Five tests here used to leave `go func() { _ = w.Run(ctx) }()` running with
// only a `defer cancel()`, so a worker outlived its test. That matters because
// NewWorker registers KindNoop UNCONDITIONALLY (internal/jobs/worker.go), which
// means every worker in this suite polls for `noop` — the kind most of these
// tests enqueue. A leaked one is therefore a live route to claiming the NEXT
// test's job.
//
// It is NOT what caused verifier FINDING V-1: the diagnostic at the moment of
// failure showed a single backend connected and the row still queued and
// unleased, and the cause was the application-vs-database clock comparison
// fixed in EnqueueJob. But `-shuffle=on` now reorders these tests on every run,
// and "the pool would probably have been closed by then" is not an isolation
// argument.
func startWorker(t *testing.T, ctx context.Context, cancel context.CancelFunc, w *jobs.Worker) {
	t.Helper()
	stopped := make(chan struct{})
	go func() { _ = w.Run(ctx); close(stopped) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(30 * time.Second):
			// Never silently: a worker that will not stop is a drain bug, and
			// the next test would inherit it.
			t.Errorf("the worker goroutine did not return within 30s of cancellation; "+
				"it is still polling and the next test would run alongside it (%s)", t.Name())
		}
	})
}

// safeBuffer is a bytes.Buffer the worker's goroutines may write to while the
// test reads it.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// firstRunes and lastRunes slice on RUNE boundaries, so a failure message about
// a mid-rune cut is not itself mid-rune.
func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[len(r)-n:]
	}
	return string(r)
}
