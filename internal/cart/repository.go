package cart

import (
	"context"

	"ecommerce/internal/database"
	"ecommerce/internal/database/dbgen"
)

type Repository struct {
	q *dbgen.Queries
}

func NewRepository(db dbgen.DBTX) *Repository { return &Repository{q: dbgen.New(db)} }

// variant is a variant's purchasable state.
type variant struct {
	Line
	Available bool
}

func (r *Repository) Variant(ctx context.Context, id int64) (variant, error) {
	row, err := r.q.GetPurchasableVariant(ctx, id)
	if database.IsNoRows(err) {
		return variant{}, ErrUnavailable
	}
	if err != nil {
		return variant{}, err
	}
	return variant{
		Line: Line{
			VariantID:     row.ID,
			ProductID:     row.ProductID,
			ProductName:   row.ProductName,
			ProductSlug:   row.ProductSlug,
			SKU:           row.Sku,
			Price:         row.Price,
			OriginalPrice: deref(row.OriginalPrice),
			Stock:         int(row.Stock),
		},
		Available: row.Available,
	}, nil
}

func (r *Repository) EnsureCart(ctx context.Context, userID int64) (int64, error) {
	return r.q.EnsureActiveCart(ctx, userID)
}

// Add atomically adds quantity to the variant's line. ok is false when the
// result would exceed maxQuantity or the cart already holds MaxItems lines.
func (r *Repository) Add(ctx context.Context, cartID, variantID int64, quantity, maxQuantity int) (lineQty, cartQty int, ok bool, err error) {
	row, err := r.q.AddCartItem(ctx, dbgen.AddCartItemParams{
		CartID:      cartID,
		VariantID:   &variantID,
		Quantity:    int32(quantity),
		MaxItems:    MaxItems,
		MaxQuantity: int32(maxQuantity),
	})
	if database.IsNoRows(err) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return int(row.Quantity), int(row.CartQuantity), true, nil
}

func (r *Repository) LineState(ctx context.Context, cartID, variantID int64) (lines, inCart int, err error) {
	row, err := r.q.CartLineState(ctx, dbgen.CartLineStateParams{CartID: cartID, VariantID: &variantID})
	return int(row.Lines), int(row.InCart), err
}

// ownedItem is a line of the user's active cart with its variant's state.
type ownedItem struct {
	Quantity  int
	Stock     int
	Available bool
}

func (r *Repository) OwnedItem(ctx context.Context, userID, itemID int64) (ownedItem, error) {
	row, err := r.q.GetOwnedCartItem(ctx, dbgen.GetOwnedCartItemParams{ID: itemID, UserID: userID})
	if database.IsNoRows(err) {
		return ownedItem{}, ErrItemNotFound
	}
	if err != nil {
		return ownedItem{}, err
	}
	it := ownedItem{Quantity: int(row.Quantity), Available: row.Available}
	if row.Stock != nil {
		it.Stock = int(*row.Stock)
	}
	return it, nil
}

func (r *Repository) SetQuantity(ctx context.Context, userID, itemID int64, quantity int) error {
	n, err := r.q.SetCartItemQuantity(ctx, dbgen.SetCartItemQuantityParams{ID: itemID, UserID: userID, Quantity: int32(quantity)})
	if err == nil && n == 0 {
		return ErrItemNotFound
	}
	return err
}

func (r *Repository) Delete(ctx context.Context, userID, itemID int64) error {
	n, err := r.q.DeleteCartItem(ctx, dbgen.DeleteCartItemParams{ID: itemID, UserID: userID})
	if err == nil && n == 0 {
		return ErrItemNotFound
	}
	return err
}

func (r *Repository) Clear(ctx context.Context, userID int64) error {
	return r.q.ClearCart(ctx, userID)
}

func (r *Repository) Quantity(ctx context.Context, userID int64) (int, error) {
	n, err := r.q.CartQuantity(ctx, userID)
	return int(n), err
}

// Items loads the whole cart in one query.
func (r *Repository) Items(ctx context.Context, userID int64) ([]Item, error) {
	rows, err := r.q.ListCartItems(ctx, userID)
	if err != nil {
		return nil, err
	}
	items := make([]Item, len(rows))
	for i, row := range rows {
		it := Item{
			ID:            row.ID,
			Quantity:      int(row.Quantity),
			ImageURL:      row.ImageUrl,
			VariantLabel:  row.VariantLabel,
			Price:         deref(row.Price),
			OriginalPrice: deref(row.OriginalPrice),
		}
		if row.VariantID != nil {
			it.VariantID = *row.VariantID
		}
		if row.Sku != nil {
			it.SKU = *row.Sku
		}
		if row.ProductName != nil {
			it.ProductName = *row.ProductName
		}
		if row.ProductSlug != nil {
			it.ProductSlug = *row.ProductSlug
		}
		if row.Stock != nil {
			it.Stock = int(*row.Stock)
		}
		switch {
		case row.VariantID == nil || !row.Available:
			it.Status = StatusUnavailable
		case it.Stock <= 0:
			it.Status = StatusOutOfStock
		case it.Quantity > it.Stock:
			it.Status = StatusInsufficientStock
		default:
			it.Status = StatusAvailable
		}
		items[i] = it
	}
	return items, nil
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
