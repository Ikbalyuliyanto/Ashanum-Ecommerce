-- Returns (refund / exchange requests) and refunds. Buyer queries are
-- scoped by user_id. A return is created in one transaction that locks the
-- order row, then the selected order items in id order.

-- name: ReturnOrder :one
SELECT o.id, o.order_number, o.status, o.completed_at,
       (o.paid_at IS NOT NULL AND EXISTS (
            SELECT 1 FROM payments p WHERE p.order_id = o.id AND p.status = 'PAID'))::boolean AS paid
FROM orders o
WHERE o.order_number = @order_number AND o.user_id = @user_id;

-- name: LockReturnOrder :one
SELECT o.id, o.order_number, o.status, o.completed_at,
       (o.paid_at IS NOT NULL AND EXISTS (
            SELECT 1 FROM payments p WHERE p.order_id = o.id AND p.status = 'PAID'))::boolean AS paid
FROM orders o
WHERE o.order_number = @order_number AND o.user_id = @user_id
FOR UPDATE OF o;

-- ReturnableItems lists an order's items with the quantity already in
-- returns that still count (not rejected or cancelled).
-- name: ReturnableItems :many
SELECT oi.id, oi.product_name, oi.variant_label, oi.image_url, oi.unit_price, oi.quantity,
       coalesce((SELECT sum(ri.quantity) FROM return_items ri
                 JOIN returns r ON r.id = ri.return_id
                 WHERE ri.order_item_id = oi.id AND r.status NOT IN ('REJECTED', 'CANCELLED')), 0)::int AS returned
FROM order_items oi
WHERE oi.order_id = @order_id
ORDER BY oi.id;

-- name: LockReturnItems :many
SELECT id FROM order_items
WHERE order_id = @order_id AND id = ANY(@ids::bigint[])
ORDER BY id
FOR UPDATE;

-- name: CreateReturn :one
INSERT INTO returns (order_id, user_id, type, reason, description, refund_amount, idempotency_key)
VALUES (@order_id, @user_id, @type, @reason, @description, @refund_amount, sqlc.narg('idempotency_key'))
RETURNING id, return_number;

-- name: AddReturnItems :exec
INSERT INTO return_items (return_id, order_item_id, quantity, unit_price)
SELECT @return_id, x.order_item_id, (@quantities::int[])[x.n], (@unit_prices::bigint[])[x.n]
FROM unnest(@order_item_ids::bigint[]) WITH ORDINALITY AS x(order_item_id, n);

-- name: AddReturnEvidence :exec
INSERT INTO return_evidence (return_id, url, media_type, position)
SELECT @return_id, x.url, (@media_types::text[])[x.n], x.n
FROM unnest(@urls::text[]) WITH ORDINALITY AS x(url, n);

-- name: AddReturnEvent :exec
INSERT INTO return_events (return_id, from_status, to_status, actor, note)
VALUES (@return_id, sqlc.narg('from_status'), @to_status, @actor, @note);

-- name: ReturnNumberByKey :one
SELECT return_number FROM returns WHERE user_id = @user_id AND idempotency_key = @idempotency_key;

-- name: ListReturns :many
SELECT r.id, r.return_number, o.order_number, r.type, r.status, r.refund_amount, r.created_at,
       (SELECT coalesce(sum(ri.quantity), 0) FROM return_items ri WHERE ri.return_id = r.id)::int AS item_count,
       fi.product_name, fi.image_url
FROM returns r
JOIN orders o ON o.id = r.order_id
JOIN LATERAL (
    SELECT oi.product_name, oi.image_url
    FROM return_items ri JOIN order_items oi ON oi.id = ri.order_item_id
    WHERE ri.return_id = r.id ORDER BY ri.id LIMIT 1
) fi ON true
WHERE r.user_id = @user_id
ORDER BY r.created_at DESC, r.id DESC
LIMIT @row_limit OFFSET @row_offset;

-- name: OrderReturns :many
SELECT r.return_number, r.type, r.status, r.created_at
FROM returns r
WHERE r.order_id = @order_id AND r.user_id = @user_id
ORDER BY r.created_at DESC, r.id DESC;

-- name: GetReturn :one
SELECT r.*, o.order_number
FROM returns r
JOIN orders o ON o.id = r.order_id
WHERE r.return_number = @return_number AND r.user_id = @user_id;

-- name: ReturnItemsOf :many
SELECT ri.id, ri.order_item_id, ri.quantity, ri.unit_price,
       oi.product_name, oi.variant_label, oi.image_url
FROM return_items ri
JOIN order_items oi ON oi.id = ri.order_item_id
WHERE ri.return_id = @return_id
ORDER BY ri.id;

-- name: ReturnEvidenceOf :many
SELECT url, media_type FROM return_evidence WHERE return_id = @return_id ORDER BY position;

-- name: ReturnEventsOf :many
SELECT from_status, to_status, actor, note, created_at FROM return_events WHERE return_id = @return_id ORDER BY id;

-- name: ReturnRefunds :many
SELECT * FROM refunds WHERE return_id = @return_id ORDER BY id;

-- Status changes (admin-ready service; no route in this phase).

-- LockReturn row-locks a return and reads the order's PAID payment (the
-- one a refund pays back).
-- name: LockReturn :one
SELECT r.id, r.return_number, r.order_id, r.user_id, r.type, r.status, r.refund_amount,
       p.id AS payment_id, p.amount AS paid_amount
FROM returns r
LEFT JOIN payments p ON p.order_id = r.order_id AND p.status = 'PAID'
WHERE r.id = @id
FOR UPDATE OF r;

-- name: LockReturnByNumber :one
SELECT r.id, r.return_number, r.order_id, r.user_id, r.type, r.status, r.refund_amount,
       p.id AS payment_id, p.amount AS paid_amount
FROM returns r
LEFT JOIN payments p ON p.order_id = r.order_id AND p.status = 'PAID'
WHERE r.return_number = @return_number AND r.user_id = @user_id
FOR UPDATE OF r;

-- SetReturnStatus moves a return from one status to the next; 0 rows when
-- the return is no longer in the expected status.
-- name: SetReturnStatus :execrows
UPDATE returns SET
    status          = @to_status,
    approved_at     = CASE WHEN @to_status::text = 'APPROVED'  THEN now() ELSE approved_at END,
    rejected_at     = CASE WHEN @to_status::text = 'REJECTED'  THEN now() ELSE rejected_at END,
    completed_at    = CASE WHEN @to_status::text = 'COMPLETED' THEN now() ELSE completed_at END,
    resolution_note = CASE WHEN @note::text <> '' THEN @note::text ELSE resolution_note END
WHERE id = @id AND status = @from_status;

-- RefundedForPayment is what is already refunded or being refunded from a
-- payment (failed and cancelled refunds do not count).
-- name: RefundedForPayment :one
SELECT coalesce(sum(amount), 0)::bigint FROM refunds
WHERE payment_id = @payment_id AND status NOT IN ('FAILED', 'CANCELLED');

-- name: CreateRefund :one
INSERT INTO refunds (return_id, payment_id, amount)
VALUES (@return_id, @payment_id, @amount)
RETURNING *;

-- name: OpenRefund :one
SELECT * FROM refunds
WHERE return_id = @return_id AND status IN ('PENDING', 'APPROVED', 'PROCESSING')
FOR UPDATE;

-- RecordRefundResult stores the outcome of a refund that was carried out
-- outside this system (no provider call is made here).
-- name: RecordRefundResult :execrows
UPDATE refunds SET status = 'SUCCEEDED', processed_at = now(),
    provider = @provider, provider_refund_id = sqlc.narg('provider_refund_id')
WHERE id = @id AND status IN ('PENDING', 'APPROVED', 'PROCESSING');
