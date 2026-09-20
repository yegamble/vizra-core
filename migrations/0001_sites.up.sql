-- 0001_sites — the site seam (ADR-007 § Site and tenant seam).
--
-- Exactly one row in core. There is deliberately NO dsn column and NO tenant_id
-- column anywhere: isolation under tenancy is database-per-tenant, and the
-- registry is this row plus the one DATABASE_URL from configuration. A
-- discriminator here would be dead weight that later code would start trusting.
--
-- Migrations are append-only. Every column ADR-007 names for this table is
-- present now, because adding one later is a second migration and a second
-- deploy-ordering problem.

CREATE TABLE sites (
    id           uuid        PRIMARY KEY,
    handle       text        NOT NULL,
    base_url     text        NOT NULL,
    -- Precedence step (1) of the frozen matrix: on a private site an anonymous
    -- viewer is denied every read surface.
    privacy_mode text        NOT NULL DEFAULT 'public',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT sites_handle_format  CHECK (handle ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    CONSTRAINT sites_privacy_mode   CHECK (privacy_mode IN ('public', 'private')),
    CONSTRAINT sites_base_url_shape CHECK (base_url ~ '^https?://')
);

CREATE UNIQUE INDEX sites_handle_key ON sites (handle);

-- EXACTLY one row, enforced by the database rather than by a comment.
--
-- `privacy_mode` is step (1) of the frozen precedence matrix: on a private site
-- an anonymous viewer is denied every read surface. Without this index a second
-- row — a bad import, an M2 admin surface, a restore that merges two dumps —
-- makes the answer depend on which handle sorts first, so a site set to
-- `private` with handle 'z-main' would lose to an accidental 'a-test' row with
-- the default 'public' and the whole instance would open to anonymous readers
-- with no error anywhere.
--
-- Permanently safe: ADR-007 § Site and tenant seam rejects schema-per-tenant
-- and chooses database-per-tenant, so it is one row per DATABASE for the life
-- of the product. Under tenancy (M5) each tenant database carries its own
-- singleton.
CREATE UNIQUE INDEX sites_singleton ON sites ((true));

-- The single default site. The id is a fixed uuidv7-shaped constant so a fresh
-- database is byte-identical on every machine and fixtures can reference it.
INSERT INTO sites (id, handle, base_url)
VALUES ('01920000-0000-7000-8000-000000000001', 'default', 'http://localhost:8080');
