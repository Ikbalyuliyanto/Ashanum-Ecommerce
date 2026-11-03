-- Every cart query is scoped by user_id through the user's ACTIVE cart, so
-- one user can never read or change another user's cart lines.

-- GetPurchasableVariant reads what add-to-cart / buy-now needs to validate a
-- variant: price, stock and whether the variant, product and category are
-- all active.
-- name: GetPurchasableVariant :one
SELECT v.id, v.sku, v.price, v.original_price, v.stock,
       (v.active AND p.active AND c.active)::boolean AS available,
       p.id AS product_id, p.name AS product_name, p.slug AS product_slug
FROM product_variants v
JOIN products p   ON p.id = v.product_id
JOIN categories c ON c.id = p.category_id
WHERE v.id = @id;

-- EnsureActiveCart returns the user's active cart, creating it if needed.
-- The upsert is race-safe: concurrent first adds end up with one cart.
-- name: EnsureActiveCart :one
INSERT INTO carts (user_id) VALUES (@user_id)
ON CONFLICT (user_id) WHERE active DO UPDATE SET updated_at = now()
RETURNING id;

-- AddCartItem inserts a line or adds to its quantity in one atomic
-- statement. It returns no row when the result would exceed max_quantity
-- (stock or the per-line limit) or when the cart already has max_items
-- other lines. cart_quantity is the cart's total quantity after the add.
-- name: AddCartItem :one
WITH up AS (
    INSERT INTO cart_items (cart_id, variant_id, quantity)
    SELECT @cart_id, @variant_id, @quantity
    WHERE EXISTS (SELECT 1 FROM cart_items x WHERE x.cart_id = @cart_id AND x.variant_id = @variant_id)
       OR (SELECT count(*) FROM cart_items x WHERE x.cart_id = @cart_id) < @max_items::int
    ON CONFLICT (cart_id, variant_id) DO UPDATE
        SET quantity = cart_items.quantity + EXCLUDED.quantity
        WHERE cart_items.quantity + EXCLUDED.quantity <= @max_quantity::int
    RETURNING quantity
)
SELECT up.quantity,
       ((SELECT coalesce(sum(x.quantity), 0) FROM cart_items x WHERE x.cart_id = @cart_id) + @quantity)::int AS cart_quantity
FROM up;

-- CartLineState explains a refused add: lines in the cart and the quantity
-- of this variant already there.
-- name: CartLineState :one
SELECT count(*)::int AS lines,
       coalesce(sum(quantity) FILTER (WHERE variant_id = @variant_id), 0)::int AS in_cart
FROM cart_items
WHERE cart_id = @cart_id;

-- GetOwnedCartItem loads one line of the user's active cart together with
-- the current state of its variant (NULL columns when the variant is gone).
-- name: GetOwnedCartItem :one
SELECT ci.id, ci.quantity, v.stock,
       coalesce(v.active AND p.active AND cat.active, false)::boolean AS available
FROM cart_items ci
JOIN carts c ON c.id = ci.cart_id AND c.user_id = @user_id AND c.active
LEFT JOIN product_variants v ON v.id = ci.variant_id
LEFT JOIN products p         ON p.id = v.product_id
LEFT JOIN categories cat     ON cat.id = p.category_id
WHERE ci.id = @id;

-- name: SetCartItemQuantity :execrows
UPDATE cart_items ci SET quantity = @quantity
FROM carts c
WHERE ci.id = @id AND ci.cart_id = c.id AND c.user_id = @user_id AND c.active;

-- name: DeleteCartItem :execrows
DELETE FROM cart_items ci
USING carts c
WHERE ci.id = @id AND ci.cart_id = c.id AND c.user_id = @user_id AND c.active;

-- name: ClearCart :exec
DELETE FROM cart_items ci
USING carts c
WHERE ci.cart_id = c.id AND c.user_id = @user_id AND c.active;

-- CartQuantity is the header badge: total quantity of all lines.
-- name: CartQuantity :one
SELECT coalesce(sum(ci.quantity), 0)::int
FROM cart_items ci
JOIN carts c ON c.id = ci.cart_id
WHERE c.user_id = @user_id AND c.active;

-- ListCartItems returns everything the cart page shows in ONE round trip:
-- line, variant, product, main image, attribute label, current price and
-- stock, and availability flags. LEFT JOINs keep lines whose variant was
-- deleted.
-- name: ListCartItems :many
SELECT ci.id, ci.quantity, ci.variant_id,
       v.sku, v.price, v.original_price, v.stock,
       coalesce(v.active AND p.active AND cat.active, false)::boolean AS available,
       p.name AS product_name, p.slug AS product_slug,
       coalesce((SELECT i.url FROM product_images i
                 WHERE i.product_id = p.id
                 ORDER BY i.display_order, i.id LIMIT 1), '')::text AS image_url,
       coalesce((SELECT string_agg(av.value, ' / ' ORDER BY a.display_order, a.id)
                 FROM product_variant_values vv
                 JOIN product_attributes a        ON a.id = vv.attribute_id
                 JOIN product_attribute_values av ON av.id = vv.value_id
                 WHERE vv.variant_id = v.id), '')::text AS variant_label
FROM cart_items ci
JOIN carts c ON c.id = ci.cart_id AND c.user_id = @user_id AND c.active
LEFT JOIN product_variants v ON v.id = ci.variant_id
LEFT JOIN products p         ON p.id = v.product_id
LEFT JOIN categories cat     ON cat.id = p.category_id
ORDER BY ci.created_at DESC, ci.id DESC;
