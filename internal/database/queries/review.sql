-- Product reviews. A review belongs to one purchased order item; buyer data
-- is always read through the user's own orders. A review hidden by
-- moderation (hidden_at) is kept but never public and never counted in the
-- product rating.

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
SELECT r.id, r.order_item_id, r.product_id, r.rating, r.comment, r.created_at, r.edited_at, r.hidden_at,
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

-- UpdateReview is the buyer's edit; moderation stays as it is.
-- name: UpdateReview :exec
UPDATE product_reviews SET rating = @rating, comment = @comment, edited_at = now()
WHERE id = @id AND user_id = @user_id;

-- LockProductRating serializes rating recalculations of one product, so
-- each recalculation sees every committed review.
-- name: LockProductRating :many
SELECT id FROM products WHERE id = @id FOR NO KEY UPDATE;

-- RecalculateProductRating rebuilds the cached rating of one product from
-- its visible reviews in product_reviews (the source of truth).
-- name: RecalculateProductRating :exec
UPDATE products p SET
    rating_average = coalesce((SELECT avg(r.rating)::float8 FROM product_reviews r
                               WHERE r.product_id = p.id AND r.hidden_at IS NULL), 0),
    rating_count   = (SELECT count(*)::int FROM product_reviews r
                      WHERE r.product_id = p.id AND r.hidden_at IS NULL)
WHERE p.id = @id;

-- RecalculateAllRatings repairs every product whose cached rating differs
-- from its visible reviews and returns how many were fixed.
-- name: RecalculateAllRatings :execrows
WITH agg AS (
    SELECT p.id,
           coalesce(avg(r.rating)::float8, 0) AS average,
           count(r.id)::int AS total
    FROM products p
    LEFT JOIN product_reviews r ON r.product_id = p.id AND r.hidden_at IS NULL
    GROUP BY p.id
)
UPDATE products p SET rating_average = agg.average, rating_count = agg.total
FROM agg
WHERE p.id = agg.id AND (p.rating_average <> agg.average OR p.rating_count <> agg.total);

-- name: UserReviews :many
SELECT r.id, r.rating, r.comment, r.created_at, r.edited_at, r.hidden_at,
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

-- ProductReviewPage is one page of a product's visible reviews, newest
-- first, with the star distribution of all its visible reviews on every
-- row: the product page needs one round trip for both. Window counts run
-- before LIMIT, over product_reviews only; buyer names are joined for the
-- page rows alone, only to be masked (email, phone and address are never
-- shown).
-- name: ProductReviewPage :many
WITH page AS (
    SELECT r.id, r.user_id, r.order_item_id, r.rating, r.comment, r.created_at,
           count(*) FILTER (WHERE r.rating = 1) OVER () AS stars1,
           count(*) FILTER (WHERE r.rating = 2) OVER () AS stars2,
           count(*) FILTER (WHERE r.rating = 3) OVER () AS stars3,
           count(*) FILTER (WHERE r.rating = 4) OVER () AS stars4,
           count(*) FILTER (WHERE r.rating = 5) OVER () AS stars5
    FROM product_reviews r
    WHERE r.product_id = @product_id AND r.hidden_at IS NULL
    ORDER BY r.created_at DESC, r.id DESC
    LIMIT @row_limit OFFSET @row_offset
)
SELECT pg.id, pg.rating, pg.comment, pg.created_at, oi.variant_label,
       u.email, coalesce(up.first_name, '')::text AS first_name, coalesce(up.last_name, '')::text AS last_name,
       pg.stars1::int AS stars1, pg.stars2::int AS stars2, pg.stars3::int AS stars3,
       pg.stars4::int AS stars4, pg.stars5::int AS stars5
FROM page pg
JOIN order_items oi ON oi.id = pg.order_item_id
JOIN users u ON u.id = pg.user_id
LEFT JOIN user_profiles up ON up.user_id = pg.user_id
ORDER BY pg.created_at DESC, pg.id DESC;

-- ProductRatingSummary is the distribution alone, for a page past the last
-- review (where ProductReviewPage returns no row to carry it).
-- name: ProductRatingSummary :many
SELECT rating, count(*)::int AS total FROM product_reviews
WHERE product_id = @product_id AND hidden_at IS NULL
GROUP BY rating;

-- name: ReviewProduct :one
SELECT id, name, slug, rating_average, rating_count FROM products WHERE slug = @slug AND active;

-- Moderation (admin).

-- AdminReviews lists reviews newest first. status: "" (all), "visible" or
-- "hidden"; rating 0 means every rating; q searches the product name.
-- Buyer names are read only to be masked.
-- name: AdminReviews :many
SELECT r.id, r.rating, r.comment, r.created_at, r.edited_at, r.hidden_at, r.moderation_reason,
       r.product_id, oi.product_name, oi.variant_label, o.order_number,
       u.email, coalesce(up.first_name, '')::text AS first_name, coalesce(up.last_name, '')::text AS last_name
FROM product_reviews r
JOIN order_items oi ON oi.id = r.order_item_id
JOIN orders o ON o.id = r.order_id
JOIN users u ON u.id = r.user_id
LEFT JOIN user_profiles up ON up.user_id = r.user_id
WHERE (@status::text = '' OR (@status::text = 'hidden') = (r.hidden_at IS NOT NULL))
  AND (@rating::int = 0 OR r.rating = @rating::int)
  AND (@q::text = '' OR oi.product_name ILIKE '%' || @q::text || '%')
ORDER BY r.created_at DESC, r.id DESC
LIMIT @row_limit OFFSET @row_offset;

-- name: AdminReview :one
SELECT r.id, r.rating, r.comment, r.created_at, r.edited_at, r.hidden_at, r.moderation_reason,
       r.product_id, oi.product_name, oi.variant_label, o.order_number,
       u.email, coalesce(up.first_name, '')::text AS first_name, coalesce(up.last_name, '')::text AS last_name
FROM product_reviews r
JOIN order_items oi ON oi.id = r.order_item_id
JOIN orders o ON o.id = r.order_id
JOIN users u ON u.id = r.user_id
LEFT JOIN user_profiles up ON up.user_id = r.user_id
WHERE r.id = @id;

-- name: LockReview :one
SELECT id, product_id, hidden_at FROM product_reviews WHERE id = @id FOR UPDATE;

-- name: HideReview :execrows
UPDATE product_reviews SET hidden_at = now(), hidden_by = @admin_user_id, moderation_reason = @reason
WHERE id = @id AND hidden_at IS NULL;

-- name: UnhideReview :execrows
UPDATE product_reviews SET hidden_at = NULL, hidden_by = NULL, moderation_reason = NULL
WHERE id = @id AND hidden_at IS NOT NULL;
