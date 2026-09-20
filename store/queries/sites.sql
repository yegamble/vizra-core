-- name: GetSiteByHandle :one
SELECT id, handle, base_url, privacy_mode, created_at, updated_at
FROM sites
WHERE handle = $1;

-- name: GetDefaultSite :one
-- Core has exactly one site row (ADR-007), enforced by the sites_singleton
-- unique index in migration 0001.
--
-- No ORDER BY and no LIMIT, deliberately. An ORDER BY ... LIMIT 1 would make a
-- second row a SILENT wrong answer — whichever handle sorts first wins, and
-- `privacy_mode` is step (1) of the frozen precedence matrix, so a private site
-- could quietly start answering as a public one. Without the LIMIT, a second
-- row is a loud :one error at the first read. "Deterministic" is not the
-- property that matters here; "correct" is.
SELECT id, handle, base_url, privacy_mode, created_at, updated_at
FROM sites;

-- name: CountSites :one
SELECT count(*) FROM sites;
