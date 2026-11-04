-- Admin order management: every order, not scoped by user. Only routes
-- behind the admin role reach these queries.

-- AdminOrders lists orders newest first; an empty statuses array means all
-- and an empty q means no search (order number, case-insensitive). The
-- caller fetches one row more than a page to know whether a next page exists.
-- name: AdminOrders :many
SELECT o.id, o.order_number, o.status, o.total, o.created_at, o.paid_at, o.recipient_name, o.city,
       (SELECT coalesce(sum(i.quantity), 0) FROM order_items i WHERE i.order_id = o.id)::int AS quantity,
       fi.product_name, fi.image_url
FROM orders o
JOIN LATERAL (
    SELECT i.product_name, i.image_url FROM order_items i WHERE i.order_id = o.id ORDER BY i.id LIMIT 1
) fi ON true
WHERE (cardinality(@statuses::text[]) = 0 OR o.status = ANY(@statuses::text[]))
  AND (@q::text = '' OR o.order_number ILIKE '%' || @q::text || '%')
ORDER BY o.created_at DESC, o.id DESC
LIMIT @row_limit OFFSET @row_offset;

-- name: AdminOrder :one
SELECT o.*,
       u.email,
       coalesce(up.first_name, '')::text AS first_name,
       coalesce(up.last_name, '')::text  AS last_name,
       s.courier_name, s.service_name, s.tracking_number, s.shipped_at, s.note AS shipment_note
FROM orders o
JOIN users u ON u.id = o.user_id
LEFT JOIN user_profiles up ON up.user_id = o.user_id
LEFT JOIN shipments s ON s.order_id = o.id
WHERE o.order_number = @order_number;

-- name: AdminOrderReturns :many
SELECT r.id, r.return_number, r.type, r.status, r.refund_amount, r.created_at
FROM returns r
WHERE r.order_id = @order_id
ORDER BY r.created_at DESC, r.id DESC;

-- name: OrderRefunds :many
SELECT f.id, f.source, f.status, f.amount, f.requested_at, f.processed_at, r.return_number
FROM refunds f
LEFT JOIN returns r ON r.id = f.return_id
WHERE f.order_id = @order_id
ORDER BY f.id;

-- LockFulfillmentOrder row-locks an order for a fulfillment change. Lock
-- order across modules: orders → payments → variants (by id).
-- name: LockFulfillmentOrder :one
SELECT o.id, o.order_number, o.status, o.total, o.paid_at, o.stock_restored_at
FROM orders o
WHERE o.order_number = @order_number
FOR UPDATE;

-- name: LockPaidPayment :one
SELECT id, amount FROM payments WHERE order_id = @order_id AND status = 'PAID' FOR UPDATE;

-- WAITING_CONFIRMATION → PROCESSING. paid_at comes only from a payment the
-- provider confirmed (Phase 6); 0 rows when the order moved meanwhile.
-- name: ConfirmOrder :execrows
UPDATE orders SET status = 'PROCESSING', confirmed_at = now()
WHERE id = @id AND status = 'WAITING_CONFIRMATION' AND paid_at IS NOT NULL;

-- name: ShipOrder :execrows
UPDATE orders SET status = 'SHIPPED' WHERE id = @id AND status = 'PROCESSING';

-- name: CreateShipment :exec
INSERT INTO shipments (order_id, courier_name, service_name, tracking_number, note, shipped_at, created_by)
VALUES (@order_id, @courier_name, @service_name, @tracking_number, @note, @shipped_at, @created_by);

-- SHIPPED → COMPLETED. completed_at starts the return window and makes the
-- order's items reviewable.
-- name: CompleteOrder :execrows
UPDATE orders SET status = 'COMPLETED', completed_at = now() WHERE id = @id AND status = 'SHIPPED';

-- CancelPaidOrder cancels a paid order that was not shipped yet; its stock
-- is restored in the same transaction, once (stock_restored_at).
-- name: CancelPaidOrder :execrows
UPDATE orders SET status = 'CANCELLED', cancel_reason = 'ADMIN_CANCELLED',
                  cancelled_at = now(), stock_restored_at = now()
WHERE id = @id AND status IN ('WAITING_CONFIRMATION', 'PROCESSING') AND stock_restored_at IS NULL;

-- name: CreateCancellationRefund :one
INSERT INTO refunds (order_id, payment_id, amount, source)
VALUES (@order_id, @payment_id, @amount, 'ORDER_CANCELLATION')
RETURNING id;

-- name: OrderStatusByNumber :one
SELECT id, status FROM orders WHERE order_number = @order_number;
