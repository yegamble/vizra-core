-- 0003_audit_events — the audit trail exists from M0 (ADR-007).
--
-- It records settings, role, moderation and privacy changes: who, what, when,
-- before and after. It exists in the first milestone because an audit trail
-- added after the fact has a hole exactly where the interesting period was.
--
-- What this schema ENFORCES, and what it only asks for. The distinction matters:
-- a header that promises a control the table does not have is worse than no
-- header, because the next reader trusts it.
--
-- ENFORCED by the database:
--   * actor_kind is one of four values, and a `user` actor must carry a user id
--     while a non-user actor must not (audit_events_actor_identified);
--   * ip_prefix cannot hold anything more specific than an IPv4 /24 or an IPv6
--     /64, in canonical lowercase (audit_events_ip_prefix_shape below) — the
--     column refuses one rather than relying on a caller to truncate.
--
-- NOT enforced here, by decision:
--   * IMMUTABILITY. The application role can still UPDATE or DELETE a row, so
--     today the trail is evidence only against someone who is not trying. The
--     BEFORE UPDATE OR DELETE trigger, and the audited retention path it has to
--     coexist with, land in M1 alongside the users FK and the role model, where
--     they can be meaningful. Tracked as a follow-up, not forgotten. A trigger
--     is an additive migration, so nothing here is frozen by deferring it.
--   * That `before`/`after` carry no credential, signed URL or session id. That
--     is a Go-side rule: the redaction layer runs before a row is written, and
--     it is not expressible in SQL.
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
        CHECK ((actor_kind = 'user') = (actor_user_id IS NOT NULL)),

    -- The column REFUSES anything more specific than a prefix, rather than
    -- trusting a caller to truncate. Without this, the first M1 caller that
    -- passes c.RealIP() stores a whole address and the privacy claim above
    -- becomes false in a queryable table — and by then the rows exist, which is
    -- what makes an append-only CHECK expensive to add later.
    --
    -- ACCEPTED, and nothing else:
    --   IPv4  a /24 whose last octet is literally 0, each of the first three
    --         octets a real 0-255 value: '203.0.113.0', '203.0.113.0/24',
    --         '10.0.0.0'. '999.999.999.0' is not an address and is refused.
    --   IPv6  at most FOUR groups then '::', optionally '/48' or '/64':
    --         'fe80::', '2001:db8::', '2001:db8:85a3:1::',
    --         '2001:db8:1234:5678::/64'.
    --
    -- The group count is bounded at four on purpose. An unbounded repetition
    -- accepted '2001:db8:1234:5678:9abc:def0::' (a /96) and
    -- '2001:db8:1234:5678:9abc:def0:1234::' (a /112) while this header promised
    -- /48 or /64 — the exact failure the column exists to prevent, found by two
    -- reviewers independently.
    --
    -- LOWERCASE ONLY. Go's netip emits lowercase, and a canonical column is what
    -- keeps prefix EQUALITY honest: '2001:DB8::' and '2001:db8::' are the same
    -- network but different text, and a queryable column that holds both cannot
    -- be grouped or joined on. '::', IPv4-mapped forms and uppercase hex are all
    -- refused; loosening a CHECK later is easy, tightening one after rows exist
    -- is not.
    --
    -- The /len suffix is deliberately NOT cross-checked against the group count.
    -- Expressing that in a CHECK costs far more than it buys, and the writer
    -- below is the thing that gets it right; this constraint is the floor that
    -- catches a writer which does not.
    --
    -- THE M1 WRITER'S CONTRACT. There is exactly ONE function that produces a
    -- value for this column, and it:
    --   1. parses with netip.ParseAddr and calls Unmap() FIRST, so an
    --      IPv4-mapped IPv6 address is treated as the IPv4 address it is;
    --   2. masks to /24 for IPv4 and /64 for IPv6 — never finer;
    --   3. formats lowercase, which netip already does;
    --   4. returns NULL when there is no usable address — behind a proxy with no
    --      trusted forwarded header, for a system actor, or on a parse failure.
    --      NULL is the honest answer; a zero address would be a lie that groups.
    -- It ships with the first writer in M1, tested for the IPv4 /24, IPv6 /64,
    -- IPv4-mapped and no-address cases.
    CONSTRAINT audit_events_ip_prefix_shape CHECK (
        ip_prefix IS NULL
        OR ip_prefix ~ '^((25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\.){3}0(/24)?$'
        OR ip_prefix ~ '^[0-9a-f]{1,4}(:[0-9a-f]{1,4}){0,3}::(/(48|64))?$'
    )
);

CREATE INDEX audit_events_occurred_at ON audit_events (occurred_at DESC);
CREATE INDEX audit_events_subject     ON audit_events (subject_type, subject_id);
CREATE INDEX audit_events_actor       ON audit_events (actor_user_id) WHERE actor_user_id IS NOT NULL;
