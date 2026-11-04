package product

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/audit"
	"ecommerce/internal/database"
	"ecommerce/internal/database/dbgen"
	"ecommerce/internal/inventory"
)

var (
	errSlugTaken       = errors.New("product: slug taken")
	errSKUTaken        = errors.New("product: sku taken")
	errUnknownCategory = errors.New("product: unknown category")
)

type Repository struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool, q: dbgen.New(pool)}
}

// listQuery is a validated listing request in database terms.
type listQuery struct {
	categoryID    int64
	minPrice      int64
	maxPrice      int64
	flashSaleOnly bool
	tsQuery       string
	sort          Sort
	limit         int
	offset        int
}

func (r *Repository) List(ctx context.Context, q listQuery) ([]Card, int, error) {
	rows, err := r.q.ListProducts(ctx, dbgen.ListProductsParams{
		CategoryID:    optional(q.categoryID),
		MinPrice:      optional(q.minPrice),
		MaxPrice:      optional(q.maxPrice),
		FlashSaleOnly: q.flashSaleOnly,
		Query:         optionalText(q.tsQuery),
		Sort:          string(q.sort),
		PageLimit:     int32(q.limit),
		PageOffset:    int32(q.offset),
	})
	if err != nil {
		return nil, 0, err
	}
	total := 0
	cards := make([]Card, len(rows))
	for i, row := range rows {
		total = int(row.Total)
		cards[i] = card(row.ID, row.Name, row.Slug, row.Brand, row.Price, row.PriceMax, row.OriginalPrice,
			row.InStock, row.FlashSale, row.SoldCount, row.RatingAverage, row.RatingCount, row.ImageUrl)
	}
	return cards, total, nil
}

func (r *Repository) Home(ctx context.Context, sectionLimit, recommendedLimit int) (Home, error) {
	rows, err := r.q.HomeProducts(ctx, dbgen.HomeProductsParams{
		SectionLimit:     int32(sectionLimit),
		RecommendedLimit: int32(recommendedLimit),
	})
	if err != nil {
		return Home{}, err
	}
	var h Home
	for _, row := range rows {
		c := card(row.ID, row.Name, row.Slug, row.Brand, row.Price, row.PriceMax, row.OriginalPrice,
			row.InStock, row.FlashSale, row.SoldCount, row.RatingAverage, row.RatingCount, row.ImageUrl)
		switch row.Section {
		case "flash_sale":
			h.FlashSale = append(h.FlashSale, c)
		case "best_seller":
			h.BestSellers = append(h.BestSellers, c)
		case "recommended":
			h.Recommended = append(h.Recommended, c)
		}
	}
	return h, nil
}

func (r *Repository) Related(ctx context.Context, categoryID, productID int64, limit int) ([]Card, error) {
	rows, err := r.q.ListRelatedProducts(ctx, dbgen.ListRelatedProductsParams{
		CategoryID: categoryID,
		ProductID:  productID,
		RowLimit:   int32(limit),
	})
	if err != nil {
		return nil, err
	}
	cards := make([]Card, len(rows))
	for i, row := range rows {
		cards[i] = card(row.ID, row.Name, row.Slug, row.Brand, row.Price, row.PriceMax, row.OriginalPrice,
			row.InStock, row.FlashSale, row.SoldCount, row.RatingAverage, row.RatingCount, row.ImageUrl)
	}
	return cards, nil
}

func (r *Repository) Detail(ctx context.Context, slug string) (Detail, error) {
	row, err := r.q.GetProductDetail(ctx, slug)
	if database.IsNoRows(err) {
		return Detail{}, ErrNotFound
	}
	if err != nil {
		return Detail{}, err
	}
	d := Detail{
		ID:            row.ID,
		CategoryID:    row.CategoryID,
		CategoryName:  row.CategoryName,
		CategorySlug:  row.CategorySlug,
		Name:          row.Name,
		Slug:          row.Slug,
		Description:   row.Description,
		Brand:         row.Brand,
		Featured:      row.Featured,
		FlashSale:     row.FlashSale,
		SoldCount:     int(row.SoldCount),
		RatingAverage: row.RatingAverage,
		RatingCount:   int(row.RatingCount),
	}
	if err := errors.Join(
		decode("images", row.Images, &d.Images),
		decode("attributes", row.Attributes, &d.Attributes),
		decode("variants", row.Variants, &d.Variants),
	); err != nil {
		return Detail{}, err
	}
	return d, nil
}

func decode(name string, raw []byte, dst any) error {
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("product: decode %s: %w", name, err)
	}
	return nil
}

// Create stores a validated product with its images, attributes and
// variants in one transaction. The variant trigger fills the price summary.
// Each variant's starting stock is recorded as an INITIAL_STOCK inventory
// adjustment; an admin creation is also written to the audit log.
func (r *Repository) Create(ctx context.Context, p validProduct, adminID int64) (int64, error) {
	var id int64
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		var err error
		id, err = q.CreateProduct(ctx, dbgen.CreateProductParams{
			CategoryID:  p.CategoryID,
			Name:        p.Name,
			Slug:        p.Slug,
			Description: p.Description,
			Brand:       p.Brand,
			Active:      p.Active,
			Featured:    p.Featured,
			FlashSale:   p.FlashSale,
		})
		if err != nil {
			return err
		}
		for i, img := range p.Images {
			if err := q.CreateProductImage(ctx, dbgen.CreateProductImageParams{
				ProductID: id, Url: img.URL, Alt: img.Alt, DisplayOrder: int32(i),
			}); err != nil {
				return err
			}
		}

		attrIDs := make([]int64, len(p.Attributes))
		valueIDs := make([]map[string]int64, len(p.Attributes))
		for i, a := range p.Attributes {
			attrIDs[i], err = q.CreateProductAttribute(ctx, dbgen.CreateProductAttributeParams{
				ProductID: id, Name: a.Name, DisplayOrder: int32(i),
			})
			if err != nil {
				return err
			}
			valueIDs[i] = make(map[string]int64, len(a.Values))
			for j, v := range a.Values {
				valueIDs[i][v], err = q.CreateProductAttributeValue(ctx, dbgen.CreateProductAttributeValueParams{
					AttributeID: attrIDs[i], Value: v, DisplayOrder: int32(j),
				})
				if err != nil {
					return err
				}
			}
		}

		for _, v := range p.Variants {
			variantID, err := q.CreateProductVariant(ctx, dbgen.CreateProductVariantParams{
				ProductID:     id,
				Sku:           v.SKU,
				Price:         v.Price,
				OriginalPrice: optional(v.OriginalPrice),
				Stock:         int32(v.Stock),
			})
			if err != nil {
				return err
			}
			for i, value := range v.Values { // value per attribute, in attribute order
				if err := q.CreateProductVariantValue(ctx, dbgen.CreateProductVariantValueParams{
					ProductID: id, VariantID: variantID, AttributeID: attrIDs[i], ValueID: valueIDs[i][value],
				}); err != nil {
					return err
				}
			}
			if v.Stock > 0 {
				if err := inventory.Log(ctx, q, inventory.Change{VariantID: variantID, SKU: v.SKU, Type: inventory.TypeInitial,
					Delta: v.Stock, After: v.Stock, Reason: "Stok awal produk baru", AdminID: adminID}); err != nil {
					return err
				}
			}
		}
		if adminID == 0 {
			return nil
		}
		return audit.Record(ctx, q, audit.Entry{AdminID: adminID, Action: "product.create", EntityType: audit.EntityProduct,
			EntityID: id, Label: p.Name, Meta: map[string]any{"variants": len(p.Variants), "active": p.Active}})
	})

	var pgErr *pgconn.PgError
	switch {
	case database.IsUniqueViolation(err, "products_slug_key"):
		return 0, errSlugTaken
	case database.IsUniqueViolation(err, "product_variants_sku_key"):
		return 0, errSKUTaken
	case errors.As(err, &pgErr) && pgErr.Code == "23503" && pgErr.ConstraintName == "products_category_id_fkey":
		return 0, errUnknownCategory
	}
	return id, err
}

func card(id int64, name, slug, brand string, price, priceMax, original *int64, inStock, flash bool,
	sold int32, rating float64, ratingCount int32, image string) Card {
	return Card{
		ID: id, Name: name, Slug: slug, Brand: brand,
		Price: deref(price), PriceMax: deref(priceMax), OriginalPrice: deref(original),
		InStock: inStock, FlashSale: flash,
		SoldCount: int(sold), RatingAverage: rating, RatingCount: int(ratingCount),
		ImageURL: image,
	}
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func optional(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func optionalText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
