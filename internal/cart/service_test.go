package cart

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/category"
	"ecommerce/internal/product"
	"ecommerce/internal/testutil"
)

type fixture struct {
	t   *testing.T
	ctx context.Context
	db  *pgxpool.Pool
	svc *Service
}

func newFixture(t *testing.T) *fixture {
	db := testutil.DB(t)
	return &fixture{t: t, ctx: context.Background(), db: db, svc: NewService(NewRepository(db))}
}

// kaos creates "Kaos" with Warna {Hitam, Putih} × Ukuran {M}: ASH-BLK-M
// (Rp129.000, stock 5) and ASH-WHT-M (Rp119.000, stock 0). Returns variant
// IDs by SKU.
func (f *fixture) kaos() map[string]int64 {
	f.t.Helper()
	cats := category.NewService(category.NewRepository(f.db))
	c, err := cats.Create(f.ctx, category.Input{Name: "Atasan", Active: true})
	if err != nil {
		f.t.Fatal(err)
	}
	products := product.NewService(product.NewRepository(f.db))
	_, err = products.Create(f.ctx, product.Input{
		CategoryID: c.ID, Name: "Kaos Ashanum", Active: true,
		Attributes: []product.AttributeInput{{Name: "Warna", Values: []string{"Hitam", "Putih"}}, {Name: "Ukuran", Values: []string{"M"}}},
		Variants: []product.VariantInput{
			{SKU: "ASH-BLK-M", Price: 129000, OriginalPrice: 159000, Stock: 5, Options: map[string]string{"Warna": "Hitam", "Ukuran": "M"}},
			{SKU: "ASH-WHT-M", Price: 119000, Stock: 0, Options: map[string]string{"Warna": "Putih", "Ukuran": "M"}},
		},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return f.variants()
}

func (f *fixture) simple(name, sku string, price int64, stock int) int64 {
	f.t.Helper()
	var catID int64
	if err := f.db.QueryRow(f.ctx, "SELECT id FROM categories LIMIT 1").Scan(&catID); err != nil {
		f.t.Fatal(err)
	}
	products := product.NewService(product.NewRepository(f.db))
	if _, err := products.Create(f.ctx, product.Input{
		CategoryID: catID, Name: name, Active: true,
		Variants: []product.VariantInput{{SKU: sku, Price: price, Stock: stock}},
	}); err != nil {
		f.t.Fatal(err)
	}
	return f.variants()[sku]
}

func (f *fixture) variants() map[string]int64 {
	rows, err := f.db.Query(f.ctx, "SELECT sku, id FROM product_variants")
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var sku string
		var id int64
		rows.Scan(&sku, &id)
		out[sku] = id
	}
	return out
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) cart(userID int64) Cart {
	f.t.Helper()
	c, err := f.svc.Get(f.ctx, userID)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func quantityErr(t *testing.T, err error) *QuantityError {
	t.Helper()
	var qe *QuantityError
	if !errors.As(err, &qe) {
		t.Fatalf("want QuantityError, got %v", err)
	}
	return qe
}

func TestAddCreatesCartAndMergesSameVariant(t *testing.T) {
	f := newFixture(t)
	v := f.kaos()
	uid := testutil.CreateUser(t, f.db, "a@example.com")

	if c := f.cart(uid); len(c.Items) != 0 || f.count("SELECT count(*) FROM carts") != 0 {
		t.Fatal("viewing an empty cart must not create one")
	}
	res, err := f.svc.AddItem(f.ctx, uid, v["ASH-BLK-M"], 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.LineQuantity != 2 || res.CartQuantity != 2 || res.Line.Price != 129000 {
		t.Errorf("first add: %+v", res)
	}
	if f.count("SELECT count(*) FROM carts WHERE user_id = $1 AND active", uid) != 1 {
		t.Error("first add must create exactly one active cart")
	}

	res, err = f.svc.AddItem(f.ctx, uid, v["ASH-BLK-M"], 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.LineQuantity != 5 || res.CartQuantity != 5 {
		t.Errorf("same variant again must add up: %+v", res)
	}
	if n := f.count("SELECT count(*) FROM cart_items"); n != 1 {
		t.Errorf("same variant must stay one line, got %d", n)
	}

	c := f.cart(uid)
	it := c.Items[0]
	if it.VariantLabel != "Hitam / M" || it.SKU != "ASH-BLK-M" || it.Status != StatusAvailable || it.DiscountPercent() != 19 {
		t.Errorf("item = %+v", it)
	}
	if n, _ := f.svc.Count(f.ctx, uid); n != 5 || c.Quantity() != 5 {
		t.Errorf("badge = %d / %d, want 5", n, c.Quantity())
	}
}

func TestQuantityAndStockValidation(t *testing.T) {
	f := newFixture(t)
	v := f.kaos()
	uid := testutil.CreateUser(t, f.db, "a@example.com")

	for _, q := range []int{0, -1, 100} {
		if _, err := f.svc.AddItem(f.ctx, uid, v["ASH-BLK-M"], q); quantityErr(t, err).Requested != q {
			t.Errorf("quantity %d must be refused", q)
		}
	}
	qe := quantityErr(t, func() error { _, err := f.svc.AddItem(f.ctx, uid, v["ASH-BLK-M"], 6); return err }())
	if qe.Available != 5 || Message(qe) != "Stok tersedia hanya 5." {
		t.Errorf("over stock: %+v %q", qe, Message(qe))
	}

	if _, err := f.svc.AddItem(f.ctx, uid, v["ASH-BLK-M"], 4); err != nil {
		t.Fatal(err)
	}
	qe = quantityErr(t, func() error { _, err := f.svc.AddItem(f.ctx, uid, v["ASH-BLK-M"], 2); return err }())
	if qe.InCart != 4 || qe.Available != 5 {
		t.Errorf("in cart + new > stock: %+v", qe)
	}
	if msg := Message(qe); msg == "" {
		t.Error("message expected")
	}
	if c := f.cart(uid); c.Items[0].Quantity != 4 {
		t.Errorf("a refused add must not change the line, quantity = %d", c.Items[0].Quantity)
	}
}

func TestUnavailableProductsAreRefused(t *testing.T) {
	f := newFixture(t)
	v := f.kaos()
	uid := testutil.CreateUser(t, f.db, "a@example.com")

	if qe := quantityErr(t, func() error { _, err := f.svc.AddItem(f.ctx, uid, v["ASH-WHT-M"], 1); return err }()); qe.Available != 0 {
		t.Errorf("zero stock: %+v", qe)
	}
	if _, err := f.svc.AddItem(f.ctx, uid, 999999, 1); !errors.Is(err, ErrUnavailable) {
		t.Errorf("unknown variant: %v", err)
	}
	for name, sql := range map[string]string{
		"inactive variant":  "UPDATE product_variants SET active = false WHERE sku = 'ASH-BLK-M'",
		"inactive product":  "UPDATE products SET active = false",
		"inactive category": "UPDATE categories SET active = false",
	} {
		f.exec(sql)
		if _, err := f.svc.AddItem(f.ctx, uid, v["ASH-BLK-M"], 1); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: err = %v", name, err)
		}
		f.exec("UPDATE product_variants SET active = true; UPDATE products SET active = true; UPDATE categories SET active = true")
	}
	if f.count("SELECT count(*) FROM cart_items") != 0 {
		t.Error("refused adds must not create lines")
	}
}

func TestUpdateRemoveClear(t *testing.T) {
	f := newFixture(t)
	v := f.kaos()
	tunik := f.simple("Tunik", "TNK-1", 50000, 3)
	uid := testutil.CreateUser(t, f.db, "a@example.com")
	f.svc.AddItem(f.ctx, uid, v["ASH-BLK-M"], 1)
	f.svc.AddItem(f.ctx, uid, tunik, 1)
	c := f.cart(uid)
	kaosID, tunikID := c.Items[1].ID, c.Items[0].ID // newest first

	if err := f.svc.UpdateQuantity(f.ctx, uid, kaosID, 5); err != nil {
		t.Fatal(err)
	}
	for _, q := range []int{0, 6, 100} {
		if err := f.svc.UpdateQuantity(f.ctx, uid, kaosID, q); quantityErr(t, err) == nil {
			t.Errorf("update to %d must be refused", q)
		}
	}
	if err := f.svc.RemoveItem(f.ctx, uid, tunikID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RemoveItem(f.ctx, uid, tunikID); !errors.Is(err, ErrItemNotFound) {
		t.Errorf("removing twice: %v", err)
	}
	c = f.cart(uid)
	if len(c.Items) != 1 || c.Items[0].Quantity != 5 {
		t.Fatalf("after update/remove: %+v", c.Items)
	}

	all := c.Summarize(func(int64) bool { return true })
	if all.Subtotal != 5*129000 || all.Quantity != 5 || all.Lines != 1 {
		t.Errorf("summary = %+v", all)
	}

	if err := f.svc.Clear(f.ctx, uid); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.svc.Count(f.ctx, uid); n != 0 {
		t.Errorf("after clear badge = %d", n)
	}
}

// The cart shows catalog changes made after items were added, never crashes
// and never counts unavailable lines.
func TestCartFollowsCatalogChanges(t *testing.T) {
	f := newFixture(t)
	v := f.kaos()
	tunik := f.simple("Tunik", "TNK-1", 50000, 3)
	gamis := f.simple("Gamis", "GMS-1", 250000, 2)
	uid := testutil.CreateUser(t, f.db, "a@example.com")
	f.svc.AddItem(f.ctx, uid, v["ASH-BLK-M"], 3)
	f.svc.AddItem(f.ctx, uid, tunik, 2)
	f.svc.AddItem(f.ctx, uid, gamis, 1)

	f.exec("UPDATE product_variants SET stock = 2, price = 99000 WHERE sku = 'ASH-BLK-M'") // less stock, new price
	f.exec("UPDATE product_variants SET stock = 0 WHERE sku = 'TNK-1'")
	f.exec("UPDATE products SET active = false WHERE slug = 'gamis'")

	status := map[string]Item{}
	for _, it := range f.cart(uid).Items {
		status[it.SKU] = it
	}
	if it := status["ASH-BLK-M"]; it.Status != StatusInsufficientStock || it.Stock != 2 || it.Price != 99000 {
		t.Errorf("kaos: %+v", it)
	}
	if status["TNK-1"].Status != StatusOutOfStock {
		t.Errorf("tunik: %+v", status["TNK-1"])
	}
	if status["GMS-1"].Status != StatusUnavailable {
		t.Errorf("gamis: %+v", status["GMS-1"])
	}
	if s := f.cart(uid).Summarize(func(int64) bool { return true }); s.Lines != 0 || s.Subtotal != 0 {
		t.Errorf("blocked lines must not count: %+v", s)
	}

	// Lowering to the available stock fixes the line; raising is refused.
	kaos := status["ASH-BLK-M"]
	if err := f.svc.UpdateQuantity(f.ctx, uid, kaos.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.UpdateQuantity(f.ctx, uid, status["GMS-1"].ID, 1); !errors.Is(err, ErrUnavailable) {
		t.Errorf("unavailable line update: %v", err)
	}

	// A deleted variant leaves a line that can only be removed.
	f.exec("DELETE FROM products WHERE slug = 'gamis'")
	var gone Item
	for _, it := range f.cart(uid).Items {
		if it.VariantID == 0 {
			gone = it
		}
	}
	if gone.ID == 0 || gone.Status != StatusUnavailable || gone.ProductName != "" {
		t.Fatalf("deleted variant line: %+v", gone)
	}
	if err := f.svc.RemoveItem(f.ctx, uid, gone.ID); err != nil {
		t.Errorf("deleted variant line must be removable: %v", err)
	}
	if s := f.cart(uid).Summarize(func(int64) bool { return true }); s.Lines != 1 || s.Subtotal != 2*99000 {
		t.Errorf("after fixes: %+v", s)
	}
}

func TestOtherUsersCannotTouchCart(t *testing.T) {
	f := newFixture(t)
	v := f.kaos()
	alice := testutil.CreateUser(t, f.db, "alice@example.com")
	bob := testutil.CreateUser(t, f.db, "bob@example.com")
	f.svc.AddItem(f.ctx, alice, v["ASH-BLK-M"], 2)
	item := f.cart(alice).Items[0].ID

	if err := f.svc.UpdateQuantity(f.ctx, bob, item, 1); !errors.Is(err, ErrItemNotFound) {
		t.Errorf("bob update: %v", err)
	}
	if err := f.svc.RemoveItem(f.ctx, bob, item); !errors.Is(err, ErrItemNotFound) {
		t.Errorf("bob remove: %v", err)
	}
	if err := f.svc.Clear(f.ctx, bob); err != nil {
		t.Fatal(err)
	}
	if c := f.cart(bob); len(c.Items) != 0 {
		t.Errorf("bob sees %+v", c.Items)
	}
	if c := f.cart(alice); len(c.Items) != 1 || c.Items[0].Quantity != 2 {
		t.Errorf("alice's cart changed: %+v", c.Items)
	}
}

func TestConcurrentAdds(t *testing.T) {
	f := newFixture(t)
	f.kaos()
	big := f.simple("Pouch", "PCH-1", 39000, 50)
	small := f.simple("Bros", "BRS-1", 25000, 5)
	uid := testutil.CreateUser(t, f.db, "a@example.com")

	run := func(variant int64, n int) (ok int) {
		var wg sync.WaitGroup
		var mu sync.Mutex
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := f.svc.AddItem(f.ctx, uid, variant, 1); err == nil {
					mu.Lock()
					ok++
					mu.Unlock()
				} else if Message(err) == "" {
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		wg.Wait()
		return ok
	}

	// First adds race to create the cart: one cart, one line, every unit counted.
	if ok := run(big, 20); ok != 20 {
		t.Errorf("successful adds = %d, want 20", ok)
	}
	if f.count("SELECT count(*) FROM carts WHERE user_id = $1", uid) != 1 {
		t.Error("concurrent first adds must create one cart")
	}
	if n := f.count("SELECT count(*) FROM cart_items WHERE variant_id = $1", big); n != 1 {
		t.Errorf("lines = %d, want 1", n)
	}
	// More concurrent adds than stock: exactly the stock is accepted.
	if ok := run(small, 12); ok != 5 {
		t.Errorf("accepted = %d, want 5 (stock)", ok)
	}
	if q := f.count("SELECT quantity FROM cart_items WHERE variant_id = $1", small); q != 5 {
		t.Errorf("quantity = %d, want 5", q)
	}
}

func TestCartItemLimitAndQuote(t *testing.T) {
	f := newFixture(t)
	f.kaos()
	uid := testutil.CreateUser(t, f.db, "a@example.com")
	ids := make([]int64, 0, MaxItems+1)
	for i := range MaxItems + 1 {
		ids = append(ids, f.simple(fmt.Sprintf("Produk %02d", i), fmt.Sprintf("P-%02d", i), 10000, 9))
	}
	for _, id := range ids[:MaxItems] {
		if _, err := f.svc.AddItem(f.ctx, uid, id, 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.svc.AddItem(f.ctx, uid, ids[MaxItems], 1); !errors.Is(err, ErrCartFull) {
		t.Errorf("line %d: err = %v", MaxItems+1, err)
	}
	if _, err := f.svc.AddItem(f.ctx, uid, ids[0], 1); err != nil {
		t.Errorf("adding to an existing line in a full cart must work: %v", err)
	}

	// Quote validates without touching any cart (Phase 5 Buy Now).
	other := testutil.CreateUser(t, f.db, "b@example.com")
	line, err := f.svc.Quote(f.ctx, ids[MaxItems], 3)
	if err != nil || line.Subtotal() != 30000 || line.Quantity != 3 {
		t.Errorf("quote = %+v, %v", line, err)
	}
	if f.count("SELECT count(*) FROM carts WHERE user_id = $1", other) != 0 {
		t.Error("Quote must not create a cart")
	}
}
