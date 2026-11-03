-- Checkout reads catalog and cart rows and, inside one transaction, locks
-- them (variants in id order) before creating the order.

-- name: CheckoutCartLines :many
SELECT ci.id, ci.variant_id, ci.quantity
FROM cart_items ci
JOIN carts c ON c.id = ci.cart_id
WHERE c.user_id = @user_id AND c.active AND ci.id = ANY(@ids::bigint[])
ORDER BY ci.id;

-- Same as CheckoutCartLines, locking the lines so the same cart lines
-- cannot be checked out twice concurrently.
-- name: LockCheckoutCartLines :many
SELECT ci.id, ci.variant_id, ci.quantity
FROM cart_items ci
JOIN carts c ON c.id = ci.cart_id
WHERE c.user_id = @user_id AND c.active AND ci.id = ANY(@ids::bigint[])
ORDER BY ci.id
FOR UPDATE OF ci;

-- name: CheckoutVariants :many
SELECT v.id, v.sku, v.price, v.original_price, v.stock,
       (v.active AND p.active AND c.active)::boolean AS available,
       p.id AS product_id, p.name AS product_name, p.slug AS product_slug,
       coalesce((SELECT i.url FROM product_images i WHERE i.product_id = p.id
                 ORDER BY i.display_order, i.id LIMIT 1), '')::text AS image_url,
       coalesce((SELECT string_agg(av.value, ' / ' ORDER BY a.display_order, a.id)
                 FROM product_variant_values vv
                 JOIN product_attributes a        ON a.id = vv.attribute_id
                 JOIN product_attribute_values av ON av.id = vv.value_id
                 WHERE vv.variant_id = v.id), '')::text AS variant_label
FROM product_variants v
JOIN products p   ON p.id = v.product_id
JOIN categories c ON c.id = p.category_id
WHERE v.id = ANY(@ids::bigint[])
ORDER BY v.id;

-- Same as CheckoutVariants, row-locking the variants. ORDER BY id makes
-- every checkout lock variants in the same order, so two checkouts never
-- deadlock on each other's variants; a concurrent checkout of the same
-- variant waits and then sees the updated stock.
-- name: LockCheckoutVariants :many
SELECT v.id, v.sku, v.price, v.original_price, v.stock,
       (v.active AND p.active AND c.active)::boolean AS available,
       p.id AS product_id, p.name AS product_name, p.slug AS product_slug,
       coalesce((SELECT i.url FROM product_images i WHERE i.product_id = p.id
                 ORDER BY i.display_order, i.id LIMIT 1), '')::text AS image_url,
       coalesce((SELECT string_agg(av.value, ' / ' ORDER BY a.display_order, a.id)
                 FROM product_variant_values vv
                 JOIN product_attributes a        ON a.id = vv.attribute_id
                 JOIN product_attribute_values av ON av.id = vv.value_id
                 WHERE vv.variant_id = v.id), '')::text AS variant_label
FROM product_variants v
JOIN products p   ON p.id = v.product_id
JOIN categories c ON c.id = p.category_id
WHERE v.id = ANY(@ids::bigint[])
ORDER BY v.id
FOR UPDATE OF v;

-- CreateOrder writes the order, its items, the stock decrements and the
-- removal of the purchased cart lines in ONE statement (one round trip).
-- stock_updated < number of lines means stock ran out: the caller rolls
-- the whole transaction back.
-- name: CreateOrder :one
WITH new_order AS (
    INSERT INTO orders (user_id, idempotency_key, subtotal, shipping_cost, discount, total,
                        address_label, recipient_name, recipient_phone, province, city, district,
                        village, postal_code, address, latitude, longitude)
    VALUES (@user_id, @idempotency_key, @subtotal, 0, 0, @subtotal,
            @address_label, @recipient_name, @recipient_phone, @province, @city, @district,
            @village, @postal_code, @address, @latitude, @longitude)
    RETURNING id, order_number
), items AS (
    INSERT INTO order_items (order_id, product_id, variant_id, product_name, product_slug, sku,
                             variant_label, image_url, unit_price, quantity, subtotal)
    SELECT new_order.id,
           (@product_ids::bigint[])[x.n], x.variant_id, (@product_names::text[])[x.n],
           (@product_slugs::text[])[x.n], (@skus::text[])[x.n], (@variant_labels::text[])[x.n],
           (@image_urls::text[])[x.n], (@unit_prices::bigint[])[x.n], (@quantities::int[])[x.n],
           (@unit_prices::bigint[])[x.n] * (@quantities::int[])[x.n]
    FROM new_order, unnest(@variant_ids::bigint[]) WITH ORDINALITY AS x(variant_id, n)
    RETURNING 1
), stock AS (
    UPDATE product_variants v
    SET stock = v.stock - (@quantities::int[])[s.n]
    FROM unnest(@variant_ids::bigint[]) WITH ORDINALITY AS s(variant_id, n)
    WHERE v.id = s.variant_id AND v.stock >= (@quantities::int[])[s.n]
    RETURNING v.id
), cleared AS (
    DELETE FROM cart_items ci
    USING carts c
    WHERE ci.cart_id = c.id AND c.user_id = @user_id AND c.active
      AND ci.id = ANY(@cart_item_ids::bigint[])
    RETURNING ci.id
)
SELECT new_order.id, new_order.order_number,
       (SELECT count(*) FROM items)::int   AS items_created,
       (SELECT count(*) FROM stock)::int   AS stock_updated,
       (SELECT count(*) FROM cleared)::int AS cart_cleared
FROM new_order;

-- name: GetOrderNumberByIdempotencyKey :one
SELECT order_number FROM orders
WHERE user_id = @user_id AND idempotency_key = @idempotency_key;
