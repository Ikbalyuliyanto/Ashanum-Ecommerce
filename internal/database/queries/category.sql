-- name: ListActiveCategories :many
SELECT * FROM categories
WHERE active
ORDER BY display_order, name;

-- name: GetCategory :one
SELECT * FROM categories WHERE id = @id;

-- name: CreateCategory :one
INSERT INTO categories (name, slug, image_url, active, display_order)
VALUES (@name, @slug, @image_url, @active, @display_order)
RETURNING *;

-- name: UpdateCategory :one
UPDATE categories
SET name          = @name,
    slug          = @slug,
    image_url     = @image_url,
    active        = @active,
    display_order = @display_order
WHERE id = @id
RETURNING *;
