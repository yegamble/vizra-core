-- Rollback of 0005. Drop order is reverse-dependency: `credentials.user_id`
-- references `users`, and the audit foreign key must go before `users` can.
--
-- audit_events rows survive with actor_user_id values that no longer reference
-- anything. That is correct: a down migration rolls back SCHEMA, and destroying
-- audit history to satisfy a constraint that no longer exists would be the worse
-- failure.
DROP TRIGGER IF EXISTS audit_events_refuse_truncation ON audit_events;
DROP TRIGGER IF EXISTS audit_events_no_update_or_delete ON audit_events;
DROP FUNCTION IF EXISTS audit_events_append_only();
ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS audit_events_actor_user_fk;
DROP TABLE IF EXISTS owner_claim_tokens;
DROP TABLE IF EXISTS credentials;
DROP TABLE IF EXISTS users;
DROP TYPE IF EXISTS user_role;
