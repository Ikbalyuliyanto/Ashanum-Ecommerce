-- Phase 10: shipping through a provider (Biteship first): delivery areas,
-- product weight, shipping quotes, the order's shipping snapshot, booked
-- shipments with tracking events.
--
-- Nothing here is provider specific except the "provider" columns: areas,
-- quotes and shipments name their provider, so another one (RajaOngkir,
-- Komerce, ...) can be added without changing checkout or orders. Raw
-- provider responses are never stored; only normalized values.

-- ---------------------------------------------------------------- areas

-- Delivery areas (kecamatan + kode pos level) found through the provider's
-- area search, cached on demand: checkout never searches areas, it reads the
-- area saved on the address.
CREATE TABLE shipping_areas (
    provider         text             NOT NULL,
    provider_area_id text             NOT NULL,
    label            text             NOT NULL,
    province         text             NOT NULL DEFAULT '',
    city             text             NOT NULL DEFAULT '',
    district         text             NOT NULL DEFAULT '',
    postal_code      text             NOT NULL DEFAULT '',
    latitude         double precision,
    longitude        double precision,
    cached_at        timestamptz      NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, provider_area_id),
    CONSTRAINT shipping_areas_id_chk    CHECK (char_length(provider_area_id) BETWEEN 1 AND 100),
    CONSTRAINT shipping_areas_label_chk CHECK (char_length(label) BETWEEN 1 AND 300),
    CONSTRAINT shipping_areas_postal_chk CHECK (postal_code = '' OR postal_code ~ '^[0-9]{5}$'),
    CONSTRAINT shipping_areas_coordinates_chk CHECK ((latitude IS NULL) = (longitude IS NULL))
);

-- The address's delivery area, provider neutral: (provider, area id) plus
-- the label shown to the buyer. NULL for addresses saved before Phase 10:
-- checkout asks the buyer to complete the location before showing rates.
ALTER TABLE user_addresses
    ADD COLUMN shipping_area_provider text,
    ADD COLUMN shipping_area_id       text,
    ADD COLUMN shipping_area_label    text,
    ADD CONSTRAINT user_addresses_shipping_area_chk CHECK (
        (shipping_area_provider IS NULL) = (shipping_area_id IS NULL)
        AND (shipping_area_id IS NULL) = (shipping_area_label IS NULL)),
    ADD CONSTRAINT user_addresses_shipping_area_fkey
        FOREIGN KEY (shipping_area_provider, shipping_area_id)
        REFERENCES shipping_areas (provider, provider_area_id);

-- ---------------------------------------------------------------- weight

-- Weight in grams, never taken from the client. A product has a default
-- weight; a variant may override it (e.g. XXL heavier than S). 0 on the
-- product means "not set yet": such a variant cannot be checked out (no
-- silent 1 kg assumption) and the admin is warned. Dimensions are optional
-- (cm), sent to the provider when known.
ALTER TABLE products
    ADD COLUMN weight_grams integer NOT NULL DEFAULT 0,
    ADD COLUMN length_cm    integer,
    ADD COLUMN width_cm     integer,
    ADD COLUMN height_cm    integer,
    ADD CONSTRAINT products_weight_chk CHECK (weight_grams BETWEEN 0 AND 100000),
    ADD CONSTRAINT products_dimensions_chk CHECK (
        (length_cm IS NULL OR length_cm BETWEEN 1 AND 300)
        AND (width_cm IS NULL OR width_cm BETWEEN 1 AND 300)
        AND (height_cm IS NULL OR height_cm BETWEEN 1 AND 300));

ALTER TABLE product_variants
    ADD COLUMN weight_grams integer,
    ADD CONSTRAINT product_variants_weight_chk CHECK (weight_grams IS NULL OR weight_grams BETWEEN 1 AND 100000);

-- ---------------------------------------------------------------- quotes

-- A shipping quote is one rate offered to one buyer for one route and one
-- package: the price the order will charge if it is placed before
-- expires_at with the same destination, origin and items. Checkout stores
-- the offered rates here and the order accepts only a quote id; the browser
-- or app never sends a shipping cost. quote_key identifies the offer (route,
-- package, courier, service), so refreshing rates updates the same unused
-- quote instead of adding rows. Once an order uses a quote (order_id), it is
-- kept as the record of what was charged.
CREATE TABLE shipping_quotes (
    id              text        PRIMARY KEY DEFAULT gen_random_uuid()::text,
    user_id         bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider        text        NOT NULL,
    quote_key       text        NOT NULL,
    origin_key      text        NOT NULL,
    destination_key text        NOT NULL,
    package_hash    text        NOT NULL,
    weight_grams    integer     NOT NULL,
    courier_code    text        NOT NULL,
    courier_name    text        NOT NULL,
    service_code    text        NOT NULL,
    service_name    text        NOT NULL,
    description     text        NOT NULL DEFAULT '',
    estimated_days  text        NOT NULL DEFAULT '',
    price           bigint      NOT NULL,
    expires_at      timestamptz NOT NULL,
    order_id        bigint      REFERENCES orders (id),
    used_at         timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT shipping_quotes_price_chk  CHECK (price > 0),
    CONSTRAINT shipping_quotes_weight_chk CHECK (weight_grams > 0),
    CONSTRAINT shipping_quotes_used_chk   CHECK ((order_id IS NULL) = (used_at IS NULL)),
    CONSTRAINT shipping_quotes_codes_chk  CHECK (
        char_length(courier_code) BETWEEN 1 AND 40 AND char_length(service_code) BETWEEN 1 AND 60)
);

-- One open quote per buyer and offer (refreshed in place).
CREATE UNIQUE INDEX shipping_quotes_open_key_idx ON shipping_quotes (user_id, quote_key) WHERE order_id IS NULL;
-- Cleanup of expired unused quotes (reconcile-shipments).
CREATE INDEX shipping_quotes_expires_idx ON shipping_quotes (expires_at) WHERE order_id IS NULL;
CREATE UNIQUE INDEX shipping_quotes_order_idx ON shipping_quotes (order_id) WHERE order_id IS NOT NULL;

CREATE TRIGGER shipping_quotes_set_updated_at
    BEFORE UPDATE ON shipping_quotes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------- orders

-- The order's shipping snapshot, copied from the quote when the order is
-- placed: what the buyer chose and paid. It stays readable when the
-- provider is down and never changes afterwards (no admin workflow edits
-- it). The destination is the existing address snapshot plus the area; the
-- origin is the shop's address at that time. Orders placed before Phase 10
-- have no snapshot (shipping_provider NULL, shipping_cost 0).
ALTER TABLE orders
    ADD COLUMN shipping_provider       text,
    ADD COLUMN shipping_courier_code   text,
    ADD COLUMN shipping_courier_name   text,
    ADD COLUMN shipping_service_code   text,
    ADD COLUMN shipping_service_name   text,
    ADD COLUMN shipping_estimated_days text,
    ADD COLUMN shipping_weight_grams   integer,
    ADD COLUMN shipping_area_id        text,
    ADD COLUMN shipping_area_label     text,
    ADD COLUMN shipping_origin         jsonb,
    ADD CONSTRAINT orders_shipping_snapshot_chk CHECK (
        shipping_provider IS NULL
        OR (shipping_courier_code IS NOT NULL AND shipping_courier_name IS NOT NULL
            AND shipping_service_code IS NOT NULL AND shipping_service_name IS NOT NULL
            AND shipping_weight_grams > 0 AND shipping_area_id IS NOT NULL
            AND shipping_origin IS NOT NULL AND shipping_cost > 0));

-- Unit weight at checkout, so a booking sends what was quoted even if the
-- catalog weight changes later. NULL for orders placed before Phase 10.
ALTER TABLE order_items
    ADD COLUMN weight_grams integer,
    ADD CONSTRAINT order_items_weight_chk CHECK (weight_grams IS NULL OR weight_grams > 0);

-- ---------------------------------------------------------------- shipments

-- shipments (Phase 8, manual only) now also holds shipments booked through
-- the provider. An order may have several rows over time (a cancelled or
-- failed booking is kept), but at most one active one.
--
-- status is the internal shipment status, never the provider's own word:
--   PENDING           booking sent, provider answer not stored yet
--   FAILED            the provider refused the booking (kept for history)
--   CREATED           booked, waiting for pickup
--   PICKED_UP, IN_TRANSIT, OUT_FOR_DELIVERY, DELIVERED
--   EXCEPTION         on hold / rejected / returning: needs attention
--   RETURNED, CANCELLED
-- raw_status is the provider's last status, for support only.
-- booking_key is the reference sent with the booking (unique): a retry
-- after a timeout reuses it, so the provider never creates two shipments.
ALTER TABLE shipments
    DROP CONSTRAINT shipments_order_key,
    DROP CONSTRAINT shipments_method_chk,
    ALTER COLUMN shipped_at DROP NOT NULL,
    ADD COLUMN provider             text,
    ADD COLUMN provider_shipment_id text,
    ADD COLUMN provider_tracking_id text,
    ADD COLUMN courier_code         text NOT NULL DEFAULT '',
    ADD COLUMN service_code         text NOT NULL DEFAULT '',
    ADD COLUMN status               text NOT NULL DEFAULT 'IN_TRANSIT',
    ADD COLUMN raw_status           text,
    ADD COLUMN estimated_days       text NOT NULL DEFAULT '',
    ADD COLUMN tracking_url         text,
    ADD COLUMN booking_key          text,
    ADD COLUMN booking_started_at   timestamptz,
    ADD COLUMN failure_reason       text,
    ADD COLUMN booked_at            timestamptz,
    ADD COLUMN delivered_at         timestamptz,
    ADD COLUMN cancelled_at         timestamptz,
    ADD COLUMN last_synced_at       timestamptz,
    ADD CONSTRAINT shipments_method_chk CHECK (method IN ('MANUAL', 'PROVIDER')),
    ADD CONSTRAINT shipments_status_chk CHECK (status IN (
        'PENDING', 'FAILED', 'CREATED', 'PICKED_UP', 'IN_TRANSIT', 'OUT_FOR_DELIVERY',
        'DELIVERED', 'EXCEPTION', 'RETURNED', 'CANCELLED')),
    ADD CONSTRAINT shipments_provider_chk CHECK (
        (method = 'PROVIDER') = (provider IS NOT NULL)
        AND (method = 'PROVIDER') = (booking_key IS NOT NULL)
        AND (method = 'MANUAL' OR status <> 'PENDING' OR booking_started_at IS NOT NULL)
        AND (status IN ('PENDING', 'FAILED') OR method = 'MANUAL' OR provider_shipment_id IS NOT NULL)),
    ADD CONSTRAINT shipments_manual_chk CHECK (method = 'PROVIDER' OR shipped_at IS NOT NULL),
    ADD CONSTRAINT shipments_failure_chk CHECK ((status = 'FAILED') = (failure_reason IS NOT NULL)),
    ADD CONSTRAINT shipments_failure_len_chk CHECK (char_length(failure_reason) <= 300),
    ADD CONSTRAINT shipments_cancelled_chk CHECK ((status = 'CANCELLED') = (cancelled_at IS NOT NULL));

CREATE UNIQUE INDEX shipments_active_order_idx ON shipments (order_id)
    WHERE status NOT IN ('CANCELLED', 'FAILED');
CREATE INDEX shipments_order_idx ON shipments (order_id, id DESC);
CREATE UNIQUE INDEX shipments_provider_id_idx ON shipments (provider, provider_shipment_id)
    WHERE provider_shipment_id IS NOT NULL;
CREATE UNIQUE INDEX shipments_booking_key_idx ON shipments (booking_key) WHERE booking_key IS NOT NULL;
-- Provider shipments still moving (reconcile-shipments).
CREATE INDEX shipments_open_idx ON shipments (last_synced_at NULLS FIRST, id)
    WHERE method = 'PROVIDER' AND status IN ('CREATED', 'PICKED_UP', 'IN_TRANSIT', 'OUT_FOR_DELIVERY', 'EXCEPTION');

-- Tracking history of a shipment, newest information from the provider
-- (webhook or tracking lookup) or the shop. event_key identifies an event
-- (for the provider: its status), so the same event delivered twice is
-- stored once; event_time keeps the earliest time it was seen.
CREATE TABLE shipment_events (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    shipment_id bigint      NOT NULL REFERENCES shipments (id) ON DELETE CASCADE,
    event_key   text        NOT NULL,
    status      text        NOT NULL,
    raw_status  text        NOT NULL DEFAULT '',
    description text        NOT NULL DEFAULT '',
    location    text        NOT NULL DEFAULT '',
    source      text        NOT NULL,
    event_time  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT shipment_events_key UNIQUE (shipment_id, event_key),
    CONSTRAINT shipment_events_status_chk CHECK (status IN (
        'CREATED', 'PICKED_UP', 'IN_TRANSIT', 'OUT_FOR_DELIVERY', 'DELIVERED', 'EXCEPTION', 'RETURNED', 'CANCELLED')),
    CONSTRAINT shipment_events_source_chk CHECK (source IN ('booking', 'webhook', 'tracking', 'admin')),
    CONSTRAINT shipment_events_text_chk CHECK (
        char_length(event_key) BETWEEN 1 AND 120 AND char_length(description) <= 500 AND char_length(location) <= 200)
);
