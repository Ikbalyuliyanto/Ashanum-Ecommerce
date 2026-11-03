-- Every query is scoped by user_id so one user can never read or change
-- another user's address. sqlc expands * into an explicit column list.

-- name: ListAddresses :many
SELECT * FROM user_addresses
WHERE user_id = @user_id
ORDER BY is_primary DESC, id DESC;

-- name: GetAddress :one
SELECT * FROM user_addresses
WHERE id = @id AND user_id = @user_id;

-- name: CountAddresses :one
SELECT count(*) FROM user_addresses WHERE user_id = @user_id;

-- LockUserAddresses serializes address writes of one user (taken first in
-- every write transaction), so concurrent "set primary" calls cannot race.
-- name: LockUserAddresses :one
SELECT id FROM users WHERE id = @user_id FOR NO KEY UPDATE;

-- name: CreateAddress :one
INSERT INTO user_addresses (
    user_id, label, recipient_name, phone,
    province, city, district, village, postal_code, street_address,
    latitude, longitude, maps_url, is_primary
) VALUES (
    @user_id, @label, @recipient_name, @phone,
    @province, @city, @district, @village, @postal_code, @street_address,
    @latitude, @longitude, @maps_url, @is_primary
)
RETURNING *;

-- name: UpdateAddress :one
UPDATE user_addresses
SET label          = @label,
    recipient_name = @recipient_name,
    phone          = @phone,
    province       = @province,
    city           = @city,
    district       = @district,
    village        = @village,
    postal_code    = @postal_code,
    street_address = @street_address,
    latitude       = @latitude,
    longitude      = @longitude,
    maps_url       = @maps_url,
    is_primary     = @is_primary
WHERE id = @id AND user_id = @user_id
RETURNING *;

-- name: ClearPrimaryAddress :exec
UPDATE user_addresses SET is_primary = false
WHERE user_id = @user_id AND is_primary;

-- name: MarkPrimaryAddress :execrows
UPDATE user_addresses SET is_primary = true
WHERE id = @id AND user_id = @user_id;

-- name: DeleteAddress :one
DELETE FROM user_addresses
WHERE id = @id AND user_id = @user_id
RETURNING is_primary;

-- name: PromoteNewestAddress :exec
UPDATE user_addresses SET is_primary = true
WHERE id = (
    SELECT a.id FROM user_addresses a
    WHERE a.user_id = @user_id
    ORDER BY a.id DESC
    LIMIT 1
);
