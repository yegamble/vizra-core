-- Durable jobs (ADR-004). Every statement here is written so the invariant it
-- protects is in the WHERE clause, not in the caller.

-- name: EnqueueJob :one
-- ON CONFLICT DO NOTHING against the live-state partial unique index: a second
-- enqueue of the same (kind, idempotency_key) while one is still queued or
-- leased must return no row, so the caller can fetch the in-flight job instead
-- of creating a duplicate.
INSERT INTO jobs (
    id, kind, payload, idempotency_key, state, priority,
    attempts, max_attempts, run_after, correlation_id
) VALUES (
    $1, $2, $3, $4, 'queued', $5, 0, $6, $7, $8
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
-- Reclaims ONLY rows whose lease has actually elapsed. There is no boot blanket
-- requeue (ADR-004): this sweep, leader-gated, is the whole recovery mechanism.
UPDATE jobs
SET state        = 'queued',
    leased_by    = NULL,
    leased_until = NULL,
    updated_at   = now()
WHERE state = 'leased' AND leased_until < now()
RETURNING id, kind, attempts;

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
