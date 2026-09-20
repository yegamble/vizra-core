-- name: GetSiteByHandle :one
SELECT id, handle, base_url, privacy_mode, created_at, updated_at
FROM sites
WHERE handle = $1;

-- name: GetDefaultSite :one
-- Core has exactly one site row (ADR-007). Ordering by handle makes the result
-- deterministic even if a future migration ever adds one.
SELECT id, handle, base_url, privacy_mode, created_at, updated_at
FROM sites
ORDER BY handle
LIMIT 1;

-- name: CountSites :one
SELECT count(*) FROM sites;
