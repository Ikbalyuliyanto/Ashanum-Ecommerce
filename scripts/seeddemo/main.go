// Command seeddemo fills an EMPTY development database with a demo Ashanum
// catalog (categories, products, attributes, variants, images) so the
// storefront can be reviewed. Images are generated SVG illustrations stored
// as data: URIs; real product photos belong in Cloudinary.
//
//	go run ./scripts/seeddemo
//
// It refuses to run when APP_ENV=production or when products already exist.
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"os"
	"strings"

	"ecommerce/internal/category"
	"ecommerce/internal/config"
	"ecommerce/internal/database"
	"ecommerce/internal/product"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seeddemo:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.IsProduction() {
		return fmt.Errorf("refusing to seed demo data with APP_ENV=production")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := database.Migrate(ctx, db, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		return err
	}

	var n int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM products").Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("database already has %d products; seed only an empty development database", n)
	}

	cats := category.NewService(category.NewRepository(db))
	products := product.NewService(product.NewRepository(db))

	catIDs := make(map[string]int64)
	for i, c := range demoCategories {
		created, err := cats.Create(ctx, category.Input{
			Name: c.name, Active: true, DisplayOrder: i,
			ImageURL: svgURI(c.shape, "#fbf4e6", c.color),
		})
		if err != nil {
			return fmt.Errorf("category %s: %w", c.name, err)
		}
		catIDs[c.name] = created.ID
	}

	for _, p := range demoProducts {
		id, err := products.Create(ctx, p.input(catIDs[p.category]))
		if err != nil {
			return fmt.Errorf("product %s: %w", p.name, err)
		}
		// Counters normally maintained by the order/review phases.
		if _, err := db.Exec(ctx,
			"UPDATE products SET sold_count = $1, rating_average = $2, rating_count = $3 WHERE id = $4",
			p.sold, p.rating, p.ratingCount, id); err != nil {
			return err
		}
	}
	fmt.Printf("seeded %d categories and %d products\n", len(demoCategories), len(demoProducts))
	return nil
}

type demoCategory struct {
	name, shape, color string
}

var demoCategories = []demoCategory{
	{"Hijab", "scarf", "#8b5a00"},
	{"Gamis & Dress", "dress", "#6e4a2f"},
	{"Atasan", "shirt", "#5e6b4e"},
	{"Mukena", "scarf", "#9a7b4f"},
	{"Tas", "bag", "#7a4b2a"},
	{"Aksesoris", "brooch", "#a06b14"},
	{"Parfum & Perawatan", "bottle", "#7d5a6b"},
	{"Perlengkapan Ibadah", "rug", "#4f5f6e"},
	{"Tumbler & Botol", "tumbler", "#466a6a"},
}

// attr is one option dimension; extra adds a price step per value (e.g. a
// larger volume costs more).
type attr struct {
	name   string
	values []string
	extra  []int64
}

type demoProduct struct {
	category, name, brand, shape, sku string
	desc                              string
	price, original                   int64
	attrs                             []attr
	featured, flash, soldOut          bool
	sold, ratingCount                 int
	rating                            float64
}

var colors = map[string]string{
	"Hitam": "#2b2622", "Putih": "#f4f1ea", "Mocca": "#8a6a4f", "Navy": "#26375c",
	"Dusty Pink": "#c99a98", "Sage": "#8fa382", "Cream": "#e6d8bb", "Maroon": "#6e2433",
}

var codes = map[string]string{
	"Hitam": "BLK", "Putih": "WHT", "Mocca": "MOC", "Navy": "NVY", "Dusty Pink": "DPK",
	"Sage": "SGE", "Cream": "CRM", "Maroon": "MRN",
}

var demoProducts = []demoProduct{
	{category: "Hijab", name: "Pashmina Ceruti Babydoll", brand: "Ashanum", shape: "scarf", sku: "PCB",
		price: 59000, original: 79000, flash: true, sold: 1250, rating: 4.9, ratingCount: 430,
		attrs: []attr{{name: "Warna", values: []string{"Hitam", "Mocca", "Dusty Pink", "Navy"}}},
		desc:  "Pashmina ceruti babydoll yang ringan, jatuh, dan mudah dibentuk.\n\n• Bahan: ceruti babydoll premium\n• Ukuran: 175 × 75 cm\n• Tidak menerawang, adem dipakai seharian"},
	{category: "Hijab", name: "Bergo Instan Jersey Premium", brand: "Ashanum Basic", shape: "scarf", sku: "BIJ",
		price: 45000, sold: 860, rating: 4.8, ratingCount: 212,
		attrs: []attr{{name: "Warna", values: []string{"Hitam", "Sage", "Cream"}}, {name: "Ukuran", values: []string{"S", "M", "L"}}},
		desc:  "Bergo instan berbahan jersey premium, praktis tanpa jarum.\n\n• Pet antem, nyaman untuk aktivitas\n• Bahan stretch, tidak gerah"},
	{category: "Hijab", name: "Segi Empat Voal Motif", brand: "Nura", shape: "scarf", sku: "SEV",
		price: 69000, original: 89000, flash: true, sold: 540, rating: 4.7, ratingCount: 98,
		attrs: []attr{{name: "Motif", values: []string{"Floral", "Geometris", "Paisley"}}},
		desc:  "Hijab segi empat voal dengan motif eksklusif, tepi lasercut rapi.\n\n• Ukuran 115 × 115 cm"},
	{category: "Hijab", name: "Khimar Syar'i Wolfis", brand: "Ashanum Signature", shape: "scarf", sku: "KSW",
		price: 119000, featured: true, sold: 210, rating: 4.8, ratingCount: 64,
		attrs: []attr{{name: "Warna", values: []string{"Hitam", "Maroon", "Navy"}}, {name: "Ukuran", values: []string{"M", "L", "XL"}}},
		desc:  "Khimar syar'i berbahan wolfis yang jatuh dan tidak mudah kusut."},
	{category: "Gamis & Dress", name: "Gamis Ashanum Signature", brand: "Ashanum Signature", shape: "dress", sku: "GAS",
		price: 289000, original: 349000, featured: true, sold: 320, rating: 4.9, ratingCount: 118,
		attrs: []attr{{name: "Warna", values: []string{"Hitam", "Navy", "Sage"}}, {name: "Ukuran", values: []string{"S", "M", "L", "XL"}}},
		desc:  "Gamis signature dengan potongan A-line yang anggun.\n\n• Bahan: crinkle airflow\n• Busui friendly (resleting depan)\n• Saku kanan-kiri"},
	{category: "Gamis & Dress", name: "Dress Linen Basic", brand: "Kayla", shape: "dress", sku: "DLB",
		price: 199000, sold: 95, rating: 4.6, ratingCount: 21,
		attrs: []attr{{name: "Warna", values: []string{"Cream", "Sage"}}, {name: "Ukuran", values: []string{"M", "L"}}},
		desc:  "Dress linen basic yang sejuk untuk harian."},
	{category: "Gamis & Dress", name: "Abaya Kaftan Brokat", brand: "Ashanum Signature", shape: "dress", sku: "AKB",
		price: 389000, featured: true, sold: 12, rating: 5, ratingCount: 4,
		desc: "Abaya kaftan dengan aksen brokat, satu ukuran (all size) hingga LD 120 cm."},
	{category: "Gamis & Dress", name: "Gamis Anak Ceria", brand: "Kayla", shape: "dress", sku: "GAC",
		price: 129000, original: 159000, flash: true, sold: 180, rating: 4.7, ratingCount: 40,
		attrs: []attr{{name: "Ukuran", values: []string{"4-5 th", "6-7 th", "8-9 th"}}},
		desc:  "Gamis anak berbahan katun rayon yang lembut di kulit."},
	{category: "Atasan", name: "Kaos Ashanum Basic", brand: "Ashanum", shape: "shirt", sku: "ASH",
		price: 129000, original: 159000, featured: true, sold: 120, rating: 4.8, ratingCount: 56,
		attrs: []attr{{name: "Warna", values: []string{"Hitam", "Putih"}}, {name: "Ukuran", values: []string{"S", "M", "L"}}},
		desc:  "Kaos basic katun combed 30s, potongan longgar dan menutup panggul."},
	{category: "Atasan", name: "Tunik Rayon Polos", brand: "Nura", shape: "shirt", sku: "TRP",
		price: 99000, sold: 410, rating: 4.7, ratingCount: 87,
		attrs: []attr{{name: "Warna", values: []string{"Mocca", "Sage", "Navy"}}, {name: "Ukuran", values: []string{"M", "L", "XL"}}},
		desc:  "Tunik rayon polos panjang selutut, adem dan mudah dipadupadankan."},
	{category: "Atasan", name: "Kemeja Oversize Katun", brand: "Ashanum Basic", shape: "shirt", sku: "KOK",
		price: 149000, original: 179000, sold: 76, rating: 4.5, ratingCount: 18,
		attrs: []attr{{name: "Warna", values: []string{"Putih", "Cream", "Navy"}}, {name: "Ukuran", values: []string{"M", "L"}}},
		desc:  "Kemeja oversize katun dengan kancing depan."},
	{category: "Atasan", name: "Cardigan Rajut Lembut", brand: "Kayla", shape: "shirt", sku: "CRL",
		price: 135000, soldOut: true, sold: 260, rating: 4.8, ratingCount: 73,
		attrs: []attr{{name: "Warna", values: []string{"Cream", "Mocca"}}},
		desc:  "Cardigan rajut lembut. Stok sedang habis, segera restock."},
	{category: "Mukena", name: "Mukena Travel Parasut", brand: "Ashanum", shape: "scarf", sku: "MTP",
		price: 159000, original: 199000, flash: true, sold: 690, rating: 4.9, ratingCount: 205,
		attrs: []attr{{name: "Warna", values: []string{"Hitam", "Navy", "Sage"}}},
		desc:  "Mukena travel parasut ringan, lengkap dengan pouch."},
	{category: "Mukena", name: "Mukena Katun Jepang Bordir", brand: "Ashanum Signature", shape: "scarf", sku: "MKJ",
		price: 279000, featured: true, sold: 150, rating: 4.9, ratingCount: 47,
		attrs: []attr{{name: "Warna", values: []string{"Putih", "Cream"}}},
		desc:  "Mukena katun Jepang dengan bordir halus di tepi."},
	{category: "Tas", name: "Tote Bag Kanvas Ashanum", brand: "Ashanum", shape: "bag", sku: "TBK",
		price: 89000, sold: 980, rating: 4.8, ratingCount: 260,
		attrs: []attr{{name: "Warna", values: []string{"Cream", "Hitam"}}},
		desc:  "Tote bag kanvas tebal dengan saku dalam dan resleting."},
	{category: "Tas", name: "Sling Bag Kulit Sintetis", brand: "Kayla", shape: "bag", sku: "SBK",
		price: 179000, original: 229000, flash: true, sold: 145, rating: 4.6, ratingCount: 33,
		attrs: []attr{{name: "Warna", values: []string{"Hitam", "Mocca", "Maroon"}}},
		desc:  "Sling bag kulit sintetis dengan tali yang bisa diatur."},
	{category: "Tas", name: "Pouch Serbaguna", brand: "Nura", shape: "bag", sku: "PSG",
		price: 39000, sold: 2100, rating: 4.8, ratingCount: 512,
		desc: "Pouch serbaguna untuk kosmetik atau perlengkapan kecil."},
	{category: "Aksesoris", name: "Bros Mutiara Elegan", brand: "Ashanum", shape: "brooch", sku: "BME",
		price: 25000, sold: 3200, rating: 4.9, ratingCount: 870,
		attrs: []attr{{name: "Model", values: []string{"Bunga", "Bulat", "Daun"}}},
		desc:  "Bros mutiara dengan jarum pengaman."},
	{category: "Aksesoris", name: "Ciput Rajut Anti Pusing", brand: "Ashanum Basic", shape: "scarf", sku: "CRA",
		price: 19000, original: 25000, flash: true, sold: 4100, rating: 4.8, ratingCount: 1100,
		attrs: []attr{{name: "Warna", values: []string{"Hitam", "Putih", "Cream", "Mocca"}}},
		desc:  "Ciput rajut elastis yang tidak menekan kepala."},
	{category: "Aksesoris", name: "Jarum Pentul Warna Isi 50", brand: "Nura", shape: "brooch", sku: "JPW",
		price: 15000, sold: 1800, rating: 4.7, ratingCount: 340,
		desc: "Jarum pentul aneka warna, isi 50 pcs."},
	{category: "Parfum & Perawatan", name: "Parfum Ashanum Oud", brand: "Ashanum Signature", shape: "bottle", sku: "PAO",
		price: 119000, featured: true, sold: 520, rating: 4.9, ratingCount: 190,
		attrs: []attr{
			{name: "Aroma", values: []string{"Oud", "Vanilla", "Musk"}},
			{name: "Volume", values: []string{"30 ml", "50 ml"}, extra: []int64{0, 50000}},
		},
		desc: "Eau de parfum non-alkohol dengan aroma tahan lama."},
	{category: "Parfum & Perawatan", name: "Body Mist Fresh Blossom", brand: "Nura", shape: "bottle", sku: "BMF",
		price: 59000, original: 75000, sold: 640, rating: 4.7, ratingCount: 150,
		attrs: []attr{{name: "Aroma", values: []string{"Blossom", "Citrus"}}},
		desc:  "Body mist segar untuk aktivitas harian."},
	{category: "Parfum & Perawatan", name: "Hand Cream Shea Butter", brand: "Kayla", shape: "bottle", sku: "HCS",
		price: 35000, sold: 300, rating: 4.6, ratingCount: 61,
		desc: "Krim tangan dengan shea butter, cepat meresap."},
	{category: "Perlengkapan Ibadah", name: "Sajadah Travel Lipat", brand: "Ashanum", shape: "rug", sku: "STL",
		price: 79000, original: 99000, flash: true, sold: 430, rating: 4.8, ratingCount: 120,
		attrs: []attr{{name: "Warna", values: []string{"Navy", "Maroon", "Sage"}}},
		desc:  "Sajadah travel tipis yang bisa dilipat kecil."},
	{category: "Perlengkapan Ibadah", name: "Tasbih Digital", brand: "Nura", shape: "brooch", sku: "TDG",
		price: 29000, desc: "Tasbih digital dengan penghitung otomatis. Produk baru."},
	{category: "Tumbler & Botol", name: "Tumbler Stainless Ashanum", brand: "Ashanum", shape: "tumbler", sku: "TSA",
		price: 99000, featured: true, sold: 870, rating: 4.8, ratingCount: 240,
		attrs: []attr{
			{name: "Kapasitas", values: []string{"500 ml", "750 ml"}, extra: []int64{0, 30000}},
			{name: "Warna", values: []string{"Hitam", "Sage", "Cream"}},
		},
		desc: "Tumbler stainless double wall, menjaga suhu hingga 12 jam."},
}

// input builds a product.Input: every combination of attribute values
// becomes a variant with a deterministic SKU and stock.
func (p demoProduct) input(categoryID int64) product.Input {
	in := product.Input{
		CategoryID: categoryID, Name: p.name, Brand: p.brand, Description: p.desc,
		Active: true, Featured: p.featured, FlashSale: p.flash,
	}

	// Images: one per colour (or a single image).
	fg := []string{"#8b5a00"}
	for _, a := range p.attrs {
		if a.name == "Warna" {
			fg = fg[:0]
			for _, v := range a.values {
				fg = append(fg, colors[v])
			}
		}
	}
	for i, c := range fg {
		if i == 4 {
			break
		}
		in.Images = append(in.Images, product.ImageInput{URL: svgURI(p.shape, backgrounds[i%len(backgrounds)], c), Alt: p.name})
	}

	for _, a := range p.attrs {
		in.Attributes = append(in.Attributes, product.AttributeInput{Name: a.name, Values: a.values})
	}

	combos := [][]int{{}}
	for _, a := range p.attrs {
		var next [][]int
		for _, c := range combos {
			for i := range a.values {
				next = append(next, append(append([]int{}, c...), i))
			}
		}
		combos = next
	}
	for _, combo := range combos {
		v := product.VariantInput{Price: p.price, OriginalPrice: p.original, Options: map[string]string{}}
		parts := []string{p.sku}
		for ai, vi := range combo {
			a := p.attrs[ai]
			value := a.values[vi]
			v.Options[a.name] = value
			if a.extra != nil {
				v.Price += a.extra[vi]
				if v.OriginalPrice > 0 {
					v.OriginalPrice += a.extra[vi]
				}
			}
			code, ok := codes[value]
			if !ok {
				code = strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(value))
			}
			parts = append(parts, code)
		}
		v.SKU = strings.Join(parts, "-")
		v.Stock = stockFor(v.SKU)
		if p.soldOut {
			v.Stock = 0
		}
		in.Variants = append(in.Variants, v)
	}
	return in
}

// stockFor gives each SKU a stable stock between 0 and 30 (about one in
// seven is sold out, to show unavailable options).
func stockFor(sku string) int {
	h := fnv.New32a()
	h.Write([]byte(sku))
	n := int(h.Sum32() % 31)
	if n%7 == 0 {
		return 0
	}
	return n
}

var backgrounds = []string{"#f3ede3", "#efe7dc", "#f5efe6", "#ece6db"}

// svgURI draws a simple flat illustration of a product type.
func svgURI(shape, bg, fg string) string {
	var body string
	switch shape {
	case "scarf":
		body = `<path fill="` + fg + `" d="M200 62c-66 0-108 50-108 112v34c0 34-27 76-50 130h316c-23-54-50-96-50-130v-34c0-62-42-112-108-112z"/>` +
			`<ellipse fill="` + bg + `" cx="200" cy="182" rx="46" ry="58"/>`
	case "dress":
		body = `<path fill="` + fg + `" d="M164 62h72l18 44-26 24 58 210H114l58-210-26-24z"/>` +
			`<path fill="` + bg + `" opacity=".35" d="M184 140h32l-6 40h-20z"/>`
	case "shirt":
		body = `<path fill="` + fg + `" d="M152 78l-76 40 32 66 38-20v176h108V164l38 20 32-66-76-40c-10 26-32 38-54 38s-44-12-54-38z"/>`
	case "bag":
		body = `<path fill="none" stroke="` + fg + `" stroke-width="18" d="M146 160v-34a54 54 0 0 1 108 0v34"/>` +
			`<rect fill="` + fg + `" x="96" y="150" width="208" height="186" rx="26"/>`
	case "bottle":
		body = `<rect fill="` + fg + `" x="172" y="70" width="56" height="56" rx="8"/>` +
			`<rect fill="` + fg + `" x="124" y="122" width="152" height="214" rx="44"/>` +
			`<rect fill="` + bg + `" opacity=".5" x="156" y="196" width="88" height="70" rx="8"/>`
	case "tumbler":
		body = `<rect fill="` + fg + `" x="128" y="62" width="144" height="34" rx="12"/>` +
			`<path fill="` + fg + `" d="M140 104h120l-16 236h-88z"/>`
	case "rug":
		body = `<rect fill="` + fg + `" x="92" y="64" width="216" height="272" rx="10"/>` +
			`<path fill="none" stroke="` + bg + `" stroke-width="8" d="M124 300V150a76 76 0 0 1 152 0v150z"/>`
	default: // brooch
		body = `<circle fill="` + fg + `" cx="200" cy="200" r="96"/><circle fill="` + bg + `" cx="200" cy="200" r="44"/>` +
			`<circle fill="` + fg + `" cx="200" cy="200" r="24"/>`
	}
	svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 400 400"><rect width="400" height="400" fill="` + bg + `"/>` +
		`<g stroke="#000" stroke-opacity=".12" stroke-width="3">` + body + `</g></svg>`
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(svg))
}
