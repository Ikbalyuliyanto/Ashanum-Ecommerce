package cart

import "context"

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service { return &Service{repo: repo} }

// Quote validates buying quantity of a variant right now: the variant, its
// product and category must be active, and the quantity within stock and
// MaxQuantity. It writes nothing. Phase 5 "Buy Now" can check out the
// returned Line directly.
func (s *Service) Quote(ctx context.Context, variantID int64, quantity int) (Line, error) {
	if quantity < 1 || quantity > MaxQuantity {
		return Line{}, &QuantityError{Requested: quantity, Available: MaxQuantity}
	}
	v, err := s.repo.Variant(ctx, variantID)
	if err != nil {
		return Line{}, err
	}
	if !v.Available {
		return Line{}, ErrUnavailable
	}
	if quantity > v.limit() {
		return Line{}, &QuantityError{Requested: quantity, Available: v.Stock}
	}
	v.Line.Quantity = quantity
	return v.Line, nil
}

// AddResult reports the line and cart totals after a successful add.
type AddResult struct {
	Line         Line
	LineQuantity int // quantity of this variant now in the cart
	CartQuantity int // total quantity in the cart (badge)
}

// AddItem adds quantity of a variant to the user's active cart, creating
// the cart on first use. Adding a variant that is already there increases
// its quantity. The add is refused (never clamped) when the new quantity
// would exceed stock or MaxQuantity.
func (s *Service) AddItem(ctx context.Context, userID, variantID int64, quantity int) (AddResult, error) {
	line, err := s.Quote(ctx, variantID, quantity)
	if err != nil {
		return AddResult{}, err
	}
	cartID, err := s.repo.EnsureCart(ctx, userID)
	if err != nil {
		return AddResult{}, err
	}
	lineQty, cartQty, ok, err := s.repo.Add(ctx, cartID, variantID, quantity, line.limit())
	if err != nil {
		return AddResult{}, err
	}
	if !ok {
		lines, inCart, err := s.repo.LineState(ctx, cartID, variantID)
		if err != nil {
			return AddResult{}, err
		}
		if inCart == 0 && lines >= MaxItems {
			return AddResult{}, ErrCartFull
		}
		return AddResult{}, &QuantityError{Requested: quantity, Available: line.Stock, InCart: inCart}
	}
	return AddResult{Line: line, LineQuantity: lineQty, CartQuantity: cartQty}, nil
}

// Get returns the user's active cart with current catalog data (empty when
// the user has no cart yet). Lines whose product became unavailable are
// kept and marked, never dropped silently.
func (s *Service) Get(ctx context.Context, userID int64) (Cart, error) {
	items, err := s.repo.Items(ctx, userID)
	return Cart{Items: items}, err
}

// Summary returns the cart and the total of the selected checkout-ready
// lines. Selection is request state (checkboxes), not stored.
func (s *Service) Summary(ctx context.Context, userID int64, selected []int64) (Cart, Summary, error) {
	c, err := s.Get(ctx, userID)
	if err != nil {
		return Cart{}, Summary{}, err
	}
	set := make(map[int64]bool, len(selected))
	for _, id := range selected {
		set[id] = true
	}
	return c, c.Summarize(func(id int64) bool { return set[id] }), nil
}

// UpdateQuantity sets a line's quantity. Lowering is always allowed while
// the product is available; raising is refused beyond current stock.
func (s *Service) UpdateQuantity(ctx context.Context, userID, itemID int64, quantity int) error {
	it, err := s.repo.OwnedItem(ctx, userID, itemID)
	if err != nil {
		return err
	}
	if !it.Available {
		return ErrUnavailable
	}
	if quantity < 1 || quantity > MaxQuantity || quantity > it.Stock {
		return &QuantityError{Requested: quantity, Available: it.Stock}
	}
	return s.repo.SetQuantity(ctx, userID, itemID, quantity)
}

// RemoveItem deletes a line of the user's cart (any status).
func (s *Service) RemoveItem(ctx context.Context, userID, itemID int64) error {
	return s.repo.Delete(ctx, userID, itemID)
}

// Clear removes every line of the user's active cart.
func (s *Service) Clear(ctx context.Context, userID int64) error {
	return s.repo.Clear(ctx, userID)
}

// Count is the total quantity in the user's cart (header badge).
func (s *Service) Count(ctx context.Context, userID int64) (int, error) {
	return s.repo.Quantity(ctx, userID)
}
