-- Phase 9: API sessions for mobile clients.
--
-- The web keeps its server-side session cookie (sessions table). The REST
-- API uses a separate transport: an opaque Bearer access token (short-lived)
-- and a rotating refresh token (long-lived). Neither is stored in plaintext:
-- only HMAC-SHA256(SESSION_SECRET, token), like web sessions.
--
-- One row is one issued pair. Refreshing rotates: the row is marked
-- rotated_at (its access token stops working at once) and a new row with
-- the same family_id is inserted. Presenting an already rotated refresh
-- token again is reuse: the whole family (the device's login) is revoked.
-- Logging out revokes the family; logout-all revokes every API session of
-- the user. Web sessions are not touched by either.

CREATE SEQUENCE api_session_family_seq;

CREATE TABLE api_sessions (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id            bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    family_id          bigint      NOT NULL,
    access_token_hash  bytea       NOT NULL,
    refresh_token_hash bytea       NOT NULL,
    access_expires_at  timestamptz NOT NULL,
    refresh_expires_at timestamptz NOT NULL,
    -- Free-text label sent by the app at login (e.g. "Pixel 8"), for a
    -- future "devices" screen. Never used for authorization.
    device_name        text        NOT NULL DEFAULT '',
    last_used_at       timestamptz NOT NULL DEFAULT now(),
    rotated_at         timestamptz,
    revoked_at         timestamptz,
    revoked_reason     text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT api_sessions_access_key  UNIQUE (access_token_hash),
    CONSTRAINT api_sessions_refresh_key UNIQUE (refresh_token_hash),
    CONSTRAINT api_sessions_expiry_chk  CHECK (refresh_expires_at >= access_expires_at),
    CONSTRAINT api_sessions_device_chk  CHECK (char_length(device_name) <= 100),
    CONSTRAINT api_sessions_revoked_chk CHECK ((revoked_at IS NULL) = (revoked_reason IS NULL)),
    CONSTRAINT api_sessions_reason_chk  CHECK (revoked_reason IN ('LOGOUT', 'LOGOUT_ALL', 'REUSE_DETECTED'))
);

-- The unique constraints index both token hashes (one indexed lookup per
-- request / refresh). These serve logout-all, family revocation and the
-- expiry cleanup.
CREATE INDEX api_sessions_user_idx    ON api_sessions (user_id) WHERE revoked_at IS NULL;
CREATE INDEX api_sessions_family_idx  ON api_sessions (family_id);
CREATE INDEX api_sessions_expires_idx ON api_sessions (refresh_expires_at);
