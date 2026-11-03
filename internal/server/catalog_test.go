package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/category"
	"ecommerce/internal/product"
)

const testImage = "data:image/svg+xml;base64,PHN2Zy8+"

// seedCatalog creates two categories (one inactive), a variant product,
// 22 simple products (to get a second listing page) and an inactive one.
func seedCatalog(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	cats := category.NewService(category.NewRepository(db))
	products := product.NewService(product.NewRepository(db))

	atasan, err := cats.Create(ctx, category.Input{Name: "Atasan", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	arsip, err := cats.Create(ctx, category.Input{Name: "Arsip", Active: false})
	if err != nil {
		t.Fatal(err)
	}

	create := func(in product.Input) {
		t.Helper()
		if _, err := products.Create(ctx, in); err != nil {
			t.Fatalf("create %s: %v", in.Name, err)
		}
	}
	create(product.Input{
		CategoryID: atasan.ID, Name: "Kaos Ashanum Basic", Brand: "Ashanum", Active: true, FlashSale: true,
		Description: "Kaos katun combed.",
		Images:      []product.ImageInput{{URL: testImage}},
		Attributes: []product.AttributeInput{
			{Name: "Warna", Values: []string{"Hitam", "Putih"}},
			{Name: "Ukuran", Values: []string{"M"}},
		},
		Variants: []product.VariantInput{
			{SKU: "ASH-BLK-M", Price: 129000, OriginalPrice: 159000, Stock: 10, Options: map[string]string{"Warna": "Hitam", "Ukuran": "M"}},
			{SKU: "ASH-WHT-M", Price: 129000, Stock: 0, Options: map[string]string{"Warna": "Putih", "Ukuran": "M"}},
		},
	})
	for i := 1; i <= 22; i++ {
		create(product.Input{
			CategoryID: atasan.ID, Name: fmt.Sprintf("Tunik Polos %02d", i), Brand: "Nura", Active: true,
			Variants: []product.VariantInput{{SKU: fmt.Sprintf("TNK-%02d", i), Price: int64(50000 + i*1000), Stock: 3}},
		})
	}
	create(product.Input{
		CategoryID: atasan.ID, Name: "Produk Nonaktif", Active: false,
		Variants: []product.VariantInput{{SKU: "OFF-1", Price: 10000, Stock: 1}},
	})
	create(product.Input{
		CategoryID: arsip.ID, Name: "Produk Kategori Arsip", Active: true,
		Variants: []product.VariantInput{{SKU: "ARS-1", Price: 10000, Stock: 1}},
	})
	if _, err := db.Exec(ctx, "UPDATE products SET sold_count = 120, rating_average = 4.8, rating_count = 56 WHERE slug = 'kaos-ashanum-basic'"); err != nil {
		t.Fatal(err)
	}
}

func TestStorefrontPages(t *testing.T) {
	base, db := newApp(t)
	seedCatalog(t, db)
	b := newBrowser(t, base)

	// Home: header search, sections, compact cards, mobile bottom navigation.
	res := b.get("/")
	expect(t, res, http.StatusOK,
		`name="q"`, "Flash Sale", "Terlaris", "Rekomendasi Untukmu",
		"Kaos Ashanum Basic", "Rp129.000", "Rp159.000", "-19%", "4.8", "120 terjual",
		`href="/categories/atasan"`, `href="/categories"`, "Keranjang", "Pesanan")
	if strings.Contains(res.body, "Produk Nonaktif") || strings.Contains(res.body, "Produk Kategori Arsip") {
		t.Error("home shows hidden products")
	}

	// Categories.
	res = b.get("/categories")
	expect(t, res, http.StatusOK, "Atasan")
	if strings.Contains(res.body, "Arsip") {
		t.Error("inactive category listed")
	}
	expect(t, b.get("/categories/atasan"), http.StatusOK, "23 produk", "Tunik Polos 22")
	expect(t, b.get("/categories/arsip"), http.StatusNotFound)
	expect(t, b.get("/categories/tidak-ada"), http.StatusNotFound)
}

func TestProductDetailPage(t *testing.T) {
	base, db := newApp(t)
	seedCatalog(t, db)
	b := newBrowser(t, base)

	res := b.get("/products/kaos-ashanum-basic")
	expect(t, res, http.StatusOK, "Kaos Ashanum Basic", "Rp129.000", "Warna", "Hitam", "Putih",
		"Ukuran", "Deskripsi Produk", "Kaos katun combed.", "Pilih Warna, Ukuran",
		"Beli Sekarang", "disabled")

	var hitam, m int64
	ctx := context.Background()
	db.QueryRow(ctx, "SELECT v.id FROM product_attribute_values v JOIN product_attributes a ON a.id = v.attribute_id WHERE a.name = 'Warna' AND v.value = 'Hitam'").Scan(&hitam)
	db.QueryRow(ctx, "SELECT v.id FROM product_attribute_values v JOIN product_attributes a ON a.id = v.attribute_id WHERE a.name = 'Ukuran' AND v.value = 'M'").Scan(&m)

	// Choosing a full combination shows that variant's stock and SKU.
	expect(t, b.get(fmt.Sprintf("/products/kaos-ashanum-basic?o=%d,%d", hitam, m)), http.StatusOK,
		"Stok tersisa 10", "SKU ASH-BLK-M")

	// With HTMX only the purchase panel comes back.
	res = b.do(http.MethodGet, fmt.Sprintf("/products/kaos-ashanum-basic?o=%d", m), nil, true)
	res2 := b.doWithTarget(fmt.Sprintf("/products/kaos-ashanum-basic?o=%d", m), "product-buy")
	expect(t, res, http.StatusOK)
	expect(t, res2, http.StatusOK, `id="product-buy"`, "is-unavailable")
	if strings.Contains(res2.body, "<html") || strings.Contains(res2.body, "Deskripsi Produk") {
		t.Error("HTMX option change must return only the purchase panel")
	}

	expect(t, b.get("/products/produk-nonaktif"), http.StatusNotFound)
	expect(t, b.get("/products/produk-kategori-arsip"), http.StatusNotFound)
	expect(t, b.get("/products/tidak-ada"), http.StatusNotFound)
}

func TestSearchFilterSortAndPagination(t *testing.T) {
	base, db := newApp(t)
	seedCatalog(t, db)
	b := newBrowser(t, base)

	expect(t, b.get("/search?q=kaos"), http.StatusOK, "1 produk", "Kaos Ashanum Basic", `value="kaos"`, "Terkait")
	expect(t, b.get("/search?q=nura"), http.StatusOK, "22 produk")
	expect(t, b.get("/search?q=atasan"), http.StatusOK, "23 produk") // category name
	expect(t, b.get("/search?q=tidakadaprodukini"), http.StatusOK, "0 produk", "Produk tidak ditemukan")
	expect(t, b.get("/search?q=%27%3B+DROP+TABLE+products+--"), http.StatusOK, "0 produk")

	// Price filter + sort: cheapest Tunik first.
	res := b.get("/search?min=50.000&max=53000&sort=price_asc")
	expect(t, res, http.StatusOK, "3 produk")
	if i1, i3 := strings.Index(res.body, "Tunik Polos 01"), strings.Index(res.body, "Tunik Polos 03"); i1 < 0 || i3 < 0 || i1 > i3 {
		t.Error("price_asc order wrong")
	}
	res = b.get("/search?sort=price_desc&category=atasan")
	if strings.Index(res.body, "Kaos Ashanum Basic") > strings.Index(res.body, "Tunik Polos 22") {
		t.Error("price_desc: the most expensive product must come first")
	}
	expect(t, b.get("/search?flash=1"), http.StatusOK, "1 produk", "Kaos Ashanum Basic")

	// Page 1 has 20 cards and a load-more link to page 2.
	res = b.get("/search")
	expect(t, res, http.StatusOK, "23 produk", `id="load-more"`, `href="/search?page=2"`)
	if n := strings.Count(res.body, `href="/products/`); n != 20 {
		t.Errorf("page 1 cards = %d", n)
	}
	// HTMX load-more returns only the next cards, without another link.
	more := b.doWithTarget("/search?page=2", "load-more")
	if n := strings.Count(more.body, `href="/products/`); n != 3 || strings.Contains(more.body, `id="load-more"`) || strings.Contains(more.body, "<html") {
		t.Errorf("load-more fragment: %d cards\n%s", n, more.body)
	}
	expect(t, b.get("/search?page=2"), http.StatusOK, "Halaman sebelumnya")
}

// doWithTarget sends an HTMX GET as if triggered for the element id target.
func (b *browser) doWithTarget(path, target string) response {
	b.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, b.base+path, nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Target", target)
	res, err := b.client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return response{status: res.StatusCode, header: res.Header, body: string(raw)}
}
