-- Dashboard and basic reports: aggregate SQL only, one statement per block.
--
-- Revenue ("penjualan") = total of orders paid in the period (by paid_at)
-- that are not cancelled. WAITING_PAYMENT orders have no paid_at and never
-- count; a paid order cancelled later is shown apart, with its refund.

-- name: Dashboard :one
WITH o AS (
    SELECT count(*) FILTER (WHERE od.created_at >= @day_start)::int         AS new_today,
           count(*) FILTER (WHERE od.status = 'WAITING_PAYMENT')::int       AS waiting_payment,
           count(*) FILTER (WHERE od.status = 'WAITING_CONFIRMATION')::int  AS waiting_confirmation,
           count(*) FILTER (WHERE od.status = 'PROCESSING')::int            AS processing,
           count(*) FILTER (WHERE od.status = 'SHIPPED')::int               AS shipped
    FROM orders od
    WHERE od.status IN ('WAITING_PAYMENT', 'WAITING_CONFIRMATION', 'PROCESSING', 'SHIPPED') OR od.created_at >= @day_start
), s AS (
    SELECT count(*) FILTER (WHERE op.paid_at >= @day_start)::int                        AS paid_today,
           coalesce(sum(op.total) FILTER (WHERE op.paid_at >= @day_start), 0)::bigint      AS sales_today,
           count(*)::int                                                             AS paid_week,
           coalesce(sum(op.total), 0)::bigint                                           AS sales_week
    FROM orders op
    WHERE op.paid_at >= @week_start AND op.status <> 'CANCELLED'
), r AS (
    SELECT count(*) FILTER (WHERE rt.status = 'REQUESTED')::int                                   AS returns_requested,
           count(*) FILTER (WHERE rt.status IN ('APPROVED', 'WAITING_RETURN', 'ITEM_RECEIVED'))::int AS returns_in_progress
    FROM returns rt
    WHERE rt.status IN ('REQUESTED', 'APPROVED', 'WAITING_RETURN', 'ITEM_RECEIVED')
), f AS (
    SELECT count(*)::int AS refunds_open, coalesce(sum(rf.amount), 0)::bigint AS refunds_open_amount
    FROM refunds rf
    WHERE rf.status IN ('PENDING', 'APPROVED', 'PROCESSING')
), v AS (
    SELECT count(*) FILTER (WHERE v.stock = 0)::int                          AS out_of_stock,
           count(*) FILTER (WHERE v.stock BETWEEN 1 AND @threshold::int)::int AS low_stock
    FROM product_variants v
    JOIN products p ON p.id = v.product_id
    WHERE v.active AND p.active
)
SELECT o.new_today, o.waiting_payment, o.waiting_confirmation, o.processing, o.shipped,
       s.paid_today, s.sales_today, s.paid_week, s.sales_week,
       r.returns_requested, r.returns_in_progress,
       f.refunds_open, f.refunds_open_amount,
       v.out_of_stock, v.low_stock
FROM o, s, r, f, v;

-- SalesSummary covers orders paid in [from, to): kept (not cancelled) ones
-- are revenue; cancelled-after-payment ones are reported apart.
-- name: SalesSummary :one
SELECT count(*) FILTER (WHERE status <> 'CANCELLED')::int                          AS paid_orders,
       coalesce(sum(total) FILTER (WHERE status <> 'CANCELLED'), 0)::bigint        AS revenue,
       coalesce(sum(subtotal) FILTER (WHERE status <> 'CANCELLED'), 0)::bigint     AS items_value,
       coalesce(sum(quantity_total) FILTER (WHERE status <> 'CANCELLED'), 0)::int  AS units,
       count(*) FILTER (WHERE status = 'CANCELLED')::int                           AS cancelled_orders,
       coalesce(sum(total) FILTER (WHERE status = 'CANCELLED'), 0)::bigint         AS cancelled_amount
FROM (
    SELECT o.status, o.total, o.subtotal,
           (SELECT sum(i.quantity) FROM order_items i WHERE i.order_id = o.id) AS quantity_total
    FROM orders o
    WHERE o.paid_at >= @from_time AND o.paid_at < @to_time
) paid;

-- OrderStatusBreakdown counts orders created in [from, to) by status.
-- name: OrderStatusBreakdown :many
SELECT status, count(*)::int AS orders, coalesce(sum(total), 0)::bigint AS amount
FROM orders
WHERE created_at >= @from_time AND created_at < @to_time
GROUP BY status;

-- TopProducts ranks products by units sold in orders paid in [from, to)
-- and not cancelled. A deleted product is grouped by its snapshot name.
-- name: TopProducts :many
SELECT oi.product_id,
       (array_agg(oi.product_name ORDER BY o.paid_at DESC))[1]::text AS product_name,
       sum(oi.quantity)::int     AS units,
       sum(oi.subtotal)::bigint  AS revenue,
       count(DISTINCT o.id)::int AS orders
FROM order_items oi
JOIN orders o ON o.id = oi.order_id
WHERE o.paid_at >= @from_time AND o.paid_at < @to_time AND o.status <> 'CANCELLED'
GROUP BY oi.product_id, CASE WHEN oi.product_id IS NULL THEN oi.product_name END
ORDER BY units DESC, revenue DESC
LIMIT @row_limit;

-- ReturnSummary counts returns requested in [from, to); refunds are counted
-- by when they were recorded as paid out (processed_at), by source.
-- name: ReturnSummary :one
SELECT
    (SELECT count(*) FROM returns r WHERE r.created_at >= @from_time AND r.created_at < @to_time)::int AS returns,
    (SELECT count(*) FROM returns r WHERE r.created_at >= @from_time AND r.created_at < @to_time
                                      AND r.type = 'REFUND')::int AS refund_returns,
    (SELECT count(*) FROM returns r WHERE r.created_at >= @from_time AND r.created_at < @to_time
                                      AND r.type = 'EXCHANGE')::int AS exchange_returns,
    (SELECT coalesce(sum(f.amount), 0) FROM refunds f WHERE f.status = 'SUCCEEDED' AND f.source = 'RETURN'
        AND f.processed_at >= @from_time AND f.processed_at < @to_time)::bigint AS refunded_returns,
    (SELECT coalesce(sum(f.amount), 0) FROM refunds f WHERE f.status = 'SUCCEEDED' AND f.source = 'ORDER_CANCELLATION'
        AND f.processed_at >= @from_time AND f.processed_at < @to_time)::bigint AS refunded_cancellations,
    (SELECT coalesce(sum(f.amount), 0) FROM refunds f
        WHERE f.status IN ('PENDING', 'APPROVED', 'PROCESSING'))::bigint AS refunds_open;
