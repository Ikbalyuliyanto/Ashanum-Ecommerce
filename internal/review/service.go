package review

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"ecommerce/internal/audit"
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

// ProductPage returns one page (1-based) of a product's visible reviews
// with the rating summary. The average and count come from the cached
// aggregate on products (no query when there are no reviews); the page and
// the star distribution come from one query.
func (s *Service) ProductPage(ctx context.Context, productID int64, average float64, count int, page int) (ProductPage, error) {
	page = min(max(page, 1), 500)
	p := ProductPage{ProductID: productID, Page: page, Summary: Summary{Average: average, Count: count}}
	if count == 0 {
		return p, nil
	}
	reviews, dist, ok, err := s.repo.ReviewPage(ctx, productID, ProductPageSize+1, (page-1)*ProductPageSize)
	if err != nil {
		return p, err
	}
	if !ok { // past the last page: the distribution needs its own query
		if dist, err = s.repo.Distribution(ctx, productID); err != nil {
			return p, err
		}
	}
	for stars := 5; stars >= 1; stars-- {
		p.Summary.Bars = append(p.Summary.Bars, Bar{Stars: stars, Count: dist[stars-1]})
	}
	if p.Reviews = reviews; len(p.Reviews) > ProductPageSize {
		p.Reviews, p.HasNext = p.Reviews[:ProductPageSize], true
	}
	return p, nil
}

// AdminList returns one page of every review for moderation, newest first.
func (s *Service) AdminList(ctx context.Context, f AdminFilter) (AdminPage, error) {
	f.Page = min(max(f.Page, 1), 1000)
	if f.PageSize <= 0 {
		f.PageSize = AdminPageSize
	}
	f.PageSize = min(f.PageSize, MaxAdminPageSize)
	if f.Status != "visible" && f.Status != "hidden" {
		f.Status = ""
	}
	if f.Rating < 0 || f.Rating > 5 {
		f.Rating = 0
	}
	f.Query = validate.SearchText(f.Query)
	rows, err := s.repo.AdminList(ctx, f, f.PageSize+1, (f.Page-1)*f.PageSize)
	if err != nil {
		return AdminPage{}, err
	}
	p := AdminPage{Filter: f}
	if len(rows) > f.PageSize {
		rows, p.HasNext = rows[:f.PageSize], true
	}
	p.Reviews = rows
	return p, nil
}

// AdminGet returns one review for moderation.
func (s *Service) AdminGet(ctx context.Context, id int64) (AdminReview, error) {
	return s.repo.AdminGet(ctx, id)
}

// Hide takes a review out of public view on an admin's decision (reason
// required) and recalculates the product rating without it, in one
// transaction: lock review → lock product → hide → recalculate → audit.
func (s *Service) Hide(ctx context.Context, adminID, id int64, reason string) error {
	reason = strings.TrimSpace(reason)
	e := validate.Errors{}
	validate.Length(e, "reason", reason, "Alasan moderasi", 3, 500)
	if reason == "" {
		e = validate.Errors{"reason": "Alasan menyembunyikan ulasan wajib diisi."}
	}
	if err := e.Err(); err != nil {
		return err
	}
	return s.moderate(ctx, adminID, id, true, reason)
}

// Unhide shows a hidden review again and recalculates the product rating
// with it.
func (s *Service) Unhide(ctx context.Context, adminID, id int64) error {
	return s.moderate(ctx, adminID, id, false, "")
}

func (s *Service) moderate(ctx context.Context, adminID, id int64, hide bool, reason string) error {
	return s.repo.InTx(ctx, func(tx *Repository) error {
		productID, hidden, err := tx.LockForModeration(ctx, id)
		if err != nil {
			return err
		}
		if hidden == hide {
			return ErrModerated
		}
		if err := tx.LockProduct(ctx, productID); err != nil {
			return err
		}
		var ok bool
		action := "review.unhide"
		if hide {
			ok, err = tx.Hide(ctx, id, adminID, reason)
			action = "review.hide"
		} else {
			ok, err = tx.Unhide(ctx, id)
		}
		if err == nil && !ok {
			err = ErrModerated
		}
		if err != nil {
			return err
		}
		if err := recalculate(ctx, tx, productID); err != nil {
			return err
		}
		meta := map[string]any{}
		if reason != "" {
			meta["reason"] = reason
		}
		if productID != nil {
			meta["product_id"] = *productID
		}
		return audit.Record(ctx, tx.Queries(), audit.Entry{AdminID: adminID, Action: action, EntityType: audit.EntityReview,
			EntityID: id, Label: fmt.Sprintf("Ulasan #%d", id), Meta: meta})
	})
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
