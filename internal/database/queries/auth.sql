-- name: CreateUser :one
INSERT INTO users (email, password_hash)
VALUES (@email, @password_hash)
RETURNING id, email, role, created_at;

-- name: GetUserCredentialsByEmail :one
SELECT id, email, password_hash, role, created_at
FROM users
WHERE email = @email;

-- name: CreateSession :exec
INSERT INTO sessions (token_hash, user_id, expires_at)
VALUES (@token_hash, @user_id, @expires_at);

-- name: GetActiveSession :one
SELECT s.user_id, s.expires_at, u.email, u.role
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = @token_hash
  AND s.expires_at > now();

-- name: ExtendSession :exec
UPDATE sessions SET expires_at = @expires_at WHERE token_hash = @token_hash;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = @token_hash;

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE expires_at <= now();

-- Admin bootstrap (create-admin command). The role is never taken from a
-- form: registration uses CreateUser and gets the CUSTOMER default.

-- name: CreateAdminUser :one
INSERT INTO users (email, password_hash, role)
VALUES (@email, @password_hash, 'ADMIN')
RETURNING id, email, role, created_at;

-- name: UserByEmail :one
SELECT id, email, role, created_at FROM users WHERE email = @email;

-- name: PromoteToAdmin :execrows
UPDATE users SET role = 'ADMIN' WHERE id = @id AND role <> 'ADMIN';

-- name: SetProfileName :exec
INSERT INTO user_profiles (user_id, first_name, last_name)
VALUES (@user_id, @first_name, @last_name)
ON CONFLICT (user_id) DO UPDATE SET first_name = EXCLUDED.first_name, last_name = EXCLUDED.last_name;

-- API sessions (mobile Bearer tokens). Token values are never stored, only
-- their HMAC.

-- CreateAPISession issues a token pair; a NULL family starts a new family
-- (a new login), a family ID continues one (refresh rotation).
-- name: CreateAPISession :one
INSERT INTO api_sessions (user_id, family_id, access_token_hash, refresh_token_hash,
                          access_expires_at, refresh_expires_at, device_name)
VALUES (@user_id, coalesce(sqlc.narg('family_id')::bigint, nextval('api_session_family_seq')),
        @access_token_hash, @refresh_token_hash, @access_expires_at, @refresh_expires_at, @device_name)
RETURNING id, family_id;

-- APISessionByAccess resolves a Bearer token to its session and user in
-- one indexed query; the caller checks expiry, rotation and revocation.
-- name: APISessionByAccess :one
SELECT s.id, s.user_id, s.family_id, s.access_expires_at, s.last_used_at, s.rotated_at, s.revoked_at,
       u.email, u.role
FROM api_sessions s
JOIN users u ON u.id = s.user_id
WHERE s.access_token_hash = @access_token_hash;

-- name: TouchAPISession :exec
UPDATE api_sessions SET last_used_at = now() WHERE id = @id;

-- name: LockAPISessionByRefresh :one
SELECT id, user_id, family_id, refresh_expires_at, rotated_at, revoked_at, device_name
FROM api_sessions
WHERE refresh_token_hash = @refresh_token_hash
FOR UPDATE;

-- name: RotateAPISession :execrows
UPDATE api_sessions SET rotated_at = now(), last_used_at = now()
WHERE id = @id AND rotated_at IS NULL AND revoked_at IS NULL;

-- name: RevokeAPIFamily :execrows
UPDATE api_sessions SET revoked_at = now(), revoked_reason = @reason
WHERE family_id = @family_id AND revoked_at IS NULL;

-- name: RevokeUserAPISessions :execrows
UPDATE api_sessions SET revoked_at = now(), revoked_reason = 'LOGOUT_ALL'
WHERE user_id = @user_id AND revoked_at IS NULL;

-- DeleteExpiredAPISessions drops rows past their refresh expiry (indexed;
-- run lazily at login). Rotated rows of a live family are kept until then,
-- so a replayed refresh token is still recognized as reuse.
-- name: DeleteExpiredAPISessions :execrows
DELETE FROM api_sessions WHERE refresh_expires_at < now();

-- DeleteStaleAPISessions also drops revoked rows older than a day
-- (cleanup-api-sessions command).
-- name: DeleteStaleAPISessions :execrows
DELETE FROM api_sessions WHERE refresh_expires_at < now() OR revoked_at < now() - interval '1 day';

-- name: UserByID :one
SELECT id, email, role, created_at FROM users WHERE id = @id;
