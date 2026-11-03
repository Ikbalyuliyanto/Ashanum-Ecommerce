package category

import (
	"context"
	"errors"

	"ecommerce/internal/database"
	"ecommerce/internal/database/dbgen"
)

var errSlugTaken = errors.New("category: slug taken")

type Repository struct {
	q *dbgen.Queries
}

func NewRepository(db dbgen.DBTX) *Repository { return &Repository{q: dbgen.New(db)} }

func (r *Repository) ListActive(ctx context.Context) ([]Category, error) {
	rows, err := r.q.ListActiveCategories(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Category, len(rows))
	for i, row := range rows {
		out[i] = fromRow(row)
	}
	return out, nil
}

func (r *Repository) Get(ctx context.Context, id int64) (Category, error) {
	row, err := r.q.GetCategory(ctx, id)
	if database.IsNoRows(err) {
		return Category{}, ErrNotFound
	}
	if err != nil {
		return Category{}, err
	}
	return fromRow(row), nil
}

func (r *Repository) Create(ctx context.Context, in Input) (Category, error) {
	row, err := r.q.CreateCategory(ctx, dbgen.CreateCategoryParams{
		Name:         in.Name,
		Slug:         in.Slug,
		ImageUrl:     nullable(in.ImageURL),
		Active:       in.Active,
		DisplayOrder: int32(in.DisplayOrder),
	})
	return mapWrite(row, err)
}

func (r *Repository) Update(ctx context.Context, id int64, in Input) (Category, error) {
	row, err := r.q.UpdateCategory(ctx, dbgen.UpdateCategoryParams{
		ID:           id,
		Name:         in.Name,
		Slug:         in.Slug,
		ImageUrl:     nullable(in.ImageURL),
		Active:       in.Active,
		DisplayOrder: int32(in.DisplayOrder),
	})
	return mapWrite(row, err)
}

func mapWrite(row dbgen.Category, err error) (Category, error) {
	switch {
	case database.IsNoRows(err):
		return Category{}, ErrNotFound
	case database.IsUniqueViolation(err, "categories_slug_key"):
		return Category{}, errSlugTaken
	case err != nil:
		return Category{}, err
	}
	return fromRow(row), nil
}

func fromRow(row dbgen.Category) Category {
	c := Category{
		ID:           row.ID,
		Name:         row.Name,
		Slug:         row.Slug,
		Active:       row.Active,
		DisplayOrder: int(row.DisplayOrder),
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
	}
	if row.ImageUrl != nil {
		c.ImageURL = *row.ImageUrl
	}
	return c
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
