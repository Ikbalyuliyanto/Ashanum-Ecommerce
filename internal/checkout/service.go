package checkout

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	"ecommerce/internal/address"
	"ecommerce/internal/shipping"
)

const maxKeyLength = 64

type Service struct {
	repo      *Repository
	addresses *address.Service
	shipping  *shipping.Service
}

// NewService builds the checkout. shipper computes shipping quotes; orders
// cannot be placed without it (there is no shipping-cost-0 fallback).
func NewService(repo *Repository, addresses *address.Service, shipper *shipping.Service) *Service {
	return &Service{repo: repo, addresses: addresses, shipping: shipper}
}

// Prepare builds the checkout page from current data, without locking.
// addressID 0 picks the primary address. Lines that cannot be ordered are
// returned with a Problem so the page can warn. Shipping quotes are
// computed for the address (rates come from the provider or the short rate
// cache); quoteID selects one of them, otherwise the cheapest is selected.
// A provider failure is not an error: the draft says shipping is
// unavailable and cannot be ordered.
func (s *Service) Prepare(ctx context.Context, userID int64, src Source, addressID int64, quoteID string) (Draft, error) {
	if !src.valid() {
		return Draft{}, ErrNothingToCheckout
	}
	lines, err := s.repo.Lines(ctx, userID, src)
	if err != nil {
		return Draft{}, err
	}
	if len(lines) == 0 {
		return Draft{}, ErrNothingToCheckout
	}
	addrs, err := s.addresses.List(ctx, userID)
	if err != nil {
		return Draft{}, err
	}

	d := Draft{Source: src, Lines: lines, Addresses: addrs}
	for i := range addrs { // primary first, so addrs[0] is the default
		if addrs[i].ID == addressID || (addressID == 0 && i == 0) {
			d.Address = &addrs[i]
			break
		}
	}
	if d.Address == nil && len(addrs) > 0 {
		d.Address = &addrs[0]
	}
	for _, l := range lines {
		d.Subtotal += l.Subtotal()
	}
	if err := s.quote(ctx, userID, &d, quoteID); err != nil {
		return Draft{}, err
	}
	if d.Shipping.Selected != nil {
		d.ShippingCost = d.Shipping.Selected.Rate.Price
	}
	d.Total = d.Subtotal + d.ShippingCost - d.Discount
	return d, nil
}

// quote fills d.Shipping. Only database errors are returned.
func (s *Service) quote(ctx context.Context, userID int64, d *Draft, quoteID string) error {
	switch {
	case s.shipping == nil || !s.shipping.Enabled():
		d.Shipping.State = ShippingNotConfigured
		return nil
	case d.Address == nil:
		d.Shipping.State = ShippingNeedAddress
		return nil
	case !d.Address.HasShippingArea():
		d.Shipping.State = ShippingNeedArea
		return nil
	case d.Blocked():
		d.Shipping.State = ShippingBlocked
		return nil
	}
	quotes, err := s.shipping.Quotes(ctx, userID, destinationOf(*d.Address), packageOf(d.Lines))
	switch {
	case errors.Is(err, shipping.ErrNoCourier):
		d.Shipping.State = ShippingNoCourier
		return nil
	case errors.Is(err, shipping.ErrAreaNotFound):
		d.Shipping.State = ShippingNeedArea // an area of another provider
		return nil
	case errors.Is(err, shipping.ErrPackageInvalid):
		d.Shipping.State = ShippingBlocked
		return nil
	case errors.Is(err, shipping.ErrUnavailable), errors.Is(err, shipping.ErrRejected), errors.Is(err, shipping.ErrNotConfigured):
		d.Shipping.State = ShippingUnavailable
		return nil
	case err != nil:
		return err
	case len(quotes) == 0:
		d.Shipping.State = ShippingNoCourier
		return nil
	}
	d.Shipping.State, d.Shipping.Options = ShippingReady, quotes
	for i := range quotes {
		if quotes[i].ID == quoteID {
			d.Shipping.Selected = &quotes[i]
		}
	}
	if d.Shipping.Selected == nil {
		d.Shipping.Selected = &quotes[0] // cheapest
		d.Shipping.Changed = quoteID != ""
	}
	return nil
}

// PlaceOrder creates an order in one transaction:
//
//  1. lock the selected cart lines (Buy Now: no cart involved),
//  2. lock their variants in id order and re-validate availability, stock
//     and quantity against current data,
//  3. copy the user's address and lock the chosen shipping quote: it must
//     be the user's, unused, unexpired and for this destination, origin
//     and package (else shipping.ErrQuoteChanged),
//  4. compare the total (items + shipping) with what the customer saw,
//  5. insert order, items and the shipping snapshot, decrement stock with
//     "stock >= qty" guards, delete only the purchased cart lines and mark
//     the quote used.
//
// Any failure rolls everything back. A repeated submission with the same
// idempotency key returns the first order (Placed.Existing).
func (s *Service) PlaceOrder(ctx context.Context, userID int64, req PlaceRequest) (Placed, error) {
	if !req.Source.valid() {
		return Placed{}, ErrNothingToCheckout
	}
	if len(req.IdempotencyKey) > maxKeyLength {
		req.IdempotencyKey = ""
	}
	if p, ok, err := s.existing(ctx, userID, req.IdempotencyKey); ok || err != nil {
		return p, err
	}

	var placed Placed
	err := retryDeadlock(func() error {
		return s.repo.InTx(ctx, func(tx *Repository) error {
			lines, err := tx.Lines(ctx, userID, req.Source)
			if err != nil {
				return err
			}
			if len(lines) == 0 {
				return ErrNothingToCheckout
			}
			var subtotal int64
			for _, l := range lines {
				switch l.Problem {
				case ProblemNone:
				case ProblemUnavailable:
					return ErrUnavailable
				case ProblemNoWeight:
					return ErrNotShippable
				default:
					return ErrStockChanged
				}
				subtotal += l.Subtotal()
			}
			if req.AddressID == 0 {
				return ErrAddressRequired
			}
			addr, err := tx.Address(ctx, userID, req.AddressID)
			if err != nil {
				return err
			}
			if s.shipping == nil {
				return shipping.ErrNotConfigured
			}
			if addr.ShippingAreaID == nil {
				return ErrAreaRequired
			}
			if req.ShippingQuoteID == "" {
				return ErrShippingRequired
			}
			dest := shipping.Destination{Provider: deref(addr.ShippingAreaProvider), AreaID: *addr.ShippingAreaID,
				Postal: addr.PostalCode, Lat: addr.Latitude, Lng: addr.Longitude}
			sq, err := s.shipping.UseQuote(ctx, tx.q, userID, req.ShippingQuoteID, dest, packageOf(lines))
			if err != nil {
				return err
			}
			total := subtotal + sq.Rate.Price
			if req.ExpectedTotal != 0 && req.ExpectedTotal != total {
				return ErrPriceChanged
			}
			c, err := tx.Create(ctx, userID, req.IdempotencyKey, addr, lines, subtotal, sq)
			if err != nil {
				return err
			}
			if c.StockUpdated != len(lines) {
				return ErrStockChanged // a stock guard failed: roll back
			}
			if !c.QuoteUsed {
				return shipping.ErrQuoteChanged // used meanwhile: roll back
			}
			placed = Placed{OrderNumber: c.Number, Total: total}
			return nil
		})
	})

	// The same submission may have just been placed by a concurrent request
	// (its cart lines are gone, or its key is taken): return that order.
	if errors.Is(err, errDuplicateKey) || errors.Is(err, ErrNothingToCheckout) {
		if p, ok, lookupErr := s.existing(ctx, userID, req.IdempotencyKey); ok || lookupErr != nil {
			return p, lookupErr
		}
	}
	if errors.Is(err, errDuplicateKey) {
		err = ErrNothingToCheckout
	}
	return placed, err
}

func (s *Service) existing(ctx context.Context, userID int64, key string) (Placed, bool, error) {
	if key == "" {
		return Placed{}, false, nil
	}
	n, ok, err := s.repo.OrderByKey(ctx, userID, key)
	return Placed{OrderNumber: n, Existing: true}, ok, err
}

// retryDeadlock runs fn again once when PostgreSQL aborted it because of a
// deadlock (possible when two checkouts sell out products at the same
// moment and the summary trigger updates the product rows).
func retryDeadlock(fn func() error) error {
	err := fn()
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "40001") {
		err = fn()
	}
	return err
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// valid reports whether the source names something to buy.
func (s Source) valid() bool {
	if s.BuyNow() {
		return s.Quantity != 0
	}
	return len(s.CartItemIDs) > 0 && len(s.CartItemIDs) <= 100
}
