package product

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"ecommerce/internal/category"
	"ecommerce/internal/view"
)

const relatedLimit = 6

type Handler struct {
	svc        *Service
	categories *category.Service
	view       *view.Renderer
}

func NewHandler(svc *Service, categories *category.Service, v *view.Renderer) *Handler {
	return &Handler{svc: svc, categories: categories, view: v}
}

type detailPage struct {
	Product   Detail
	Selection Selection
	Related   []Card
}

// OptionURL links to this product with a set of chosen option values.
func (p detailPage) OptionURL(valueIDs []int64) string {
	u := "/products/" + p.Product.Slug
	if len(valueIDs) == 0 {
		return u
	}
	parts := make([]string, len(valueIDs))
	for i, id := range valueIDs {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return u + "?o=" + strings.Join(parts, ",")
}

// Detail renders /products/{slug}. Option chips are plain links (?o=…);
// with HTMX only the purchase panel is swapped.
func (h *Handler) Detail(w http.ResponseWriter, r *http.Request) {
	d, err := h.svc.Detail(r.Context(), chi.URLParam(r, "slug"))
	if errors.Is(err, ErrNotFound) {
		h.view.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	page := detailPage{Product: d, Selection: d.Select(parseIDs(r.URL.Query().Get("o")))}

	if view.IsHTMX(r) && r.Header.Get("HX-Target") == "product-buy" {
		h.view.Fragment(w, r, http.StatusOK, "product/detail", "product-buy", page)
		return
	}
	if page.Related, err = h.svc.Related(r.Context(), d, relatedLimit); err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	h.view.Render(w, r, http.StatusOK, "product/detail", page)
}

type sortTab struct {
	Label  string
	URL    string
	Active bool
}

type listingPage struct {
	Title      string
	BasePath   string             // /search or /categories/{slug}
	Category   *category.Category // set on a category page
	Categories []category.Category
	Filter     string // category slug chosen in the /search filter
	Query      string
	MinPrice   string
	MaxPrice   string
	FlashSale  bool
	SortParam  string // sort chosen explicitly in the URL
	Result     Page
	Sorts      []sortTab
}

// URL rebuilds the current listing URL with overrides given as key/value
// pairs; an empty value removes the key. The page number is dropped unless
// overridden, so changing a filter or sort starts at page 1.
func (p listingPage) URL(kv ...string) string {
	v := url.Values{}
	set := func(k, val string) {
		if val != "" {
			v.Set(k, val)
		}
	}
	set("q", p.Query)
	set("category", p.Filter)
	set("min", p.MinPrice)
	set("max", p.MaxPrice)
	if p.FlashSale {
		v.Set("flash", "1")
	}
	set("sort", p.SortParam)
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			v.Del(kv[i])
		} else {
			v.Set(kv[i], kv[i+1])
		}
	}
	if enc := v.Encode(); enc != "" {
		return p.BasePath + "?" + enc
	}
	return p.BasePath
}

// NextURL is the link behind "Muat lebih banyak" (also a plain link).
func (p listingPage) NextURL() string {
	return p.URL("page", strconv.Itoa(p.Result.Page+1))
}

func (p listingPage) PrevURL() string {
	if p.Result.Page <= 2 {
		return p.URL()
	}
	return p.URL("page", strconv.Itoa(p.Result.Page-1))
}

// HasFilters reports whether a price or flash-sale filter is active.
func (p listingPage) HasFilters() bool {
	return p.MinPrice != "" || p.MaxPrice != "" || p.FlashSale || p.Filter != ""
}

// Search renders /search?q=… (without q: all products).
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	params, page := parseListing(r)
	page.BasePath = "/search"

	cats, err := h.categories.List(r.Context())
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	page.Categories = cats
	if slug := r.URL.Query().Get("category"); slug != "" {
		for _, c := range cats {
			if c.Slug == slug {
				params.CategoryID, page.Filter = c.ID, c.Slug
			}
		}
	}

	switch {
	case page.Query != "":
		page.Title = "Hasil pencarian “" + page.Query + "”"
	case page.FlashSale:
		page.Title = "Flash Sale"
	default:
		page.Title = "Semua Produk"
	}
	h.renderListing(w, r, params, page)
}

// CategoryProducts renders /categories/{slug}.
func (h *Handler) CategoryProducts(w http.ResponseWriter, r *http.Request) {
	c, err := h.categories.BySlug(r.Context(), chi.URLParam(r, "slug"))
	if errors.Is(err, category.ErrNotFound) {
		h.view.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	params, page := parseListing(r)
	params.Query, page.Query = "", "" // the header search goes to /search
	params.CategoryID = c.ID
	page.Category, page.BasePath, page.Title = &c, "/categories/"+c.Slug, c.Name
	h.renderListing(w, r, params, page)
}

func (h *Handler) renderListing(w http.ResponseWriter, r *http.Request, params ListParams, page listingPage) {
	res, err := h.svc.List(r.Context(), params)
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	page.Result = res
	page.Sorts = sortTabs(page)

	if view.IsHTMX(r) && r.Header.Get("HX-Target") == "load-more" {
		h.view.Fragment(w, r, http.StatusOK, "product/listing", "result-items", page)
		return
	}
	h.view.Render(w, r, http.StatusOK, "product/listing", page)
}

func sortTabs(p listingPage) []sortTab {
	type opt struct {
		sort  Sort
		label string
	}
	opts := []opt{
		{SortNewest, "Terbaru"},
		{SortBestSelling, "Terlaris"},
		{SortPriceAsc, "Harga Terendah"},
		{SortPriceDesc, "Harga Tertinggi"},
	}
	if p.Query != "" {
		opts = append([]opt{{SortRelevance, "Terkait"}}, opts...)
	}
	tabs := make([]sortTab, len(opts))
	for i, o := range opts {
		tabs[i] = sortTab{Label: o.label, URL: p.URL("sort", string(o.sort)), Active: p.Result.Sort == o.sort}
	}
	return tabs
}

// parseListing reads listing query parameters. Invalid numbers are ignored.
func parseListing(r *http.Request) (ListParams, listingPage) {
	q := r.URL.Query()
	page := listingPage{
		Query:     strings.TrimSpace(q.Get("q")),
		MinPrice:  digits(q.Get("min")),
		MaxPrice:  digits(q.Get("max")),
		FlashSale: q.Get("flash") == "1",
	}
	switch s := Sort(q.Get("sort")); s {
	case SortRelevance, SortNewest, SortBestSelling, SortPriceAsc, SortPriceDesc:
		page.SortParam = string(s)
	}
	n, _ := strconv.Atoi(q.Get("page"))
	params := ListParams{
		Query:         page.Query,
		MinPrice:      atoi64(page.MinPrice),
		MaxPrice:      atoi64(page.MaxPrice),
		FlashSaleOnly: page.FlashSale,
		Sort:          Sort(page.SortParam),
		Page:          n,
	}
	return params, page
}

// digits keeps the digits of a rupiah amount typed as "150.000" or "Rp150rb".
func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' && b.Len() < 12 {
			b.WriteRune(r)
		}
	}
	return strings.TrimLeft(b.String(), "0")
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// parseIDs reads "12,15" (at most 8 IDs).
func parseIDs(s string) []int64 {
	var ids []int64
	for _, part := range strings.Split(s, ",") {
		if len(ids) == 8 {
			break
		}
		if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	return ids
}
