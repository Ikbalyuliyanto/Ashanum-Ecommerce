package product_test

import (
	"context"
	"errors"
	"testing"

	"ecommerce/internal/category"
	"ecommerce/internal/product"
	"ecommerce/internal/testutil"
	"ecommerce/internal/validate"
)

func TestAdminProductManagement(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	svc := product.NewService(product.NewRepository(db))
	c, err := category.NewService(category.NewRepository(db)).Create(ctx, category.Input{Name: "Atasan", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	admin := testutil.CreateAdmin(t, db, "admin@example.com")
	count := func(sql string, args ...any) (n int) { db.QueryRow(ctx, sql, args...).Scan(&n); return }
	field := func(err error, key string) bool {
		var v validate.Errors
		return errors.As(err, &v) && v[key] != ""
	}

	in := product.NewProduct{
		Info:       product.Info{CategoryID: c.ID, Name: "Kemeja Linen", Brand: "Ashanum", Active: true},
		ImageURLs:  []string{"https://res.cloudinary.com/demo/image/upload/kemeja.jpg"},
		Attributes: []product.AttributeInput{{Name: "Warna", Values: []string{"Putih", "Biru Muda"}}, {Name: "Ukuran", Values: []string{"S", "M", "L"}}},
		SKUPrefix:  "kemeja linen", Price: 199000, Stock: 4,
	}
	bad := in
	bad.CategoryID = 999999
	if _, err := svc.CreateByAdmin(ctx, admin, bad); !field(err, "category_id") {
		t.Errorf("unknown category: %v", err)
	}
	bad = in
	bad.ImageURLs = []string{"https://evil.example/x.jpg"}
	if _, err := svc.CreateByAdmin(ctx, admin, bad); !field(err, "images[0]") {
		t.Errorf("non-Cloudinary image: %v", err)
	}
	id, err := svc.CreateByAdmin(ctx, admin, in)
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.AdminGet(ctx, id)
	if err != nil || len(p.Variants) != 6 || len(p.Attributes) != 2 || p.Variants[0].SKU != "KEMEJA-LINEN-PUTIH-S" {
		t.Fatalf("generated variants: %+v %v", p.Variants, err)
	}
	if got := count("SELECT count(*) FROM inventory_adjustments WHERE type = 'INITIAL_STOCK' AND stock_after = 4 AND admin_user_id = $1", admin); got != 6 {
		t.Errorf("initial stock rows = %d, want 6", got)
	}
	if got := count("SELECT count(*) FROM admin_audit_logs WHERE action = 'product.create' AND entity_id = $1", id); got != 1 {
		t.Errorf("create audit = %d", got)
	}
	if _, err := svc.CreateByAdmin(ctx, admin, in); !field(err, "slug") {
		t.Errorf("duplicate slug: %v", err)
	}

	// Edit the product fields; price/stock/rating are not part of the form.
	info := product.Info{CategoryID: c.ID, Name: "Kemeja Linen Premium", Slug: "kemeja-linen-premium", Active: true, Featured: true}
	if err := svc.UpdateInfo(ctx, admin, id, info, nil); err != nil {
		t.Fatal(err)
	}
	if p, _ = svc.AdminGet(ctx, id); p.Name != "Kemeja Linen Premium" || !p.Featured || len(p.Images) != 0 {
		t.Errorf("updated info: %+v", p)
	}
	info.CategoryID = 999999
	if err := svc.UpdateInfo(ctx, admin, id, info, nil); !field(err, "category_id") {
		t.Errorf("update to an unknown category: %v", err)
	}

	// Deactivate: gone from the storefront, kept in the admin.
	if err := svc.SetActive(ctx, admin, id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Detail(ctx, "kemeja-linen-premium"); !errors.Is(err, product.ErrNotFound) {
		t.Errorf("inactive product on the storefront: %v", err)
	}
	if err := svc.SetActive(ctx, admin, id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Detail(ctx, "kemeja-linen-premium"); err != nil {
		t.Errorf("reactivated product: %v", err)
	}
	if got := count("SELECT count(*) FROM admin_audit_logs WHERE action IN ('product.activate', 'product.deactivate') AND entity_id = $1", id); got != 2 {
		t.Errorf("activation audit = %d", got)
	}

	// Variants: a new value is added to its attribute; combinations and
	// SKUs stay unique; price edits never touch stock.
	v := product.VariantInput{SKU: "kemeja-hijau-m", Price: 209000, Stock: 2, Options: map[string]string{"Warna": "Hijau", "Ukuran": "M"}}
	vid, err := svc.AddVariant(ctx, admin, id, v)
	if err != nil {
		t.Fatal(err)
	}
	if got := count("SELECT count(*) FROM product_attribute_values av JOIN product_attributes a ON a.id = av.attribute_id WHERE a.product_id = $1 AND av.value = 'Hijau'", id); got != 1 {
		t.Error("new attribute value not created")
	}
	v.SKU = "KEMEJA-HIJAU-M-2"
	if _, err := svc.AddVariant(ctx, admin, id, v); !field(err, "variant") {
		t.Errorf("duplicate combination: %v", err)
	}
	v.Options = map[string]string{"Warna": "Hijau", "Ukuran": "L"}
	v.SKU = "KEMEJA-LINEN-PUTIH-S"
	if _, err := svc.AddVariant(ctx, admin, id, v); !field(err, "sku") {
		t.Errorf("duplicate SKU: %v", err)
	}
	v.Options = map[string]string{"Warna": "Hijau"}
	v.SKU = "KEMEJA-X"
	if _, err := svc.AddVariant(ctx, admin, id, v); err == nil {
		t.Error("a variant needs a value for every attribute")
	}
	stockBefore := count("SELECT stock FROM product_variants WHERE id = $1", vid)
	if err := svc.UpdateVariant(ctx, admin, id, vid, product.VariantUpdate{SKU: "KEMEJA-HIJAU-M", Price: 189000, OriginalPrice: 229000, Active: true}); err != nil {
		t.Fatal(err)
	}
	if got := count("SELECT price FROM product_variants WHERE id = $1", vid); got != 189000 {
		t.Errorf("price = %d", got)
	}
	if got := count("SELECT stock FROM product_variants WHERE id = $1", vid); got != stockBefore {
		t.Errorf("a price edit changed stock: %d → %d", stockBefore, got)
	}
	if got := count("SELECT price FROM products WHERE id = $1", id); got != 189000 {
		t.Errorf("product price summary = %d, want the cheapest active variant", got)
	}
	if err := svc.UpdateVariant(ctx, admin, id, vid, product.VariantUpdate{SKU: "KEMEJA-LINEN-PUTIH-M", Price: 189000, Active: true}); !field(err, "sku") {
		t.Errorf("rename to a taken SKU: %v", err)
	}
	if err := svc.UpdateVariant(ctx, admin, id, vid, product.VariantUpdate{SKU: "KEMEJA-HIJAU-M", Price: 0, Active: true}); !field(err, "price") {
		t.Errorf("zero price: %v", err)
	}
	if err := svc.UpdateVariant(ctx, admin, id, vid, product.VariantUpdate{SKU: "KEMEJA-HIJAU-M", Price: 189000, Active: false}); err != nil {
		t.Fatal(err)
	}
	if got := count("SELECT count(*) FROM admin_audit_logs WHERE action = 'variant.deactivate' AND entity_id = $1", vid); got != 1 {
		t.Errorf("deactivate audit = %d", got)
	}
	if err := svc.UpdateVariant(ctx, admin, 999999, vid, product.VariantUpdate{SKU: "X-1", Price: 1, Active: true}); !errors.Is(err, product.ErrNotFound) {
		t.Errorf("variant of another product: %v", err)
	}

	// A free-form attribute is added to every existing variant.
	if err := svc.AddAttribute(ctx, admin, id, "Bahan", "Linen"); err != nil {
		t.Fatal(err)
	}
	if got := count("SELECT count(*) FROM product_variant_values vv JOIN product_attributes a ON a.id = vv.attribute_id WHERE a.product_id = $1 AND a.name = 'Bahan'", id); got != 7 {
		t.Errorf("variants with the new attribute = %d, want 7", got)
	}
	if err := svc.AddAttribute(ctx, admin, id, "bahan", "Katun"); !field(err, "attribute_name") {
		t.Errorf("duplicate attribute: %v", err)
	}

	page, err := svc.AdminList(ctx, product.AdminFilter{Query: "HIJAU"})
	if err != nil || len(page.Products) != 1 || page.Products[0].ID != id {
		t.Errorf("search by SKU: %+v %v", page.Products, err)
	}
	if page, _ := svc.AdminList(ctx, product.AdminFilter{Status: "inactive"}); len(page.Products) != 0 {
		t.Errorf("inactive filter: %+v", page.Products)
	}
}
