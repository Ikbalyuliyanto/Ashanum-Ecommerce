package shipping

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"ecommerce/internal/audit"
	"ecommerce/internal/database"
	"ecommerce/internal/database/dbgen"
)

const (
	// bookingLease is how long a PENDING booking is left to the request
	// that sent it before another request may retry it (same reference).
	bookingLease = 30 * time.Second
	// syncInterval: reconcile-shipments skips shipments synced recently.
	syncInterval = 30 * time.Minute
	// stalePending: a booking still PENDING after this is listed for review.
	stalePending = 10 * time.Minute
)

// originJSON is the shop address copied into orders.shipping_origin.
type originJSON struct {
	Name       string   `json:"name"`
	Phone      string   `json:"phone"`
	Address    string   `json:"address"`
	PostalCode string   `json:"postal_code"`
	AreaID     string   `json:"area_id,omitempty"`
	Latitude   *float64 `json:"latitude,omitempty"`
	Longitude  *float64 `json:"longitude,omitempty"`
}

// OriginSnapshot encodes the origin for the order's shipping snapshot.
func OriginSnapshot(o Origin) []byte {
	b, _ := json.Marshal(originJSON{Name: o.Name, Phone: o.Phone, Address: o.Address, PostalCode: o.PostalCode,
		AreaID: o.AreaID, Latitude: o.Latitude, Longitude: o.Longitude})
	return b
}

func originFrom(raw []byte) (Origin, error) {
	var o originJSON
	if err := json.Unmarshal(raw, &o); err != nil {
		return Origin{}, err
	}
	return Origin{Name: o.Name, Phone: o.Phone, Address: o.Address, PostalCode: o.PostalCode, AreaID: o.AreaID,
		Latitude: o.Latitude, Longitude: o.Longitude}, nil
}

// ---------------------------------------------------------------- booking

// Book books, with the provider, the courier service the buyer chose at
// checkout, for a paid order the shop is processing (never at checkout,
// never for an unpaid order). It is idempotent:
//
//  1. in one transaction the order is locked and checked, and a PENDING
//     shipment row with a unique booking key is created (the active-
//     shipment index allows one per order), with the audit entry;
//  2. the provider is called with the booking key as its reference;
//  3. the answer is stored.
//
// A double click finds the PENDING row (ErrBookingPending) or the booked
// one (returned as is): never a second provider shipment. If the provider
// cannot be reached the row stays PENDING and a retry after a short lease
// sends the same reference, which the provider answers with the shipment
// it may already have made. A refusal marks the row FAILED; the order is
// untouched either way (still PROCESSING), so the shop can retry or ship
// manually.
func (s *Service) Book(ctx context.Context, adminID int64, number string) error {
	if s.provider == nil {
		return ErrNotConfigured
	}
	if number == "" || len(number) > 40 {
		return ErrNotFound
	}
	var (
		orderID    int64
		shipmentID int64
		booking    Booking
		done       bool
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		o, err := q.BookingOrder(ctx, number)
		if database.IsNoRows(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if o.Status != "PROCESSING" || o.PaidAt == nil {
			return ErrNotBookable
		}
		orderID = o.ID
		if o.ShippingProvider == nil || *o.ShippingProvider != s.provider.Name() || o.ShippingCourierCode == nil ||
			o.ShippingServiceCode == nil || o.ShippingAreaID == nil {
			return ErrNoOrderShipping
		}
		cur, err := q.ActiveShipment(ctx, o.ID)
		switch {
		case database.IsNoRows(err):
			n, err := q.CountShipmentAttempts(ctx, o.ID)
			if err != nil {
				return err
			}
			key := fmt.Sprintf("%s-S%d", o.OrderNumber, n+1)
			if shipmentID, err = q.CreatePendingShipment(ctx, dbgen.CreatePendingShipmentParams{OrderID: o.ID,
				Provider: &[]string{s.provider.Name()}[0], CourierCode: *o.ShippingCourierCode, CourierName: deref(o.ShippingCourierName),
				ServiceCode: *o.ShippingServiceCode, ServiceName: deref(o.ShippingServiceName),
				EstimatedDays: deref(o.ShippingEstimatedDays), BookingKey: &key, CreatedBy: &adminID}); err != nil {
				return err
			}
			booking.Reference = key
			if err := audit.Record(ctx, q, audit.Entry{AdminID: adminID, Action: "shipment.book", EntityType: audit.EntityOrder,
				EntityID: o.ID, Label: o.OrderNumber, Meta: map[string]any{"provider": s.provider.Name(),
					"courier": *o.ShippingCourierCode, "service": *o.ShippingServiceCode, "reference": key}}); err != nil {
				return err
			}
		case err != nil:
			return err
		case cur.Status == string(StatusPending) && cur.Method == "PROVIDER":
			n, err := q.RenewBookingLease(ctx, dbgen.RenewBookingLeaseParams{ID: cur.ID, LeaseSeconds: int32(bookingLease / time.Second)})
			if err != nil {
				return err
			}
			if n == 0 {
				return ErrBookingPending // another request is talking to the provider
			}
			shipmentID, booking.Reference = cur.ID, deref(cur.BookingKey)
		default:
			done = true // already booked (or shipped manually): nothing to do
			if cur.Method == "MANUAL" {
				return ErrNotBookable
			}
			return nil
		}

		items, err := q.BookingItems(ctx, o.ID)
		if err != nil {
			return err
		}
		for _, it := range items {
			name := it.ProductName
			if it.VariantLabel != "" {
				name += " - " + it.VariantLabel
			}
			booking.Package.Items = append(booking.Package.Items, Item{Name: name, SKU: it.Sku, Value: it.UnitPrice,
				WeightGrams: int(it.WeightGrams), Quantity: int(it.Quantity), LengthCM: int(it.LengthCm),
				WidthCM: int(it.WidthCm), HeightCM: int(it.HeightCm)})
		}
		if !booking.Package.Valid() {
			return ErrNoOrderShipping // order without item weights (placed before Phase 10)
		}
		if booking.Origin, err = originFrom(o.ShippingOrigin); err != nil {
			return ErrNoOrderShipping
		}
		street := strings.Join(nonEmptyParts(o.Address, o.Village, o.District, o.City, o.Province, o.PostalCode), ", ")
		booking.Destination = Contact{Name: o.RecipientName, Phone: o.RecipientPhone, Address: street,
			PostalCode: o.PostalCode, AreaID: *o.ShippingAreaID, Latitude: o.Latitude, Longitude: o.Longitude}
		booking.CourierCode, booking.ServiceCode = *o.ShippingCourierCode, *o.ShippingServiceCode
		booking.Note = "Pesanan " + o.OrderNumber
		return nil
	})
	if err != nil || done {
		return err
	}

	booked, err := s.provider.CreateShipment(ctx, booking)
	switch {
	case errors.Is(err, ErrRejected):
		reason := clip("Ditolak penyedia: "+errorText(err), 300)
		if ferr := s.q.MarkShipmentFailed(ctx, dbgen.MarkShipmentFailedParams{ID: shipmentID, FailureReason: &reason}); ferr != nil {
			return ferr
		}
		s.opts.Log.Warn("shipment booking rejected", "order", number, "reference", booking.Reference, "error", err)
		return err
	case err != nil:
		// Unknown outcome: the row stays PENDING; a retry reuses the reference.
		s.opts.Log.Warn("shipment booking: provider unavailable", "order", number, "reference", booking.Reference,
			"kind", errorKind(err), "error", err)
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return s.storeBooked(ctx, orderID, shipmentID, booked)
}

// storeBooked records the provider's shipment on the PENDING row and its
// first event, in one transaction (orders → shipments lock order).
func (s *Service) storeBooked(ctx context.Context, orderID, shipmentID int64, b Booked) error {
	if b.ShipmentID == "" {
		return fmt.Errorf("%w: booking without shipment id", ErrUnavailable)
	}
	status := b.Status
	if status == "" || status == StatusPending || status == StatusFailed {
		status = StatusCreated
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if _, err := q.LockShipmentOrder(ctx, orderID); err != nil {
			return err
		}
		sh, err := q.LockShipment(ctx, shipmentID)
		if err != nil {
			return err
		}
		if sh.Status != string(StatusPending) {
			return nil // stored by a concurrent retry
		}
		if err := q.MarkShipmentBooked(ctx, dbgen.MarkShipmentBookedParams{ID: shipmentID, ProviderShipmentID: &b.ShipmentID,
			ProviderTrackingID: optional(b.TrackingID), TrackingNumber: b.Waybill, TrackingUrl: optional(b.TrackingURL),
			Status: string(StatusCreated), RawStatus: optional(b.RawStatus)}); err != nil {
			return err
		}
		ev := Event{Status: status, RawStatus: b.RawStatus, Time: s.opts.Now()}
		return s.applyLocked(ctx, q, shipmentID, []Event{ev}, "booking", update{Waybill: b.Waybill, TrackingURL: b.TrackingURL, Synced: true})
	})
}

// ---------------------------------------------------------------- cancel

// CancelBooking cancels the order's provider shipment before pickup (the
// provider is asked first), or abandons a booking that never got an answer
// (PENDING: marked FAILED, nothing to cancel at the provider). The order
// stays PROCESSING: the shop can book again or ship manually.
func (s *Service) CancelBooking(ctx context.Context, adminID int64, number, reason string) error {
	if s.provider == nil {
		return ErrNotConfigured
	}
	reason = strings.Join(strings.Fields(reason), " ")
	if reason == "" {
		reason = "Dibatalkan oleh toko"
	}
	reason = clip(reason, 200)
	orderID, err := s.q.OrderIDByNumber(ctx, number)
	if database.IsNoRows(err) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	cur, err := s.q.ActiveShipment(ctx, orderID) // read only (no transaction): checked again below
	if database.IsNoRows(err) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if cur.Method != "PROVIDER" {
		return ErrNotCancellable
	}
	switch Status(cur.Status) {
	case StatusPending:
	case StatusCreated:
		if err := s.provider.CancelShipment(ctx, deref(cur.ProviderShipmentID), reason); err != nil {
			if errors.Is(err, ErrRejected) {
				return ErrNotCancellable
			}
			return fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
	default:
		return ErrNotCancellable
	}

	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if _, err := q.LockShipmentOrder(ctx, orderID); err != nil {
			return err
		}
		sh, err := q.LockShipment(ctx, cur.ID)
		if err != nil {
			return err
		}
		switch {
		case Status(sh.Status) == StatusPending && Status(cur.Status) == StatusPending:
			r := clip("Dibatalkan admin sebelum penyedia menjawab: "+reason, 300)
			if err := q.MarkShipmentFailed(ctx, dbgen.MarkShipmentFailedParams{ID: sh.ID, FailureReason: &r}); err != nil {
				return err
			}
		case Status(sh.Status) == StatusCreated || Status(cur.Status) == StatusCreated:
			ev := Event{Status: StatusCancelled, RawStatus: "cancelled", Description: "Dibatalkan oleh toko: " + reason, Time: s.opts.Now()}
			if err := s.applyLocked(ctx, q, sh.ID, []Event{ev}, "admin", update{}); err != nil {
				return err
			}
		default:
			return ErrNotCancellable
		}
		return audit.Record(ctx, q, audit.Entry{AdminID: adminID, Action: "shipment.cancel", EntityType: audit.EntityOrder,
			EntityID: orderID, Label: number, Meta: map[string]any{"from": sh.Status, "reason": reason,
				"provider_shipment": deref(sh.ProviderShipmentID)}})
	})
}

// ---------------------------------------------------------------- tracking

// Sync refreshes the order's provider shipment from the provider (admin
// "Perbarui status"). Nothing changes when the provider is down.
func (s *Service) Sync(ctx context.Context, adminID int64, number string) error {
	if s.provider == nil {
		return ErrNotConfigured
	}
	orderID, err := s.q.OrderIDByNumber(ctx, number)
	if database.IsNoRows(err) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	cur, err := s.q.ActiveShipment(ctx, orderID)
	if database.IsNoRows(err) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.sync(ctx, cur)
}

func (s *Service) sync(ctx context.Context, cur dbgen.Shipment) error {
	if cur.Method != "PROVIDER" || cur.ProviderShipmentID == nil {
		return ErrNotFound
	}
	t, err := s.provider.Tracking(ctx, *cur.ProviderShipmentID, deref(cur.ProviderTrackingID))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	events := t.Events
	if len(events) == 0 && t.Status != "" {
		events = []Event{{Status: t.Status, RawStatus: t.RawStatus, Time: s.opts.Now()}}
	}
	return s.apply(ctx, cur.ID, cur.OrderID, events, "tracking", update{Waybill: t.Waybill, TrackingURL: t.TrackingURL, Synced: true})
}

// WebhookResult says what a notification did (for the log and tests).
type WebhookResult string

const (
	WebhookApplied WebhookResult = "applied"
	WebhookIgnored WebhookResult = "ignored" // ping, unknown shipment or event
)

// HandleWebhook verifies and applies a provider notification. Only the
// provider adapter decides authenticity (shared secret/signature); the
// event is stored once however often it is delivered.
func (s *Service) HandleWebhook(ctx context.Context, header http.Header, body []byte) (WebhookResult, error) {
	if s.provider == nil {
		return "", ErrNotConfigured
	}
	ev, err := s.provider.ParseWebhook(header, body)
	if err != nil {
		return "", err
	}
	if ev.Kind == WebhookPing || ev.Kind == WebhookOther {
		return WebhookIgnored, nil
	}
	sh, err := s.q.ShipmentByProviderID(ctx, dbgen.ShipmentByProviderIDParams{Provider: &[]string{s.provider.Name()}[0],
		ProviderShipmentID: &ev.ShipmentID})
	if database.IsNoRows(err) {
		s.opts.Log.Warn("shipping webhook for an unknown shipment", "provider", s.provider.Name(), "shipment", ev.ShipmentID)
		return WebhookIgnored, nil
	}
	if err != nil {
		return "", err
	}
	var events []Event
	if ev.Kind == WebhookStatus {
		if ev.Status == "" {
			s.opts.Log.Warn("shipping webhook with an unknown status", "shipment", ev.ShipmentID, "status", ev.RawStatus)
			return WebhookIgnored, nil
		}
		events = []Event{{Status: ev.Status, RawStatus: ev.RawStatus, Time: s.opts.Now()}}
	}
	if err := s.apply(ctx, sh.ID, sh.OrderID, events, "webhook", update{Waybill: ev.Waybill, TrackingURL: ev.TrackingURL}); err != nil {
		return "", err
	}
	return WebhookApplied, nil
}

// update carries what besides events a provider answer may change.
type update struct {
	Waybill     string
	TrackingURL string
	Synced      bool // a full read from the provider (sets last_synced_at)
}

// apply stores events of a shipment in one transaction: orders → shipments.
func (s *Service) apply(ctx context.Context, shipmentID, orderID int64, events []Event, source string, u update) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if _, err := q.LockShipmentOrder(ctx, orderID); err != nil {
			return err
		}
		if _, err := q.LockShipment(ctx, shipmentID); err != nil {
			return err
		}
		return s.applyLocked(ctx, q, shipmentID, events, source, u)
	})
}

// applyLocked inserts the events (once per event key, earliest time kept),
// derives the shipment status from all its events (Current: late or
// repeated events never move it backwards), and moves the order
// PROCESSING → SHIPPED once the parcel is picked up. DELIVERED does not
// complete the order: the shop (or a later policy) completes it, which is
// when the return window starts. The caller holds the order and shipment
// locks.
func (s *Service) applyLocked(ctx context.Context, q *dbgen.Queries, shipmentID int64, events []Event, source string, u update) error {
	sh, err := q.LockShipment(ctx, shipmentID)
	if err != nil {
		return err
	}
	cur := Status(sh.Status)
	if cur == StatusPending || cur == StatusFailed || sh.Method != "PROVIDER" {
		return nil // not booked: nothing to track
	}
	for _, e := range events {
		key := "status:" + e.RawStatus
		if e.RawStatus == "" {
			key = "status:" + strings.ToLower(string(e.Status))
		}
		if err := q.InsertShipmentEvent(ctx, dbgen.InsertShipmentEventParams{ShipmentID: shipmentID, EventKey: clip(key, 120),
			Status: string(e.Status), RawStatus: clip(e.RawStatus, 40), Description: clip(e.Description, 500),
			Location: clip(e.Location, 200), Source: source, EventTime: e.Time}); err != nil {
			return err
		}
	}
	rows, err := q.ShipmentEvents(ctx, shipmentID)
	if err != nil {
		return err
	}
	all := make([]Event, len(rows))
	for i, r := range rows {
		all[i] = Event{Status: Status(r.Status), RawStatus: r.RawStatus, Time: r.EventTime}
	}
	next := Current(all, StatusCreated)
	if cur == StatusCancelled {
		next = StatusCancelled // final once cancelled
	}
	raw := sh.RawStatus
	for _, e := range all { // oldest first: the last match is the newest
		if e.Status == next && e.RawStatus != "" {
			raw = optional(e.RawStatus)
		}
	}

	p := dbgen.UpdateShipmentTrackingParams{ID: shipmentID, Status: string(next), RawStatus: raw,
		TrackingNumber: sh.TrackingNumber, TrackingUrl: sh.TrackingUrl, ShippedAt: sh.ShippedAt,
		DeliveredAt: sh.DeliveredAt, CancelledAt: sh.CancelledAt}
	if u.Waybill != "" {
		p.TrackingNumber = clip(u.Waybill, 60)
	}
	if u.TrackingURL != "" {
		p.TrackingUrl = &u.TrackingURL
	}
	if u.Synced {
		now := s.opts.Now()
		p.LastSyncedAt = &now
	}
	if next.Moving() && p.ShippedAt == nil {
		p.ShippedAt = firstTime(all, func(st Status) bool { return st.Moving() })
	}
	if next == StatusDelivered && p.DeliveredAt == nil {
		p.DeliveredAt = firstTime(all, func(st Status) bool { return st == StatusDelivered })
	}
	if next == StatusCancelled && p.CancelledAt == nil {
		p.CancelledAt = firstTime(all, func(st Status) bool { return st == StatusCancelled })
		if p.CancelledAt == nil {
			now := s.opts.Now()
			p.CancelledAt = &now
		}
	}
	if err := q.UpdateShipmentTracking(ctx, p); err != nil {
		return err
	}
	if next.Moving() {
		if _, err := q.ShipOrderByShipment(ctx, sh.OrderID); err != nil {
			return err
		}
	}
	return nil
}

func firstTime(events []Event, match func(Status) bool) *time.Time {
	for _, e := range events { // oldest first
		if match(e.Status) {
			t := e.Time
			return &t
		}
	}
	return nil
}

// ---------------------------------------------------------------- reconcile

// Report is the result of one reconcile-shipments pass.
type Report struct {
	Checked       int
	Errors        int
	StalePending  []string // order numbers with a booking still unanswered
	QuotesDeleted int64
}

// Reconcile refreshes provider shipments still moving that were not synced
// for a while (oldest first, at most limit), lists bookings stuck in
// PENDING and deletes stale unused quotes. Meant for a systemd timer or a
// manual run; there is no resident worker.
func (s *Service) Reconcile(ctx context.Context, limit int) (Report, error) {
	var rep Report
	if s.provider == nil {
		return rep, ErrNotConfigured
	}
	before := s.opts.Now().Add(-syncInterval)
	open, err := s.q.OpenProviderShipments(ctx, dbgen.OpenProviderShipmentsParams{SyncedBefore: &before, RowLimit: int32(limit)})
	if err != nil {
		return rep, err
	}
	for _, o := range open {
		sh, err := s.q.LockShipment(ctx, o.ID) // outside a transaction: a plain read
		if err == nil {
			err = s.sync(ctx, sh)
		}
		rep.Checked++
		if err != nil {
			rep.Errors++
			s.opts.Log.Warn("reconcile shipment", "order", o.OrderNumber, "kind", errorKind(err), "error", err)
		}
	}
	started := s.opts.Now().Add(-stalePending)
	stale, err := s.q.StalePendingShipments(ctx, dbgen.StalePendingShipmentsParams{StartedBefore: &started, RowLimit: int32(limit)})
	if err != nil {
		return rep, err
	}
	for _, p := range stale {
		rep.StalePending = append(rep.StalePending, p.OrderNumber)
	}
	if rep.QuotesDeleted, err = s.CleanupQuotes(ctx); err != nil {
		return rep, err
	}
	return rep, nil
}

// ---------------------------------------------------------------- reading

// FromRow converts a stored shipment.
func FromRow(r dbgen.Shipment) Shipment {
	return Shipment{ID: r.ID, Method: r.Method, Provider: deref(r.Provider), Courier: r.CourierName, CourierCode: r.CourierCode,
		Service: r.ServiceName, ServiceCode: r.ServiceCode, Tracking: r.TrackingNumber, TrackingURL: deref(r.TrackingUrl),
		Status: Status(r.Status), EstimatedDays: r.EstimatedDays, Note: r.Note, FailureReason: deref(r.FailureReason),
		ShippedAt: r.ShippedAt, BookedAt: r.BookedAt, DeliveredAt: r.DeliveredAt, CancelledAt: r.CancelledAt,
		LastSyncedAt: r.LastSyncedAt, CreatedAt: r.CreatedAt}
}

// Timeline converts stored events (newest first).
func Timeline(rows []dbgen.ShipmentTimelineRow) []TimelineEvent {
	out := make([]TimelineEvent, len(rows))
	for i, r := range rows {
		out[i] = TimelineEvent{Status: Status(r.Status), Description: r.Description, Location: r.Location, Time: r.EventTime}
	}
	return out
}

// History returns every shipment of an order (newest first) with the
// active one's tracking events: the admin order page.
func (s *Service) History(ctx context.Context, orderID int64) ([]Shipment, error) {
	rows, err := s.q.OrderShipments(ctx, orderID)
	if err != nil {
		return nil, err
	}
	out := make([]Shipment, len(rows))
	for i, r := range rows {
		out[i] = FromRow(r)
		if out[i].Status.Active() && !out[i].Manual() {
			ev, err := s.q.ShipmentTimeline(ctx, r.ID)
			if err != nil {
				return nil, err
			}
			out[i].Events = Timeline(ev)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- helpers

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nonEmptyParts(parts ...string) []string {
	out := parts[:0:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// errorText is the provider's message without wrapping noise.
func errorText(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		return msg[i+2:]
	}
	return msg
}
