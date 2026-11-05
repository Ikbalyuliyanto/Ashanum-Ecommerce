// Package shipping computes shipping rates and manages shipments through a
// shipping provider, behind one provider-neutral Service:
//
//	checkout / orders / admin / API  →  shipping.Service  →  shipping.Provider  →  Biteship (or another)
//
// Domain types here never carry raw provider JSON. Checkout asks the Service
// for quotes (rates stored server-side with an id and an expiry); an order
// accepts only a quote id and copies the quote into its shipping snapshot,
// so the client never sends a shipping cost. Shipments are booked only for
// paid orders the shop is processing, idempotently; tracking comes from the
// provider's webhook, an admin refresh or the reconcile-shipments command,
// never from a page load. The Service has no HTTP dependency except the
// webhook handler in handler.go.
package shipping

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrNotConfigured: no shipping provider (no API key).
	ErrNotConfigured = errors.New("shipping: provider not configured")
	// ErrUnavailable: the provider could not be reached or answered with a
	// server error or something unreadable; trying again later may work.
	ErrUnavailable = errors.New("shipping: provider unavailable")
	// ErrRejected: the provider refused the request (4xx).
	ErrRejected = errors.New("shipping: provider rejected the request")
	// ErrNoCourier: no courier serves the route.
	ErrNoCourier = errors.New("shipping: no courier available")
	// ErrInvalidSignature: a webhook without the shared secret.
	ErrInvalidSignature = errors.New("shipping: invalid webhook signature")
	// ErrMalformed: a webhook body that is not the expected JSON.
	ErrMalformed = errors.New("shipping: malformed webhook")

	ErrAreaNotFound    = errors.New("shipping: area not found")
	ErrInvalidQuery    = errors.New("shipping: invalid area query")
	ErrQuoteChanged    = errors.New("shipping: quote expired or no longer matches")
	ErrPackageInvalid  = errors.New("shipping: package weight missing")
	ErrNotFound        = errors.New("shipping: shipment not found")
	ErrNotBookable     = errors.New("shipping: order cannot be booked now")
	ErrBookingPending  = errors.New("shipping: booking in progress")
	ErrNotCancellable  = errors.New("shipping: shipment can no longer be cancelled")
	ErrNoOrderShipping = errors.New("shipping: order has no shipping choice")
)

// Message is the customer/admin-facing (Indonesian) text for a known error,
// or "" for unexpected errors.
func Message(err error) string {
	switch {
	case errors.Is(err, ErrUnavailable), errors.Is(err, ErrNotConfigured):
		return "Pengiriman sementara tidak dapat dihitung. Silakan coba lagi."
	case errors.Is(err, ErrNoCourier):
		return "Belum ada kurir yang melayani alamat ini."
	case errors.Is(err, ErrQuoteChanged):
		return "Ongkos kirim sudah berubah atau kedaluwarsa. Periksa pilihan pengiriman lalu buat pesanan kembali."
	case errors.Is(err, ErrPackageInvalid):
		return "Ongkos kirim belum bisa dihitung untuk produk ini. Hubungi penjual."
	case errors.Is(err, ErrAreaNotFound):
		return "Lokasi pengiriman tidak dikenal. Cari dan pilih ulang kecamatan atau kode pos."
	case errors.Is(err, ErrInvalidQuery):
		return "Ketik minimal 3 huruf nama kecamatan, kota, atau kode pos."
	case errors.Is(err, ErrRejected):
		return "Penyedia pengiriman menolak permintaan ini. Periksa data alamat lalu coba lagi."
	case errors.Is(err, ErrNotBookable):
		return "Pengiriman hanya bisa dibuat untuk pesanan yang sudah dibayar dan sedang diproses, tanpa pengiriman aktif."
	case errors.Is(err, ErrBookingPending):
		return "Pengiriman sedang dibuat di penyedia. Tunggu sebentar lalu muat ulang halaman."
	case errors.Is(err, ErrNotCancellable):
		return "Pengiriman yang sudah dijemput kurir tidak bisa dibatalkan."
	case errors.Is(err, ErrNoOrderShipping):
		return "Pesanan ini tidak punya pilihan kurir dari checkout. Gunakan pengiriman manual."
	case errors.Is(err, ErrNotFound):
		return "Pengiriman tidak ditemukan."
	}
	return ""
}

// Area is a delivery area (kecamatan + kode pos level) of a provider.
type Area struct {
	Provider   string
	ID         string // provider area id
	Label      string // "Coblong, Bandung, Jawa Barat. 40132"
	Province   string
	City       string
	District   string
	PostalCode string
	Latitude   *float64
	Longitude  *float64
}

// Location is one end of a route. Providers use the area when given,
// otherwise the postal code.
type Location struct {
	AreaID     string
	PostalCode string
	Latitude   *float64
	Longitude  *float64
}

// key identifies the location for rates: a quote is valid only for the
// same key.
func (l Location) key() string {
	k := l.AreaID + "|" + l.PostalCode
	if l.Latitude != nil && l.Longitude != nil {
		k += fmt.Sprintf("|%.4f,%.4f", *l.Latitude, *l.Longitude)
	}
	return k
}

// Origin is the shop's address (SHIP_FROM_*): where parcels are picked up.
type Origin struct {
	Name       string
	Phone      string
	Address    string
	PostalCode string
	AreaID     string
	Latitude   *float64
	Longitude  *float64
}

func (o Origin) Location() Location {
	return Location{AreaID: o.AreaID, PostalCode: o.PostalCode, Latitude: o.Latitude, Longitude: o.Longitude}
}

// Item is one order line in a package. Weight comes from the catalog
// (variant override or product default), never from the client.
type Item struct {
	VariantID   int64
	Name        string
	SKU         string
	Value       int64 // unit price, rupiah
	WeightGrams int   // per unit
	Quantity    int
	LengthCM    int // 0 = unknown
	WidthCM     int
	HeightCM    int
}

// Package is what is shipped: the items of one checkout.
type Package struct {
	Items []Item
}

// WeightGrams is the total weight.
func (p Package) WeightGrams() int {
	n := 0
	for _, it := range p.Items {
		n += it.WeightGrams * it.Quantity
	}
	return n
}

// Valid reports whether every item has a weight and a quantity.
func (p Package) Valid() bool {
	if len(p.Items) == 0 {
		return false
	}
	for _, it := range p.Items {
		if it.WeightGrams <= 0 || it.Quantity <= 0 {
			return false
		}
	}
	return true
}

// RateRequest asks a provider for rates. Everything in it is computed by the
// server from the database.
type RateRequest struct {
	Origin      Location
	Destination Location
	Package     Package
	Couriers    []string // courier codes, e.g. jne, sicepat
}

// Rate is one courier service offered by a provider, normalized.
type Rate struct {
	Provider      string
	CourierCode   string // "jne"
	CourierName   string // "JNE"
	ServiceCode   string // "reg"
	ServiceName   string // "Reguler"
	Description   string
	Price         int64  // rupiah
	EstimatedDays string // "2-3", "1"; "0" same day; "" unknown
}

// Key identifies the courier service within a provider.
func (r Rate) Key() string { return r.CourierCode + ":" + r.ServiceCode }

// EstimateLabel is the estimate for people: "2–3 hari".
func (r Rate) EstimateLabel() string { return EstimateLabel(r.EstimatedDays) }

// EstimateLabel formats an estimated_days value.
func EstimateLabel(days string) string {
	switch days {
	case "":
		return ""
	case "0":
		return "Hari ini"
	}
	return strings.Replace(days, "-", "–", 1) + " hari"
}

// Quote is a rate offered to one buyer for one route and package, stored
// with an id and an expiry. An order accepts only a quote id.
type Quote struct {
	ID        string
	Rate      Rate
	ExpiresAt time.Time
}

// Status is the internal shipment status; provider statuses are mapped to
// it and never become business state themselves.
type Status string

const (
	StatusPending        Status = "PENDING" // booking sent, answer not stored
	StatusFailed         Status = "FAILED"  // provider refused the booking
	StatusCreated        Status = "CREATED"
	StatusPickedUp       Status = "PICKED_UP"
	StatusInTransit      Status = "IN_TRANSIT"
	StatusOutForDelivery Status = "OUT_FOR_DELIVERY"
	StatusDelivered      Status = "DELIVERED"
	StatusException      Status = "EXCEPTION"
	StatusReturned       Status = "RETURNED"
	StatusCancelled      Status = "CANCELLED"
)

// Label is the Indonesian status text.
func (s Status) Label() string {
	switch s {
	case StatusPending:
		return "Sedang Dibuat"
	case StatusFailed:
		return "Gagal Dibuat"
	case StatusCreated:
		return "Menunggu Penjemputan"
	case StatusPickedUp:
		return "Dijemput Kurir"
	case StatusInTransit:
		return "Dalam Perjalanan"
	case StatusOutForDelivery:
		return "Sedang Diantar"
	case StatusDelivered:
		return "Terkirim"
	case StatusException:
		return "Ada Kendala"
	case StatusReturned:
		return "Dikembalikan"
	case StatusCancelled:
		return "Dibatalkan"
	}
	return string(s)
}

// Moving reports whether the parcel has left the shop: the order then
// counts as shipped.
func (s Status) Moving() bool {
	switch s {
	case StatusPickedUp, StatusInTransit, StatusOutForDelivery, StatusDelivered:
		return true
	}
	return false
}

// Active reports whether the shipment still counts for its order.
func (s Status) Active() bool { return s != StatusCancelled && s != StatusFailed }

// Booking is what is sent to book a shipment.
type Booking struct {
	Reference   string // our booking key, unique per attempt chain
	Origin      Origin
	Destination Contact
	CourierCode string
	ServiceCode string
	Package     Package
	Note        string
}

// Contact is the recipient.
type Contact struct {
	Name       string
	Phone      string
	Address    string
	PostalCode string
	AreaID     string
	Latitude   *float64
	Longitude  *float64
}

// Booked is the provider's answer to a booking.
type Booked struct {
	ShipmentID  string // provider's id of the shipment/order
	TrackingID  string // provider's tracking id (may equal ShipmentID)
	Waybill     string // courier tracking number (resi); may come later
	Status      Status
	RawStatus   string
	TrackingURL string
	Existing    bool // the reference was booked before (a retry)
}

// Event is one tracking update, normalized.
type Event struct {
	Status      Status
	RawStatus   string
	Description string
	Location    string
	Time        time.Time
}

// Tracking is a shipment's state at the provider.
type Tracking struct {
	Status      Status
	RawStatus   string
	Waybill     string
	TrackingURL string
	Events      []Event
}

// WebhookKind is what a provider notification is about.
type WebhookKind string

const (
	WebhookStatus  WebhookKind = "status"  // shipment status changed
	WebhookWaybill WebhookKind = "waybill" // courier tracking number assigned
	WebhookOther   WebhookKind = "other"   // e.g. price update: ignored
	WebhookPing    WebhookKind = "ping"    // empty test request
)

// WebhookEvent is a verified provider notification, normalized.
type WebhookEvent struct {
	Kind        WebhookKind
	ShipmentID  string // provider shipment id
	Waybill     string
	TrackingURL string
	Status      Status // "" when the provider status is unknown
	RawStatus   string
}

// Shipment is a shipment of an order as shown to the buyer and the admin.
type Shipment struct {
	ID            int64
	Method        string // MANUAL | PROVIDER
	Provider      string
	Courier       string
	CourierCode   string
	Service       string
	ServiceCode   string
	Tracking      string // resi
	TrackingURL   string
	Status        Status
	EstimatedDays string
	Note          string
	FailureReason string
	ShippedAt     *time.Time
	BookedAt      *time.Time
	DeliveredAt   *time.Time
	CancelledAt   *time.Time
	LastSyncedAt  *time.Time
	CreatedAt     time.Time
	Events        []TimelineEvent // newest first
}

// ProviderLabel is the provider's display name ("Biteship").
func ProviderLabel(name string) string {
	switch name {
	case "biteship":
		return "Biteship"
	}
	return name
}

// ProviderLabel is the display name of the shipment's provider.
func (s Shipment) ProviderLabel() string { return ProviderLabel(s.Provider) }

// Manual reports a shipment the shop entered by hand (no live tracking).
func (s Shipment) Manual() bool { return s.Method == "MANUAL" }

// EstimateLabel is the delivery estimate for people.
func (s Shipment) EstimateLabel() string { return EstimateLabel(s.EstimatedDays) }

// CanCancel reports whether the provider booking may still be cancelled
// (before pickup) or abandoned (no answer stored).
func (s Shipment) CanCancel() bool {
	return !s.Manual() && (s.Status == StatusCreated || s.Status == StatusPending)
}

// CanSync reports whether a tracking refresh makes sense.
func (s Shipment) CanSync() bool {
	switch s.Status {
	case StatusCreated, StatusPickedUp, StatusInTransit, StatusOutForDelivery, StatusException:
		return !s.Manual()
	}
	return false
}

// TimelineEvent is one stored tracking event.
type TimelineEvent struct {
	Status      Status
	Description string
	Location    string
	Time        time.Time
}

// Text is what the timeline shows: the provider's description, or the
// status label.
func (e TimelineEvent) Text() string {
	if e.Description != "" {
		return e.Description
	}
	return e.Status.Label()
}
