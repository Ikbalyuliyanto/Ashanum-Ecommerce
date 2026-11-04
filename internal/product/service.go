package product

import (
	"context"
	"errors"
	"strings"

	"ecommerce/internal/validate"
)

const (
	homeSectionLimit     = 10
	homeRecommendedLimit = 20
	maxQueryRunes        = 100
)

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service { return &Service{repo: repo} }

// List returns one page of active products matching p (listing, category
// page and search share it).
func (s *Service) List(ctx context.Context, p ListParams) (Page, error) {
	p = p.normalized()
	page := Page{Page: p.Page, PageSize: p.PageSize, Sort: p.Sort}

	q := listQuery{
		categoryID:    p.CategoryID,
		minPrice:      p.MinPrice,
		maxPrice:      p.MaxPrice,
		flashSaleOnly: p.FlashSaleOnly,
		sort:          p.Sort,
		limit:         p.PageSize,
		offset:        (p.Page - 1) * p.PageSize,
	}
	if p.Query != "" {
		if q.tsQuery = tsQuery(p.Query); q.tsQuery == "" {
			return page, nil // nothing searchable, e.g. only punctuation
		}
	}

	items, total, err := s.repo.List(ctx, q)
	if err != nil {
		return Page{}, err
	}
	page.Items, page.Total = items, total
	return page, nil
}

// Home returns the flash sale, best seller and recommended sections.
// "Recommended" is rule-based (featured first, then in-stock and newest),
// not personalized.
func (s *Service) Home(ctx context.Context) (Home, error) {
	return s.repo.Home(ctx, homeSectionLimit, homeRecommendedLimit)
}

// Detail returns an active product in an active category, or ErrNotFound.
func (s *Service) Detail(ctx context.Context, slug string) (Detail, error) {
	if !validate.IsSlug(slug) {
		return Detail{}, ErrNotFound
	}
	return s.repo.Detail(ctx, slug)
}

// Related returns other products from the same category.
func (s *Service) Related(ctx context.Context, d Detail, limit int) ([]Card, error) {
	return s.repo.Related(ctx, d.CategoryID, d.ID, limit)
}

// Create adds a product with images, attributes and variants. Invalid
// input returns validate.Errors.
func (s *Service) Create(ctx context.Context, in Input) (int64, error) {
	return s.create(ctx, 0, in)
}

// create validates and stores a product; adminID (0 for seed data and
// tests) is recorded on the INITIAL_STOCK adjustments and the audit entry.
func (s *Service) create(ctx context.Context, adminID int64, in Input) (int64, error) {
	p, err := normalizeInput(in)
	if err != nil {
		return 0, err
	}
	id, err := s.repo.Create(ctx, p, adminID)
	switch {
	case errors.Is(err, errSlugTaken):
		return 0, validate.Errors{"slug": "Slug sudah dipakai produk lain."}
	case errors.Is(err, errSKUTaken):
		return 0, validate.Errors{"variants": "Salah satu SKU sudah dipakai produk lain."}
	case errors.Is(err, errUnknownCategory):
		return 0, validate.Errors{"category_id": "Kategori tidak ditemukan."}
	}
	return id, err
}

func (p ListParams) normalized() ListParams {
	p.Query = strings.TrimSpace(p.Query)
	if r := []rune(p.Query); len(r) > maxQueryRunes {
		p.Query = string(r[:maxQueryRunes])
	}
	p.CategoryID = max(p.CategoryID, 0)
	p.MinPrice, p.MaxPrice = max(p.MinPrice, 0), max(p.MaxPrice, 0)
	if p.MinPrice > 0 && p.MaxPrice > 0 && p.MinPrice > p.MaxPrice {
		p.MinPrice, p.MaxPrice = p.MaxPrice, p.MinPrice
	}
	if p.PageSize <= 0 {
		p.PageSize = DefaultPageSize
	}
	p.PageSize = min(p.PageSize, MaxPageSize)
	p.Page = min(max(p.Page, 1), MaxPage)

	switch p.Sort {
	case SortNewest, SortBestSelling, SortPriceAsc, SortPriceDesc:
	case SortRelevance:
		if p.Query == "" {
			p.Sort = SortNewest
		}
	default:
		if p.Query != "" {
			p.Sort = SortRelevance
		} else {
			p.Sort = SortNewest
		}
	}
	return p
}
