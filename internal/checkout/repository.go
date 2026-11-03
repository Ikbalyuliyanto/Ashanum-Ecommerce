package checkout

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/cart"
	"ecommerce/internal/database"
	"ecommerce/internal/database/dbgen"
)

var errDuplicateKey = errors.New("checkout: idempotency key already used")

type Repository struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
	tx   bool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool, q: dbgen.New(pool)}
}

// InTx runs fn with a repository bound to one transaction; it commits when
// fn returns nil and rolls back otherwise.
func (r *Repository) InTx(ctx context.Context, fn func(tx *Repository) error) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		return fn(&Repository{q: r.q.WithTx(tx), tx: true})
	})
}

type cartLine struct {
	ID        int64
	VariantID int64 // 0 when the variant was deleted
	Quantity  int
}

// Lines resolves a Source into lines with current catalog data. Inside a
// transaction (InTx) the cart lines and variants are row-locked, variants
// in id order. Cart IDs that are not the user's are simply not returned.
func (r *Repository) Lines(ctx context.Context, userID int64, src Source) ([]Line, error) {
	var cls []cartLine
	if src.BuyNow() {
		cls = []cartLine{{VariantID: src.VariantID, Quantity: src.Quantity}}
	} else if len(src.CartItemIDs) > 0 {
		params := dbgen.CheckoutCartLinesParams{UserID: userID, Ids: src.CartItemIDs}
		var rows []dbgen.CheckoutCartLinesRow
		var err error
		if r.tx {
			var locked []dbgen.LockCheckoutCartLinesRow
			locked, err = r.q.LockCheckoutCartLines(ctx, dbgen.LockCheckoutCartLinesParams(params))
			for _, l := range locked {
				rows = append(rows, dbgen.CheckoutCartLinesRow(l))
			}
		} else {
			rows, err = r.q.CheckoutCartLines(ctx, params)
		}
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			cl := cartLine{ID: row.ID, Quantity: int(row.Quantity)}
			if row.VariantID != nil {
				cl.VariantID = *row.VariantID
			}
			cls = append(cls, cl)
		}
	}

	var ids []int64
	for _, cl := range cls {
		if cl.VariantID != 0 {
			ids = append(ids, cl.VariantID)
		}
	}
	variants := make(map[int64]dbgen.CheckoutVariantsRow, len(ids))
	if len(ids) > 0 {
		if r.tx {
			locked, err := r.q.LockCheckoutVariants(ctx, ids)
			if err != nil {
				return nil, err
			}
			for _, v := range locked {
				variants[v.ID] = dbgen.CheckoutVariantsRow(v)
			}
		} else {
			rows, err := r.q.CheckoutVariants(ctx, ids)
			if err != nil {
				return nil, err
			}
			for _, v := range rows {
				variants[v.ID] = v
			}
		}
	}

	lines := make([]Line, 0, len(cls))
	for _, cl := range cls {
		l := Line{CartItemID: cl.ID, VariantID: cl.VariantID, Quantity: cl.Quantity}
		v, ok := variants[cl.VariantID]
		if !ok {
			l.Problem = ProblemUnavailable
			lines = append(lines, l)
			continue
		}
		l.ProductID, l.ProductName, l.ProductSlug = v.ProductID, v.ProductName, v.ProductSlug
		l.SKU, l.VariantLabel, l.ImageURL = v.Sku, v.VariantLabel, v.ImageUrl
		l.UnitPrice, l.Stock = v.Price, int(v.Stock)
		if v.OriginalPrice != nil {
			l.OriginalPrice = *v.OriginalPrice
		}
		l.Problem = evaluate(v.Available, l.Stock, l.Quantity)
		lines = append(lines, l)
	}
	return lines, nil
}

// evaluate applies the purchase rules (same limits as the cart).
func evaluate(available bool, stock, quantity int) Problem {
	switch {
	case !available:
		return ProblemUnavailable
	case quantity < 1 || quantity > cart.MaxQuantity:
		return ProblemInvalidQuantity
	case stock <= 0:
		return ProblemOutOfStock
	case quantity > stock:
		return ProblemInsufficientStock
	}
	return ProblemNone
}

// Address returns the user's address snapshot source.
func (r *Repository) Address(ctx context.Context, userID, id int64) (dbgen.UserAddress, error) {
	a, err := r.q.GetAddress(ctx, dbgen.GetAddressParams{ID: id, UserID: userID})
	if database.IsNoRows(err) {
		return a, ErrAddressRequired
	}
	return a, err
}

type created struct {
	ID           int64
	Number       string
	StockUpdated int
}

// Create writes order, items, stock decrements and cart cleanup in one
// statement (see CreateOrder in queries/checkout.sql).
func (r *Repository) Create(ctx context.Context, userID int64, key string, a dbgen.UserAddress, lines []Line, subtotal int64) (created, error) {
	p := dbgen.CreateOrderParams{
		UserID:         userID,
		Subtotal:       subtotal,
		AddressLabel:   a.Label,
		RecipientName:  a.RecipientName,
		RecipientPhone: a.Phone,
		Province:       a.Province,
		City:           a.City,
		District:       a.District,
		Village:        a.Village,
		PostalCode:     a.PostalCode,
		Address:        a.StreetAddress,
		Latitude:       a.Latitude,
		Longitude:      a.Longitude,
		CartItemIds:    []int64{},
	}
	if key != "" {
		p.IdempotencyKey = &key
	}
	for _, l := range lines {
		p.ProductIds = append(p.ProductIds, l.ProductID)
		p.VariantIds = append(p.VariantIds, l.VariantID)
		p.ProductNames = append(p.ProductNames, l.ProductName)
		p.ProductSlugs = append(p.ProductSlugs, l.ProductSlug)
		p.Skus = append(p.Skus, l.SKU)
		p.VariantLabels = append(p.VariantLabels, l.VariantLabel)
		p.ImageUrls = append(p.ImageUrls, l.ImageURL)
		p.UnitPrices = append(p.UnitPrices, l.UnitPrice)
		p.Quantities = append(p.Quantities, int32(l.Quantity))
		if l.CartItemID != 0 {
			p.CartItemIds = append(p.CartItemIds, l.CartItemID)
		}
	}
	row, err := r.q.CreateOrder(ctx, p)
	if database.IsUniqueViolation(err, "orders_idempotency_key") {
		return created{}, errDuplicateKey
	}
	if err != nil {
		return created{}, err
	}
	return created{ID: row.ID, Number: row.OrderNumber, StockUpdated: int(row.StockUpdated)}, nil
}

// OrderByKey finds an order created earlier with the same idempotency key.
func (r *Repository) OrderByKey(ctx context.Context, userID int64, key string) (string, bool, error) {
	n, err := r.q.GetOrderNumberByIdempotencyKey(ctx, dbgen.GetOrderNumberByIdempotencyKeyParams{UserID: userID, IdempotencyKey: &key})
	if database.IsNoRows(err) {
		return "", false, nil
	}
	return n, err == nil, err
}
