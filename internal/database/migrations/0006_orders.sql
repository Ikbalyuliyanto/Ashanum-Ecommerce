-- Phase 5: orders.
--
-- An order is a self-contained record: the shipping address and every line
-- (name, SKU, variant, image, unit price) are copied at checkout, so later
-- edits to addresses or the catalog never change order history. Catalog
-- references (product_id, variant_id) are kept only as optional links.
--
-- Not here yet, by design:
--   * payments (Phase 6) will be a separate table referencing orders.id;
--     WAITING_PAYMENT is the only status Phase 5 creates.
--   * shipping (provider, courier, service, tracking number, ETA) will be a
--     separate shipments table referencing orders.id; shipping_cost stays 0
--     until then and the UI says it is not calculated yet.

-- Readable order numbers ASH-YYYYMMDD-NNNNNN (date in Asia/Jakarta). The
-- counter is a sequence: unique and concurrency-safe without locks; numbers
-- may skip after a rolled-back checkout.
CREATE SEQUENCE order_number_seq;

CREATE TABLE orders (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_number    text        NOT NULL DEFAULT
                    ('ASH-' || to_char(now() AT TIME ZONE 'Asia/Jakarta', 'YYYYMMDD') || '-'
                            || lpad(nextval('order_number_seq')::text, 6, '0')),
    user_id         bigint      NOT NULL REFERENCES users (id),
    -- One order per checkout page submission: a double click or retry
    -- returns the existing order instead of creating a second one.
    idempotency_key text,
    status          text        NOT NULL DEFAULT 'WAITING_PAYMENT',

    subtotal        bigint      NOT NULL,
    shipping_cost   bigint      NOT NULL DEFAULT 0,
    discount        bigint      NOT NULL DEFAULT 0,
    total           bigint      NOT NULL,

    -- Shipping address snapshot.
    address_label   text        NOT NULL DEFAULT '',
    recipient_name  text        NOT NULL,
    recipient_phone text        NOT NULL,
    province        text        NOT NULL,
    city            text        NOT NULL,
    district        text        NOT NULL,
    village         text        NOT NULL,
    postal_code     text        NOT NULL,
    address         text        NOT NULL,
    latitude        double precision,
    longitude       double precision,

    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT orders_order_number_key UNIQUE (order_number),
    CONSTRAINT orders_idempotency_key  UNIQUE (user_id, idempotency_key),
    CONSTRAINT orders_status_chk CHECK (status IN (
        'WAITING_PAYMENT', 'WAITING_CONFIRMATION', 'PROCESSING', 'SHIPPED', 'COMPLETED', 'CANCELLED')),
    CONSTRAINT orders_amounts_chk CHECK (
        subtotal >= 0 AND shipping_cost >= 0 AND discount >= 0
        AND total = subtotal + shipping_cost - discount AND total >= 0)
);

-- Order history: a user's orders, newest first.
CREATE INDEX orders_user_created_idx ON orders (user_id, created_at DESC);

CREATE TRIGGER orders_set_updated_at
    BEFORE UPDATE ON orders
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE order_items (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id      bigint      NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    -- Optional links back to the catalog (NULL once deleted there).
    product_id    bigint      REFERENCES products (id) ON DELETE SET NULL,
    variant_id    bigint      REFERENCES product_variants (id) ON DELETE SET NULL,
    -- Snapshot at checkout.
    product_name  text        NOT NULL,
    product_slug  text        NOT NULL,
    sku           text        NOT NULL,
    variant_label text        NOT NULL DEFAULT '',
    image_url     text        NOT NULL DEFAULT '',
    unit_price    bigint      NOT NULL,
    quantity      integer     NOT NULL,
    subtotal      bigint      NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT order_items_price_chk    CHECK (unit_price > 0),
    CONSTRAINT order_items_quantity_chk CHECK (quantity BETWEEN 1 AND 99),
    CONSTRAINT order_items_subtotal_chk CHECK (subtotal = unit_price * quantity)
);

CREATE INDEX order_items_order_id_idx   ON order_items (order_id);
-- Catalog links: keep ON DELETE SET NULL cheap, and serve "has this user
-- bought this product" for reviews (Phase 7).
CREATE INDEX order_items_product_id_idx ON order_items (product_id);
CREATE INDEX order_items_variant_id_idx ON order_items (variant_id);
