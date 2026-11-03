-- Phase 3: catalog — categories, products, images, attributes, variants.
--
-- Source of truth for price and stock is product_variants only. A product
-- without options has exactly one variant with no attribute values.
--
-- products.price / price_max / original_price / in_stock are a read-only
-- summary of the active variants, kept current by a trigger so listings can
-- filter and sort by price without aggregating variants. The application
-- never writes them. sold_count and rating_* are counters that the order and
-- review phases will maintain from their own tables.

CREATE TABLE categories (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name          text        NOT NULL,
    slug          text        NOT NULL,
    image_url     text,
    active        boolean     NOT NULL DEFAULT true,
    display_order integer     NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT categories_slug_key    UNIQUE (slug),
    CONSTRAINT categories_slug_format CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$')
);

CREATE TRIGGER categories_set_updated_at
    BEFORE UPDATE ON categories
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE products (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    category_id    bigint           NOT NULL REFERENCES categories (id),
    name           text             NOT NULL,
    slug           text             NOT NULL,
    description    text             NOT NULL DEFAULT '',
    brand          text             NOT NULL DEFAULT '',
    active         boolean          NOT NULL DEFAULT true,
    featured       boolean          NOT NULL DEFAULT false,
    flash_sale     boolean          NOT NULL DEFAULT false,
    sold_count     integer          NOT NULL DEFAULT 0,
    rating_average double precision NOT NULL DEFAULT 0,
    rating_count   integer          NOT NULL DEFAULT 0,

    -- Summary of active variants (trigger-maintained, read-only).
    price          bigint,
    price_max      bigint,
    original_price bigint,
    in_stock       boolean          NOT NULL DEFAULT false,

    search_vector  tsvector GENERATED ALWAYS AS
                   (to_tsvector('simple'::regconfig, name || ' ' || brand)) STORED,
    created_at     timestamptz      NOT NULL DEFAULT now(),
    updated_at     timestamptz      NOT NULL DEFAULT now(),
    CONSTRAINT products_slug_key       UNIQUE (slug),
    CONSTRAINT products_slug_format    CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
    CONSTRAINT products_sold_count_chk CHECK (sold_count >= 0),
    CONSTRAINT products_rating_chk     CHECK (rating_average BETWEEN 0 AND 5 AND rating_count >= 0)
);

CREATE INDEX products_category_id_idx ON products (category_id);
CREATE INDEX products_search_idx      ON products USING gin (search_vector);

CREATE TRIGGER products_set_updated_at
    BEFORE UPDATE ON products
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Images are referenced by URL (external storage such as Cloudinary); no
-- file is stored on the application server. The lowest display_order is the
-- main image.
CREATE TABLE product_images (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    product_id    bigint      NOT NULL REFERENCES products (id) ON DELETE CASCADE,
    url           text        NOT NULL,
    alt           text        NOT NULL DEFAULT '',
    display_order integer     NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX product_images_product_idx ON product_images (product_id, display_order);

-- Option dimensions per product (Warna, Ukuran, Kapasitas, Aroma, ...).
CREATE TABLE product_attributes (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    product_id    bigint  NOT NULL REFERENCES products (id) ON DELETE CASCADE,
    name          text    NOT NULL,
    display_order integer NOT NULL DEFAULT 0,
    CONSTRAINT product_attributes_name_key UNIQUE (product_id, name),
    CONSTRAINT product_attributes_owner_key UNIQUE (product_id, id)
);

CREATE TABLE product_attribute_values (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    attribute_id  bigint  NOT NULL REFERENCES product_attributes (id) ON DELETE CASCADE,
    value         text    NOT NULL,
    display_order integer NOT NULL DEFAULT 0,
    CONSTRAINT product_attribute_values_value_key UNIQUE (attribute_id, value),
    CONSTRAINT product_attribute_values_owner_key UNIQUE (attribute_id, id)
);

-- A sellable combination. Price is in rupiah.
CREATE TABLE product_variants (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    product_id     bigint      NOT NULL REFERENCES products (id) ON DELETE CASCADE,
    sku            text        NOT NULL,
    price          bigint      NOT NULL,
    original_price bigint,
    stock          integer     NOT NULL DEFAULT 0,
    active         boolean     NOT NULL DEFAULT true,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT product_variants_sku_key        UNIQUE (sku),
    CONSTRAINT product_variants_owner_key      UNIQUE (product_id, id),
    CONSTRAINT product_variants_sku_format     CHECK (sku ~ '^[A-Z0-9][A-Z0-9._-]{0,63}$'),
    CONSTRAINT product_variants_price_chk      CHECK (price > 0),
    CONSTRAINT product_variants_original_chk   CHECK (original_price IS NULL OR original_price > price),
    CONSTRAINT product_variants_stock_chk      CHECK (stock >= 0)
);

CREATE TRIGGER product_variants_set_updated_at
    BEFORE UPDATE ON product_variants
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Which value a variant has for each attribute. The composite foreign keys
-- guarantee the variant, the attribute and the value all belong to the same
-- product, and the primary key allows one value per attribute per variant.
CREATE TABLE product_variant_values (
    product_id   bigint NOT NULL,
    variant_id   bigint NOT NULL,
    attribute_id bigint NOT NULL,
    value_id     bigint NOT NULL,
    PRIMARY KEY (variant_id, attribute_id),
    FOREIGN KEY (product_id, variant_id)
        REFERENCES product_variants (product_id, id) ON DELETE CASCADE,
    FOREIGN KEY (product_id, attribute_id)
        REFERENCES product_attributes (product_id, id) ON DELETE CASCADE,
    FOREIGN KEY (attribute_id, value_id)
        REFERENCES product_attribute_values (attribute_id, id) ON DELETE CASCADE
);

-- Recompute the summary columns of one product from its active variants.
-- Writes only when something changed.
CREATE FUNCTION refresh_product_summary(p_product_id bigint) RETURNS void
LANGUAGE sql AS $$
    UPDATE products p
    SET price          = s.price,
        price_max      = s.price_max,
        original_price = s.original_price,
        in_stock       = s.in_stock
    FROM (
        SELECT min(v.price)                                           AS price,
               max(v.price)                                           AS price_max,
               (array_agg(v.original_price ORDER BY v.price, v.id))[1] AS original_price,
               coalesce(bool_or(v.stock > 0), false)                  AS in_stock
        FROM product_variants v
        WHERE v.product_id = p_product_id AND v.active
    ) s
    WHERE p.id = p_product_id
      AND (p.price, p.price_max, p.original_price, p.in_stock)
          IS DISTINCT FROM (s.price, s.price_max, s.original_price, s.in_stock);
$$;

CREATE FUNCTION product_variants_refresh_summary() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        PERFORM refresh_product_summary(OLD.product_id);
    END IF;
    IF TG_OP = 'INSERT' OR (TG_OP = 'UPDATE' AND NEW.product_id <> OLD.product_id) THEN
        PERFORM refresh_product_summary(NEW.product_id);
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER product_variants_summary_insert_delete
    AFTER INSERT OR DELETE ON product_variants
    FOR EACH ROW EXECUTE FUNCTION product_variants_refresh_summary();

-- Stock changes (future checkout) refresh the summary only when a variant
-- runs out or comes back, so ordinary sales do not rewrite the product row.
CREATE TRIGGER product_variants_summary_update
    AFTER UPDATE ON product_variants
    FOR EACH ROW
    WHEN (OLD.price IS DISTINCT FROM NEW.price
          OR OLD.original_price IS DISTINCT FROM NEW.original_price
          OR OLD.active IS DISTINCT FROM NEW.active
          OR OLD.product_id IS DISTINCT FROM NEW.product_id
          OR (OLD.stock > 0) IS DISTINCT FROM (NEW.stock > 0))
    EXECUTE FUNCTION product_variants_refresh_summary();
