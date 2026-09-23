-- Owner claim (VZ-INSTALL-003, migration 0005).
--
-- The raw token never appears here: the service hashes the presented value and
-- compares the digest in constant time in Go, then passes THE ROW'S OWN digest to
-- ClaimOwner. Attacker-controlled bytes never reach SQL, so no index probe can
-- become a timing oracle.

-- name: AnyUserExists :one
-- The claim gate. "Unclaimed" is "no user rows at all", not "no owner row", so an
-- instance that already has users is implicitly claimed and never mints again —
-- and tombstoning the owner does not reopen the claim endpoint.
SELECT EXISTS (SELECT 1 FROM users) AS any_user;

-- name: GetOwnerClaimToken :one
-- The single token row, if any. A plain fetch, never a lookup keyed by the
-- presented value. `live` is computed with the DATABASE clock (ADR-004), so
-- expiry is never decided by the application host.
SELECT token_sha256,
       generation,
       minted_at,
       expires_at,
       consumed_at,
       superseded_at,
       (consumed_at IS NULL AND superseded_at IS NULL AND expires_at > now()) AS live
  FROM owner_claim_tokens
 WHERE id;

-- name: MintOwnerClaimToken :one
-- Mint or re-mint the single token row. Re-minting bumps the generation and
-- overwrites the digest IN PLACE, so invalidation of the previous token is
-- atomic and total: the old digest ceases to exist at commit, and no second copy
-- of a credential verifier is retained at rest. Timestamps come from the
-- database clock.
--
-- WHERE NOT EXISTS (SELECT 1 FROM users) makes "never mint on a claimed
-- instance" a property of the STATEMENT rather than of the callers that happen
-- to check first. Both current callers do check — the CLI under the advisory
-- lock, boot by branching earlier — so there is no live defect; but this slice's
-- thesis is that an invariant of this class belongs in the database, and a
-- future owner-transfer or re-claim route is exactly where a Go-only guard
-- breaks. Zero rows -> pgx.ErrNoRows -> the caller's existing ErrHasUsers.
INSERT INTO owner_claim_tokens (id, token_sha256, generation, expires_at)
SELECT true, sqlc.arg('token_sha256')::bytea, 1, now() + sqlc.arg('ttl')::interval
 WHERE NOT EXISTS (SELECT 1 FROM users)
ON CONFLICT (id) DO UPDATE
   SET token_sha256  = EXCLUDED.token_sha256,
       generation    = owner_claim_tokens.generation + 1,
       minted_at     = now(),
       expires_at    = now() + sqlc.arg('ttl')::interval,
       consumed_at   = NULL,
       superseded_at = NULL
RETURNING generation, minted_at, expires_at;

-- name: SupersedeLiveOwnerClaimToken :execrows
-- Retire a live token without consuming it: used by `vizra claim-token` before it
-- mints, and by boot when users already exist (an implicitly claimed instance
-- must never hold a live owner-creating credential).
UPDATE owner_claim_tokens
   SET superseded_at = now()
 WHERE id
   AND consumed_at   IS NULL
   AND superseded_at IS NULL;

-- name: ClaimOwner :one
-- Redeem the token and create THE owner plus its password credential, atomically.
--
-- The guarded UPDATE must be the CTE the INSERT selects from. A data-modifying
-- CTE always executes, so if the insert came first and the guarded update matched
-- nothing, the owner row would still be written. `owner` selecting FROM consumed
-- forces the redeem to produce a row before any user exists.
--
-- Under READ COMMITTED (pinned explicitly on the transaction, never inherited
-- from the server GUC) a losing concurrent claimer blocks on the row lock,
-- re-evaluates `consumed_at IS NULL` against the committed row, matches nothing
-- and returns no row. `users_one_owner` is the second, independent enforcer.
WITH consumed AS (
    UPDATE owner_claim_tokens
       SET consumed_at = now()
     WHERE id
       AND token_sha256  = sqlc.arg('token_sha256')::bytea
       AND consumed_at   IS NULL
       -- superseded_at is unreachable today: the only writer is a boot that
       -- already found users, in which state Claim refuses before reading the
       -- token. It is kept deliberately as DEFENCE IN DEPTH — it costs one
       -- predicate and becomes live the moment a slice supersedes without
       -- minting on an unclaimed instance.
       AND superseded_at IS NULL
       AND expires_at    > now()
       -- "an instance that already has users is implicitly claimed" is the one
       -- claim invariant that was enforced only in Go. This makes it a property
       -- of the STATEMENT as well. The Go gate stays, because it decides the
       -- ANSWER (409 vs 403); this decides the GUARANTEE.
       --
       -- The sibling CTE's INSERT is not visible here: every CTE sees the same
       -- pre-statement snapshot, so this cannot refuse the owner the same
       -- statement is creating.
       AND NOT EXISTS (SELECT 1 FROM users)
    RETURNING generation
),
owner AS (
    INSERT INTO users (id, username, email, role)
    SELECT sqlc.arg('user_id')::uuid,
           sqlc.arg('username')::text,
           sqlc.arg('email')::text,
           'owner'
      FROM consumed
    RETURNING id, username, role
),
cred AS (
    INSERT INTO credentials (id, user_id, kind, secret)
    SELECT sqlc.arg('credential_id')::uuid,
           owner.id,
           'password',
           sqlc.arg('password_hash')::text
      FROM owner
    RETURNING id
)
SELECT owner.id,
       owner.username,
       owner.role,
       (SELECT generation FROM consumed) AS token_generation
  FROM owner, cred;
