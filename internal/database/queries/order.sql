-- Every order query is scoped by user_id: a user only ever sees own orders.

-- ListOrders is the order history: one row per order with its first item
-- and line count, newest first. An empty statuses array means all.
-- name: ListOrders :many
SELECT o.id, o.order_number, o.status, o.total, o.created_at, o.payment_due_at,
       (SELECT count(*) FROM order_items i WHERE i.order_id = o.id)::int AS item_count,
       fi.product_name, fi.variant_label, fi.image_url, fi.quantity, fi.unit_price
FROM orders o
JOIN LATERAL (
    SELECT i.product_name, i.variant_label, i.image_url, i.quantity, i.unit_price
    FROM order_items i WHERE i.order_id = o.id ORDER BY i.id LIMIT 1
) fi ON true
WHERE o.user_id = @user_id
  AND (cardinality(@statuses::text[]) = 0 OR o.status = ANY(@statuses::text[]))
ORDER BY o.created_at DESC, o.id DESC
LIMIT @row_limit OFFSET @row_offset;

-- name: GetOrder :one
SELECT * FROM orders WHERE order_number = @order_number AND user_id = @user_id;

-- name: ListOrderItems :many
SELECT * FROM order_items WHERE order_id = @order_id ORDER BY id;
