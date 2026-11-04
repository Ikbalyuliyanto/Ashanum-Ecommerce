package review

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/database"
	"ecommerce/internal/database/dbgen"
)

type Repository struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
	tx   bool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool, q: dbgen.New(pool)}
}

// InTx runs fn in one transaction (commit on nil, rollback otherwise).
func (r *Repository) InTx(ctx context.Context, fn func(tx *Repository) error) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		return fn(&Repository{q: r.q.WithTx(tx), tx: true})
	})
}

type target struct {
	Target
	OrderID   int64
	ProductID *int64
	Status    string
}

// Target loads an order item of the user; inside a transaction the order
// row is share-locked so its status cannot change before commit.
func (r *Repository) Target(ctx context.Context, userID, orderItemID int64) (target, error) {
	var row dbgen.ReviewTargetRow
	var err error
	if r.tx {
		var locked dbgen.LockReviewTargetRow
		locked, err = r.q.LockReviewTarget(ctx, dbgen.LockReviewTargetParams{OrderItemID: orderItemID, UserID: userID})
		row = dbgen.ReviewTargetRow(locked)
	} else {
		row, err = r.q.ReviewTarget(ctx, dbgen.ReviewTargetParams{OrderItemID: orderItemID, UserID: userID})
	}
	if database.IsNoRows(err) {
		return target{}, ErrNotFound
	}
	if err != nil {
		return target{}, err
	}
	t := target{OrderID: row.OrderID, ProductID: row.ProductID, Status: row.Status, Target: Target{
		OrderItemID: row.ID, OrderNumber: row.OrderNumber, ProductName: row.ProductName, ProductSlug: row.ProductSlug,
		VariantLabel: row.VariantLabel, ImageURL: row.ImageUrl,
	}}
	if row.ReviewID != nil {
		t.ReviewID = *row.ReviewID
	}
	return t, nil
}

// Create inserts a review; a second review of the same item is
// ErrAlreadyReviewed (unique constraint, the final guard).
func (r *Repository) Create(ctx context.Context, userID int64, t target, in Input) (int64, error) {
	id, err := r.q.CreateReview(ctx, dbgen.CreateReviewParams{
		UserID: userID, OrderID: t.OrderID, OrderItemID: t.OrderItemID, ProductID: t.ProductID,
		Rating: int16(in.Rating), Comment: in.Comment,
	})
	if database.IsUniqueViolation(err, "product_reviews_order_item_key") {
		return 0, ErrAlreadyReviewed
	}
	return id, err
}

func (r *Repository) Get(ctx context.Context, userID, id int64) (Review, error) {
	row, err := r.q.UserReview(ctx, dbgen.UserReviewParams{ID: id, UserID: userID})
	if database.IsNoRows(err) {
		return Review{}, ErrNotFound
	}
	if err != nil {
		return Review{}, err
	}
	return Review{
		ID: row.ID, OrderItemID: row.OrderItemID, Rating: int(row.Rating), Comment: row.Comment,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, ProductName: row.ProductName, ProductSlug: row.ProductSlug,
		VariantLabel: row.VariantLabel, ImageURL: row.ImageUrl, OrderNumber: row.OrderNumber,
	}, nil
}

// LockOwn row-locks one of the user's reviews and returns its product.
func (r *Repository) LockOwn(ctx context.Context, userID, id int64) (*int64, error) {
	row, err := r.q.LockUserReview(ctx, dbgen.LockUserReviewParams{ID: id, UserID: userID})
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	return row.ProductID, err
}

func (r *Repository) Update(ctx context.Context, userID, id int64, in Input) error {
	return r.q.UpdateReview(ctx, dbgen.UpdateReviewParams{ID: id, UserID: userID, Rating: int16(in.Rating), Comment: in.Comment})
}

// LockProduct serializes rating recalculations of a product. A deleted
// product (nil) has nothing to lock.
func (r *Repository) LockProduct(ctx context.Context, productID *int64) error {
	if productID == nil {
		return nil
	}
	_, err := r.q.LockProductRating(ctx, *productID)
	return err
}

func (r *Repository) Recalculate(ctx context.Context, productID int64) error {
	return r.q.RecalculateProductRating(ctx, productID)
}

func (r *Repository) RecalculateAll(ctx context.Context) (int64, error) {
	return r.q.RecalculateAllRatings(ctx)
}

func (r *Repository) History(ctx context.Context, userID int64, limit, offset int) ([]Review, error) {
	rows, err := r.q.UserReviews(ctx, dbgen.UserReviewsParams{UserID: userID, RowLimit: int32(limit), RowOffset: int32(offset)})
	if err != nil {
		return nil, err
	}
	out := make([]Review, len(rows))
	for i, row := range rows {
		out[i] = Review{
			ID: row.ID, Rating: int(row.Rating), Comment: row.Comment, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
			ProductName: row.ProductName, ProductSlug: row.ProductSlug, VariantLabel: row.VariantLabel,
			ImageURL: row.ImageUrl, OrderNumber: row.OrderNumber, ProductListed: row.ProductListed,
		}
	}
	return out, nil
}

func (r *Repository) Pending(ctx context.Context, userID int64, limit, offset int) ([]PendingItem, error) {
	rows, err := r.q.PendingReviews(ctx, dbgen.PendingReviewsParams{UserID: userID, RowLimit: int32(limit), RowOffset: int32(offset)})
	if err != nil {
		return nil, err
	}
	out := make([]PendingItem, len(rows))
	for i, row := range rows {
		out[i] = PendingItem{OrderItemID: row.ID, ProductName: row.ProductName, VariantLabel: row.VariantLabel,
			ImageURL: row.ImageUrl, OrderNumber: row.OrderNumber, CompletedAt: row.CompletedAt}
	}
	return out, nil
}

func (r *Repository) PendingCount(ctx context.Context, userID int64) (int, error) {
	n, err := r.q.PendingReviewCount(ctx, userID)
	return int(n), err
}

// OrderReviews maps the order's reviewed item IDs to review IDs.
func (r *Repository) OrderReviews(ctx context.Context, userID, orderID int64) (map[int64]int64, error) {
	rows, err := r.q.OrderReviews(ctx, dbgen.OrderReviewsParams{OrderID: orderID, UserID: userID})
	if err != nil {
		return nil, err
	}
	m := make(map[int64]int64, len(rows))
	for _, row := range rows {
		m[row.OrderItemID] = row.ID
	}
	return m, nil
}

// Product returns an active product by slug, for its reviews page.
func (r *Repository) Product(ctx context.Context, slug string) (dbgen.ReviewProductRow, error) {
	row, err := r.q.ReviewProduct(ctx, slug)
	if database.IsNoRows(err) {
		return row, ErrNotFound
	}
	return row, err
}

func (r *Repository) ProductReviews(ctx context.Context, productID int64, limit, offset int) ([]PublicReview, error) {
	rows, err := r.q.ProductReviews(ctx, dbgen.ProductReviewsParams{ProductID: &productID, RowLimit: int32(limit), RowOffset: int32(offset)})
	if err != nil {
		return nil, err
	}
	out := make([]PublicReview, len(rows))
	for i, row := range rows {
		out[i] = PublicReview{ID: row.ID, Author: MaskName(row.FirstName, row.LastName, row.Email), Rating: int(row.Rating),
			VariantLabel: row.VariantLabel, Comment: row.Comment, CreatedAt: row.CreatedAt}
	}
	return out, nil
}

// Distribution counts reviews per star value (index 0 = 1 star).
func (r *Repository) Distribution(ctx context.Context, productID int64) ([5]int, error) {
	var d [5]int
	rows, err := r.q.ProductRatingSummary(ctx, &productID)
	if err != nil {
		return d, err
	}
	for _, row := range rows {
		if row.Rating >= 1 && row.Rating <= 5 {
			d[row.Rating-1] = int(row.Total)
		}
	}
	return d, nil
}
