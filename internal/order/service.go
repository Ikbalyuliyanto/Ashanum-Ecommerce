package order

import "context"

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service { return &Service{repo: repo} }

// List returns one page (1-based) of the user's orders for a history tab.
func (s *Service) List(ctx context.Context, userID int64, tabKey string, page int) (Page, error) {
	tab := TabByKey(tabKey)
	page = min(max(page, 1), 100)
	rows, err := s.repo.List(ctx, userID, tab.Statuses, PageSize+1, (page-1)*PageSize)
	if err != nil {
		return Page{}, err
	}
	p := Page{Tab: tab, Page: page}
	if len(rows) > PageSize {
		rows, p.HasNext = rows[:PageSize], true
	}
	p.Orders = rows
	return p, nil
}

// Get returns an order of the user; another user's order is ErrNotFound.
func (s *Service) Get(ctx context.Context, userID int64, number string) (Order, error) {
	if number == "" || len(number) > 40 {
		return Order{}, ErrNotFound
	}
	return s.repo.Get(ctx, userID, number)
}
