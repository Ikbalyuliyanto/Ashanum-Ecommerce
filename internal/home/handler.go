// Package home serves the storefront home page.
package home

import (
	"net/http"

	"ecommerce/internal/category"
	"ecommerce/internal/product"
	"ecommerce/internal/view"
)

// homeCategories is how many categories the home page shows before
// "Lihat Semua".
const homeCategories = 10

type Handler struct {
	products   *product.Service
	categories *category.Service
	view       *view.Renderer
}

func NewHandler(products *product.Service, categories *category.Service, v *view.Renderer) *Handler {
	return &Handler{products: products, categories: categories, view: v}
}

type page struct {
	Categories     []category.Category
	MoreCategories bool
	Sections       product.Home
}

func (h *Handler) Index(w http.ResponseWriter, r *http.Request) {
	cats, err := h.categories.List(r.Context())
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	sections, err := h.products.Home(r.Context())
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	p := page{Categories: cats, Sections: sections}
	if len(cats) > homeCategories {
		p.Categories, p.MoreCategories = cats[:homeCategories], true
	}
	h.view.Render(w, r, http.StatusOK, "home/index", p)
}
