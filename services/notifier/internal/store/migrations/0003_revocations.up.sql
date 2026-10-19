-- Credential revocation (task 4.R2, ADR 0015): when auth revokes a user's credentials, their
-- endpoints are disabled until the owner enables them again (with a new secret).
ALTER TABLE endpoints
    ADD COLUMN disabled_at     TIMESTAMPTZ,
    ADD COLUMN disabled_reason TEXT;

-- The newest revocation per user: access tokens issued at or before it are refused, and a
-- CredentialsRevoked that isn't newer than it was handled before (a replay changes nothing).
CREATE TABLE revocations (
    owner_id   TEXT PRIMARY KEY,
    revoked_at TIMESTAMPTZ NOT NULL
);
