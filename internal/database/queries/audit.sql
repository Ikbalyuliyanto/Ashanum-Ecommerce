-- Admin audit log: append-only (a trigger refuses UPDATE and DELETE).

-- name: InsertAdminAudit :exec
INSERT INTO admin_audit_logs (admin_user_id, action, entity_type, entity_id, entity_label, metadata)
VALUES (@admin_user_id, @action, @entity_type, sqlc.narg('entity_id'), @entity_label, sqlc.narg('metadata'));

-- AdminAuditLogs lists entries newest first; entity_type "" means all and
-- a NULL entity_id means every entity of that type.
-- name: AdminAuditLogs :many
SELECT a.id, a.action, a.entity_type, a.entity_id, a.entity_label, a.metadata, a.created_at, u.email AS admin_email
FROM admin_audit_logs a
JOIN users u ON u.id = a.admin_user_id
WHERE (@entity_type::text = '' OR a.entity_type = @entity_type::text)
  AND (sqlc.narg(entity_id)::bigint IS NULL OR a.entity_id = sqlc.narg(entity_id))
ORDER BY a.created_at DESC, a.id DESC
LIMIT @row_limit OFFSET @row_offset;
