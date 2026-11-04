package admin

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"ecommerce/internal/category"
	"ecommerce/internal/inventory"
	"ecommerce/internal/product"
	"ecommerce/internal/validate"
	"ecommerce/internal/view"
)

// ---------------------------------------------------------------- products

type productsPage struct {
	shell
	Page       product.AdminPage
	Categories []category.AdminCategory
	Pager      pager
}

func (h *Handler) products(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := product.AdminFilter{Query: q.Get("q"), Status: q.Get("status"), CategoryID: int64(queryInt(r, "category")),
		Page: queryInt(r, "page"), PageSize: perPage(r)}
	p, err := h.Products.AdminList(r.Context(), f)
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	cats, err := h.Categories.AdminList(r.Context())
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	h.View.Render(w, r, http.StatusOK, "admin/products", productsPage{shell: shell{Nav: navProducts}, Page: p, Categories: cats,
		Pager: newPager(r, p.Filter.Page, p.HasNext)})
}

// productForm is the new product form as typed (re-shown on errors).
type productForm struct {
	CategoryID    int64
	Name          string
	Slug          string
	Brand         string
	Description   string
	Active        bool
	Featured      bool
	FlashSale     bool
	Images        string
	AttrNames     [3]string
	AttrValues    [3]string
	SKUPrefix     string
	Price         string
	OriginalPrice string
	Stock         string
}

type productNewPage struct {
	shell
	Form       productForm
	Categories []category.AdminCategory
	Errors     validate.Errors
}

func (h *Handler) productNew(w http.ResponseWriter, r *http.Request) {
	h.renderProductNew(w, r, http.StatusOK, productForm{Active: true, Stock: "0"}, nil)
}

func (h *Handler) renderProductNew(w http.ResponseWriter, r *http.Request, status int, f productForm, errs validate.Errors) {
	cats, err := h.Categories.AdminList(r.Context())
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	h.View.Render(w, r, status, "admin/product-new", productNewPage{shell: shell{Nav: navProducts}, Form: f, Categories: cats, Errors: errs})
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (h *Handler) productCreate(w http.ResponseWriter, r *http.Request) {
	if err := view.ParseForm(w, r); err != nil {
		h.View.Error(w, r, http.StatusBadRequest)
		return
	}
	f := productForm{
		CategoryID: int64(parseInt(r.PostFormValue("category_id"))), Name: r.PostFormValue("name"), Slug: r.PostFormValue("slug"),
		Brand: r.PostFormValue("brand"), Description: r.PostFormValue("description"), Active: r.PostFormValue("active") == "1",
		Featured: r.PostFormValue("featured") == "1", FlashSale: r.PostFormValue("flash_sale") == "1", Images: r.PostFormValue("images"),
		SKUPrefix: r.PostFormValue("sku_prefix"), Price: r.PostFormValue("price"), OriginalPrice: r.PostFormValue("original_price"),
		Stock: r.PostFormValue("stock"),
	}
	in := product.NewProduct{
		Info: product.Info{CategoryID: f.CategoryID, Name: f.Name, Slug: strings.TrimSpace(f.Slug), Brand: f.Brand,
			Description: f.Description, Active: f.Active, Featured: f.Featured, FlashSale: f.FlashSale},
		ImageURLs: lines(f.Images), SKUPrefix: f.SKUPrefix,
		Price: parseRupiah(f.Price), OriginalPrice: parseRupiah(f.OriginalPrice), Stock: parseInt(f.Stock),
	}
	for i := range 3 {
		f.AttrNames[i] = r.PostFormValue(fmt.Sprintf("attr_name_%d", i+1))
		f.AttrValues[i] = r.PostFormValue(fmt.Sprintf("attr_values_%d", i+1))
		name := strings.TrimSpace(f.AttrNames[i])
		var values []string
		for _, v := range strings.Split(f.AttrValues[i], ",") {
			if v = strings.TrimSpace(v); v != "" {
				values = append(values, v)
			}
		}
		if name != "" || len(values) > 0 {
			in.Attributes = append(in.Attributes, product.AttributeInput{Name: name, Values: values})
		}
	}
	id, err := h.Products.CreateByAdmin(r.Context(), adminID(r), in)
	var verr validate.Errors
	if errors.As(err, &verr) {
		h.renderProductNew(w, r, http.StatusUnprocessableEntity, f, verr)
		return
	}
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	h.done(w, r, fmt.Sprintf("/admin/products/%d/edit", id), "Produk dibuat. Atur harga, stok, dan SKU tiap varian di bawah.")
}

type productEditPage struct {
	shell
	Product    product.AdminProduct
	Info       product.Info
	Images     string
	Categories []category.AdminCategory
	History    []inventory.Adjustment
	Errors     validate.Errors
	Threshold  int
}

func (h *Handler) productEdit(w http.ResponseWriter, r *http.Request) {
	h.renderProductEdit(w, r, http.StatusOK, pathID(r, "id"), nil, nil, "")
}

// renderProductEdit shows the edit page; info/images override the stored
// values when re-showing a rejected form.
func (h *Handler) renderProductEdit(w http.ResponseWriter, r *http.Request, status int, id int64, info *product.Info, errs validate.Errors, images string) {
	p, err := h.Products.AdminGet(r.Context(), id)
	if errors.Is(err, product.ErrNotFound) {
		h.View.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	cats, err := h.Categories.AdminList(r.Context())
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	hist, err := h.Inventory.ProductHistory(r.Context(), id, 10)
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	data := productEditPage{shell: shell{Nav: navProducts}, Product: p, Categories: cats, History: hist, Errors: errs,
		Threshold: h.Inventory.LowStockThreshold(),
		Info: product.Info{CategoryID: p.CategoryID, Name: p.Name, Slug: p.Slug, Brand: p.Brand, Description: p.Description,
			Active: p.Active, Featured: p.Featured, FlashSale: p.FlashSale}, Images: p.ImageText()}
	if info != nil {
		data.Info, data.Images = *info, images
	}
	h.View.Render(w, r, status, "admin/product-edit", data)
}

func productPath(id int64) string { return fmt.Sprintf("/admin/products/%d/edit", id) }

func (h *Handler) productUpdate(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if err := view.ParseForm(w, r); err != nil {
		h.View.Error(w, r, http.StatusBadRequest)
		return
	}
	info := product.Info{CategoryID: int64(parseInt(r.PostFormValue("category_id"))), Name: r.PostFormValue("name"),
		Slug: strings.TrimSpace(r.PostFormValue("slug")), Brand: r.PostFormValue("brand"), Description: r.PostFormValue("description"),
		Active: r.PostFormValue("active") == "1", Featured: r.PostFormValue("featured") == "1", FlashSale: r.PostFormValue("flash_sale") == "1"}
	images := r.PostFormValue("images")
	err := h.Products.UpdateInfo(r.Context(), adminID(r), id, info, lines(images))
	var verr validate.Errors
	switch {
	case errors.As(err, &verr):
		h.renderProductEdit(w, r, http.StatusUnprocessableEntity, id, &info, verr, images)
	case errors.Is(err, product.ErrNotFound):
		h.View.Error(w, r, http.StatusNotFound)
	case err != nil:
		h.View.ServerError(w, r, err)
	default:
		h.done(w, r, productPath(id), "Produk disimpan.")
	}
}

func (h *Handler) productActivate(w http.ResponseWriter, r *http.Request) {
	h.productSetActive(w, r, true)
}
func (h *Handler) productDeactivate(w http.ResponseWriter, r *http.Request) {
	h.productSetActive(w, r, false)
}

func (h *Handler) productSetActive(w http.ResponseWriter, r *http.Request, active bool) {
	id := pathID(r, "id")
	if err := view.ParseForm(w, r); err != nil {
		h.View.Error(w, r, http.StatusBadRequest)
		return
	}
	err := h.Products.SetActive(r.Context(), adminID(r), id, active)
	if errors.Is(err, product.ErrNotFound) {
		h.View.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	msg := "Produk dinonaktifkan: tidak tampil di toko, riwayat pesanan tetap utuh."
	if active {
		msg = "Produk diaktifkan dan tampil di toko."
	}
	h.done(w, r, returnTo(r, productPath(id)), msg)
}

func (h *Handler) variantCreate(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if err := view.ParseForm(w, r); err != nil {
		h.View.Error(w, r, http.StatusBadRequest)
		return
	}
	opts := map[string]string{}
	for k, v := range r.PostForm {
		if name, ok := strings.CutPrefix(k, "opt:"); ok && len(v) > 0 {
			opts[name] = v[0]
		}
	}
	in := product.VariantInput{SKU: r.PostFormValue("sku"), Price: parseRupiah(r.PostFormValue("price")),
		OriginalPrice: parseRupiah(r.PostFormValue("original_price")), Stock: parseInt(r.PostFormValue("stock")), Options: opts}
	_, err := h.Products.AddVariant(r.Context(), adminID(r), id, in)
	h.catalogResult(w, r, productPath(id)+"#varian", err, "Varian ditambahkan.")
}

func (h *Handler) variantUpdate(w http.ResponseWriter, r *http.Request) {
	id, vid := pathID(r, "id"), pathID(r, "variant")
	if err := view.ParseForm(w, r); err != nil {
		h.View.Error(w, r, http.StatusBadRequest)
		return
	}
	in := product.VariantUpdate{SKU: r.PostFormValue("sku"), Price: parseRupiah(r.PostFormValue("price")),
		OriginalPrice: parseRupiah(r.PostFormValue("original_price")), Active: r.PostFormValue("active") == "1"}
	err := h.Products.UpdateVariant(r.Context(), adminID(r), id, vid, in)
	h.catalogResult(w, r, productPath(id)+"#varian", err, "Varian disimpan.")
}

func (h *Handler) attributeCreate(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if err := view.ParseForm(w, r); err != nil {
		h.View.Error(w, r, http.StatusBadRequest)
		return
	}
	err := h.Products.AddAttribute(r.Context(), adminID(r), id, r.PostFormValue("name"), r.PostFormValue("value"))
	h.catalogResult(w, r, productPath(id)+"#varian", err, "Atribut ditambahkan ke semua varian.")
}

func (h *Handler) catalogResult(w http.ResponseWriter, r *http.Request, to string, err error, msg string) {
	switch {
	case err == nil:
		h.done(w, r, to, msg)
	case errors.Is(err, product.ErrNotFound):
		h.View.Error(w, r, http.StatusNotFound)
	default:
		h.refuse(w, r, to, err)
	}
}

// ---------------------------------------------------------------- stock

type stockPage struct {
	shell
	Page  inventory.Page
	Pager pager
}

func (h *Handler) stock(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, err := h.Inventory.List(r.Context(), inventory.Filter{Status: q.Get("status"), Query: q.Get("q"), Page: queryInt(r, "page"), PageSize: perPage(r)})
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	h.View.Render(w, r, http.StatusOK, "admin/stock", stockPage{shell: shell{Nav: navStock}, Page: p, Pager: newPager(r, p.Filter.Page, p.HasNext)})
}

type stockFormPage struct {
	shell
	Item    inventory.Item
	History []inventory.Adjustment
	Form    inventory.AdjustInput
	Qty     string
	Errors  validate.Errors
	Back    string
}

func (h *Handler) stockForm(w http.ResponseWriter, r *http.Request) {
	h.renderStockForm(w, r, http.StatusOK, pathID(r, "variant"), inventory.AdjustInput{Mode: inventory.ModeAdd}, "", nil)
}

func (h *Handler) renderStockForm(w http.ResponseWriter, r *http.Request, status int, id int64, in inventory.AdjustInput, qty string, errs validate.Errors) {
	it, err := h.Inventory.Variant(r.Context(), id)
	if errors.Is(err, inventory.ErrNotFound) {
		h.View.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	hist, err := h.Inventory.History(r.Context(), id, 20)
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	back := view.SafePath(r.FormValue("return_to"), "")
	if !strings.HasPrefix(back, "/admin") {
		back = "/admin/stock"
	}
	h.View.Render(w, r, status, "admin/stock-form", stockFormPage{shell: shell{Nav: navStock}, Item: it, History: hist, Form: in, Qty: qty, Errors: errs, Back: back})
}

func (h *Handler) stockAdjust(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "variant")
	if err := view.ParseForm(w, r); err != nil {
		h.View.Error(w, r, http.StatusBadRequest)
		return
	}
	qty := r.PostFormValue("quantity")
	in := inventory.AdjustInput{VariantID: id, Mode: r.PostFormValue("mode"), Quantity: parseInt(qty), Reason: r.PostFormValue("reason")}
	adj, err := h.Inventory.Adjust(r.Context(), adminID(r), in)
	var verr validate.Errors
	switch {
	case errors.As(err, &verr):
		h.renderStockForm(w, r, http.StatusUnprocessableEntity, id, in, qty, verr)
	case errors.Is(err, inventory.ErrNotFound):
		h.View.Error(w, r, http.StatusNotFound)
	case err != nil:
		h.View.ServerError(w, r, err)
	default:
		h.done(w, r, fmt.Sprintf("/admin/stock/%d", id), fmt.Sprintf("Stok %s: %d → %d.", adj.SKU, adj.Before, adj.After))
	}
}

// ---------------------------------------------------------------- categories

type categoriesPage struct {
	shell
	Categories []category.AdminCategory
	Form       category.Input
	Errors     validate.Errors
}

func (h *Handler) categories(w http.ResponseWriter, r *http.Request) {
	h.renderCategories(w, r, http.StatusOK, category.Input{Active: true}, nil)
}

func (h *Handler) renderCategories(w http.ResponseWriter, r *http.Request, status int, in category.Input, errs validate.Errors) {
	list, err := h.Categories.AdminList(r.Context())
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	h.View.Render(w, r, status, "admin/categories", categoriesPage{shell: shell{Nav: navCategories}, Categories: list, Form: in, Errors: errs})
}

func categoryInput(r *http.Request) category.Input {
	return category.Input{Name: r.PostFormValue("name"), Slug: strings.TrimSpace(r.PostFormValue("slug")),
		ImageURL: strings.TrimSpace(r.PostFormValue("image_url")), Active: r.PostFormValue("active") == "1",
		DisplayOrder: optionalInt(r.PostFormValue("display_order"))}
}

func (h *Handler) categoryCreate(w http.ResponseWriter, r *http.Request) {
	if err := view.ParseForm(w, r); err != nil {
		h.View.Error(w, r, http.StatusBadRequest)
		return
	}
	in := categoryInput(r)
	_, err := h.Categories.CreateBy(r.Context(), adminID(r), in)
	var verr validate.Errors
	switch {
	case errors.As(err, &verr):
		h.renderCategories(w, r, http.StatusUnprocessableEntity, in, verr)
	case err != nil:
		h.View.ServerError(w, r, err)
	default:
		h.done(w, r, "/admin/categories", "Kategori ditambahkan.")
	}
}

type categoryEditPage struct {
	shell
	Category category.Category
	Form     category.Input
	Errors   validate.Errors
}

func (h *Handler) categoryEdit(w http.ResponseWriter, r *http.Request) {
	c, err := h.Categories.Get(r.Context(), pathID(r, "id"))
	if errors.Is(err, category.ErrNotFound) {
		h.View.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	in := category.Input{Name: c.Name, Slug: c.Slug, ImageURL: c.ImageURL, Active: c.Active, DisplayOrder: c.DisplayOrder}
	h.View.Render(w, r, http.StatusOK, "admin/category-edit", categoryEditPage{shell: shell{Nav: navCategories}, Category: c, Form: in})
}

func (h *Handler) categoryUpdate(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if err := view.ParseForm(w, r); err != nil {
		h.View.Error(w, r, http.StatusBadRequest)
		return
	}
	in := categoryInput(r)
	c, err := h.Categories.UpdateBy(r.Context(), adminID(r), id, in)
	var verr validate.Errors
	switch {
	case errors.As(err, &verr):
		h.View.Render(w, r, http.StatusUnprocessableEntity, "admin/category-edit",
			categoryEditPage{shell: shell{Nav: navCategories}, Category: category.Category{ID: id, Name: in.Name}, Form: in, Errors: verr})
	case errors.Is(err, category.ErrNotFound):
		h.View.Error(w, r, http.StatusNotFound)
	case err != nil:
		h.View.ServerError(w, r, err)
	default:
		h.done(w, r, "/admin/categories", "Kategori "+c.Name+" disimpan.")
	}
}
