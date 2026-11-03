-- Phase 6: payments (Midtrans Snap, sandbox only), payment deadline,
-- cancellation of unpaid orders and stock restoration.
--
-- Stock is decremented when an order is created (Phase 5). An unpaid order
-- therefore holds stock until it is paid, cancelled by the buyer, or its
-- payment deadline passes; cancelling returns the stock exactly once,
-- guarded by orders.stock_restored_at.
--
-- Payment always charges orders.total. Shipping cost and discounts must be
-- written to the order before a payment attempt is created; shipping_cost is
-- still 0 in this phase, so production payments must stay disabled.

-- Payment deadline: 24 hours after the order is created. Every payment
-- attempt expires at this time too, so the buyer cannot pay after it.
ALTER TABLE orders
    ADD COLUMN payment_due_at    timestamptz,
    ADD COLUMN paid_at           timestamptz,
    ADD COLUMN cancelled_at      timestamptz,
    ADD COLUMN cancel_reason     text,
    ADD COLUMN stock_restored_at timestamptz;

UPDATE orders SET payment_due_at = created_at + interval '24 hours';

ALTER TABLE orders
    ALTER COLUMN payment_due_at SET DEFAULT (now() + interval '24 hours'),
    ALTER COLUMN payment_due_at SET NOT NULL,
    ADD CONSTRAINT orders_cancel_reason_chk CHECK (cancel_reason IN ('USER_CANCELLED', 'PAYMENT_EXPIRED')),
    ADD CONSTRAINT orders_cancelled_chk CHECK ((status = 'CANCELLED') = (cancelled_at IS NOT NULL)),
    -- Stock is only ever given back by cancelling the order.
    ADD CONSTRAINT orders_stock_restored_chk CHECK (stock_restored_at IS NULL OR status = 'CANCELLED');

-- Overdue unpaid orders (lazy expiry and reconcile-payments).
CREATE INDEX orders_payment_due_idx ON orders (payment_due_at) WHERE status = 'WAITING_PAYMENT';

-- One row per payment attempt. An order may need several attempts (a card
-- is declined, a virtual account expires), but at most one is PENDING and
-- at most one is PAID. provider_order_id is the transaction ID sent to the
-- provider (ASH-YYYYMMDD-NNNNNN-P<attempt>): unique, never reused.
-- Provider credentials are never stored here; they live in the environment.
CREATE TABLE payments (
    id                      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id                bigint      NOT NULL REFERENCES orders (id),
    attempt_no              integer     NOT NULL,
    provider                text        NOT NULL,
    provider_order_id       text        NOT NULL,
    provider_transaction_id text,
    -- Copied from orders.total when the attempt is created.
    amount                  bigint      NOT NULL,
    status                  text        NOT NULL DEFAULT 'PENDING',
    payment_type            text,
    snap_token              text,
    snap_redirect_url       text,
    -- Last raw provider status (e.g. settlement, pending, expire) and fraud
    -- status, for support and reconciliation; the domain uses status.
    provider_status         text,
    fraud_status            text,
    -- Set when the provider reported something the system will not apply
    -- automatically (amount mismatch, payment after cancellation, refund):
    -- listed by reconcile-payments for manual handling.
    review_reason           text,
    expires_at              timestamptz NOT NULL,
    paid_at                 timestamptz,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT payments_attempt_key           UNIQUE (order_id, attempt_no),
    CONSTRAINT payments_provider_order_id_key UNIQUE (provider_order_id),
    CONSTRAINT payments_attempt_chk  CHECK (attempt_no >= 1),
    CONSTRAINT payments_amount_chk   CHECK (amount > 0),
    CONSTRAINT payments_status_chk   CHECK (status IN ('PENDING', 'PAID', 'FAILED', 'DENIED', 'CANCELLED', 'EXPIRED')),
    CONSTRAINT payments_paid_at_chk  CHECK ((status = 'PAID') = (paid_at IS NOT NULL))
);

-- Final guards: one active (PENDING) attempt and one PAID attempt per order.
CREATE UNIQUE INDEX payments_one_pending_idx ON payments (order_id) WHERE status = 'PENDING';
CREATE UNIQUE INDEX payments_one_paid_idx    ON payments (order_id) WHERE status = 'PAID';
-- reconcile-payments: pending attempts to verify, attempts needing review.
CREATE INDEX payments_pending_idx ON payments (created_at) WHERE status = 'PENDING';
CREATE INDEX payments_review_idx  ON payments (updated_at) WHERE review_reason IS NOT NULL;

CREATE TRIGGER payments_set_updated_at
    BEFORE UPDATE ON payments
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Append-only audit trail of payment and unpaid-order events: provider
-- notifications and status checks (applied, duplicate, ignored, flagged),
-- cancellations and stock restoration. Holds no credentials or tokens.
CREATE TABLE payment_events (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id        bigint      NOT NULL REFERENCES orders (id),
    payment_id      bigint      REFERENCES payments (id),
    source          text        NOT NULL,
    kind            text        NOT NULL,
    from_status     text,
    to_status       text,
    provider_status text,
    detail          text        NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX payment_events_order_idx ON payment_events (order_id, id);

-- A variant held by an unpaid order cannot be deleted: cancelling that order
-- must be able to give its stock back. Deactivate it instead. (Deleting a
-- product cascades to its variants and is refused the same way.)
CREATE FUNCTION prevent_reserved_variant_delete() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM order_items oi JOIN orders o ON o.id = oi.order_id
        WHERE oi.variant_id = OLD.id AND o.status = 'WAITING_PAYMENT'
    ) THEN
        RAISE EXCEPTION 'variant % is reserved by an unpaid order', OLD.id
            USING ERRCODE = 'restrict_violation',
                  HINT = 'Deactivate the variant, or wait until the order is paid or cancelled.';
    END IF;
    RETURN OLD;
END
$$;

CREATE TRIGGER product_variants_reserved_delete
    BEFORE DELETE ON product_variants
    FOR EACH ROW EXECUTE FUNCTION prevent_reserved_variant_delete();
