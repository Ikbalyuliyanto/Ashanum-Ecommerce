package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"ecommerce/internal/category"
	"ecommerce/internal/product"
	"ecommerce/internal/review"
)

// Catalog endpoints are public (no token) and read the same storefront
// queries as the web: only active products in active categories.

func (h *Handler) categories(w http.ResponseWriter, r *http.Request) {
	list, err := h.Categories.List(r.Context()) // in-memory cache
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]categoryDTO, len(list))
	for i, c := range list {
		out[i] = categoryOut(c)
	}
	ok(w, http.StatusOK, out)
}

type homeDTO struct {
	Categories  []categoryDTO    `json:"categories"`
	FlashSale   []productCardDTO `json:"flash_sale"`
	BestSellers []productCardDTO `json:"best_sellers"`
	Recommended []productCardDTO `json:"recommended"`
}

// home is the app's home screen in one round trip (categories are cached).
func (h *Handler) home(w http.ResponseWriter, r *http.Request) {
	home, err := h.Products.Home(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	cats, err := h.Categories.List(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := homeDTO{FlashSale: cards(home.FlashSale), BestSellers: cards(home.BestSellers), Recommended: cards(home.Recommended),
		Categories: make([]categoryDTO, len(cats))}
	for i, c := range cats {
		out.Categories[i] = categoryOut(c)
	}
	ok(w, http.StatusOK, out)
}

var sorts = map[string]product.Sort{
	"":            "",
	"relevance":   product.SortRelevance,
	"newest":      product.SortNewest,
	"bestselling": product.SortBestSelling,
	"price_asc":   product.SortPriceAsc,
	"price_desc":  product.SortPriceDesc,
}

// products lists and searches the catalog: q (search), category (slug),
// min_price, max_price, flash_sale, sort, page, per_page. One query.
func (h *Handler) products(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := product.ListParams{Query: q.Get("q")}
	fields := map[string]string{}
	var err error
	ints := map[string]*int{}
	var page, per, minP, maxP int
	ints["page"], ints["per_page"], ints["min_price"], ints["max_price"] = &page, &per, &minP, &maxP
	for k, dst := range ints {
		if *dst, err = queryInt(r, k); err != nil {
			fields[k] = "Harus bilangan bulat ≥ 0."
		}
	}
	sort, known := sorts[q.Get("sort")]
	if !known {
		fields["sort"] = "Pilih relevance, newest, bestselling, price_asc, atau price_desc."
	}
	switch q.Get("flash_sale") {
	case "", "false", "0":
	case "true", "1":
		p.FlashSaleOnly = true
	default:
		fields["flash_sale"] = "Gunakan true atau false."
	}
	if slug := q.Get("category"); slug != "" {
		c, err := h.Categories.BySlug(r.Context(), slug)
		if errors.Is(err, category.ErrNotFound) {
			fields["category"] = "Kategori tidak ditemukan."
		} else if err != nil {
			h.fail(w, r, err)
			return
		}
		p.CategoryID = c.ID
	}
	if len(fields) > 0 {
		h.fail(w, r, badRequest("Parameter tidak valid.", fields))
		return
	}
	p.Sort, p.Page, p.PageSize, p.MinPrice, p.MaxPrice = sort, page, per, int64(minP), int64(maxP)
	res, err := h.Products.List(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	total := res.Total
	okPage(w, cards(res.Items), listMeta{pageMeta: pageMeta{Page: res.Page, PerPage: res.PageSize, Total: &total,
		HasMore: res.HasNext()}, Sort: string(res.Sort)})
}

// listMeta is pageMeta plus the sort actually applied.
type listMeta struct {
	pageMeta
	Sort string `json:"sort"`
}

func (h *Handler) product(w http.ResponseWriter, r *http.Request) {
	d, err := h.Products.Detail(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ok(w, http.StatusOK, productOut(d))
}

type reviewsMeta struct {
	pageMeta
	Rating ratingSummaryDTO `json:"rating"`
}

// productReviews pages a product's visible reviews (hidden ones never
// appear); the rating summary travels in meta.
func (h *Handler) productReviews(w http.ResponseWriter, r *http.Request) {
	page, err := queryInt(r, "page")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	p, err := h.Reviews.ProductPageBySlug(r.Context(), chi.URLParam(r, "slug"), page)
	if errors.Is(err, review.ErrNotFound) {
		h.fail(w, r, product.ErrNotFound)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	okPage(w, publicReviews(p.Reviews), reviewsMeta{pageMeta: pageMeta{Page: p.Page, PerPage: review.ProductPageSize, HasMore: p.HasNext},
		Rating: ratingSummary(p.Summary)})
}
