-- 0002_jobs — one durable jobs table (ADR-004).
--
-- One table for every kind of external effect: search events, federation
-- delivery, CDN purge, IPFS pin, webhooks, email. Not twelve per-feature queue
-- tables and not a best-effort side-write, because a committed mutation whose
-- enqueue failed is a silently lost effect.
--
-- The row is written in the SAME transaction as the business mutation. The Go
-- API enforces that at compile time: Enqueue takes a pgx.Tx, so an enqueue
-- outside a transaction does not build.
--
-- No site_id or tenant_id column: a job lives in the database of its site and
-- the worker fans out over site.Resolver.Sites() (ADR-004, Q-008 item 5).

CREATE TABLE jobs (
    id              uuid        PRIMARY KEY,           -- uuidv7
    kind            text        NOT NULL,
    payload         jsonb       NOT NULL,
    idempotency_key text        NULL,
    state           text        NOT NULL,
    priority        smallint    NOT NULL DEFAULT 100,
    attempts        int         NOT NULL DEFAULT 0,
    max_attempts    int         NOT NULL DEFAULT 9,
    run_after       timestamptz NOT NULL DEFAULT now(),
    leased_until    timestamptz NULL,
    leased_by       text        NULL,
    last_error      text        NULL,
    correlation_id  text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz NULL,

    CONSTRAINT jobs_state CHECK (state IN ('queued', 'leased', 'succeeded', 'failed', 'dead')),
    CONSTRAINT jobs_attempts_bounded CHECK (attempts >= 0 AND attempts <= max_attempts),
    CONSTRAINT jobs_max_attempts_positive CHECK (max_attempts >= 1),
    -- A leased row must say by whom and until when, or the sweep cannot tell a
    -- live lease from a corrupt one.
    CONSTRAINT jobs_lease_complete CHECK (
        (state <> 'leased') OR (leased_until IS NOT NULL AND leased_by IS NOT NULL)
    ),
    -- A terminal row must carry its finish time, so retention is a query and
    -- not a guess.
    CONSTRAINT jobs_terminal_finished CHECK (
        (state IN ('queued', 'leased')) = (finished_at IS NULL)
    ),

    -- Size bounds on the hottest table in the system. ClaimJob RETURNs
    -- `payload` on every claim, so a fat row is pulled over the wire and
    -- through TOAST each time it is looked at; it also inflates every backup.
    -- AGENTS.md requires queue resources bounded.
    --
    -- 64 KiB is generous for "ids and a version". A job that needs more should
    -- reference a row rather than carry it — and a future kind that genuinely
    -- needs more raises this in an additive migration with a stated reason,
    -- which is the reviewed decision this constraint exists to force.
    --
    -- These land now because there is no product caller of Enqueue yet. Adding
    -- them later means ADD CONSTRAINT ... NOT VALID plus a VALIDATE pass plus
    -- deciding what to do with the rows that already violate them.
    CONSTRAINT jobs_payload_bounded CHECK (octet_length(payload::text) <= 65536),
    -- Also keeps (kind, idempotency_key) inside the btree index entry limit.
    CONSTRAINT jobs_kind_bounded CHECK (char_length(kind) BETWEEN 1 AND 64),
    CONSTRAINT jobs_correlation_bounded CHECK (char_length(correlation_id) BETWEEN 1 AND 128),
    CONSTRAINT jobs_last_error_bounded CHECK (last_error IS NULL OR char_length(last_error) <= 4096)
);

-- Idempotency over LIVE states only: a finished job must not block a legitimate
-- re-enqueue of the same logical work later.
CREATE UNIQUE INDEX jobs_idem ON jobs (kind, idempotency_key)
    WHERE state IN ('queued', 'leased') AND idempotency_key IS NOT NULL;

-- The claim's access path. Column order MATCHES the claim's ORDER BY
-- (priority, run_after), and the partial predicate keeps terminal rows out of
-- the index entirely.
--
-- Measured on a realistic steady state inside the ADR-004 retention window
-- (200k terminal + 50k queued rows): an index of (state, run_after, priority)
-- makes every claim sort the whole eligible backlog and spill to disk —
-- `Sort Method: external merge Disk: 2352kB`, 815 buffers, 14.5 ms. This shape
-- gives an Index Scan: 3 buffers, 0.031 ms. The sort cost grew with backlog
-- depth, which is exactly when the queue is deepest.
--
-- ADR-004's table sketch carries the other order; it is being corrected in the
-- amending ADR.
CREATE INDEX jobs_claim ON jobs (priority, run_after) WHERE state = 'queued';

-- The lease-elapsed sweep's access path. There is no boot blanket requeue
-- (ADR-004): two api replicas doing that is how Vidra requeued each other's
-- work.
CREATE INDEX jobs_lease_sweep ON jobs (leased_until) WHERE state = 'leased';
