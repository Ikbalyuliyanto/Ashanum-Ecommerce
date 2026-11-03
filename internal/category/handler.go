package category

import (
	"net/http"

	"ecommerce/internal/view"
)

type Handler struct {
	svc  *Service
	view *view.Renderer
}

func NewHandler(svc *Service, v *view.Renderer) *Handler { return &Handler{svc: svc, view: v} }

// Index renders "Kategori": every active category (mobile tab target).
func (h *Handler) Index(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.List(r.Context())
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	h.view.Render(w, r, http.StatusOK, "category/index", list)
}
