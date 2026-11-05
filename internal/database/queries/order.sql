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

-- GetOrder also reads whether a PAID payment exists (returns need it) and
-- the order's active shipment, so the detail page needs no extra round trip
-- (tracking events are read only when a shipment exists).
-- name: GetOrder :one
SELECT o.*,
       (o.paid_at IS NOT NULL AND EXISTS (
            SELECT 1 FROM payments p WHERE p.order_id = o.id AND p.status = 'PAID'))::boolean AS paid,
       s.id AS shipment_id, s.method AS shipment_method, s.provider AS shipment_provider,
       s.courier_name, s.courier_code AS shipment_courier_code, s.service_name, s.service_code AS shipment_service_code,
       s.tracking_number, s.tracking_url AS shipment_tracking_url, s.status AS shipment_status,
       s.estimated_days AS shipment_estimated_days, s.shipped_at, s.booked_at AS shipment_booked_at,
       s.delivered_at AS shipment_delivered_at, s.cancelled_at AS shipment_cancelled_at,
       s.last_synced_at AS shipment_synced_at, s.created_at AS shipment_created_at, s.note AS shipment_note,
       s.failure_reason AS shipment_failure_reason
FROM orders o
-- At most one active shipment per order (shipments_active_order_idx).
LEFT JOIN shipments s ON s.order_id = o.id AND s.status NOT IN ('CANCELLED', 'FAILED')
WHERE o.order_number = @order_number AND o.user_id = @user_id;

-- ListOrderItems returns the lines with their review (if any) and the
-- quantity held by returns that still count (not rejected or cancelled;
-- the approved quantity once the shop reviewed the return). Order detail,
-- review and return actions all read this one query.
-- name: ListOrderItems :many
SELECT oi.*, r.id AS review_id,
       coalesce((SELECT sum(coalesce(ri.approved_quantity, ri.quantity)) FROM return_items ri
                 JOIN returns rt ON rt.id = ri.return_id
                 WHERE ri.order_item_id = oi.id AND rt.status NOT IN ('REJECTED', 'CANCELLED')), 0)::int AS returned
FROM order_items oi
LEFT JOIN product_reviews r ON r.order_item_id = oi.id
WHERE oi.order_id = @order_id
ORDER BY oi.id;
