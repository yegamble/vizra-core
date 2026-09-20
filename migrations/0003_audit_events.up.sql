-- 0003_audit_events — the audit trail exists from M0 (ADR-007).
--
-- It records settings, role, moderation and privacy changes: who, what, when,
-- before and after. It exists in the first milestone because an audit trail
-- added after the fact has a hole exactly where the interesting period was.
--
-- Privacy rules baked into the shape, not left to callers:
--   * no raw IP column — ip_prefix holds a truncated prefix only;
--   * before/after are jsonb and must never carry a credential, a signed URL or
--     a session id (ADR-002 § Logging and redaction). The redaction layer runs
--     before a row is written.
--
-- actor_user_id has no foreign key yet because `users` arrives in M1. The
-- reference is added there; adding a constraint is an additive migration.

CREATE TABLE audit_events (
    id             uuid        PRIMARY KEY,           -- uuidv7
    occurred_at    timestamptz NOT NULL DEFAULT now(),
    actor_kind     text        NOT NULL,
    actor_user_id  uuid        NULL,
    actor_label    text        NULL,
    action         text        NOT NULL,
    subject_type   text        NOT NULL,
    subject_id     text        NULL,
    before         jsonb       NULL,
    after          jsonb       NULL,
    correlation_id text        NULL,
    ip_prefix      text        NULL,

    CONSTRAINT audit_events_actor_kind
        CHECK (actor_kind IN ('user', 'api_key', 'system', 'anonymous')),
    -- A user actor must be identified; a system actor must not claim a user id.
    CONSTRAINT audit_events_actor_identified
        CHECK ((actor_kind = 'user') = (actor_user_id IS NOT NULL))
);

CREATE INDEX audit_events_occurred_at ON audit_events (occurred_at DESC);
CREATE INDEX audit_events_subject     ON audit_events (subject_type, subject_id);
CREATE INDEX audit_events_actor       ON audit_events (actor_user_id) WHERE actor_user_id IS NOT NULL;
