-- Phase 7: product reviews, returns (refund / exchange requests) and the
-- refund domain foundation.
--
-- Reviews and returns are only possible for COMPLETED orders. Nothing in
-- this phase moves an order to COMPLETED: that arrives with the admin and
-- shipping phases. Refunds are recorded here but never sent to a payment
-- provider in this phase.

-- When an order was completed: the start of the return window.
ALTER TABLE orders
    ADD COLUMN completed_at timestamptz,
    ADD CONSTRAINT orders_completed_at_chk CHECK (status <> 'COMPLETED' OR completed_at IS NOT NULL);

-- Targets for composite foreign keys below: a review or return row must
-- point at an order item of its own order, and at an order of its own user.
ALTER TABLE orders      ADD CONSTRAINT orders_id_user_key      UNIQUE (id, user_id);
ALTER TABLE order_items ADD CONSTRAINT order_items_id_order_key UNIQUE (id, order_id);

-- ---------------------------------------------------------------- reviews

-- One review per purchased order item. product_reviews is the source of
-- truth for ratings; products.rating_average / rating_count are a cache
-- recalculated in the same transaction as every review change.
CREATE TABLE product_reviews (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id       bigint      NOT NULL,
    order_id      bigint      NOT NULL,
    order_item_id bigint      NOT NULL,
    -- Link to the catalog; NULL once the product is deleted (the review and
    -- the order item snapshot stay).
    product_id    bigint      REFERENCES products (id) ON DELETE SET NULL,
    rating        smallint    NOT NULL,
    comment       text        NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT product_reviews_order_item_key UNIQUE (order_item_id),
    CONSTRAINT product_reviews_order_fk FOREIGN KEY (order_id, user_id) REFERENCES orders (id, user_id),
    CONSTRAINT product_reviews_item_fk  FOREIGN KEY (order_item_id, order_id) REFERENCES order_items (id, order_id),
    CONSTRAINT product_reviews_rating_chk  CHECK (rating BETWEEN 1 AND 5),
    CONSTRAINT product_reviews_comment_chk CHECK (char_length(comment) <= 1000)
);

-- Product page (newest first) and "Ulasan Saya".
CREATE INDEX product_reviews_product_idx ON product_reviews (product_id, created_at DESC, id DESC);
CREATE INDEX product_reviews_user_idx    ON product_reviews (user_id, created_at DESC, id DESC);
CREATE INDEX product_reviews_order_idx   ON product_reviews (order_id);

CREATE TRIGGER product_reviews_set_updated_at
    BEFORE UPDATE ON product_reviews
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Ratings now come only from product_reviews (none exist yet); earlier
-- values were demo placeholders.
UPDATE products SET rating_average = 0, rating_count = 0
WHERE rating_average <> 0 OR rating_count <> 0;

-- Review photos are not stored yet (no uploader). They will be a separate
-- review_images (review_id, url, position) table holding URLs only.

-- ---------------------------------------------------------------- returns

-- Readable return numbers RET-YYYYMMDD-NNNNNN (date in Asia/Jakarta), from a
-- sequence: unique and concurrency-safe, not the primary key.
CREATE SEQUENCE return_number_seq;

CREATE TABLE returns (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    return_number   text        NOT NULL DEFAULT
                    ('RET-' || to_char(now() AT TIME ZONE 'Asia/Jakarta', 'YYYYMMDD') || '-'
                            || lpad(nextval('return_number_seq')::text, 6, '0')),
    order_id        bigint      NOT NULL,
    user_id         bigint      NOT NULL,
    type            text        NOT NULL,
    status          text        NOT NULL DEFAULT 'REQUESTED',
    reason          text        NOT NULL,
    description     text        NOT NULL DEFAULT '',
    -- Server-computed from order_items snapshot prices × quantities; 0 for
    -- an exchange. Items only: shipping and fees are not refunded until a
    -- return shipping policy exists.
    refund_amount   bigint      NOT NULL,
    -- Message from the shop to the buyer (e.g. why a request was rejected).
    resolution_note text        NOT NULL DEFAULT '',
    -- One request per form submission (double click, retry).
    idempotency_key text,
    requested_at    timestamptz NOT NULL DEFAULT now(),
    approved_at     timestamptz,
    rejected_at     timestamptz,
    completed_at    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT returns_number_key      UNIQUE (return_number),
    CONSTRAINT returns_idempotency_key UNIQUE (user_id, idempotency_key),
    CONSTRAINT returns_order_fk FOREIGN KEY (order_id, user_id) REFERENCES orders (id, user_id),
    CONSTRAINT returns_type_chk   CHECK (type IN ('REFUND', 'EXCHANGE')),
    CONSTRAINT returns_status_chk CHECK (status IN ('REQUESTED', 'APPROVED', 'REJECTED', 'WAITING_RETURN',
        'ITEM_RECEIVED', 'REFUND_PENDING', 'REFUNDED', 'EXCHANGED', 'COMPLETED', 'CANCELLED')),
    CONSTRAINT returns_reason_chk CHECK (reason IN ('DAMAGED', 'WRONG_ITEM', 'MISSING_ITEM',
        'NOT_AS_DESCRIBED', 'VARIANT_ISSUE', 'OTHER')),
    CONSTRAINT returns_description_chk CHECK (char_length(description) <= 1000
        AND (reason <> 'OTHER' OR char_length(btrim(description)) > 0)),
    CONSTRAINT returns_amount_chk CHECK (refund_amount >= 0 AND (type = 'REFUND' OR refund_amount = 0))
);

CREATE INDEX returns_user_created_idx ON returns (user_id, created_at DESC, id DESC);
CREATE INDEX returns_order_idx        ON returns (order_id);

CREATE TRIGGER returns_set_updated_at
    BEFORE UPDATE ON returns
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE return_items (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    return_id     bigint  NOT NULL REFERENCES returns (id) ON DELETE CASCADE,
    order_item_id bigint  NOT NULL REFERENCES order_items (id),
    quantity      integer NOT NULL,
    -- Copied from order_items.unit_price (the price paid).
    unit_price    bigint  NOT NULL,
    reason        text,
    CONSTRAINT return_items_item_key     UNIQUE (return_id, order_item_id),
    CONSTRAINT return_items_quantity_chk CHECK (quantity BETWEEN 1 AND 99),
    CONSTRAINT return_items_price_chk    CHECK (unit_price > 0),
    CONSTRAINT return_items_reason_chk   CHECK (reason IN ('DAMAGED', 'WRONG_ITEM', 'MISSING_ITEM',
        'NOT_AS_DESCRIBED', 'VARIANT_ISSUE', 'OTHER'))
);

-- Quantity already requested per order item.
CREATE INDEX return_items_order_item_idx ON return_items (order_item_id);

-- Evidence is a URL to external storage (https only), never a file on the
-- server or a blob in the database.
CREATE TABLE return_evidence (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    return_id  bigint      NOT NULL REFERENCES returns (id) ON DELETE CASCADE,
    url        text        NOT NULL,
    media_type text        NOT NULL,
    position   smallint    NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT return_evidence_position_key UNIQUE (return_id, position),
    CONSTRAINT return_evidence_url_chk      CHECK (url LIKE 'https://%' AND char_length(url) <= 2048),
    CONSTRAINT return_evidence_type_chk     CHECK (media_type IN ('IMAGE', 'VIDEO', 'LINK')),
    CONSTRAINT return_evidence_position_chk CHECK (position BETWEEN 1 AND 5)
);

-- Status history shown as the return timeline (append-only).
CREATE TABLE return_events (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    return_id   bigint      NOT NULL REFERENCES returns (id) ON DELETE CASCADE,
    from_status text,
    to_status   text        NOT NULL,
    actor       text        NOT NULL,
    note        text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT return_events_actor_chk CHECK (actor IN ('USER', 'ADMIN', 'SYSTEM'))
);

CREATE INDEX return_events_return_idx ON return_events (return_id, id);

-- ---------------------------------------------------------------- refunds

-- Money owed back for a refund return. In this phase a refund is only ever
-- created as PENDING (after the returned items were received); no payment
-- provider is called. Executing refunds (Midtrans or manual transfer) is a
-- later phase; provider is NULL until then.
CREATE TABLE refunds (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    return_id          bigint      NOT NULL REFERENCES returns (id),
    payment_id         bigint      REFERENCES payments (id),
    amount             bigint      NOT NULL,
    status             text        NOT NULL DEFAULT 'PENDING',
    provider           text,
    provider_refund_id text,
    requested_at       timestamptz NOT NULL DEFAULT now(),
    processed_at       timestamptz,
    failure_reason     text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT refunds_amount_chk    CHECK (amount > 0),
    CONSTRAINT refunds_status_chk    CHECK (status IN ('PENDING', 'APPROVED', 'PROCESSING', 'SUCCEEDED', 'FAILED', 'CANCELLED')),
    CONSTRAINT refunds_processed_chk CHECK (status <> 'SUCCEEDED' OR processed_at IS NOT NULL)
);

-- At most one open or succeeded refund per return.
CREATE UNIQUE INDEX refunds_one_active_idx ON refunds (return_id) WHERE status NOT IN ('FAILED', 'CANCELLED');
CREATE INDEX refunds_payment_idx ON refunds (payment_id);

CREATE TRIGGER refunds_set_updated_at
    BEFORE UPDATE ON refunds
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
