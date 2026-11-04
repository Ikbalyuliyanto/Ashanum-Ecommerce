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

-- AdminCategories lists every category (active or not) with its product
-- count. Categories are deactivated rather than deleted: products and
-- order history keep pointing at them.
-- name: AdminCategories :many
SELECT c.*, (SELECT count(*) FROM products p WHERE p.category_id = c.id)::int AS product_count
FROM categories c
ORDER BY c.display_order, c.name, c.id;
