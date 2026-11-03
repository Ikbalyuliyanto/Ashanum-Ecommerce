-- Payments and the unpaid-order lifecycle. Every state change runs in one
-- transaction that locks the order row first, then its payment rows, then
-- (when stock is given back) the variants in id order, the same variant
-- order checkout uses, so these transactions cannot deadlock each other.

-- name: PaymentOrderID :one
SELECT id FROM orders WHERE order_number = @order_number AND user_id = @user_id;

-- name: PaymentOrder :one
SELECT o.id, o.order_number, o.status, o.subtotal, o.shipping_cost, o.discount, o.total,
       o.payment_due_at, o.stock_restored_at, o.recipient_name, o.recipient_phone, u.email
FROM orders o
JOIN users u ON u.id = o.user_id
WHERE o.id = @id;

-- Same as PaymentOrder, row-locking the order.
-- name: LockPaymentOrder :one
SELECT o.id, o.order_number, o.status, o.subtotal, o.shipping_cost, o.discount, o.total,
       o.payment_due_at, o.stock_restored_at, o.recipient_name, o.recipient_phone, u.email
FROM orders o
JOIN users u ON u.id = o.user_id
WHERE o.id = @id
FOR UPDATE OF o;

-- name: PaymentOrderItems :many
SELECT sku, product_name, variant_label, unit_price, quantity, subtotal
FROM order_items WHERE order_id = @order_id ORDER BY id;

-- name: ListOrderPayments :many
SELECT * FROM payments WHERE order_id = @order_id ORDER BY attempt_no;

-- name: PendingPayment :one
SELECT * FROM payments WHERE order_id = @order_id AND status = 'PENDING';

-- name: LockPayment :one
SELECT * FROM payments WHERE id = @id FOR UPDATE;

-- name: PaymentRef :one
SELECT id, order_id FROM payments WHERE provider_order_id = @provider_order_id;

-- name: HasPaidPayment :one
SELECT EXISTS (SELECT 1 FROM payments WHERE order_id = @order_id AND status = 'PAID');

-- name: NextAttemptNo :one
SELECT (coalesce(max(attempt_no), 0) + 1)::int FROM payments WHERE order_id = @order_id;

-- name: CreatePayment :one
INSERT INTO payments (order_id, attempt_no, provider, provider_order_id, amount, expires_at)
VALUES (@order_id, @attempt_no, @provider, @provider_order_id, @amount, @expires_at)
RETURNING *;

-- SetPaymentCheckout stores the provider checkout (Snap token and redirect
-- URL) of a pending attempt, once.
-- name: SetPaymentCheckout :execrows
UPDATE payments SET snap_token = @snap_token, snap_redirect_url = @snap_redirect_url
WHERE id = @id AND status = 'PENDING' AND snap_token IS NULL;

-- RecordProviderStatus keeps the latest provider details of an attempt.
-- name: RecordProviderStatus :exec
UPDATE payments SET
    provider_status         = @provider_status,
    fraud_status            = sqlc.narg('fraud_status'),
    payment_type            = coalesce(sqlc.narg('payment_type'), payment_type),
    provider_transaction_id = coalesce(sqlc.narg('provider_transaction_id'), provider_transaction_id)
WHERE id = @id;

-- SetPaymentStatus moves a PENDING attempt to another status; terminal
-- attempts never change (the WHERE clause is the final guard).
-- name: SetPaymentStatus :execrows
UPDATE payments SET status = @status, paid_at = sqlc.narg('paid_at')
WHERE id = @id AND status = 'PENDING';

-- name: FlagPayment :exec
UPDATE payments SET review_reason = @review_reason WHERE id = @id;

-- name: ClosePendingPayments :execrows
UPDATE payments SET status = @status WHERE order_id = @order_id AND status = 'PENDING';

-- name: MarkOrderPaid :execrows
UPDATE orders SET status = 'WAITING_CONFIRMATION', paid_at = @paid_at
WHERE id = @id AND status = 'WAITING_PAYMENT' AND stock_restored_at IS NULL;

-- name: LockOrderVariants :many
SELECT v.id FROM product_variants v
WHERE v.id IN (SELECT oi.variant_id FROM order_items oi WHERE oi.order_id = @order_id)
ORDER BY v.id
FOR UPDATE OF v;

-- RestoreOrderStock gives the order's quantities back to their variants
-- (call LockOrderVariants first). Lines whose variant was deleted have no
-- stock to return; they are counted as missing.
-- name: RestoreOrderStock :one
WITH items AS (
    SELECT oi.variant_id, sum(oi.quantity)::int AS quantity
    FROM order_items oi
    WHERE oi.order_id = @order_id AND oi.variant_id IS NOT NULL
    GROUP BY oi.variant_id
), restored AS (
    UPDATE product_variants v SET stock = v.stock + i.quantity
    FROM items i
    WHERE v.id = i.variant_id
    RETURNING v.id
)
SELECT (SELECT count(*) FROM items)::int    AS variants,
       (SELECT count(*) FROM restored)::int AS restored,
       (SELECT count(*) FROM order_items oi
        WHERE oi.order_id = @order_id AND oi.variant_id IS NULL)::int AS missing;

-- CancelUnpaidOrder cancels an unpaid order whose stock is being restored in
-- the same transaction; it changes nothing (0 rows) when already done.
-- name: CancelUnpaidOrder :execrows
UPDATE orders SET status = 'CANCELLED', cancel_reason = @cancel_reason,
                  cancelled_at = now(), stock_restored_at = now()
WHERE id = @id AND status = 'WAITING_PAYMENT' AND stock_restored_at IS NULL;

-- name: InsertPaymentEvent :exec
INSERT INTO payment_events (order_id, payment_id, source, kind, from_status, to_status, provider_status, detail)
VALUES (@order_id, sqlc.narg('payment_id'), @source, @kind, sqlc.narg('from_status'),
        sqlc.narg('to_status'), sqlc.narg('provider_status'), @detail);

-- Reconciliation.

-- name: StalePendingPayments :many
SELECT id, order_id, provider_order_id FROM payments
WHERE status = 'PENDING' AND snap_token IS NOT NULL AND created_at < @created_before
ORDER BY created_at
LIMIT @row_limit;

-- name: OverdueOrders :many
SELECT id FROM orders
WHERE status = 'WAITING_PAYMENT' AND payment_due_at < @due_before
ORDER BY payment_due_at
LIMIT @row_limit;

-- name: PaymentsNeedingReview :many
SELECT o.order_number, p.provider_order_id, p.status, p.provider_status, p.review_reason, p.updated_at
FROM payments p
JOIN orders o ON o.id = p.order_id
WHERE p.review_reason IS NOT NULL
ORDER BY p.updated_at DESC
LIMIT @row_limit;
