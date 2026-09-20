-- A purely additive migration, to prove the append-only gate does not block
-- normal work. Nothing references this table; the branch is deleted after the
-- CI run.
CREATE TABLE throwaway_probe (
    id         uuid        PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now()
);
