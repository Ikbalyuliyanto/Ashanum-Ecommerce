-- Returns (refund / exchange requests) and refunds. Buyer queries are
-- scoped by user_id; admin queries (below) are reached only through the
-- admin role. A return is created in one transaction that locks the order
-- row, then the selected order items in id order.
--
-- A return that still counts (not rejected or cancelled) holds
-- coalesce(approved_quantity, quantity) units of each of its order items:
-- the requested quantity until the shop reviews it, then what was approved.

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

-- ReturnableItems lists an order's items with the quantity already held by
-- returns that still count.
-- name: ReturnableItems :many
SELECT oi.id, oi.product_name, oi.variant_label, oi.image_url, oi.unit_price, oi.quantity,
       coalesce((SELECT sum(coalesce(ri.approved_quantity, ri.quantity)) FROM return_items ri
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
SELECT ri.id, ri.order_item_id, ri.quantity, ri.unit_price, ri.approved_quantity, ri.condition,
       ri.inspected_at, ri.restocked_quantity, ri.restocked_at,
       ri.replacement_variant_id, ri.replacement_sku, ri.replacement_label, ri.replacement_quantity,
       oi.product_id, oi.variant_id, oi.product_name, oi.sku, oi.variant_label, oi.image_url
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

-- Status changes (admin).

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

-- LockReturnLines row-locks a return's lines (id order) with their order
-- item, for approval, inspection, restock and exchange.
-- name: LockReturnLines :many
SELECT ri.id, ri.order_item_id, ri.quantity, ri.unit_price, ri.approved_quantity, ri.condition,
       ri.restocked_at, ri.replacement_quantity,
       oi.product_id, oi.variant_id, oi.product_name, oi.sku, oi.variant_label
FROM return_items ri
JOIN order_items oi ON oi.id = ri.order_item_id
WHERE ri.return_id = @return_id
ORDER BY ri.id
FOR UPDATE OF ri;

-- name: SetApprovedQuantities :exec
UPDATE return_items ri SET approved_quantity = (@quantities::int[])[x.n]
FROM unnest(@ids::bigint[]) WITH ORDINALITY AS x(id, n)
WHERE ri.id = x.id AND ri.return_id = @return_id;

-- SetReturnAmount stores the refund owed for the approved quantities.
-- name: SetReturnAmount :exec
UPDATE returns SET refund_amount = @refund_amount WHERE id = @id;

-- name: InspectReturnItems :exec
UPDATE return_items ri SET condition = (@conditions::text[])[x.n], inspected_at = now()
FROM unnest(@ids::bigint[]) WITH ORDINALITY AS x(id, n)
WHERE ri.id = x.id AND ri.return_id = @return_id;

-- MarkRestocked records the restock of one line, once: 0 rows when it was
-- already restocked (a repeated request changes nothing).
-- name: MarkRestocked :execrows
UPDATE return_items SET restocked_quantity = @quantity, restocked_at = now()
WHERE id = @id AND restocked_at IS NULL AND condition = 'RESTOCKABLE';

-- name: SetReplacement :execrows
UPDATE return_items SET replacement_variant_id = @variant_id, replacement_sku = @sku,
                        replacement_label = @label, replacement_quantity = @quantity
WHERE id = @id AND replacement_quantity IS NULL;

-- ReplacementVariants are the active variants of the given products, the
-- choices for an exchange.
-- name: ReplacementVariants :many
SELECT v.id, v.product_id, v.sku, v.stock, v.price,
       coalesce((SELECT string_agg(av.value, ' / ' ORDER BY a.display_order, a.id)
                 FROM product_variant_values vv
                 JOIN product_attributes a        ON a.id = vv.attribute_id
                 JOIN product_attribute_values av ON av.id = vv.value_id
                 WHERE vv.variant_id = v.id), '')::text AS variant_label
FROM product_variants v
JOIN products p ON p.id = v.product_id
WHERE v.product_id = ANY(@product_ids::bigint[]) AND v.active
ORDER BY v.product_id, v.id;

-- name: ReturnIDByNumber :one
SELECT id FROM returns WHERE return_number = @return_number;

-- AdminReturns lists every return newest first; empty statuses means all,
-- q searches the return or order number.
-- name: AdminReturns :many
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
WHERE (cardinality(@statuses::text[]) = 0 OR r.status = ANY(@statuses::text[]))
  AND (@q::text = '' OR r.return_number ILIKE '%' || @q::text || '%' OR o.order_number ILIKE '%' || @q::text || '%')
ORDER BY r.created_at DESC, r.id DESC
LIMIT @row_limit OFFSET @row_offset;

-- name: AdminReturn :one
SELECT r.*, o.order_number, o.total AS order_total, u.email,
       coalesce(up.first_name, '')::text AS first_name, coalesce(up.last_name, '')::text AS last_name
FROM returns r
JOIN orders o ON o.id = r.order_id
JOIN users u ON u.id = r.user_id
LEFT JOIN user_profiles up ON up.user_id = r.user_id
WHERE r.return_number = @return_number;

-- Refunds. No payment provider is called: the shop carries the refund out
-- (e.g. a bank transfer) and records the outcome here.

-- LockRefundPayment serializes refunds of one payment, so concurrent
-- refunds can never add up to more than was paid.
-- name: LockRefundPayment :one
SELECT id, amount FROM payments WHERE id = @id FOR UPDATE;

-- RefundedForPayment is what is already refunded or being refunded from a
-- payment (failed and cancelled refunds do not count).
-- name: RefundedForPayment :one
SELECT coalesce(sum(amount), 0)::bigint FROM refunds
WHERE payment_id = @payment_id AND status NOT IN ('FAILED', 'CANCELLED');

-- name: CreateRefund :one
INSERT INTO refunds (return_id, order_id, payment_id, amount, source)
VALUES (@return_id, @order_id, @payment_id, @amount, 'RETURN')
RETURNING *;

-- name: OpenRefund :one
SELECT * FROM refunds
WHERE return_id = @return_id AND status IN ('PENDING', 'APPROVED', 'PROCESSING')
FOR UPDATE;

-- name: HasActiveRefund :one
SELECT EXISTS (SELECT 1 FROM refunds WHERE return_id = @return_id AND status NOT IN ('FAILED', 'CANCELLED'))::boolean;

-- RecordRefundResult stores the outcome of a refund that was carried out
-- outside this system (no provider call is made here).
-- name: RecordRefundResult :execrows
UPDATE refunds SET status = 'SUCCEEDED', processed_at = now(),
    provider = @provider, provider_refund_id = sqlc.narg('provider_refund_id'), note = @note
WHERE id = @id AND status IN ('PENDING', 'APPROVED', 'PROCESSING');

-- name: MarkRefundProcessing :execrows
UPDATE refunds SET status = 'PROCESSING', note = CASE WHEN @note::text <> '' THEN @note::text ELSE note END
WHERE id = @id AND status IN ('PENDING', 'APPROVED');

-- name: FailRefund :execrows
UPDATE refunds SET status = 'FAILED', failure_reason = @reason, processed_at = now()
WHERE id = @id AND status IN ('PENDING', 'APPROVED', 'PROCESSING');

-- LockRefund row-locks a refund with its return's status (if any).
-- name: LockRefund :one
SELECT f.id, f.return_id, f.order_id, f.source, f.status, f.amount, r.status AS return_status
FROM refunds f
LEFT JOIN returns r ON r.id = f.return_id
WHERE f.id = @id
FOR UPDATE OF f;

-- name: RefundReturnID :one
SELECT return_id FROM refunds WHERE id = @id;

-- AdminRefunds lists refunds newest first; empty statuses means all, q
-- searches the order or return number.
-- name: AdminRefunds :many
SELECT f.id, f.source, f.status, f.amount, f.requested_at, f.processed_at, f.provider, f.provider_refund_id,
       o.order_number, r.return_number, p.provider_order_id
FROM refunds f
JOIN orders o ON o.id = f.order_id
LEFT JOIN returns r ON r.id = f.return_id
LEFT JOIN payments p ON p.id = f.payment_id
WHERE (cardinality(@statuses::text[]) = 0 OR f.status = ANY(@statuses::text[]))
  AND (@q::text = '' OR o.order_number ILIKE '%' || @q::text || '%' OR r.return_number ILIKE '%' || @q::text || '%')
ORDER BY f.requested_at DESC, f.id DESC
LIMIT @row_limit OFFSET @row_offset;

-- name: AdminRefund :one
SELECT f.*, o.order_number, r.return_number, r.status AS return_status,
       p.provider AS payment_provider, p.provider_order_id, p.provider_transaction_id, p.payment_type, p.amount AS paid_amount
FROM refunds f
JOIN orders o ON o.id = f.order_id
LEFT JOIN returns r ON r.id = f.return_id
LEFT JOIN payments p ON p.id = f.payment_id
WHERE f.id = @id;
