package shipping

import (
	"context"
	"net/http"
	"time"
)

// Provider is a shipping aggregator (Biteship first). Implementations map
// their API to the normalized types: ErrUnavailable for network errors,
// timeouts, 5xx and unreadable answers; ErrRejected for refusals. They
// never log or return their API key.
type Provider interface {
	// Name is stored with areas, quotes and shipments ("biteship").
	Name() string
	// SearchAreas finds delivery areas by kecamatan, city or postal code.
	SearchAreas(ctx context.Context, query string) ([]Area, error)
	// Rates lists courier services for a route and package.
	Rates(ctx context.Context, req RateRequest) ([]Rate, error)
	// CreateShipment books a pickup. Booking the same Reference again must
	// return the first shipment (Booked.Existing), never a second one.
	CreateShipment(ctx context.Context, b Booking) (Booked, error)
	// Tracking returns the current status and history of a shipment.
	Tracking(ctx context.Context, shipmentID, trackingID string) (Tracking, error)
	// CancelShipment cancels a booking that was not picked up yet.
	CancelShipment(ctx context.Context, shipmentID, reason string) error
	// ParseWebhook verifies a notification (shared secret or signature)
	// and normalizes it; ErrInvalidSignature or ErrMalformed otherwise.
	ParseWebhook(header http.Header, body []byte) (WebhookEvent, error)
}

// rank orders statuses along the normal journey.
var rank = map[Status]int{
	StatusCreated:        1,
	StatusPickedUp:       2,
	StatusInTransit:      3,
	StatusOutForDelivery: 4,
	StatusDelivered:      5,
	StatusReturned:       6,
	StatusCancelled:      7,
}

// Current derives a shipment's status from all its events, whatever order
// they arrived in (webhooks may be late, repeated or out of order):
//
//   - progress never goes backwards: the furthest status reached wins
//     (a late "picked" after "dropping off" changes nothing);
//   - RETURNED and CANCELLED from the provider are final;
//   - EXCEPTION (on hold, rejected, returning) applies only while it is the
//     newest event before delivery; a later progress event clears it.
//
// fallback is returned when there are no events.
func Current(events []Event, fallback Status) Status {
	if len(events) == 0 {
		return fallback
	}
	best, bestRank := fallback, rank[fallback]
	var lastProgress, lastException time.Time
	for _, e := range events {
		if e.Status == StatusException {
			if e.Time.After(lastException) {
				lastException = e.Time
			}
			continue
		}
		if r := rank[e.Status]; r > bestRank {
			best, bestRank = e.Status, r
		}
		if e.Time.After(lastProgress) {
			lastProgress = e.Time
		}
	}
	if !lastException.IsZero() && bestRank < rank[StatusDelivered] && !lastException.Before(lastProgress) {
		return StatusException
	}
	return best
}
