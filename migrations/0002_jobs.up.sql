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
    max_attempts    int         NOT NULL DEFAULT 5,
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
    )
);

-- Idempotency over LIVE states only: a finished job must not block a legitimate
-- re-enqueue of the same logical work later.
CREATE UNIQUE INDEX jobs_idem ON jobs (kind, idempotency_key)
    WHERE state IN ('queued', 'leased') AND idempotency_key IS NOT NULL;

CREATE INDEX jobs_claim ON jobs (state, run_after, priority);

-- The lease-elapsed sweep's access path. There is no boot blanket requeue
-- (ADR-004): two api replicas doing that is how Vidra requeued each other's
-- work.
CREATE INDEX jobs_lease_sweep ON jobs (leased_until) WHERE state = 'leased';
