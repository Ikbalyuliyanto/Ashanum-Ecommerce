// Package home serves the storefront landing page.
package home

import (
	"net/http"

	"ecommerce/internal/view"
)

type Handler struct {
	view *view.Renderer
}

func NewHandler(v *view.Renderer) *Handler { return &Handler{view: v} }

func (h *Handler) Index(w http.ResponseWriter, r *http.Request) {
	h.view.Render(w, r, http.StatusOK, "home/index", nil)
}
