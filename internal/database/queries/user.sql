-- name: GetAccount :one
SELECT u.id, u.email, u.role, u.created_at,
       coalesce(p.first_name, '')::text AS first_name,
       coalesce(p.last_name, '')::text  AS last_name,
       p.phone,
       p.gender
FROM users u
LEFT JOIN user_profiles p ON p.user_id = u.id
WHERE u.id = @user_id;

-- name: UpsertProfile :exec
INSERT INTO user_profiles (user_id, first_name, last_name, phone, gender)
VALUES (@user_id, @first_name, @last_name, @phone, @gender)
ON CONFLICT (user_id) DO UPDATE
SET first_name = EXCLUDED.first_name,
    last_name  = EXCLUDED.last_name,
    phone      = EXCLUDED.phone,
    gender     = EXCLUDED.gender;
