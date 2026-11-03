package checkout

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	"ecommerce/internal/address"
)

const maxKeyLength = 64

type Service struct {
	repo      *Repository
	addresses *address.Service
}

func NewService(repo *Repository, addresses *address.Service) *Service {
	return &Service{repo: repo, addresses: addresses}
}

// Prepare builds the checkout page from current data, without locking.
// addressID 0 picks the primary address. Lines that cannot be ordered are
// returned with a Problem so the page can warn.
func (s *Service) Prepare(ctx context.Context, userID int64, src Source, addressID int64) (Draft, error) {
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
	d.Total = d.Subtotal + d.ShippingCost - d.Discount
	return d, nil
}

// PlaceOrder creates an order in one transaction:
//
//  1. lock the selected cart lines (Buy Now: no cart involved),
//  2. lock their variants in id order and re-validate availability, stock
//     and quantity against current data,
//  3. compare the total with what the customer saw,
//  4. copy the user's address,
//  5. insert order and items, decrement stock with "stock >= qty" guards,
//     and delete only the purchased cart lines.
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
				default:
					return ErrStockChanged
				}
				subtotal += l.Subtotal()
			}
			if req.ExpectedTotal != 0 && req.ExpectedTotal != subtotal {
				return ErrPriceChanged
			}
			if req.AddressID == 0 {
				return ErrAddressRequired
			}
			addr, err := tx.Address(ctx, userID, req.AddressID)
			if err != nil {
				return err
			}
			c, err := tx.Create(ctx, userID, req.IdempotencyKey, addr, lines, subtotal)
			if err != nil {
				return err
			}
			if c.StockUpdated != len(lines) {
				return ErrStockChanged // a stock guard failed: roll back
			}
			placed = Placed{OrderNumber: c.Number, Total: subtotal}
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

// valid reports whether the source names something to buy.
func (s Source) valid() bool {
	if s.BuyNow() {
		return s.Quantity != 0
	}
	return len(s.CartItemIDs) > 0 && len(s.CartItemIDs) <= 100
}
