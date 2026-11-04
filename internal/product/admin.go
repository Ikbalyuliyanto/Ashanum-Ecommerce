package product

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"ecommerce/internal/audit"
	"ecommerce/internal/database"
	"ecommerce/internal/database/dbgen"
	"ecommerce/internal/inventory"
	"ecommerce/internal/validate"
)

// Admin catalog management. It uses the one product model of Phase 3
// (products, images, attributes, variants): nothing here is a second model.
// Products and variants are deactivated, never deleted, so carts, orders and
// returns keep their links. Stock is never written by these methods except
// as the initial stock of a new variant (recorded as INITIAL_STOCK); later
// changes go through package inventory, which keeps the audit trail.

// AdminCard is one product in the admin list.
type AdminCard struct {
	ID             int64
	Name           string
	Slug           string
	Brand          string
	Active         bool
	Price          int64
	PriceMax       int64
	InStock        bool
	CategoryName   string
	CategoryActive bool
	VariantCount   int
	Stock          int // units across active variants
	ImageURL       string
}

const (
	AdminPageSize    = 20
	MaxAdminPageSize = 100
)

// AdminFilter selects the admin list. Status is "" | "active" | "inactive";
// Query searches the name or a SKU.
type AdminFilter struct {
	Query      string
	CategoryID int64
	Status     string
	Page       int
	PageSize   int
}

type AdminPage struct {
	Products []AdminCard
	Filter   AdminFilter
	HasNext  bool
}

// AdminVariant is a variant on the edit page (inactive ones included).
type AdminVariant struct {
	ID            int64   `json:"id"`
	SKU           string  `json:"sku"`
	Price         int64   `json:"price"`
	OriginalPrice int64   `json:"original_price"`
	Stock         int     `json:"stock"`
	Active        bool    `json:"active"`
	ValueIDs      []int64 `json:"value_ids"`
	Label         string  `json:"-"`
}

// AdminProduct is any product (active or not) with everything the edit
// page needs.
type AdminProduct struct {
	ID            int64
	CategoryID    int64
	Name          string
	Slug          string
	Description   string
	Brand         string
	Active        bool
	Featured      bool
	FlashSale     bool
	SoldCount     int
	RatingAverage float64
	RatingCount   int
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Images        []Image
	Attributes    []Attribute
	Variants      []AdminVariant
}

// ImageText is the image URLs one per line, for the edit form.
func (p AdminProduct) ImageText() string {
	urls := make([]string, len(p.Images))
	for i, img := range p.Images {
		urls[i] = img.URL
	}
	return strings.Join(urls, "\n")
}

// Info is the product fields an admin edits. Only these are read from the
// form: price, stock, sold count and rating are never taken from it.
type Info struct {
	CategoryID  int64
	Name        string
	Slug        string
	Brand       string
	Description string
	Active      bool
	Featured    bool
	FlashSale   bool
}

// NewProduct is the admin's new product form: the product fields, image
// URLs, up to three attributes, and the price/stock every generated
// variant starts with (one variant per combination of attribute values).
type NewProduct struct {
	Info
	ImageURLs     []string
	Attributes    []AttributeInput
	SKUPrefix     string
	Price         int64
	OriginalPrice int64
	Stock         int
}

// VariantUpdate is what an admin may change on a variant. Stock is not
// here: it changes only through an inventory adjustment.
type VariantUpdate struct {
	SKU           string
	Price         int64
	OriginalPrice int64
	Active        bool
}

var ErrAttributeExists = errors.New("product: attribute already exists")

// AdminList returns one page of every product, newest first.
func (s *Service) AdminList(ctx context.Context, f AdminFilter) (AdminPage, error) {
	f.Page = min(max(f.Page, 1), 1000)
	if f.PageSize <= 0 {
		f.PageSize = AdminPageSize
	}
	f.PageSize = min(f.PageSize, MaxAdminPageSize)
	if f.Status != "active" && f.Status != "inactive" {
		f.Status = ""
	}
	f.Query = validate.SearchText(f.Query)
	p := dbgen.AdminProductsParams{Status: f.Status, Q: database.Like(f.Query),
		RowLimit: int32(f.PageSize + 1), RowOffset: int32((f.Page - 1) * f.PageSize)}
	if f.CategoryID > 0 {
		p.CategoryID = &f.CategoryID
	}
	rows, err := s.repo.q.AdminProducts(ctx, p)
	if err != nil {
		return AdminPage{}, err
	}
	page := AdminPage{Filter: f}
	if len(rows) > f.PageSize {
		rows, page.HasNext = rows[:f.PageSize], true
	}
	for _, r := range rows {
		page.Products = append(page.Products, AdminCard{ID: r.ID, Name: r.Name, Slug: r.Slug, Brand: r.Brand, Active: r.Active,
			Price: deref(r.Price), PriceMax: deref(r.PriceMax), InStock: r.InStock, CategoryName: r.CategoryName,
			CategoryActive: r.CategoryActive, VariantCount: int(r.VariantCount), Stock: int(r.Stock), ImageURL: r.ImageUrl})
	}
	return page, nil
}

// AdminGet returns any product with all its variants.
func (s *Service) AdminGet(ctx context.Context, id int64) (AdminProduct, error) {
	return adminProduct(ctx, s.repo.q, id)
}

func adminProduct(ctx context.Context, q *dbgen.Queries, id int64) (AdminProduct, error) {
	row, err := q.AdminProduct(ctx, id)
	if database.IsNoRows(err) {
		return AdminProduct{}, ErrNotFound
	}
	if err != nil {
		return AdminProduct{}, err
	}
	p := AdminProduct{ID: row.ID, CategoryID: row.CategoryID, Name: row.Name, Slug: row.Slug, Description: row.Description,
		Brand: row.Brand, Active: row.Active, Featured: row.Featured, FlashSale: row.FlashSale, SoldCount: int(row.SoldCount),
		RatingAverage: row.RatingAverage, RatingCount: int(row.RatingCount), CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if err := errors.Join(
		decode("images", row.Images, &p.Images),
		decode("attributes", row.Attributes, &p.Attributes),
		decode("variants", row.Variants, &p.Variants),
	); err != nil {
		return AdminProduct{}, err
	}
	values := map[int64]string{}
	for _, a := range p.Attributes {
		for _, v := range a.Values {
			values[v.ID] = v.Value
		}
	}
	for i, v := range p.Variants {
		parts := make([]string, len(v.ValueIDs))
		for j, id := range v.ValueIDs {
			parts[j] = values[id]
		}
		p.Variants[i].Label = strings.Join(parts, " / ")
	}
	return p, nil
}

// CreateByAdmin validates the new product form, generates its variants and
// creates the product (with INITIAL_STOCK adjustments and an audit entry)
// in one transaction. Returns the product ID.
func (s *Service) CreateByAdmin(ctx context.Context, adminID int64, in NewProduct) (int64, error) {
	variants, err := generateVariants(in)
	if err != nil {
		return 0, err
	}
	images := make([]ImageInput, 0, len(in.ImageURLs))
	for _, u := range in.ImageURLs {
		if u = strings.TrimSpace(u); u != "" {
			images = append(images, ImageInput{URL: u})
		}
	}
	return s.create(ctx, adminID, Input{CategoryID: in.CategoryID, Name: in.Name, Slug: in.Slug, Description: in.Description,
		Brand: in.Brand, Active: in.Active, Featured: in.Featured, FlashSale: in.FlashSale, Images: images,
		Attributes: in.Attributes, Variants: variants})
}

// generateVariants makes one variant per combination of attribute values,
// SKU = prefix + each value (upper case, A–Z 0–9 and "-"), all with the same
// starting price and stock (the admin fine-tunes each variant afterwards).
func generateVariants(in NewProduct) ([]VariantInput, error) {
	prefix := skuToken(in.SKUPrefix)
	if prefix == "" {
		return nil, validate.Errors{"sku_prefix": "Kode SKU wajib diisi (huruf besar, angka, tanda hubung)."}
	}
	combos := [][]string{{}}
	for _, a := range in.Attributes {
		var next [][]string
		for _, c := range combos {
			for _, v := range a.Values {
				next = append(next, append(slices.Clone(c), v))
			}
		}
		combos = next
		if len(combos) > maxVariants {
			return nil, validate.Errors{"attributes": fmt.Sprintf("Kombinasi varian melebihi %d.", maxVariants)}
		}
	}
	out := make([]VariantInput, 0, len(combos))
	for i, c := range combos {
		sku := prefix
		opts := map[string]string{}
		for j, v := range c {
			tok := skuToken(v)
			if tok == "" {
				tok = fmt.Sprint(i + 1)
			}
			sku += "-" + tok
			opts[in.Attributes[j].Name] = v
		}
		out = append(out, VariantInput{SKU: sku, Price: in.Price, OriginalPrice: in.OriginalPrice, Stock: in.Stock, Options: opts})
	}
	return out, nil
}

// skuToken turns text into an SKU part: "Merah Maroon" → "MERAH-MAROON".
func skuToken(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToUpper(strings.TrimSpace(s)) {
		switch {
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// UpdateInfo changes a product's fields and images (replaced as a whole) in
// one transaction with its audit entry.
func (s *Service) UpdateInfo(ctx context.Context, adminID, id int64, in Info, imageURLs []string) error {
	images := make([]ImageInput, 0, len(imageURLs))
	for _, u := range imageURLs {
		if u = strings.TrimSpace(u); u != "" {
			images = append(images, ImageInput{URL: u})
		}
	}
	// Reuse the creation rules; one placeholder variant passes the variant
	// checks, which do not apply to this form.
	p, err := normalizeInput(Input{CategoryID: in.CategoryID, Name: in.Name, Slug: in.Slug, Description: in.Description,
		Brand: in.Brand, Active: in.Active, Featured: in.Featured, FlashSale: in.FlashSale, Images: images,
		Variants: []VariantInput{{SKU: "X", Price: 1}}})
	if err != nil {
		return err
	}
	err = s.repo.inTx(ctx, func(q *dbgen.Queries) error {
		cur, err := q.LockProductRow(ctx, id)
		if database.IsNoRows(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := q.UpdateProduct(ctx, dbgen.UpdateProductParams{ID: id, CategoryID: p.CategoryID, Name: p.Name, Slug: p.Slug,
			Description: p.Description, Brand: p.Brand, Active: p.Active, Featured: p.Featured, FlashSale: p.FlashSale}); err != nil {
			return err
		}
		if err := q.DeleteProductImages(ctx, id); err != nil {
			return err
		}
		for i, img := range p.Images {
			if err := q.CreateProductImage(ctx, dbgen.CreateProductImageParams{ProductID: id, Url: img.URL, Alt: img.Alt, DisplayOrder: int32(i)}); err != nil {
				return err
			}
		}
		meta := map[string]any{"name": p.Name, "slug": p.Slug, "category_id": p.CategoryID, "images": len(p.Images)}
		if cur.Active != p.Active {
			meta["active"] = p.Active
		}
		return audit.Record(ctx, q, audit.Entry{AdminID: adminID, Action: "product.update", EntityType: audit.EntityProduct,
			EntityID: id, Label: p.Name, Meta: meta})
	})
	return productErr(err)
}

// SetActive shows or hides a product in the storefront (never deletes it).
func (s *Service) SetActive(ctx context.Context, adminID, id int64, active bool) error {
	return s.repo.inTx(ctx, func(q *dbgen.Queries) error {
		cur, err := q.LockProductRow(ctx, id)
		if database.IsNoRows(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		n, err := q.SetProductActive(ctx, dbgen.SetProductActiveParams{ID: id, Active: active})
		if err != nil || n == 0 {
			return err // already in that state: nothing to record
		}
		action := "product.deactivate"
		if active {
			action = "product.activate"
		}
		return audit.Record(ctx, q, audit.Entry{AdminID: adminID, Action: action, EntityType: audit.EntityProduct, EntityID: id, Label: cur.Name})
	})
}

// AddVariant adds a variant: one value for every attribute (an unknown
// value is added to the attribute), a combination no other variant has, and
// its initial stock recorded as INITIAL_STOCK.
func (s *Service) AddVariant(ctx context.Context, adminID, productID int64, in VariantInput) (int64, error) {
	var id int64
	err := s.repo.inTx(ctx, func(q *dbgen.Queries) error {
		cur, err := q.LockProductRow(ctx, productID)
		if database.IsNoRows(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		p, err := adminProduct(ctx, q, productID)
		if err != nil {
			return err
		}
		if len(p.Variants) >= maxVariants {
			return validate.Errors{"variant": fmt.Sprintf("Maksimal %d varian per produk.", maxVariants)}
		}
		e := validate.Errors{}
		sku := strings.ToUpper(strings.TrimSpace(in.SKU))
		checkVariant(e, sku, in.Price, in.OriginalPrice)
		if in.Stock < 0 || in.Stock > maxStock {
			e.Add("stock", "Stok awal tidak valid.")
		}
		values := make([]int64, len(p.Attributes))
		var pending []int // attributes whose value must be created
		names := make([]string, len(p.Attributes))
		for i, a := range p.Attributes {
			v, _ := option(in.Options, a.Name)
			validate.Length(e, "option_"+fmt.Sprint(a.ID), v, a.Name, 1, 40)
			names[i] = v
			for _, known := range a.Values {
				if strings.EqualFold(known.Value, v) {
					values[i], names[i] = known.ID, known.Value
				}
			}
			if values[i] == 0 {
				pending = append(pending, i)
			}
		}
		if len(p.Attributes) == 0 && len(p.Variants) > 0 {
			e.Add("variant", "Produk tanpa atribut hanya punya satu varian. Tambahkan atribut (mis. Warna) lebih dulu.")
		}
		if err := e.Err(); err != nil {
			return err
		}
		for _, i := range pending {
			a := p.Attributes[i]
			order, err := q.NextValueOrder(ctx, a.ID)
			if err != nil {
				return err
			}
			if values[i], err = q.CreateProductAttributeValue(ctx, dbgen.CreateProductAttributeValueParams{
				AttributeID: a.ID, Value: names[i], DisplayOrder: order}); err != nil {
				return err
			}
		}
		combos, err := q.VariantCombos(ctx, productID)
		if err != nil {
			return err
		}
		for _, c := range combos {
			if slices.Equal(c.ValueIds, values) {
				return validate.Errors{"variant": "Kombinasi varian ini sudah ada."}
			}
		}
		if id, err = q.CreateVariantRow(ctx, dbgen.CreateVariantRowParams{ProductID: productID, Sku: sku, Price: in.Price,
			OriginalPrice: optional(in.OriginalPrice), Stock: int32(in.Stock)}); err != nil {
			return err
		}
		for i, a := range p.Attributes {
			if err := q.CreateProductVariantValue(ctx, dbgen.CreateProductVariantValueParams{
				ProductID: productID, VariantID: id, AttributeID: a.ID, ValueID: values[i]}); err != nil {
				return err
			}
		}
		if in.Stock > 0 {
			if err := inventory.Log(ctx, q, inventory.Change{VariantID: id, SKU: sku, Type: inventory.TypeInitial,
				Delta: in.Stock, After: in.Stock, Reason: "Stok awal varian baru", AdminID: adminID}); err != nil {
				return err
			}
		}
		return audit.Record(ctx, q, audit.Entry{AdminID: adminID, Action: "variant.create", EntityType: audit.EntityVariant,
			EntityID: id, Label: sku, Meta: map[string]any{"product_id": productID, "product": cur.Name, "options": strings.Join(names, " / "),
				"price": in.Price, "stock": in.Stock}})
	})
	return id, productErr(err)
}

// UpdateVariant changes a variant's SKU, price, strike-through price and
// whether it is sold. Stock is not touched (see package inventory).
func (s *Service) UpdateVariant(ctx context.Context, adminID, productID, variantID int64, in VariantUpdate) error {
	e := validate.Errors{}
	sku := strings.ToUpper(strings.TrimSpace(in.SKU))
	checkVariant(e, sku, in.Price, in.OriginalPrice)
	if err := e.Err(); err != nil {
		return err
	}
	err := s.repo.inTx(ctx, func(q *dbgen.Queries) error {
		cur, err := q.LockProductVariant(ctx, dbgen.LockProductVariantParams{ID: variantID, ProductID: productID})
		if database.IsNoRows(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := q.UpdateVariant(ctx, dbgen.UpdateVariantParams{ID: variantID, ProductID: productID, Sku: sku, Price: in.Price,
			OriginalPrice: optional(in.OriginalPrice), Active: in.Active}); err != nil {
			return err
		}
		meta := map[string]any{}
		if cur.Sku != sku {
			meta["sku"] = map[string]string{"from": cur.Sku, "to": sku}
		}
		if cur.Price != in.Price {
			meta["price"] = map[string]int64{"from": cur.Price, "to": in.Price}
		}
		if deref(cur.OriginalPrice) != in.OriginalPrice {
			meta["original_price"] = map[string]int64{"from": deref(cur.OriginalPrice), "to": in.OriginalPrice}
		}
		action := "variant.update"
		switch {
		case cur.Active && !in.Active:
			action = "variant.deactivate"
		case !cur.Active && in.Active:
			action = "variant.activate"
		}
		if len(meta) == 0 && action == "variant.update" {
			return nil // nothing changed
		}
		meta["product_id"] = productID
		return audit.Record(ctx, q, audit.Entry{AdminID: adminID, Action: action, EntityType: audit.EntityVariant,
			EntityID: variantID, Label: sku, Meta: meta})
	})
	return productErr(err)
}

// AddAttribute adds an option dimension (e.g. "Bahan") to a product; every
// existing variant gets defaultValue for it, so each variant keeps exactly
// one value per attribute. Attribute names are free text (not only Warna
// and Ukuran).
func (s *Service) AddAttribute(ctx context.Context, adminID, productID int64, name, defaultValue string) error {
	name, defaultValue = validate.Text(name), validate.Text(defaultValue)
	e := validate.Errors{}
	validate.Length(e, "attribute_name", name, "Nama atribut", 1, 30)
	validate.Length(e, "attribute_value", defaultValue, "Nilai untuk varian yang ada", 1, 40)
	if err := e.Err(); err != nil {
		return err
	}
	err := s.repo.inTx(ctx, func(q *dbgen.Queries) error {
		cur, err := q.LockProductRow(ctx, productID)
		if database.IsNoRows(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		p, err := adminProduct(ctx, q, productID)
		if err != nil {
			return err
		}
		if len(p.Attributes) >= maxAttributes {
			return validate.Errors{"attribute_name": fmt.Sprintf("Maksimal %d atribut varian.", maxAttributes)}
		}
		for _, a := range p.Attributes {
			if strings.EqualFold(a.Name, name) {
				return validate.Errors{"attribute_name": "Atribut ini sudah ada."}
			}
		}
		order, err := q.NextAttributeOrder(ctx, productID)
		if err != nil {
			return err
		}
		attrID, err := q.CreateProductAttribute(ctx, dbgen.CreateProductAttributeParams{ProductID: productID, Name: name, DisplayOrder: order})
		if err != nil {
			return err
		}
		valueID, err := q.CreateProductAttributeValue(ctx, dbgen.CreateProductAttributeValueParams{AttributeID: attrID, Value: defaultValue})
		if err != nil {
			return err
		}
		if err := q.AssignValueToAllVariants(ctx, dbgen.AssignValueToAllVariantsParams{ProductID: productID, AttributeID: attrID, ValueID: valueID}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{AdminID: adminID, Action: "product.add_attribute", EntityType: audit.EntityProduct,
			EntityID: productID, Label: cur.Name, Meta: map[string]any{"attribute": name, "default_value": defaultValue}})
	})
	return productErr(err)
}

func checkVariant(e validate.Errors, sku string, price, original int64) {
	if !skuPattern.MatchString(sku) {
		e.Add("sku", "SKU wajib diisi: huruf besar, angka, titik, garis bawah, atau tanda hubung.")
	}
	if price <= 0 || price > maxPrice {
		e.Add("price", "Harga harus lebih dari 0.")
	}
	if original != 0 && original <= price {
		e.Add("original_price", "Harga coret harus lebih besar dari harga jual.")
	}
}

// productErr maps constraint violations to form errors.
func productErr(err error) error {
	switch {
	case err == nil:
		return nil
	case database.IsUniqueViolation(err, "products_slug_key"):
		return validate.Errors{"slug": "Slug sudah dipakai produk lain."}
	case database.IsUniqueViolation(err, "product_variants_sku_key"):
		return validate.Errors{"sku": "SKU sudah dipakai varian lain."}
	case database.IsForeignKeyViolation(err, "products_category_id_fkey"):
		return validate.Errors{"category_id": "Kategori tidak ditemukan."}
	}
	return err
}

// inTx runs fn with queries bound to one transaction.
func (r *Repository) inTx(ctx context.Context, fn func(q *dbgen.Queries) error) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error { return fn(r.q.WithTx(tx)) })
}
