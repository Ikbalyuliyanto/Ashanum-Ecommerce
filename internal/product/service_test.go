package product

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/category"
	"ecommerce/internal/testutil"
	"ecommerce/internal/validate"
)

const img = "data:image/svg+xml;base64,PHN2Zy8+"

type fixture struct {
	t    *testing.T
	ctx  context.Context
	db   *pgxpool.Pool
	svc  *Service
	cats *category.Service
}

func newFixture(t *testing.T) *fixture {
	db := testutil.DB(t)
	return &fixture{t: t, ctx: context.Background(), db: db,
		svc: NewService(NewRepository(db)), cats: category.NewService(category.NewRepository(db))}
}

func (f *fixture) category(name string, active bool) int64 {
	f.t.Helper()
	c, err := f.cats.Create(f.ctx, category.Input{Name: name, Active: active})
	if err != nil {
		f.t.Fatal(err)
	}
	return c.ID
}

// simple creates a product with one variant (no attributes).
func (f *fixture) simple(catID int64, name, brand string, price, original int64, mutate func(*Input)) int64 {
	f.t.Helper()
	in := Input{
		CategoryID: catID, Name: name, Brand: brand, Active: true,
		Images:   []ImageInput{{URL: img}},
		Variants: []VariantInput{{SKU: "SKU-" + validate.Slug(name), Price: price, OriginalPrice: original, Stock: 5}},
	}
	if mutate != nil {
		mutate(&in)
	}
	id, err := f.svc.Create(f.ctx, in)
	if err != nil {
		f.t.Fatalf("create %s: %v", name, err)
	}
	return id
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *fixture) list(p ListParams) Page {
	f.t.Helper()
	page, err := f.svc.List(f.ctx, p)
	if err != nil {
		f.t.Fatal(err)
	}
	return page
}

func names(p Page) []string {
	out := make([]string, len(p.Items))
	for i, c := range p.Items {
		out[i] = c.Name
	}
	return out
}

func equal(t *testing.T, label string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: got %v, want %v", label, got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s: got %v, want %v", label, got, want)
			return
		}
	}
}

// catalog: 3 visible products, one inactive product and one in an
// inactive category.
func (f *fixture) catalog() (hijab, tas int64) {
	hijab, tas = f.category("Hijab", true), f.category("Tas", true)
	arsip := f.category("Arsip", false)
	a := f.simple(hijab, "Pashmina Ceruti", "Ashanum", 59000, 79000, func(in *Input) { in.FlashSale = true })
	b := f.simple(hijab, "Bergo Jersey", "Nura", 45000, 0, nil)
	c := f.simple(tas, "Tote Kanvas", "Ashanum", 89000, 0, func(in *Input) { in.Featured = true })
	f.simple(tas, "Sling Kulit", "Kayla", 179000, 0, func(in *Input) { in.Active = false })
	f.simple(arsip, "Produk Arsip Tote", "Ashanum", 10000, 0, nil)
	f.exec("UPDATE products SET sold_count = 100 WHERE id = $1", a)
	f.exec("UPDATE products SET sold_count = 300 WHERE id = $1", b)
	f.exec("UPDATE products SET sold_count = 50 WHERE id = $1", c)
	return hijab, tas
}

func TestListingVisibilityFiltersAndSort(t *testing.T) {
	f := newFixture(t)
	hijab, _ := f.catalog()

	all := f.list(ListParams{})
	equal(t, "default newest, visible only", names(all), "Tote Kanvas", "Bergo Jersey", "Pashmina Ceruti")
	if all.Total != 3 || all.Sort != SortNewest {
		t.Errorf("total %d sort %s", all.Total, all.Sort)
	}

	equal(t, "category", names(f.list(ListParams{CategoryID: hijab})), "Bergo Jersey", "Pashmina Ceruti")
	equal(t, "price range", names(f.list(ListParams{MinPrice: 40000, MaxPrice: 60000})), "Bergo Jersey", "Pashmina Ceruti")
	equal(t, "flash sale", names(f.list(ListParams{FlashSaleOnly: true})), "Pashmina Ceruti")
	equal(t, "price asc", names(f.list(ListParams{Sort: SortPriceAsc})), "Bergo Jersey", "Pashmina Ceruti", "Tote Kanvas")
	equal(t, "price desc", names(f.list(ListParams{Sort: SortPriceDesc})), "Tote Kanvas", "Pashmina Ceruti", "Bergo Jersey")
	equal(t, "best selling", names(f.list(ListParams{Sort: SortBestSelling})), "Bergo Jersey", "Pashmina Ceruti", "Tote Kanvas")

	card := f.list(ListParams{FlashSaleOnly: true}).Items[0]
	if card.Price != 59000 || card.OriginalPrice != 79000 || card.DiscountPercent() != 25 || !card.InStock || card.ImageURL != img {
		t.Errorf("card = %+v", card)
	}
}

func TestPagination(t *testing.T) {
	f := newFixture(t)
	f.catalog()

	p1 := f.list(ListParams{PageSize: 2, Page: 1})
	p2 := f.list(ListParams{PageSize: 2, Page: 2})
	p3 := f.list(ListParams{PageSize: 2, Page: 3})
	equal(t, "page 1", names(p1), "Tote Kanvas", "Bergo Jersey")
	equal(t, "page 2", names(p2), "Pashmina Ceruti")
	if !p1.HasNext() || p2.HasNext() || !p2.HasPrev() || p1.Total != 3 || p2.Total != 3 {
		t.Errorf("p1 %+v / p2 %+v", p1, p2)
	}
	if len(p3.Items) != 0 || p3.HasNext() {
		t.Errorf("past the end: %+v", p3)
	}
}

func TestSearch(t *testing.T) {
	f := newFixture(t)
	f.catalog()

	for q, want := range map[string][]string{
		"pash":          {"Pashmina Ceruti"},                // name prefix
		"NURA":          {"Bergo Jersey"},                   // brand, any case
		"tas":           {"Tote Kanvas"},                    // category name; inactive Sling hidden
		"ashanum tote":  {"Tote Kanvas"},                    // all words must match
		"ashanum":       {"Tote Kanvas", "Pashmina Ceruti"}, // archived category hidden
		"arsip":         nil,                                // inactive category is not searchable
		"tidak-ada-xyz": nil,
		"!!! ???":       nil,
	} {
		page := f.list(ListParams{Query: q, Sort: SortNewest})
		equal(t, "search "+q, names(page), want...)
	}
	if s := f.list(ListParams{Query: "jersey"}).Sort; s != SortRelevance {
		t.Errorf("default sort with a query = %s", s)
	}
}

func kaosInput(catID int64) Input {
	return Input{
		CategoryID: catID, Name: "Kaos Ashanum Basic", Brand: "Ashanum", Active: true,
		Description: "Kaos katun.\r\nNyaman.",
		Images:      []ImageInput{{URL: img, Alt: "Depan"}, {URL: img}},
		Attributes: []AttributeInput{
			{Name: "Warna", Values: []string{"Hitam", "Putih"}},
			{Name: "Ukuran", Values: []string{"S", "M"}},
		},
		Variants: []VariantInput{
			{SKU: "ash-blk-s", Price: 119000, Stock: 5, Options: map[string]string{"Warna": "Hitam", "Ukuran": "S"}},
			{SKU: "ASH-BLK-M", Price: 129000, OriginalPrice: 159000, Stock: 10, Options: map[string]string{"warna": "hitam", "Ukuran": "M"}},
			{SKU: "ASH-WHT-S", Price: 129000, Stock: 3, Options: map[string]string{"Warna": "Putih", "Ukuran": "S"}},
			{SKU: "ASH-WHT-M", Price: 129000, Stock: 0, Options: map[string]string{"Warna": "Putih", "Ukuran": "M"}},
		},
	}
}

func TestDetailVariantsAndVisibility(t *testing.T) {
	f := newFixture(t)
	cat := f.category("Atasan", true)
	if _, err := f.svc.Create(f.ctx, kaosInput(cat)); err != nil {
		t.Fatal(err)
	}

	d, err := f.svc.Detail(f.ctx, "kaos-ashanum-basic")
	if err != nil {
		t.Fatal(err)
	}
	if d.CategoryName != "Atasan" || d.CategorySlug != "atasan" || d.Description != "Kaos katun.\nNyaman." {
		t.Errorf("detail header: %+v", d)
	}
	if len(d.Images) != 2 || d.Images[0].Alt != "Depan" || d.Images[1].Alt != "Kaos Ashanum Basic" {
		t.Errorf("images = %+v", d.Images)
	}
	if len(d.Attributes) != 2 || d.Attributes[0].Name != "Warna" || d.Attributes[1].Values[1].Value != "M" {
		t.Errorf("attributes = %+v", d.Attributes)
	}
	if len(d.Variants) != 4 || d.Variants[0].SKU != "ASH-BLK-S" || len(d.Variants[0].ValueIDs) != 2 {
		t.Fatalf("variants = %+v", d.Variants)
	}

	hitam, m := d.Attributes[0].Values[0].ID, d.Attributes[1].Values[1].ID
	sel := d.Select([]int64{hitam, m})
	if sel.Variant == nil || sel.Variant.SKU != "ASH-BLK-M" || sel.Stock != 10 || sel.DiscountPercent() != 19 {
		t.Errorf("selection = %+v", sel.Variant)
	}

	// Variant stock change is visible in detail; a deactivated variant is not.
	f.exec("UPDATE product_variants SET stock = 2 WHERE sku = 'ASH-BLK-M'")
	f.exec("UPDATE product_variants SET active = false WHERE sku = 'ASH-WHT-M'")
	d, _ = f.svc.Detail(f.ctx, "kaos-ashanum-basic")
	if len(d.Variants) != 3 || d.Select([]int64{hitam, m}).Stock != 2 {
		t.Errorf("after stock/active change: %d variants", len(d.Variants))
	}

	for name, slug := range map[string]string{"unknown": "tidak-ada", "invalid slug": "../etc"} {
		if _, err := f.svc.Detail(f.ctx, slug); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	f.exec("UPDATE products SET active = false WHERE slug = 'kaos-ashanum-basic'")
	if _, err := f.svc.Detail(f.ctx, "kaos-ashanum-basic"); !errors.Is(err, ErrNotFound) {
		t.Errorf("inactive product: err = %v", err)
	}
	f.exec("UPDATE products SET active = true WHERE slug = 'kaos-ashanum-basic'")
	f.exec("UPDATE categories SET active = false WHERE id = $1", cat)
	if _, err := f.svc.Detail(f.ctx, "kaos-ashanum-basic"); !errors.Is(err, ErrNotFound) {
		t.Errorf("product in inactive category: err = %v", err)
	}
}

// The listing price/stock summary must always follow the variants.
func TestVariantSummaryStaysConsistent(t *testing.T) {
	f := newFixture(t)
	cat := f.category("Atasan", true)
	id, err := f.svc.Create(f.ctx, kaosInput(cat))
	if err != nil {
		t.Fatal(err)
	}
	summary := func() (price, max, orig *int64, inStock bool) {
		t.Helper()
		if err := f.db.QueryRow(f.ctx, "SELECT price, price_max, original_price, in_stock FROM products WHERE id = $1", id).
			Scan(&price, &max, &orig, &inStock); err != nil {
			t.Fatal(err)
		}
		return
	}
	check := func(label string, wantPrice, wantMax, wantOrig int64, wantStock bool) {
		t.Helper()
		p, m, o, s := summary()
		if deref(p) != wantPrice || deref(m) != wantMax || deref(o) != wantOrig || s != wantStock {
			t.Errorf("%s: price %d max %d orig %d inStock %v", label, deref(p), deref(m), deref(o), s)
		}
	}

	check("after create", 119000, 129000, 0, true)
	f.exec("UPDATE product_variants SET price = 99000, original_price = 139000 WHERE sku = 'ASH-WHT-S'")
	check("cheapest variant changed", 99000, 129000, 139000, true)
	f.exec("UPDATE product_variants SET active = false WHERE sku = 'ASH-WHT-S'")
	check("cheapest variant deactivated", 119000, 129000, 0, true)
	f.exec("UPDATE product_variants SET stock = 0 WHERE product_id = $1", id)
	check("all sold out", 119000, 129000, 0, false)
	if c := f.list(ListParams{}).Items[0]; c.InStock {
		t.Error("listing must show the product as sold out")
	}
	f.exec("UPDATE product_variants SET active = false WHERE product_id = $1", id)
	if p, _, _, _ := summary(); p != nil {
		t.Errorf("no active variant must clear the price, got %d", *p)
	}
	if got := f.list(ListParams{}); len(got.Items) != 0 {
		t.Error("a product without active variants must not be listed")
	}
	f.exec("DELETE FROM products WHERE id = $1", id) // cascade must not trip the trigger
}

func TestCreateValidation(t *testing.T) {
	f := newFixture(t)
	cat := f.category("Atasan", true)
	if _, err := f.svc.Create(f.ctx, kaosInput(cat)); err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		mutate func(*Input)
		field  string
	}{
		"duplicate sku in input": {func(in *Input) { in.Variants[1].SKU = "ASH-BLK-S" }, "variants[1].sku"},
		"missing option":         {func(in *Input) { delete(in.Variants[0].Options, "Ukuran") }, "variants[0].options"},
		"unknown option value":   {func(in *Input) { in.Variants[0].Options["Warna"] = "Ungu" }, "variants[0].options"},
		"duplicate combination":  {func(in *Input) { in.Variants[1].Options = in.Variants[0].Options }, "variants[1].options"},
		"original not higher":    {func(in *Input) { in.Variants[0].OriginalPrice = 100000 }, "variants[0].original_price"},
		"zero price":             {func(in *Input) { in.Variants[0].Price = 0 }, "variants[0].price"},
		"bad image url":          {func(in *Input) { in.Images[0].URL = "https://evil.example/x.jpg" }, "images[0]"},
		"no variants":            {func(in *Input) { in.Variants = nil }, "variants"},
		"no attributes, 2 vars":  {func(in *Input) { in.Attributes = nil }, "variants"},
		"unknown category":       {func(in *Input) { in.CategoryID = 999999 }, "category_id"},
		"slug taken":             {func(in *Input) {}, "slug"},
		"sku taken elsewhere": {func(in *Input) {
			in.Slug = "kaos-lain"
			in.Variants[0].SKU = "ASH-BLK-S-X"
			in.Variants[2].SKU, in.Variants[3].SKU = "ASH-WHT-S-X", "ASH-WHT-M-X"
		}, "variants"},
	}
	for name, tc := range cases {
		in := kaosInput(cat)
		if name != "slug taken" && name != "sku taken elsewhere" {
			in.Slug = "kaos-" + validate.Slug(name)
		}
		tc.mutate(&in)
		_, err := f.svc.Create(f.ctx, in)
		var ve validate.Errors
		if !errors.As(err, &ve) || ve[tc.field] == "" {
			t.Errorf("%s: want error on %q, got %v", name, tc.field, err)
		}
	}

	var n int
	f.db.QueryRow(f.ctx, "SELECT count(*) FROM products").Scan(&n)
	if n != 1 {
		t.Errorf("failed creates must not leave rows behind, products = %d", n)
	}
}

// The schema itself refuses variant data that mixes products or attributes.
func TestDatabaseRejectsInconsistentVariantData(t *testing.T) {
	f := newFixture(t)
	cat := f.category("Atasan", true)
	kaos, err := f.svc.Create(f.ctx, kaosInput(cat))
	if err != nil {
		t.Fatal(err)
	}
	other := f.simple(cat, "Tumbler", "Ashanum", 99000, 0, func(in *Input) {
		in.Attributes = []AttributeInput{{Name: "Kapasitas", Values: []string{"500 ml"}}}
		in.Variants[0].Options = map[string]string{"Kapasitas": "500 ml"}
	})

	var variant, warna, ukuran, hitam, mValue, kapasitas, ml500 int64
	q := func(sql string, dst *int64, args ...any) {
		t.Helper()
		if err := f.db.QueryRow(f.ctx, sql, args...).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	q("SELECT id FROM product_variants WHERE sku = 'ASH-BLK-S'", &variant)
	q("SELECT id FROM product_attributes WHERE product_id = $1 AND name = 'Warna'", &warna, kaos)
	q("SELECT id FROM product_attributes WHERE product_id = $1 AND name = 'Ukuran'", &ukuran, kaos)
	q("SELECT id FROM product_attribute_values WHERE attribute_id = $1 AND value = 'Hitam'", &hitam, warna)
	q("SELECT id FROM product_attribute_values WHERE attribute_id = $1 AND value = 'M'", &mValue, ukuran)
	q("SELECT id FROM product_attributes WHERE product_id = $1", &kapasitas, other)
	q("SELECT id FROM product_attribute_values WHERE attribute_id = $1", &ml500, kapasitas)

	insert := "INSERT INTO product_variant_values (product_id, variant_id, attribute_id, value_id) VALUES ($1, $2, $3, $4)"
	for name, args := range map[string][]any{
		"attribute of another product":   {kaos, variant, kapasitas, ml500},
		"value of another attribute":     {kaos, variant, warna, mValue},
		"second value for one attribute": {kaos, variant, warna, hitam},
		"variant of another product":     {other, variant, kapasitas, ml500},
	} {
		if _, err := f.db.Exec(f.ctx, insert, args...); err == nil {
			t.Errorf("%s: insert must fail", name)
		}
	}

	var pgErr *pgconn.PgError
	if _, err := f.db.Exec(f.ctx, "DELETE FROM categories WHERE id = $1", cat); !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Errorf("deleting a category with products must be refused, got %v", err)
	}
	if _, err := f.db.Exec(f.ctx, "UPDATE product_variants SET original_price = price WHERE id = $1", variant); err == nil {
		t.Error("original_price <= price must be refused by the database")
	}
}

func TestHomeSections(t *testing.T) {
	f := newFixture(t)
	f.catalog()
	h, err := f.svc.Home(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "flash sale", namesOf(h.FlashSale), "Pashmina Ceruti")
	equal(t, "best sellers", namesOf(h.BestSellers), "Bergo Jersey", "Pashmina Ceruti", "Tote Kanvas")
	equal(t, "recommended (featured first)", namesOf(h.Recommended), "Tote Kanvas", "Bergo Jersey", "Pashmina Ceruti")
}

func namesOf(cards []Card) []string { return names(Page{Items: cards}) }
