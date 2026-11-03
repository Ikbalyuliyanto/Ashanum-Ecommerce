package category

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"ecommerce/internal/media"
	"ecommerce/internal/validate"
)

// cacheTTL bounds how stale the storefront's category list can be when it
// is changed outside this process (e.g. directly in SQL). Writes through
// this Service invalidate the cache immediately.
const cacheTTL = 5 * time.Minute

type Service struct {
	repo *Repository
	now  func() time.Time

	mu      sync.Mutex
	active  []Category
	expires time.Time
}

func NewService(repo *Repository) *Service { return &Service{repo: repo, now: time.Now} }

// List returns active categories in display order, from a small in-memory
// cache (categories appear on almost every storefront page).
func (s *Service) List(ctx context.Context) ([]Category, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil && s.now().Before(s.expires) {
		return slices.Clone(s.active), nil
	}
	list, err := s.repo.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	s.active, s.expires = list, s.now().Add(cacheTTL)
	return slices.Clone(list), nil
}

// BySlug returns an active category; inactive or unknown is ErrNotFound.
func (s *Service) BySlug(ctx context.Context, slug string) (Category, error) {
	list, err := s.List(ctx)
	if err != nil {
		return Category{}, err
	}
	for _, c := range list {
		if c.Slug == slug {
			return c, nil
		}
	}
	return Category{}, ErrNotFound
}

// Get returns any category by id, active or not (admin use).
func (s *Service) Get(ctx context.Context, id int64) (Category, error) {
	return s.repo.Get(ctx, id)
}

func (s *Service) Create(ctx context.Context, in Input) (Category, error) {
	in, err := normalize(in)
	if err != nil {
		return Category{}, err
	}
	c, err := s.repo.Create(ctx, in)
	return s.afterWrite(c, err)
}

func (s *Service) Update(ctx context.Context, id int64, in Input) (Category, error) {
	in, err := normalize(in)
	if err != nil {
		return Category{}, err
	}
	c, err := s.repo.Update(ctx, id, in)
	return s.afterWrite(c, err)
}

// Invalidate drops the cached list so the next List reads the database.
func (s *Service) Invalidate() {
	s.mu.Lock()
	s.active = nil
	s.mu.Unlock()
}

func (s *Service) afterWrite(c Category, err error) (Category, error) {
	if errors.Is(err, errSlugTaken) {
		return Category{}, validate.Errors{"slug": "Slug sudah dipakai kategori lain."}
	}
	if err != nil {
		return Category{}, err
	}
	s.Invalidate()
	return c, nil
}

func normalize(in Input) (Input, error) {
	errs := validate.Errors{}
	in.Name = validate.Text(in.Name)
	validate.Length(errs, "name", in.Name, "Nama kategori", 2, 50)

	if in.Slug == "" {
		in.Slug = validate.Slug(in.Name)
	}
	if !validate.IsSlug(in.Slug) {
		errs.Add("slug", "Slug hanya boleh huruf kecil, angka, dan tanda hubung.")
	}
	if in.ImageURL != "" && !media.Valid(in.ImageURL) {
		errs.Add("image_url", "Gambar harus berupa URL Cloudinary (https://res.cloudinary.com/…).")
	}
	if in.DisplayOrder < 0 || in.DisplayOrder > 10000 {
		errs.Add("display_order", "Urutan tampil harus antara 0 dan 10000.")
	}
	return in, errs.Err()
}
