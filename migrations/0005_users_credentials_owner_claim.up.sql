-- 0005: the owner account, its password credential, and the one-time claim token
-- that creates it (VZ-INSTALL-003, ADR-003 § Credentials, ADR-007 § Entities and keys).
--
-- Invariants that are DATABASE facts here, not application conventions:
--   1. users_one_owner          — at most one LIVE row with role 'owner'.
--   2. owner_claim_tokens PK    — at most one claim-token row, ever.
--   3. audit_events_append_only — UPDATE, DELETE and TRUNCATE are all refused.
--
-- There is no site_id and no tenant_id column: isolation under tenancy is
-- database-per-tenant (ADR-007), so "one owner per site" IS "one owner per
-- database" and needs no discriminator.
--
-- FOLDING RULE (frozen here; M1-B inherits it).
-- username_fold and email_fold are GENERATED ALWAYS AS (lower(...)) STORED.
-- Folding is PostgreSQL's lower(), in SQL, on write AND on every later lookup.
-- Never fold in Go: strings.ToLower and PostgreSQL's lower() are not the same
-- function for non-ASCII input, and `email` accepts UTF-8 (its shape CHECK only
-- excludes whitespace and '@'). A sign-in that folds Go-side can lock the owner
-- out of the only privileged account on the instance. Generated columns make the
-- divergence unrepresentable rather than merely detectable.
--
-- If the folding rule ever has to change, the generated expressions cannot be
-- ALTERed in place — but an additive escape exists and is not matched by
-- migrate-lint's destructive pattern:
--     ALTER TABLE users ALTER COLUMN email_fold DROP EXPRESSION   (PostgreSQL 13+)
-- followed by a backfill. Recorded so the next writer does not assume the column
-- is frozen beyond recovery.
--
-- PASSWORD RULE (frozen here; M1-B inherits it).
-- The password is hashed as the RAW UTF-8 BYTES of the submitted field, with NO
-- Unicode normalisation, and is bounded in BYTES (<= 1024 octets) as well as in
-- characters. M1-A writes the hash and M1-B verifies it; a silent divergence in
-- normalisation is the same lockout as a divergence in folding.
--
-- ERASURE AND RETENTION.
-- 0003 deferred the immutability trigger and "the audited retention path it has
-- to coexist with" as a pair. This migration lands the trigger only, by chair
-- ruling (war-room tick 86). The consequences, stated so the next writer does not
-- have to rediscover them:
--   * Deleting a user row is IMPOSSIBLE while audit rows name it, because
--     audit_events_actor_user_fk is ON DELETE RESTRICT. It must be: the frozen
--     audit_events_actor_identified CHECK asserts
--     (actor_kind = 'user') = (actor_user_id IS NOT NULL), so ON DELETE SET NULL
--     would leave a row violating a constraint 0003 froze.
--   * Erasure is therefore scrub-and-tombstone of `users`, never DELETE.
--   * Consequently audit `before`, `after` and `actor_label` must NEVER carry an
--     email address or any PII beyond the username. Every later emitter inherits
--     that rule.
--   * The audited retention / anonymisation path is still owed and carries its
--     own ledger ID. It can be added ADDITIVELY: migrate-lint's destructive
--     pattern does not match CREATE OR REPLACE FUNCTION, so a later migration can
--     widen audit_events_append_only() without an annotated-destructive drop.

-- Rank order is load-bearing: it must equal authz.Role's ranking for the five
-- STORED roles. 'anonymous' is deliberately absent — it is never a stored
-- principal, only a request-time subject. A future role is added with
-- ALTER TYPE ... ADD VALUE in its own migration carrying '-- no-down: <reason>',
-- because an enum value cannot be removed.
CREATE TYPE user_role AS ENUM ('guest', 'member', 'manager', 'admin', 'owner');

CREATE TABLE users (
    id             uuid        PRIMARY KEY,              -- uuidv7, minted in Go
    username       text        NOT NULL,
    username_fold  text        GENERATED ALWAYS AS (lower(username)) STORED,
    email          text        NOT NULL,
    email_fold     text        GENERATED ALWAYS AS (lower(email)) STORED,
    role           user_role   NOT NULL DEFAULT 'member',
    disabled_at    timestamptz NULL,
    tombstoned_at  timestamptz NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    -- 3..30 characters, letters/digits/hyphen/underscore, leading alphanumeric.
    -- The Go validator compiles THIS literal; a test reads this file's bytes and
    -- asserts the two are identical, so no input the API accepts can reach the
    -- CHECK and become a 500.
    --
    -- ASCII-ONLY IS A DECISION, not an oversight, and it is effectively forever:
    -- widening it later needs an annotated-destructive CHECK swap. The username
    -- appears in /u/{username} and is the account's public handle, where a
    -- Unicode homograph is an impersonation primitive — two visually identical
    -- handles owned by different people. `display_name` carries the Unicode
    -- identity instead, which is where a global photo community needs it
    -- (VZ-I18N-001). The trade is legibility of a URL-bearing identifier against
    -- expressiveness of a display field, and it is taken knowingly.
    CONSTRAINT users_username_shape CHECK (username ~ '^[A-Za-z0-9][A-Za-z0-9_-]{2,29}$'),
    -- The bound is BYTES. JSON Schema maxLength counts characters, so a
    -- character bound alone would disagree with this CHECK on any non-ASCII
    -- address and turn a legal request into a 23514.
    CONSTRAINT users_email_shape CHECK (
        email ~ '^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$'
        AND octet_length(email) BETWEEN 3 AND 254)
);

CREATE UNIQUE INDEX users_username_fold_key ON users (username_fold);

-- Partial from day one. VZ-AUTH-006 declares an email-less social signup as a
-- future state; 'ALTER COLUMN email DROP NOT NULL' is additive, while replacing a
-- total index with a partial one later would be an annotated-destructive
-- DROP INDEX.
CREATE UNIQUE INDEX users_email_fold_key ON users (email_fold) WHERE email_fold IS NOT NULL;

-- THE invariant of this slice. Every live row with role 'owner' carries the same
-- index key, so a second one raises 23505. This is what decides a concurrent
-- claim race: not an advisory lock, not SELECT ... FOR UPDATE, not a Go mutex.
--
-- The tombstone predicate is deliberate. Without it, tombstoning the owner would
-- brick the instance permanently: the key would stay held, ADR-003 forbids
-- demoting the owner, no claim token can be minted because that gate is
-- EXISTS(users), and the row cannot be deleted because audit_events RESTRICTs it.
-- Tombstoning does NOT reopen the claim endpoint, because that gate is
-- EXISTS(users) and not this index.
CREATE UNIQUE INDEX users_one_owner ON users (role)
    WHERE role = 'owner' AND tombstoned_at IS NULL;

-- `credentials` holds at most ONE VERIFIER SECRET PER (user, kind) — 'password'
-- now, 'totp' later. It is NOT the home for api_keys (many per user, scoped,
-- revocable, with last_used), oauth_identities (unique on (provider, subject)) or
-- step-up rows: ADR-003 and ADR-007 give each of those its own table. Do not
-- widen this one into them.
--
-- kind is text + CHECK rather than an enum: it has no ordering semantics, and a
-- text CHECK widens in one reversible annotated migration whereas an enum value
-- can never be removed.
CREATE TABLE credentials (
    id          uuid        PRIMARY KEY,
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind        text        NOT NULL,
    secret      text        NOT NULL,   -- a verifier, never a reversible value
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT credentials_kind CHECK (kind IN ('password')),
    -- This CHECK's job is exactly "never a plaintext value, never another
    -- scheme". The precise parameters (v=19, m=19456,t=2,p=1, a 16-byte salt and
    -- a 32-byte tag — ADR-003 § Credentials) are pinned by a Go unit test, which
    -- can move when those parameters are raised. Pinning them here instead would
    -- make a parameter raise a schema fight.
    CONSTRAINT credentials_password_is_argon2id CHECK (
        kind <> 'password' OR secret LIKE '$argon2id$%'),
    -- 1024 octets leaves room for an envelope-encrypted TOTP secret plus its KEK
    -- metadata, so adding 'totp' later needs no CHECK swap.
    CONSTRAINT credentials_secret_bounded CHECK (octet_length(secret) BETWEEN 1 AND 1024)
);

CREATE UNIQUE INDEX credentials_one_per_user_per_kind ON credentials (user_id, kind);

CREATE TABLE owner_claim_tokens (
    -- Boolean primary key pinned TRUE by the CHECK: the table can hold at most
    -- one row, ever, so "the live claim token" is a schema fact and a re-mint is
    -- an upsert on a fixed key.
    id            boolean     PRIMARY KEY DEFAULT true,
    -- SHA-256 of the normalised raw token. The raw token is NEVER stored, never
    -- returned and unrecoverable by design: a lost token is re-minted, not
    -- recovered. SHA-256 rather than a slow KDF because the token is 256 bits of
    -- uniform randomness — there is nothing to grind, and a slow KDF on an
    -- unauthenticated endpoint is a CPU amplifier handed to the attacker.
    token_sha256  bytea       NOT NULL,
    -- Increments on every mint. Audit rows and operator-facing output reference
    -- the GENERATION, so a claim is traceable with no secret material anywhere.
    generation    bigint      NOT NULL DEFAULT 1,
    minted_at     timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    consumed_at   timestamptz NULL,
    -- Set when a boot or `vizra claim-token` retires a token without consuming it.
    superseded_at timestamptz NULL,

    CONSTRAINT owner_claim_tokens_singleton  CHECK (id),
    CONSTRAINT owner_claim_tokens_digest_len CHECK (octet_length(token_sha256) = 32),
    CONSTRAINT owner_claim_tokens_ttl        CHECK (expires_at > minted_at),
    CONSTRAINT owner_claim_tokens_generation CHECK (generation >= 1),
    -- A token is consumed or superseded, never both.
    CONSTRAINT owner_claim_tokens_one_terminal_state CHECK (
        consumed_at IS NULL OR superseded_at IS NULL)
);

-- ---------------------------------------------------------------------------
-- The two obligations 0003_audit_events deferred to "the first M1 writer".
-- ---------------------------------------------------------------------------

ALTER TABLE audit_events
    ADD CONSTRAINT audit_events_actor_user_fk
    FOREIGN KEY (actor_user_id) REFERENCES users (id) ON DELETE RESTRICT;

CREATE FUNCTION audit_events_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only (attempted %)', TG_OP
        USING ERRCODE = '42501';
END;
$$;

CREATE TRIGGER audit_events_no_update_or_delete
    BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_append_only();

-- A row-level trigger does not fire on TRUNCATE, which would let a single
-- statement destroy the entire trail the other trigger exists to protect.
-- Verified before adding this: no TRUNCATE appears in any Go, SQL or shell source
-- in this repository, and the integration harness resets a test database with
-- DROP SCHEMA public CASCADE, so nothing depends on truncating this table.
--
-- Written on one line because migrate-lint's destructive pattern is a
-- case-insensitive keyword match and its exemption comment must sit on the line
-- immediately above every matching line.
-- allow-destructive: this statement NAMES truncation only in order to REFUSE it; it destroys nothing.
CREATE TRIGGER audit_events_refuse_truncation BEFORE TRUNCATE ON audit_events FOR EACH STATEMENT EXECUTE FUNCTION audit_events_append_only();
