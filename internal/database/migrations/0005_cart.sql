-- Phase 4: shopping cart.
--
-- A cart line only records WHICH variant and HOW MANY. Price, stock and
-- availability are read from the catalog every time the cart is shown, and
-- checkout (Phase 5) validates them again. Nothing here reserves or
-- decrements stock.

CREATE TABLE carts (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    active     boolean     NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- At most one active cart per user (older carts may be kept inactive).
CREATE UNIQUE INDEX carts_one_active_per_user ON carts (user_id) WHERE active;

CREATE TRIGGER carts_set_updated_at
    BEFORE UPDATE ON carts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE cart_items (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    cart_id    bigint      NOT NULL REFERENCES carts (id) ON DELETE CASCADE,
    -- NULL once the variant is deleted from the catalog: the line stays so the
    -- customer sees the product is gone and can remove it.
    variant_id bigint      REFERENCES product_variants (id) ON DELETE SET NULL,
    quantity   integer     NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    -- Adding the same variant again increments quantity (UPSERT); this
    -- constraint is the final guard against duplicate lines under races.
    CONSTRAINT cart_items_variant_key  UNIQUE (cart_id, variant_id),
    CONSTRAINT cart_items_quantity_chk CHECK (quantity BETWEEN 1 AND 99)
);

-- Lets ON DELETE SET NULL find the lines of a deleted variant without a scan.
CREATE INDEX cart_items_variant_id_idx ON cart_items (variant_id);

CREATE TRIGGER cart_items_set_updated_at
    BEFORE UPDATE ON cart_items
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
