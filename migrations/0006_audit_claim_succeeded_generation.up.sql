-- 0006_audit_claim_succeeded_generation
--
-- A `setup.owner_claim.succeeded` audit row must name the token generation it
-- redeemed (sentinel S-0007).
--
-- Migration 0005 says: "Audit rows and operator-facing output reference the
-- GENERATION, so a claim is traceable with no secret material anywhere." The
-- `minted` and `superseded` rows did; the row recording the redemption did not,
-- so the trail could link a claim to its credential only through the MUTABLE
-- owner_claim_tokens row — which a re-mint racing the claim could overwrite
-- (sentinel S-0002). The code now writes `token_generation`; this makes the
-- database refuse a succeeded row that lacks it, the same way 0003 refuses an
-- ip_prefix more specific than a prefix rather than trusting the writer.
--
-- NOT VALID: the constraint is enforced for every row inserted from now on, and
-- existing rows are not re-checked. Rows written before this migration (a
-- development database that claimed under 0005) predate the obligation, and
-- audit_events is append-only, so there is no correct way to "fix" them —
-- validating would only make this migration fail on such a database. A fresh
-- install has no such rows.
--
-- jsonb_typeof(...) = 'number' also refuses a string "1", a null, and a missing
-- key (jsonb_typeof(NULL) is NULL, and a NULL CHECK result would PASS, hence the
-- IS TRUE form).
ALTER TABLE audit_events
    ADD CONSTRAINT audit_events_claim_succeeded_names_generation CHECK (
        action <> 'setup.owner_claim.succeeded'
        OR (jsonb_typeof(after -> 'token_generation') = 'number') IS TRUE
    ) NOT VALID;
