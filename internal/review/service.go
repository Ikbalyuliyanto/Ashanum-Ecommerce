package review

import (
	"context"
	"strconv"

	"ecommerce/internal/validate"
)

const orderCompleted = "COMPLETED"

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service { return &Service{repo: repo} }

// Target returns an item of the user's order that may be reviewed now
// (order COMPLETED). An item already reviewed has ReviewID set; another
// user's item is ErrNotFound.
func (s *Service) Target(ctx context.Context, userID, orderItemID int64) (Target, error) {
	t, err := s.repo.Target(ctx, userID, orderItemID)
	if err != nil {
		return Target{}, err
	}
	if t.Status != orderCompleted {
		return Target{}, ErrNotEligible
	}
	return t.Target, nil
}

// Create writes the buyer's review of one order item and rebuilds the
// product rating in the same transaction:
//
//	lock order (share) → check owner + COMPLETED → lock product
//	→ insert review (unique per item) → recalculate rating → commit
func (s *Service) Create(ctx context.Context, userID, orderItemID int64, in Input) (int64, error) {
	in, err := clean(in)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.repo.InTx(ctx, func(tx *Repository) error {
		t, err := tx.Target(ctx, userID, orderItemID)
		if err != nil {
			return err
		}
		if t.Status != orderCompleted {
			return ErrNotEligible
		}
		if t.ReviewID != 0 {
			return ErrAlreadyReviewed
		}
		if err := tx.LockProduct(ctx, t.ProductID); err != nil {
			return err
		}
		if id, err = tx.Create(ctx, userID, t, in); err != nil {
			return err
		}
		return recalculate(ctx, tx, t.ProductID)
	})
	return id, err
}

// Get returns one of the user's reviews (ErrNotFound for others' reviews).
func (s *Service) Get(ctx context.Context, userID, id int64) (Review, error) {
	return s.repo.Get(ctx, userID, id)
}

// Update changes the user's own review and rebuilds the product rating in
// the same transaction.
func (s *Service) Update(ctx context.Context, userID, id int64, in Input) error {
	in, err := clean(in)
	if err != nil {
		return err
	}
	return s.repo.InTx(ctx, func(tx *Repository) error {
		productID, err := tx.LockOwn(ctx, userID, id)
		if err != nil {
			return err
		}
		if err := tx.LockProduct(ctx, productID); err != nil {
			return err
		}
		if err := tx.Update(ctx, userID, id, in); err != nil {
			return err
		}
		return recalculate(ctx, tx, productID)
	})
}

// RecalculateProductRating rebuilds products.rating_average/rating_count of
// one product from product_reviews (repair tool; reviews already do this).
func (s *Service) RecalculateProductRating(ctx context.Context, productID int64) error {
	return s.repo.InTx(ctx, func(tx *Repository) error {
		if err := tx.LockProduct(ctx, &productID); err != nil {
			return err
		}
		return tx.Recalculate(ctx, productID)
	})
}

// RecalculateAll repairs every product whose cached rating differs from
// product_reviews and returns how many were fixed.
func (s *Service) RecalculateAll(ctx context.Context) (int64, error) {
	return s.repo.RecalculateAll(ctx)
}

func recalculate(ctx context.Context, tx *Repository, productID *int64) error {
	if productID == nil { // product deleted: no aggregate to keep
		return nil
	}
	return tx.Recalculate(ctx, *productID)
}

// History returns one page (1-based) of "Ulasan Saya": the user's reviews,
// or with tab "pending" the purchased items still waiting for a review.
func (s *Service) History(ctx context.Context, userID int64, tab string, page int) (HistoryPage, error) {
	page = min(max(page, 1), 500)
	h := HistoryPage{Page: page}
	var err error
	if h.PendingCount, err = s.repo.PendingCount(ctx, userID); err != nil {
		return h, err
	}
	offset := (page - 1) * PageSize
	if tab == "pending" {
		h.Tab = tab
		if h.Pending, err = s.repo.Pending(ctx, userID, PageSize+1, offset); err != nil {
			return h, err
		}
		if len(h.Pending) > PageSize {
			h.Pending, h.HasNext = h.Pending[:PageSize], true
		}
		return h, nil
	}
	if h.Reviews, err = s.repo.History(ctx, userID, PageSize+1, offset); err != nil {
		return h, err
	}
	if len(h.Reviews) > PageSize {
		h.Reviews, h.HasNext = h.Reviews[:PageSize], true
	}
	return h, nil
}

// ReviewedItems maps the order's reviewed item IDs to their review IDs.
func (s *Service) ReviewedItems(ctx context.Context, userID, orderID int64) (map[int64]int64, error) {
	return s.repo.OrderReviews(ctx, userID, orderID)
}

// ProductPage returns one page (1-based) of a product's reviews with the
// rating summary. The summary uses the cached aggregate on products.
func (s *Service) ProductPage(ctx context.Context, productID int64, average float64, count int, page int) (ProductPage, error) {
	page = min(max(page, 1), 500)
	p := ProductPage{ProductID: productID, Page: page, Summary: Summary{Average: average, Count: count}}
	if count == 0 {
		return p, nil
	}
	dist, err := s.repo.Distribution(ctx, productID)
	if err != nil {
		return p, err
	}
	for stars := 5; stars >= 1; stars-- {
		p.Summary.Bars = append(p.Summary.Bars, Bar{Stars: stars, Count: dist[stars-1]})
	}
	if p.Reviews, err = s.repo.ProductReviews(ctx, productID, ProductPageSize+1, (page-1)*ProductPageSize); err != nil {
		return p, err
	}
	if len(p.Reviews) > ProductPageSize {
		p.Reviews, p.HasNext = p.Reviews[:ProductPageSize], true
	}
	return p, nil
}

// ProductPageBySlug is ProductPage for the reviews page of an active product.
func (s *Service) ProductPageBySlug(ctx context.Context, slug string, page int) (ProductPage, error) {
	prod, err := s.repo.Product(ctx, slug)
	if err != nil {
		return ProductPage{}, err
	}
	p, err := s.ProductPage(ctx, prod.ID, prod.RatingAverage, int(prod.RatingCount), page)
	p.ProductName, p.ProductSlug = prod.Name, prod.Slug
	return p, err
}

// clean validates a review: rating 1–5, optional comment up to MaxComment.
func clean(in Input) (Input, error) {
	e := validate.Errors{}
	if in.Rating < 1 || in.Rating > 5 {
		e.Add("rating", "Pilih rating 1 sampai 5 bintang.")
	}
	in.Comment = cleanComment(in.Comment)
	validate.Length(e, "comment", in.Comment, "Ulasan", 0, MaxComment)
	return in, e.Err()
}

// ParseRating reads a rating form value; invalid input becomes 0 (rejected
// by validation).
func ParseRating(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
