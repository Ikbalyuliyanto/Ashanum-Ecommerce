package category

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"ecommerce/internal/audit"
	"ecommerce/internal/database/dbgen"
)

// AdminCategory is a category in the admin list (active or not).
type AdminCategory struct {
	Category
	ProductCount int
}

// AdminList returns every category, active or not, with its product count.
// Categories are never deleted: products and order history point at them,
// so an unused one is deactivated instead.
func (s *Service) AdminList(ctx context.Context) ([]AdminCategory, error) {
	rows, err := s.repo.q.AdminCategories(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]AdminCategory, len(rows))
	for i, row := range rows {
		out[i] = AdminCategory{Category: fromRow(dbgen.Category{ID: row.ID, Name: row.Name, Slug: row.Slug, ImageUrl: row.ImageUrl,
			Active: row.Active, DisplayOrder: row.DisplayOrder, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}),
			ProductCount: int(row.ProductCount)}
	}
	return out, nil
}

// CreateBy creates a category on an admin's behalf, with its audit entry in
// the same transaction.
func (s *Service) CreateBy(ctx context.Context, adminID int64, in Input) (Category, error) {
	in, err := normalize(in)
	if err != nil {
		return Category{}, err
	}
	var c Category
	err = s.repo.inTx(ctx, func(tx *Repository) error {
		var err error
		if c, err = tx.Create(ctx, in); err != nil {
			return err
		}
		return audit.Record(ctx, tx.q, audit.Entry{AdminID: adminID, Action: "category.create", EntityType: audit.EntityCategory,
			EntityID: c.ID, Label: c.Name, Meta: map[string]any{"slug": c.Slug, "active": c.Active}})
	})
	return s.afterWrite(c, err)
}

// UpdateBy changes a category on an admin's behalf (deactivating hides it
// and its products from the storefront), with its audit entry.
func (s *Service) UpdateBy(ctx context.Context, adminID, id int64, in Input) (Category, error) {
	in, err := normalize(in)
	if err != nil {
		return Category{}, err
	}
	var c Category
	err = s.repo.inTx(ctx, func(tx *Repository) error {
		before, err := tx.Get(ctx, id)
		if err != nil {
			return err
		}
		if c, err = tx.Update(ctx, id, in); err != nil {
			return err
		}
		meta := map[string]any{"slug": c.Slug, "display_order": c.DisplayOrder}
		if before.Active != c.Active {
			meta["active"] = c.Active
		}
		return audit.Record(ctx, tx.q, audit.Entry{AdminID: adminID, Action: "category.update", EntityType: audit.EntityCategory,
			EntityID: c.ID, Label: c.Name, Meta: meta})
	})
	return s.afterWrite(c, err)
}

// inTx runs fn on a repository bound to one transaction.
func (r *Repository) inTx(ctx context.Context, fn func(tx *Repository) error) error {
	b, ok := r.db.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return errors.New("category: transactions need a connection pool")
	}
	return pgx.BeginFunc(ctx, b, func(tx pgx.Tx) error { return fn(&Repository{db: tx, q: r.q.WithTx(tx)}) })
}
