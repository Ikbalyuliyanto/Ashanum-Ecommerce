package order

import (
	"context"
	"time"

	"ecommerce/internal/payment"
	"ecommerce/internal/returns"
)

// Payments is the part of payment.Service the order pages use.
type Payments interface {
	Enabled() bool
	Sandbox() bool
	Attempts(ctx context.Context, orderID int64) ([]payment.Attempt, error)
	ExpireOverdue(ctx context.Context, orderID int64) (bool, error)
	Sync(ctx context.Context, orderID int64) error
}

// Reviews is the part of review.Service the order detail uses.
type Reviews interface {
	ReviewedItems(ctx context.Context, userID, orderID int64) (map[int64]int64, error)
}

// Returns is the part of returns.Service the order detail uses.
type Returns interface {
	ForOrder(ctx context.Context, userID int64, number string) (returns.OrderInfo, error)
}

// Deps are optional collaborators; nil ones leave that information out.
type Deps struct {
	Payments Payments
	Reviews  Reviews
	Returns  Returns
}

type Service struct {
	repo     *Repository
	payments Payments
	reviews  Reviews
	returns  Returns
}

func NewService(repo *Repository, deps Deps) *Service {
	return &Service{repo: repo, payments: deps.Payments, reviews: deps.Reviews, returns: deps.Returns}
}

// List returns one page (1-based) of the user's orders for a history tab.
// Unpaid orders past their payment deadline are expired first (lazy
// expiry), so the page never lists them as payable.
func (s *Service) List(ctx context.Context, userID int64, tabKey string, page int) (Page, error) {
	tab := TabByKey(tabKey)
	page = min(max(page, 1), 100)
	rows, err := s.repo.List(ctx, userID, tab.Statuses, PageSize+1, (page-1)*PageSize)
	if err != nil {
		return Page{}, err
	}
	if s.payments != nil {
		expired := false
		for _, o := range rows {
			if o.Status == StatusWaitingPayment && !time.Now().Before(o.DueAt) {
				done, err := s.payments.ExpireOverdue(ctx, o.ID)
				if err != nil {
					return Page{}, err
				}
				expired = expired || done
			}
		}
		if expired {
			if rows, err = s.repo.List(ctx, userID, tab.Statuses, PageSize+1, (page-1)*PageSize); err != nil {
				return Page{}, err
			}
		}
	}
	p := Page{Tab: tab, Page: page}
	if len(rows) > PageSize {
		rows, p.HasNext = rows[:PageSize], true
	}
	p.Orders = rows
	return p, nil
}

// Get returns an order of the user with its payment attempts, and for a
// completed order its reviews and returns; another user's order is
// ErrNotFound. An overdue unpaid order is expired first.
func (s *Service) Get(ctx context.Context, userID int64, number string) (Order, error) {
	o, err := s.get(ctx, userID, number)
	if err != nil {
		return o, err
	}
	if o, err = s.withAfterSales(ctx, userID, o); err != nil || s.payments == nil {
		return o, err
	}
	if o.Status == StatusWaitingPayment && !time.Now().Before(o.PaymentDueAt) {
		done, err := s.payments.ExpireOverdue(ctx, o.ID)
		if err != nil {
			return Order{}, err
		}
		if done {
			if o, err = s.get(ctx, userID, number); err != nil {
				return Order{}, err
			}
		}
	}
	return s.withPayments(ctx, o)
}

// Sync refreshes the payment status of the user's order from the provider
// (the buyer came back from the provider checkout) and returns the order.
func (s *Service) Sync(ctx context.Context, userID int64, number string) (Order, error) {
	o, err := s.get(ctx, userID, number)
	if err != nil || s.payments == nil {
		return o, err
	}
	if o.Status == StatusWaitingPayment {
		if err := s.payments.Sync(ctx, o.ID); err != nil {
			return Order{}, err
		}
		if o, err = s.get(ctx, userID, number); err != nil {
			return Order{}, err
		}
	}
	return s.withPayments(ctx, o)
}

func (s *Service) get(ctx context.Context, userID int64, number string) (Order, error) {
	if number == "" || len(number) > 40 {
		return Order{}, ErrNotFound
	}
	return s.repo.Get(ctx, userID, number)
}

// withAfterSales adds reviews and returns to a completed order.
func (s *Service) withAfterSales(ctx context.Context, userID int64, o Order) (Order, error) {
	if o.Status != StatusCompleted {
		return o, nil
	}
	var err error
	if s.reviews != nil {
		if o.Reviewed, err = s.reviews.ReviewedItems(ctx, userID, o.ID); err != nil {
			return Order{}, err
		}
	}
	if s.returns != nil {
		if o.Returns, err = s.returns.ForOrder(ctx, userID, o.Number); err != nil {
			return Order{}, err
		}
	}
	return o, nil
}

func (s *Service) withPayments(ctx context.Context, o Order) (Order, error) {
	list, err := s.payments.Attempts(ctx, o.ID)
	if err != nil {
		return Order{}, err
	}
	o.Payments, o.PaymentEnabled, o.Sandbox = list, s.payments.Enabled(), s.payments.Sandbox()
	return o, nil
}
