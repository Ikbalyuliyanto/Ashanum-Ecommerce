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
