-- 0004_storage_locations — the storage registry (ADR-005).
--
-- Created in M0 with one `local` default row. `asset_files.storage_location_id`
-- will reference this row in the first media migration (M1); multi-location
-- routing itself is M4 (VZ-STORAGE-004). It exists now because migrations are
-- append-only and a representation row without a storage location from its
-- first day cannot be given one cheaply.
--
-- `config` holds endpoint style, region and SSE flags. It never holds a
-- credential: secrets do not enter a queryable table (ADR-002).

CREATE TABLE storage_locations (
    id         uuid        PRIMARY KEY,
    handle     text        NOT NULL,
    kind       text        NOT NULL,
    is_default boolean     NOT NULL DEFAULT false,
    read_only  boolean     NOT NULL DEFAULT false,
    config     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT storage_locations_kind        CHECK (kind IN ('local', 's3')),
    CONSTRAINT storage_locations_handle_fmt  CHECK (handle ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    -- A read-only location can never be the default: new objects would have
    -- nowhere to go and the failure would surface at upload time.
    CONSTRAINT storage_locations_default_writable CHECK (NOT (is_default AND read_only))
);

CREATE UNIQUE INDEX storage_locations_handle_key ON storage_locations (handle);

-- Exactly one default, enforced by the database rather than by application
-- discipline (AGENTS.md: database constraints for concurrency invariants).
CREATE UNIQUE INDEX storage_locations_one_default ON storage_locations ((is_default)) WHERE is_default;

INSERT INTO storage_locations (id, handle, kind, is_default)
VALUES ('01920000-0000-7000-8000-000000000002', 'local', 'local', true);
