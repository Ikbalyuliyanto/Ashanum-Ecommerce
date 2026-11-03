package payment

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Order statuses this package reads and writes (see package order).
const (
	orderWaitingPayment      = "WAITING_PAYMENT"
	orderWaitingConfirmation = "WAITING_CONFIRMATION"
	orderCancelled           = "CANCELLED"

	reasonUserCancelled  = "USER_CANCELLED"
	reasonPaymentExpired = "PAYMENT_EXPIRED"
)

const (
	// MaxAttempts is the number of payment attempts one order may have.
	MaxAttempts = 10
	// minPayWindow is the least time before the deadline a new attempt needs.
	minPayWindow = time.Minute
	// expiryGrace: after the deadline, an order whose attempt may still be
	// open at the provider is left alone this long, so a payment made in the
	// last moment arrives before the order is cancelled.
	expiryGrace = 5 * time.Minute
	// stalePending: reconcile asks the provider about pending attempts
	// older than this (a notification may have been missed).
	stalePending = 15 * time.Minute
)

// Event sources, recorded in payment_events.
const (
	sourceUser        = "user"
	sourceWebhook     = "webhook"
	sourceStatusCheck = "status_check"
	sourceReconcile   = "reconcile"
	sourceSystem      = "system"
)

// decision is what applying a provider status to an attempt does.
type decision int

const (
	keep   decision = iota // same status again (duplicate notification)
	move                   // PENDING → new status
	ignore                 // out of order or stale: nothing changes
	review                 // needs a person: flagged, nothing changes
)

// decide is the payment status transition policy:
//
//	PENDING → PAID | FAILED | DENIED | CANCELLED | EXPIRED
//
// Terminal statuses are final. A payment reported for an attempt that is
// already closed (e.g. paid after the buyer cancelled) and provider
// statuses without a domain meaning (refund, chargeback) are never applied
// automatically: they are flagged for manual reconciliation. Anything else
// out of order (PAID → EXPIRED, EXPIRED → PENDING, ...) is ignored.
func decide(current, incoming Status) decision {
	switch {
	case incoming == "":
		return review
	case incoming == current:
		return keep
	case current == StatusPending:
		return move
	case incoming == StatusPaid:
		return review
	}
	return ignore
}

// ProviderOrderID is the transaction ID sent to the provider for attempt n
// of an order, e.g. ASH-20261103-000123-P2. It is unique per attempt (and
// enforced unique in the database), deterministic, and well under the
// 50-character limit of Midtrans.
func ProviderOrderID(orderNumber string, n int) string {
	return fmt.Sprintf("%s-P%d", orderNumber, n)
}

// attemptWindow returns when a new attempt starts and expires: it ends at
// the order's payment deadline, rounded down to whole minutes (the unit the
// provider expiry uses).
func attemptWindow(now, due time.Time) (start, expires time.Time, ok bool) {
	start = now.Truncate(time.Second)
	minutes := due.Sub(start) / time.Minute
	if minutes*time.Minute < minPayWindow {
		return start, start, false
	}
	return start, start.Add(minutes * time.Minute), true
}

// paidMismatch explains why a provider-confirmed payment must not mark the
// order paid automatically, or returns "".
func paidMismatch(o orderRow, a Attempt, st ProviderStatus, otherPaid bool) string {
	switch {
	case st.Amount != a.Amount || a.Amount != o.Total:
		return fmt.Sprintf("amount mismatch: provider %d, attempt %d, order %d", st.Amount, a.Amount, o.Total)
	case o.Status != orderWaitingPayment || o.StockRestored:
		return "paid while order is " + strings.ToLower(o.Status) + ": refund or reinstate manually"
	case otherPaid:
		return "order already paid by another attempt: refund manually"
	}
	return ""
}

// keyedMutex serializes work on one order inside this process (the app is a
// single binary), so two quick "Bayar Sekarang" clicks wait for each other
// instead of opening two provider transactions. The database constraints
// remain the final guard across processes.
type keyedMutex struct {
	mu sync.Mutex
	m  map[int64]*keyedEntry
}

type keyedEntry struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedMutex) lock(key int64) (unlock func()) {
	k.mu.Lock()
	if k.m == nil {
		k.m = make(map[int64]*keyedEntry)
	}
	e := k.m[key]
	if e == nil {
		e = &keyedEntry{}
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		if e.refs--; e.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}
