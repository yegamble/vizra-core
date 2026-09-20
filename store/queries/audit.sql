-- name: InsertAuditEvent :one
INSERT INTO audit_events (
    id, occurred_at, actor_kind, actor_user_id, actor_label,
    action, subject_type, subject_id, before, after, correlation_id, ip_prefix
) VALUES (
    $1, now(), $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
)
RETURNING id, occurred_at;

-- name: CountAuditEvents :one
SELECT count(*) FROM audit_events;
