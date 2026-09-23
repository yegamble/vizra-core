-- Rollback of 0005. Drop order is reverse-dependency: `credentials.user_id`
-- references `users`, and the audit foreign key must go before `users` can.
--
-- audit_events rows survive with actor_user_id values that no longer reference
-- anything. That is correct: a down migration rolls back SCHEMA, and destroying
-- audit history to satisfy a constraint that no longer exists would be the worse
-- failure.
--
-- CONSEQUENCE, stated here because these bytes freeze on merge: RE-APPLYING 0005
-- after this down will FAIL with 23503 on any database that ever recorded an
-- actor_kind='user' audit event — which is every database that was ever claimed.
-- The up file's
--     ALTER TABLE audit_events ADD CONSTRAINT audit_events_actor_user_fk
--     FOREIGN KEY (actor_user_id) REFERENCES users (id) ON DELETE RESTRICT
-- VALIDATES existing rows, and after this down their actor_user_id resolves to
-- nothing because `users` was dropped.
--
-- So: THIS FILE EXISTS FOR A DEVELOPMENT DATABASE THAT WAS NEVER CLAIMED. On a
-- claimed instance the supported rollback is a restore from backup, not this
-- file.
--
-- The up file deliberately does NOT use ADD CONSTRAINT ... NOT VALID: that would
-- weaken the constraint's guarantee on every fresh database in order to buy a
-- rollback path for a case restore already covers.
DROP TRIGGER IF EXISTS audit_events_refuse_truncation ON audit_events;
DROP TRIGGER IF EXISTS audit_events_no_update_or_delete ON audit_events;
DROP FUNCTION IF EXISTS audit_events_append_only();
ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS audit_events_actor_user_fk;
DROP TABLE IF EXISTS owner_claim_tokens;
DROP TABLE IF EXISTS credentials;
DROP TABLE IF EXISTS users;
DROP TYPE IF EXISTS user_role;
