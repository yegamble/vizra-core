-- Durable jobs (ADR-004). Every statement here is written so the invariant it
-- protects is in the WHERE clause, not in the caller.

-- name: EnqueueJob :one
-- ON CONFLICT DO NOTHING against the live-state partial unique index: a second
-- enqueue of the same (kind, idempotency_key) while one is still queued or
-- leased must return no row, so the caller can fetch the in-flight job instead
-- of creating a duplicate.
--
-- POSTGRESQL IS THE SINGLE CLOCK AUTHORITY. A NULL @run_after means "run now",
-- and now() resolves it HERE, in the same transaction that stamps created_at.
-- It must never be resolved in Go: ClaimJob's eligibility predicate is
-- `run_after <= now()` against the DATABASE clock, so an application-side
-- time.Now() compares two different clocks. When the database trails the
-- application host — ordinary NTP skew between two containers, measured at
-- ~1 ms locally and routinely far more in production — a job whose intent was
-- "run now" is not claimable until the skew has elapsed, and a claim inside
-- that window gets no rows on a queued, unleased row that is sitting right
-- there. That was verifier FINDING V-1: two integration tests failed 3 runs in
-- 20. A non-NULL @run_after is the caller's deliberate schedule and is stored
-- verbatim, past or future.
INSERT INTO jobs (
    id, kind, payload, idempotency_key, state, priority,
    attempts, max_attempts, run_after, correlation_id
) VALUES (
    @id, @kind, @payload, @idempotency_key, 'queued', @priority, 0, @max_attempts,
    COALESCE(sqlc.narg(run_after)::timestamptz, now()), @correlation_id
)
ON CONFLICT DO NOTHING
RETURNING id, kind, state, run_after, created_at;

-- name: GetLiveJobByIdempotencyKey :one
SELECT id, kind, state, run_after, created_at
FROM jobs
WHERE kind = $1 AND idempotency_key = $2 AND state IN ('queued', 'leased');

-- name: GetJob :one
SELECT id, kind, payload, idempotency_key, state, priority, attempts, max_attempts,
       run_after, leased_until, leased_by, last_error, correlation_id,
       created_at, updated_at, finished_at
FROM jobs
WHERE id = $1;

-- name: ClaimJob :one
-- FOR UPDATE SKIP LOCKED: two workers never claim the same row, and a locked
-- row never blocks the other worker's scan.
--
-- `attempts` counts CLAIMS, not failures: it is incremented here, at claim
-- time, so a worker that dies mid-job (OOM kill, SIGKILL, node eviction,
-- libvips crash — ADR-004 expects all of these and has no boot blanket
-- requeue) consumes one attempt. That is deliberate: it is the crash-loop
-- brake, and without it a job that kills its worker every time is retried
-- forever.
--
-- `AND attempts < max_attempts` is what makes that safe. Without it, a row that
-- had already burned its budget was still SELECTED — and then the UPDATE's
-- `attempts + 1` violated jobs_attempts_bounded, the worker logged a transient
-- claim failure and slept, and because the row sorts first by (priority,
-- run_after) it was re-selected on the very next poll. One poisoned job stalled
-- the entire site's queue and needed an operator with psql. The sweep below
-- dead-letters such rows, so this predicate is the second half of that fix,
-- not a way of hiding them.
UPDATE jobs
SET state        = 'leased',
    leased_by    = @leased_by,
    leased_until = now() + @lease_duration::interval,
    attempts     = attempts + 1,
    updated_at   = now()
WHERE id = (
    SELECT id FROM jobs
    WHERE state = 'queued'
      AND run_after <= now()
      AND attempts < max_attempts
      AND kind = ANY(@kinds::text[])
    ORDER BY priority, run_after
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING id, kind, payload, attempts, max_attempts, correlation_id, leased_until;

-- name: HeartbeatJob :one
-- Renewal is conditional on still holding the lease. A worker whose lease was
-- swept must not be able to extend it and resume writing.
UPDATE jobs
SET leased_until = now() + @lease_duration::interval,
    updated_at   = now()
WHERE id = @id AND state = 'leased' AND leased_by = @leased_by
RETURNING id, leased_until;

-- name: CompleteJob :execrows
UPDATE jobs
SET state       = 'succeeded',
    finished_at = now(),
    updated_at  = now(),
    last_error  = NULL
WHERE id = @id AND state = 'leased' AND leased_by = @leased_by;

-- name: RetryJob :execrows
-- A retryable failure with attempts left: back to queued, moved out along the
-- ladder.
UPDATE jobs
SET state      = 'queued',
    run_after  = now() + @backoff::interval,
    last_error = @last_error,
    leased_by  = NULL,
    leased_until = NULL,
    updated_at = now()
WHERE id = @id AND state = 'leased' AND leased_by = @leased_by
  AND attempts < max_attempts;

-- name: DeadLetterJob :execrows
-- attempts exhausted.
UPDATE jobs
SET state       = 'dead',
    last_error  = @last_error,
    finished_at = now(),
    leased_by   = NULL,
    leased_until = NULL,
    updated_at  = now()
WHERE id = @id AND state = 'leased' AND leased_by = @leased_by;

-- name: FailJob :execrows
-- A terminal error bypasses the retry ladder entirely.
UPDATE jobs
SET state       = 'failed',
    last_error  = @last_error,
    finished_at = now(),
    leased_by   = NULL,
    leased_until = NULL,
    updated_at  = now()
WHERE id = @id AND state = 'leased' AND leased_by = @leased_by;

-- name: SweepExpiredLeases :many
-- Reclaims rows whose lease has actually elapsed. There is no boot blanket
-- requeue (ADR-004): this sweep, leader-gated, is the whole recovery mechanism.
--
-- A row whose attempts are exhausted is DEAD-LETTERED rather than requeued.
-- That is what ADR-004's "max_attempts exhaustion yields dead" means for the
-- crash path: a worker that dies repeatedly burns the budget without any
-- handler ever returning an error, so the retry path never sees it and only
-- the sweep can declare it dead. Requeueing it instead left an unclaimable row
-- at the head of the claim order forever.
--
-- CONTRACT FOR ANY FUTURE REQUEUE OF A DEAD OR EXHAUSTED ROW — an M2
-- `vizra jobs retry`, an admin "retry this job" control, a bulk replay:
-- IT MUST RESET `attempts` TO 0 (or raise `max_attempts`) IN THE SAME
-- STATEMENT that sets state back to 'queued'. Setting state alone recreates
-- exactly the row this sweep exists to prevent: `state='queued' AND
-- attempts >= max_attempts`, which ClaimJob's `AND attempts < max_attempts`
-- can never take, and which the claim subselect nevertheless re-orders to the
-- head of the queue on every poll. That is the unclaimable-but-queued zombie,
-- and clearing it needs an operator with psql. The invariant is greppable:
--
--   SELECT count(*) FROM jobs WHERE state='queued' AND attempts>=max_attempts; -- must be 0
--
-- `attempts` counts CLAIMS, so resetting it is the correct semantic for a
-- deliberate human replay: the operator is granting a fresh budget, not
-- pretending the earlier crashes never happened — last_error still records
-- them. Written here rather than in a design note because this is the
-- statement whoever writes that command will read.
-- (Backend seat's carried-forward M2 note, PR #1 final closure.)
--
-- One statement, so the two outcomes cannot diverge under concurrency.
UPDATE jobs
SET state = CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'queued' END,
    last_error = CASE
        WHEN attempts >= max_attempts
        THEN 'lease elapsed with attempts exhausted: the worker holding this job stopped without recording an outcome'
        ELSE last_error
    END,
    finished_at  = CASE WHEN attempts >= max_attempts THEN now() ELSE NULL END,
    leased_by    = NULL,
    leased_until = NULL,
    updated_at   = now()
WHERE state = 'leased' AND leased_until < now()
RETURNING id, kind, attempts, state;

-- name: JobDepthByKindState :many
SELECT kind, state, count(*)::bigint AS depth
FROM jobs
WHERE state IN ('queued', 'leased')
GROUP BY kind, state;

-- name: OldestQueuedAgeByKind :many
-- Feeds vizra_jobs_oldest_queued_age_seconds, which readiness and doctor read
-- against the Q-028 threshold.
SELECT kind,
       EXTRACT(EPOCH FROM (now() - min(created_at)))::float8 AS oldest_age_seconds
FROM jobs
WHERE state = 'queued'
GROUP BY kind;

-- name: CountStaleLeases :one
SELECT count(*)::bigint FROM jobs WHERE state = 'leased' AND leased_until < now();

-- name: TryAdvisoryLock :one
-- Leader election by a SESSION-scoped advisory lock in its TWO-INTEGER form, so
-- it cannot collide with golang-migrate's single-bigint lock (ADR-004).
SELECT pg_try_advisory_lock(@class_id::int, @object_id::int);

-- name: AdvisoryUnlock :one
SELECT pg_advisory_unlock(@class_id::int, @object_id::int);
