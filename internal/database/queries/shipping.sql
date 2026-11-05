-- Shipping: delivery area cache, quotes, provider shipments and their
-- tracking events. Lock order across modules: orders → shipments.

-- ---------------------------------------------------------------- areas

-- UpsertShippingAreas caches areas found by a search (one statement).
-- name: UpsertShippingAreas :exec
INSERT INTO shipping_areas (provider, provider_area_id, label, province, city, district, postal_code,
                            latitude, longitude, cached_at)
SELECT @provider, x.id, (@labels::text[])[x.n], (@provinces::text[])[x.n], (@cities::text[])[x.n],
       (@districts::text[])[x.n], (@postal_codes::text[])[x.n],
       CASE WHEN (@has_coordinates::boolean[])[x.n] THEN (@latitudes::float8[])[x.n] END,
       CASE WHEN (@has_coordinates::boolean[])[x.n] THEN (@longitudes::float8[])[x.n] END,
       now()
FROM unnest(@ids::text[]) WITH ORDINALITY AS x(id, n)
ON CONFLICT (provider, provider_area_id) DO UPDATE
SET label = excluded.label, province = excluded.province, city = excluded.city, district = excluded.district,
    postal_code = excluded.postal_code, latitude = excluded.latitude, longitude = excluded.longitude,
    cached_at = excluded.cached_at;

-- name: GetShippingArea :one
SELECT * FROM shipping_areas WHERE provider = @provider AND provider_area_id = @provider_area_id;

-- ---------------------------------------------------------------- quotes

-- UpsertShippingQuotes stores the rates offered to a buyer (one statement):
-- an unused quote for the same offer is refreshed in place, so its id stays
-- the same while price and expiry follow the provider.
-- name: UpsertShippingQuotes :many
INSERT INTO shipping_quotes (user_id, provider, quote_key, origin_key, destination_key, package_hash, weight_grams,
                             courier_code, courier_name, service_code, service_name, description, estimated_days,
                             price, expires_at)
SELECT @user_id, @provider, x.quote_key, @origin_key, @destination_key, @package_hash, @weight_grams,
       (@courier_codes::text[])[x.n], (@courier_names::text[])[x.n], (@service_codes::text[])[x.n],
       (@service_names::text[])[x.n], (@descriptions::text[])[x.n], (@estimated_days::text[])[x.n],
       (@prices::bigint[])[x.n], @expires_at
FROM unnest(@quote_keys::text[]) WITH ORDINALITY AS x(quote_key, n)
ON CONFLICT (user_id, quote_key) WHERE order_id IS NULL DO UPDATE
SET courier_name = excluded.courier_name, service_name = excluded.service_name, description = excluded.description,
    estimated_days = excluded.estimated_days, price = excluded.price, expires_at = excluded.expires_at
RETURNING id, quote_key;

-- LockShippingQuote reads an unused quote of the user for placing an
-- order, row-locked so two checkouts cannot use it at once.
-- name: LockShippingQuote :one
SELECT * FROM shipping_quotes
WHERE id = @id AND user_id = @user_id AND order_id IS NULL
FOR UPDATE;

-- name: UseShippingQuote :execrows
UPDATE shipping_quotes SET order_id = @order_id, used_at = now()
WHERE id = @id AND order_id IS NULL;

-- DeleteStaleShippingQuotes removes unused quotes that expired before
-- @before (used ones stay: they record what an order was charged).
-- name: DeleteStaleShippingQuotes :execrows
DELETE FROM shipping_quotes WHERE order_id IS NULL AND expires_at < @before;

-- ---------------------------------------------------------------- shipments

-- BookingOrder row-locks an order for booking and reads what the booking
-- sends: the shipping snapshot, the address snapshot and the shop origin.
-- name: BookingOrder :one
SELECT o.id, o.order_number, o.status, o.paid_at, o.recipient_name, o.recipient_phone, o.address, o.village,
       o.district, o.city, o.province, o.postal_code, o.latitude, o.longitude,
       o.shipping_provider, o.shipping_courier_code, o.shipping_courier_name, o.shipping_service_code,
       o.shipping_service_name, o.shipping_estimated_days, o.shipping_area_id, o.shipping_origin
FROM orders o
WHERE o.order_number = @order_number
FOR UPDATE;

-- name: BookingItems :many
SELECT oi.product_name, oi.variant_label, oi.sku, oi.unit_price, oi.quantity,
       coalesce(oi.weight_grams, 0)::int AS weight_grams,
       coalesce(p.length_cm, 0)::int AS length_cm, coalesce(p.width_cm, 0)::int AS width_cm,
       coalesce(p.height_cm, 0)::int AS height_cm
FROM order_items oi
LEFT JOIN products p ON p.id = oi.product_id
WHERE oi.order_id = @order_id
ORDER BY oi.id;

-- ActiveShipment is the order's shipment that still counts (at most one).
-- name: ActiveShipment :one
SELECT * FROM shipments
WHERE order_id = @order_id AND status NOT IN ('CANCELLED', 'FAILED')
FOR UPDATE;

-- name: CountShipmentAttempts :one
SELECT count(*)::int FROM shipments WHERE order_id = @order_id AND method = 'PROVIDER';

-- name: CreatePendingShipment :one
INSERT INTO shipments (order_id, method, provider, courier_code, courier_name, service_code, service_name,
                       estimated_days, status, booking_key, booking_started_at, created_by)
VALUES (@order_id, 'PROVIDER', @provider, @courier_code, @courier_name, @service_code, @service_name,
        @estimated_days, 'PENDING', @booking_key, now(), @created_by)
RETURNING id;

-- RenewBookingLease claims a PENDING booking for one more provider call;
-- 0 rows when another request holds a fresh claim.
-- name: RenewBookingLease :execrows
UPDATE shipments SET booking_started_at = now()
WHERE id = @id AND status = 'PENDING' AND booking_started_at < now() - make_interval(secs => @lease_seconds::int);

-- name: MarkShipmentBooked :exec
UPDATE shipments
SET provider_shipment_id = @provider_shipment_id, provider_tracking_id = @provider_tracking_id,
    tracking_number = @tracking_number, tracking_url = @tracking_url, status = @status,
    raw_status = @raw_status, booked_at = now(), last_synced_at = now()
WHERE id = @id AND status = 'PENDING';

-- name: MarkShipmentFailed :exec
UPDATE shipments SET status = 'FAILED', failure_reason = @failure_reason
WHERE id = @id AND status = 'PENDING';

-- name: ShipmentByProviderID :one
SELECT s.id, s.order_id FROM shipments s
WHERE s.provider = @provider AND s.provider_shipment_id = @provider_shipment_id;

-- name: LockShipmentOrder :one
SELECT id, status FROM orders WHERE id = @id FOR UPDATE;

-- name: LockShipment :one
SELECT * FROM shipments WHERE id = @id FOR UPDATE;

-- InsertShipmentEvent stores an event once per (shipment, event_key); a
-- repeat keeps the earliest time and fills a missing description.
-- name: InsertShipmentEvent :exec
INSERT INTO shipment_events (shipment_id, event_key, status, raw_status, description, location, source, event_time)
VALUES (@shipment_id, @event_key, @status, @raw_status, @description, @location, @source, @event_time)
ON CONFLICT (shipment_id, event_key) DO UPDATE
SET event_time  = least(shipment_events.event_time, excluded.event_time),
    description = CASE WHEN shipment_events.description = '' THEN excluded.description ELSE shipment_events.description END;

-- name: ShipmentEvents :many
SELECT status, raw_status, description, location, event_time
FROM shipment_events
WHERE shipment_id = @shipment_id
ORDER BY event_time, id;

-- name: UpdateShipmentTracking :exec
UPDATE shipments
SET status = @status, raw_status = @raw_status, tracking_number = @tracking_number, tracking_url = @tracking_url,
    shipped_at = @shipped_at, delivered_at = @delivered_at, cancelled_at = @cancelled_at,
    last_synced_at = coalesce(@last_synced_at, last_synced_at)
WHERE id = @id;

-- name: ShipOrderByShipment :execrows
UPDATE orders SET status = 'SHIPPED' WHERE id = @id AND status = 'PROCESSING';

-- OpenProviderShipments are provider shipments still moving, least
-- recently synced first (reconcile-shipments).
-- name: OpenProviderShipments :many
SELECT s.id, s.order_id, o.order_number
FROM shipments s
JOIN orders o ON o.id = s.order_id
WHERE s.method = 'PROVIDER' AND s.status IN ('CREATED', 'PICKED_UP', 'IN_TRANSIT', 'OUT_FOR_DELIVERY', 'EXCEPTION')
  AND (s.last_synced_at IS NULL OR s.last_synced_at < @synced_before)
ORDER BY s.last_synced_at NULLS FIRST, s.id
LIMIT @row_limit;

-- name: StalePendingShipments :many
SELECT s.id, o.order_number, s.booking_started_at
FROM shipments s
JOIN orders o ON o.id = s.order_id
WHERE s.status = 'PENDING' AND s.booking_started_at < @started_before
ORDER BY s.id
LIMIT @row_limit;

-- OrderShipments lists an order's shipments, newest first (the admin sees
-- failed and cancelled bookings too).
-- name: OrderShipments :many
SELECT * FROM shipments WHERE order_id = @order_id ORDER BY id DESC;

-- name: ShipmentTimeline :many
SELECT status, description, location, event_time
FROM shipment_events
WHERE shipment_id = @shipment_id
ORDER BY event_time DESC, id DESC
LIMIT 50;

-- name: OrderIDByNumber :one
SELECT id FROM orders WHERE order_number = @order_number;
