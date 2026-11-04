-- Product reviews. A review belongs to one purchased order item; buyer data
-- is always read through the user's own orders.

-- ReviewTarget is an order item of the user with its order status and the
-- existing review, if any.
-- name: ReviewTarget :one
SELECT oi.id, oi.order_id, oi.product_id, oi.product_name, oi.product_slug, oi.variant_label, oi.image_url,
       o.order_number, o.status, r.id AS review_id
FROM order_items oi
JOIN orders o ON o.id = oi.order_id
LEFT JOIN product_reviews r ON r.order_item_id = oi.id
WHERE oi.id = @order_item_id AND o.user_id = @user_id;

-- Same as ReviewTarget, holding the order row (its status) until commit.
-- name: LockReviewTarget :one
SELECT oi.id, oi.order_id, oi.product_id, oi.product_name, oi.product_slug, oi.variant_label, oi.image_url,
       o.order_number, o.status, r.id AS review_id
FROM order_items oi
JOIN orders o ON o.id = oi.order_id
LEFT JOIN product_reviews r ON r.order_item_id = oi.id
WHERE oi.id = @order_item_id AND o.user_id = @user_id
FOR SHARE OF o;

-- name: CreateReview :one
INSERT INTO product_reviews (user_id, order_id, order_item_id, product_id, rating, comment)
VALUES (@user_id, @order_id, @order_item_id, @product_id, @rating, @comment)
RETURNING id;

-- name: UserReview :one
SELECT r.id, r.order_item_id, r.product_id, r.rating, r.comment, r.created_at, r.updated_at,
       oi.product_name, oi.product_slug, oi.variant_label, oi.image_url, o.order_number
FROM product_reviews r
JOIN order_items oi ON oi.id = r.order_item_id
JOIN orders o ON o.id = r.order_id
WHERE r.id = @id AND r.user_id = @user_id;

-- name: LockUserReview :one
SELECT r.id, r.product_id
FROM product_reviews r
WHERE r.id = @id AND r.user_id = @user_id
FOR UPDATE;

-- name: UpdateReview :exec
UPDATE product_reviews SET rating = @rating, comment = @comment
WHERE id = @id AND user_id = @user_id;

-- LockProductRating serializes rating recalculations of one product, so
-- each recalculation sees every committed review.
-- name: LockProductRating :many
SELECT id FROM products WHERE id = @id FOR NO KEY UPDATE;

-- RecalculateProductRating rebuilds the cached rating of one product from
-- product_reviews (the source of truth).
-- name: RecalculateProductRating :exec
UPDATE products p SET
    rating_average = coalesce((SELECT avg(r.rating)::float8 FROM product_reviews r WHERE r.product_id = p.id), 0),
    rating_count   = (SELECT count(*)::int FROM product_reviews r WHERE r.product_id = p.id)
WHERE p.id = @id;

-- RecalculateAllRatings repairs every product whose cached rating differs
-- from product_reviews and returns how many were fixed.
-- name: RecalculateAllRatings :execrows
WITH agg AS (
    SELECT p.id,
           coalesce(avg(r.rating)::float8, 0) AS average,
           count(r.id)::int AS total
    FROM products p
    LEFT JOIN product_reviews r ON r.product_id = p.id
    GROUP BY p.id
)
UPDATE products p SET rating_average = agg.average, rating_count = agg.total
FROM agg
WHERE p.id = agg.id AND (p.rating_average <> agg.average OR p.rating_count <> agg.total);

-- name: UserReviews :many
SELECT r.id, r.rating, r.comment, r.created_at, r.updated_at,
       oi.product_name, oi.product_slug, oi.variant_label, oi.image_url, o.order_number,
       (p.id IS NOT NULL AND p.active)::boolean AS product_listed
FROM product_reviews r
JOIN order_items oi ON oi.id = r.order_item_id
JOIN orders o ON o.id = r.order_id
LEFT JOIN products p ON p.id = r.product_id
WHERE r.user_id = @user_id
ORDER BY r.created_at DESC, r.id DESC
LIMIT @row_limit OFFSET @row_offset;

-- PendingReviews are items of the user's completed orders without a review.
-- name: PendingReviews :many
SELECT oi.id, oi.product_name, oi.variant_label, oi.image_url, o.order_number, o.completed_at
FROM order_items oi
JOIN orders o ON o.id = oi.order_id
WHERE o.user_id = @user_id AND o.status = 'COMPLETED'
  AND NOT EXISTS (SELECT 1 FROM product_reviews r WHERE r.order_item_id = oi.id)
ORDER BY o.completed_at DESC, oi.id
LIMIT @row_limit OFFSET @row_offset;

-- name: PendingReviewCount :one
SELECT count(*)::int
FROM order_items oi
JOIN orders o ON o.id = oi.order_id
WHERE o.user_id = @user_id AND o.status = 'COMPLETED'
  AND NOT EXISTS (SELECT 1 FROM product_reviews r WHERE r.order_item_id = oi.id);

-- name: OrderReviews :many
SELECT order_item_id, id FROM product_reviews WHERE order_id = @order_id AND user_id = @user_id;

-- ProductReviews lists a product's reviews, newest first. Names are only
-- read to be masked; email, phone and address are never shown.
-- name: ProductReviews :many
SELECT r.id, r.rating, r.comment, r.created_at, oi.variant_label,
       u.email, coalesce(up.first_name, '')::text AS first_name, coalesce(up.last_name, '')::text AS last_name
FROM product_reviews r
JOIN order_items oi ON oi.id = r.order_item_id
JOIN users u ON u.id = r.user_id
LEFT JOIN user_profiles up ON up.user_id = r.user_id
WHERE r.product_id = @product_id
ORDER BY r.created_at DESC, r.id DESC
LIMIT @row_limit OFFSET @row_offset;

-- name: ProductRatingSummary :many
SELECT rating, count(*)::int AS total FROM product_reviews WHERE product_id = @product_id GROUP BY rating;

-- name: ReviewProduct :one
SELECT id, name, slug, rating_average, rating_count FROM products WHERE slug = @slug AND active;
