-- name: GetDefaultStorageLocation :one
SELECT id, handle, kind, is_default, read_only, config, created_at, updated_at
FROM storage_locations
WHERE is_default
LIMIT 1;

-- name: ListStorageLocations :many
SELECT id, handle, kind, is_default, read_only, config, created_at, updated_at
FROM storage_locations
ORDER BY handle;
