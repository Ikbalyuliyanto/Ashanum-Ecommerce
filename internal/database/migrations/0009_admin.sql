-- Phase 8: admin — roles, order fulfillment (manual shipment), inventory
-- audit trail, return approval/inspection/restock/exchange, refunds for
-- cancelled paid orders, review moderation and the admin audit log.
--
-- Still not here, by design: shipping aggregators (shipments.method is
-- MANUAL only), payment provider refunds (refunds are recorded, never sent
-- to a provider), production payments.

-- ---------------------------------------------------------------- roles

-- CUSTOMER replaces USER as the name of the buyer role. The role is never
-- taken from a form: registration always gets the default, and ADMIN is
-- granted only by the create-admin command (or directly in the database).
ALTER TABLE users DROP CONSTRAINT users_role_check;
UPDATE users SET role = 'CUSTOMER' WHERE role = 'USER';
ALTER TABLE users
    ALTER COLUMN role SET DEFAULT 'CUSTOMER',
    ADD CONSTRAINT users_role_check CHECK (role IN ('CUSTOMER', 'ADMIN'));

-- ---------------------------------------------------------------- orders

-- Admin may cancel an order; a paid one gets a refund record (below).
-- confirmed_at: when the shop accepted the paid order (→ PROCESSING).
ALTER TABLE orders
    DROP CONSTRAINT orders_cancel_reason_chk,
    ADD CONSTRAINT orders_cancel_reason_chk
        CHECK (cancel_reason IN ('USER_CANCELLED', 'PAYMENT_EXPIRED', 'ADMIN_CANCELLED')),
    ADD COLUMN confirmed_at timestamptz,
    -- Only a payment confirmed by the provider moves an order past
    -- WAITING_PAYMENT; nothing may process, ship or complete an unpaid order.
    ADD CONSTRAINT orders_paid_status_chk CHECK (
        status NOT IN ('WAITING_CONFIRMATION', 'PROCESSING', 'SHIPPED', 'COMPLETED') OR paid_at IS NOT NULL);

-- Admin order list (newest first, optionally by status) and sales reports
-- (paid orders by payment time).
CREATE INDEX orders_created_idx        ON orders (created_at DESC, id DESC);
CREATE INDEX orders_status_created_idx ON orders (status, created_at DESC, id DESC);
CREATE INDEX orders_paid_at_idx        ON orders (paid_at) WHERE paid_at IS NOT NULL;

-- Shipment data entered by the shop when an order is shipped. Manual only:
-- no courier API is called and nothing here is live tracking. The shipping
-- phase can add methods (aggregator), cost and status history to this
-- table without changing orders.
CREATE TABLE shipments (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id        bigint      NOT NULL REFERENCES orders (id),
    method          text        NOT NULL DEFAULT 'MANUAL',
    courier_name    text        NOT NULL,
    service_name    text        NOT NULL DEFAULT '',
    tracking_number text        NOT NULL DEFAULT '',
    note            text        NOT NULL DEFAULT '',
    shipped_at      timestamptz NOT NULL,
    created_by      bigint      REFERENCES users (id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT shipments_order_key    UNIQUE (order_id),
    CONSTRAINT shipments_method_chk   CHECK (method IN ('MANUAL')),
    CONSTRAINT shipments_courier_chk  CHECK (char_length(courier_name) BETWEEN 1 AND 60),
    CONSTRAINT shipments_service_chk  CHECK (char_length(service_name) <= 60),
    CONSTRAINT shipments_tracking_chk CHECK (char_length(tracking_number) <= 60),
    CONSTRAINT shipments_note_chk     CHECK (char_length(note) <= 500)
);

CREATE TRIGGER shipments_set_updated_at
    BEFORE UPDATE ON shipments
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------- inventory

-- Audit trail of stock changes made outside checkout. product_variants.stock
-- stays the only source of truth; each row records one change made in the
-- same transaction as the stock update. Sales are traced by order_items.
-- The SKU is copied so the trail survives a deleted variant.
CREATE TABLE inventory_adjustments (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    variant_id     bigint      REFERENCES product_variants (id) ON DELETE SET NULL,
    sku            text        NOT NULL,
    type           text        NOT NULL,
    quantity_delta integer     NOT NULL,
    stock_before   integer     NOT NULL,
    stock_after    integer     NOT NULL,
    reason         text        NOT NULL DEFAULT '',
    reference_type text,
    reference_id   bigint,
    admin_user_id  bigint      REFERENCES users (id),
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT inventory_adjustments_type_chk CHECK (type IN (
        'INITIAL_STOCK', 'MANUAL_ADJUSTMENT', 'RETURN_RESTOCK', 'EXCHANGE_OUT', 'ORDER_RESTORE')),
    CONSTRAINT inventory_adjustments_delta_chk CHECK (quantity_delta <> 0),
    CONSTRAINT inventory_adjustments_stock_chk CHECK (
        stock_before >= 0 AND stock_after >= 0 AND stock_after = stock_before + quantity_delta),
    CONSTRAINT inventory_adjustments_reason_chk CHECK (char_length(reason) <= 500),
    CONSTRAINT inventory_adjustments_ref_chk CHECK (
        (reference_type IS NULL) = (reference_id IS NULL) AND reference_type IN ('ORDER', 'RETURN'))
);

CREATE INDEX inventory_adjustments_variant_idx ON inventory_adjustments (variant_id, id DESC);

-- ---------------------------------------------------------------- returns

-- approved_quantity: what the shop accepted (0..quantity), NULL until the
-- review. A return holds coalesce(approved_quantity, quantity) units of its
-- order item; units not approved may be requested again.
-- condition/inspected_at: inspection of the received goods. Receiving
-- never changes stock; only an explicit restock of RESTOCKABLE lines does,
-- once (restocked_at). Damaged goods are never put back on sale.
-- replacement_*: the variant sent for an exchange; its stock is taken when
-- the shop confirms the replacement is being sent.
ALTER TABLE return_items
    ADD COLUMN approved_quantity      integer,
    ADD COLUMN condition              text,
    ADD COLUMN inspected_at           timestamptz,
    ADD COLUMN restocked_quantity     integer,
    ADD COLUMN restocked_at           timestamptz,
    ADD COLUMN replacement_variant_id bigint REFERENCES product_variants (id) ON DELETE SET NULL,
    ADD COLUMN replacement_sku        text,
    ADD COLUMN replacement_label      text,
    ADD COLUMN replacement_quantity   integer,
    ADD CONSTRAINT return_items_approved_chk
        CHECK (approved_quantity IS NULL OR approved_quantity BETWEEN 0 AND quantity),
    ADD CONSTRAINT return_items_condition_chk
        CHECK (condition IN ('RESTOCKABLE', 'DAMAGED', 'NON_RESTOCKABLE')),
    ADD CONSTRAINT return_items_inspected_chk CHECK ((condition IS NULL) = (inspected_at IS NULL)),
    ADD CONSTRAINT return_items_restock_chk CHECK (
        (restocked_at IS NULL) = (restocked_quantity IS NULL)
        AND (restocked_at IS NULL OR (condition = 'RESTOCKABLE' AND restocked_quantity BETWEEN 1 AND approved_quantity))),
    ADD CONSTRAINT return_items_replacement_chk CHECK (
        (replacement_quantity IS NULL) = (replacement_sku IS NULL)
        AND (replacement_quantity IS NULL OR replacement_quantity BETWEEN 1 AND approved_quantity));

-- Returns approved before this migration (service-only in Phase 7) were
-- approved in full.
UPDATE return_items ri SET approved_quantity = ri.quantity
FROM returns r
WHERE r.id = ri.return_id AND ri.approved_quantity IS NULL
  AND r.status NOT IN ('REQUESTED', 'REJECTED', 'CANCELLED');

-- Admin return list (newest first, optionally by status).
CREATE INDEX returns_status_created_idx ON returns (status, created_at DESC, id DESC);
CREATE INDEX returns_created_idx        ON returns (created_at DESC, id DESC);

-- ---------------------------------------------------------------- refunds

-- A refund now comes from a return (source RETURN) or from cancelling an
-- order that was already paid (ORDER_CANCELLATION, no return). Refunds are
-- still carried out outside this system: the shop records the outcome
-- (processing, succeeded with a reference, or failed with a reason).
ALTER TABLE refunds
    ALTER COLUMN return_id DROP NOT NULL,
    ADD COLUMN order_id bigint REFERENCES orders (id),
    ADD COLUMN source   text NOT NULL DEFAULT 'RETURN',
    ADD COLUMN note     text NOT NULL DEFAULT '';

UPDATE refunds f SET order_id = r.order_id FROM returns r WHERE r.id = f.return_id;

ALTER TABLE refunds
    ALTER COLUMN order_id SET NOT NULL,
    ALTER COLUMN source DROP DEFAULT,
    ADD CONSTRAINT refunds_source_chk CHECK (source IN ('RETURN', 'ORDER_CANCELLATION')),
    ADD CONSTRAINT refunds_source_ref_chk CHECK ((source = 'RETURN') = (return_id IS NOT NULL)),
    ADD CONSTRAINT refunds_failed_chk CHECK (status <> 'FAILED' OR char_length(btrim(coalesce(failure_reason, ''))) > 0),
    ADD CONSTRAINT refunds_note_chk CHECK (char_length(note) <= 500);

-- One open or succeeded cancellation refund per order.
CREATE UNIQUE INDEX refunds_one_cancellation_idx ON refunds (order_id)
    WHERE source = 'ORDER_CANCELLATION' AND status NOT IN ('FAILED', 'CANCELLED');
CREATE INDEX refunds_order_idx  ON refunds (order_id);
CREATE INDEX refunds_status_idx ON refunds (status, requested_at DESC, id DESC);

-- ---------------------------------------------------------------- reviews

-- Moderation: a hidden review stays stored (the buyer still sees it in
-- "Ulasan Saya") but is not public and does not count in the product
-- rating. Nothing is hard-deleted.
-- edited_at: when the buyer last changed the review; updated_at also moves
-- on moderation, so it no longer means "edited".
ALTER TABLE product_reviews
    ADD COLUMN edited_at         timestamptz,
    ADD COLUMN hidden_at         timestamptz,
    ADD COLUMN hidden_by         bigint REFERENCES users (id),
    ADD COLUMN moderation_reason text,
    ADD CONSTRAINT product_reviews_hidden_chk CHECK (
        (hidden_at IS NULL) = (moderation_reason IS NULL)
        AND (moderation_reason IS NULL OR char_length(btrim(moderation_reason)) BETWEEN 1 AND 500));

UPDATE product_reviews SET edited_at = updated_at WHERE updated_at - created_at > interval '1 second';

-- Admin moderation list (newest first).
CREATE INDEX product_reviews_created_idx ON product_reviews (created_at DESC, id DESC);

-- ---------------------------------------------------------------- audit

-- Append-only record of admin actions. metadata holds small facts (status
-- before/after, quantities, notes) and never secrets, passwords or tokens.
CREATE TABLE admin_audit_logs (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    admin_user_id bigint      NOT NULL REFERENCES users (id),
    action        text        NOT NULL,
    entity_type   text        NOT NULL,
    entity_id     bigint,
    entity_label  text        NOT NULL DEFAULT '',
    metadata      jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT admin_audit_logs_action_chk CHECK (action ~ '^[a-z_]+\.[a-z_]+$'),
    CONSTRAINT admin_audit_logs_entity_chk CHECK (entity_type IN (
        'ORDER', 'PRODUCT', 'VARIANT', 'CATEGORY', 'RETURN', 'REFUND', 'REVIEW', 'USER'))
);

CREATE INDEX admin_audit_logs_created_idx ON admin_audit_logs (created_at DESC, id DESC);
CREATE INDEX admin_audit_logs_entity_idx  ON admin_audit_logs (entity_type, entity_id, id DESC);

CREATE FUNCTION admin_audit_logs_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'admin_audit_logs is append-only' USING ERRCODE = 'insufficient_privilege';
END
$$;

CREATE TRIGGER admin_audit_logs_no_change
    BEFORE UPDATE OR DELETE ON admin_audit_logs
    FOR EACH ROW EXECUTE FUNCTION admin_audit_logs_append_only();
