package checkout

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/address"
	"ecommerce/internal/cart"
	"ecommerce/internal/category"
	"ecommerce/internal/order"
	"ecommerce/internal/product"
	"ecommerce/internal/testutil"
)

type fixture struct {
	t      *testing.T
	ctx    context.Context
	db     *pgxpool.Pool
	svc    *Service
	carts  *cart.Service
	orders *order.Service
	addrs  *address.Service
	catID  int64
}

func newFixture(t *testing.T) *fixture {
	db := testutil.DB(t)
	addrs := address.NewService(address.NewRepository(db))
	f := &fixture{t: t, ctx: context.Background(), db: db, addrs: addrs,
		svc:    NewService(NewRepository(db), addrs),
		carts:  cart.NewService(cart.NewRepository(db)),
		orders: order.NewService(order.NewRepository(db), nil)}
	c, err := category.NewService(category.NewRepository(db)).Create(f.ctx, category.Input{Name: "Atasan", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	f.catID = c.ID
	return f
}

// product creates a one-variant product and returns its variant id.
func (f *fixture) product(name, sku string, price int64, stock int) int64 {
	f.t.Helper()
	if _, err := product.NewService(product.NewRepository(f.db)).Create(f.ctx, product.Input{
		CategoryID: f.catID, Name: name, Active: true,
		Images:   []product.ImageInput{{URL: "data:image/svg+xml;base64,PHN2Zy8+"}},
		Variants: []product.VariantInput{{SKU: sku, Price: price, Stock: stock}},
	}); err != nil {
		f.t.Fatal(err)
	}
	return f.id("SELECT id FROM product_variants WHERE sku = $1", sku)
}

func (f *fixture) user(email string) (userID, addressID int64) {
	f.t.Helper()
	userID = testutil.CreateUser(f.t, f.db, email)
	a, err := f.addrs.Create(f.ctx, userID, address.Input{
		Label: "Rumah", RecipientName: "Budi Santoso", Phone: "081234567890",
		Province: "Jawa Barat", City: "Kota Bandung", District: "Coblong", Village: "Dago",
		PostalCode: "40135", StreetAddress: "Jl. Ir. H. Juanda No. 100",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return userID, a.ID
}

func (f *fixture) add(userID, variantID int64, qty int) int64 {
	f.t.Helper()
	if _, err := f.carts.AddItem(f.ctx, userID, variantID, qty); err != nil {
		f.t.Fatal(err)
	}
	return f.id("SELECT ci.id FROM cart_items ci JOIN carts c ON c.id = ci.cart_id WHERE c.user_id = $1 AND ci.variant_id = $2", userID, variantID)
}

func (f *fixture) id(sql string, args ...any) int64 {
	f.t.Helper()
	var id int64
	if err := f.db.QueryRow(f.ctx, sql, args...).Scan(&id); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return id
}

func (f *fixture) stock(variantID int64) int64 {
	return f.id("SELECT stock FROM product_variants WHERE id = $1", variantID)
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

var orderNumber = regexp.MustCompile(`^ASH-\d{8}-\d{6,}$`)

func TestCheckoutSelectedCartItems(t *testing.T) {
	f := newFixture(t)
	uid, addrID := f.user("a@example.com")
	a := f.product("Kaos A", "A-1", 100000, 5)
	b := f.product("Kaos B", "B-1", 50000, 5)
	c := f.product("Kaos C", "C-1", 20000, 5)
	itemA, itemB, itemC := f.add(uid, a, 2), f.add(uid, b, 1), f.add(uid, c, 3)

	src := Source{CartItemIDs: []int64{itemA, itemC}}
	d, err := f.svc.Prepare(f.ctx, uid, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Ready() || d.Address == nil || d.Address.ID != addrID || d.Total != 260000 || len(d.Lines) != 2 {
		t.Fatalf("draft: ready %v total %d lines %d", d.Ready(), d.Total, len(d.Lines))
	}

	placed, err := f.svc.PlaceOrder(f.ctx, uid, PlaceRequest{Source: src, AddressID: addrID, ExpectedTotal: d.Total, IdempotencyKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	if !orderNumber.MatchString(placed.OrderNumber) || placed.Total != 260000 || placed.Existing {
		t.Errorf("placed = %+v", placed)
	}

	// Stock decremented; only the purchased cart lines are gone.
	if f.stock(a) != 3 || f.stock(c) != 2 || f.stock(b) != 5 {
		t.Errorf("stock a=%d b=%d c=%d", f.stock(a), f.stock(b), f.stock(c))
	}
	left, _ := f.carts.Get(f.ctx, uid)
	if len(left.Items) != 1 || left.Items[0].ID != itemB {
		t.Errorf("cart after order: %+v", left.Items)
	}

	o, err := f.orders.Get(f.ctx, uid, placed.OrderNumber)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != order.StatusWaitingPayment || o.Subtotal != 260000 || o.ShippingCost != 0 || o.Discount != 0 || o.Total != 260000 {
		t.Errorf("order totals: %+v", o)
	}
	if o.Address.RecipientName != "Budi Santoso" || o.Address.Phone != "+6281234567890" || o.Address.Street != "Jl. Ir. H. Juanda No. 100" || o.Address.PostalCode != "40135" {
		t.Errorf("address snapshot: %+v", o.Address)
	}
	if len(o.Items) != 2 || o.Items[0].ProductName != "Kaos A" || o.Items[0].SKU != "A-1" || o.Items[0].UnitPrice != 100000 ||
		o.Items[0].Quantity != 2 || o.Items[0].Subtotal != 200000 || o.Items[0].ImageURL == "" || o.Items[1].ProductName != "Kaos C" {
		t.Errorf("items: %+v", o.Items)
	}

	// Idempotent retry returns the same order, nothing new is written.
	again, err := f.svc.PlaceOrder(f.ctx, uid, PlaceRequest{Source: src, AddressID: addrID, ExpectedTotal: d.Total, IdempotencyKey: "k1"})
	if err != nil || !again.Existing || again.OrderNumber != placed.OrderNumber {
		t.Errorf("retry: %+v %v", again, err)
	}
	if n := f.id("SELECT count(*) FROM orders"); n != 1 || f.stock(a) != 3 {
		t.Errorf("retry must not create another order: orders=%d stock=%d", n, f.stock(a))
	}
}

func TestBuyNowLeavesCartAlone(t *testing.T) {
	f := newFixture(t)
	uid, addrID := f.user("a@example.com")
	a := f.product("Kaos A", "A-1", 100000, 5)
	other := f.product("Tas", "T-1", 80000, 5)
	inCart := f.add(uid, other, 2)

	src := Source{VariantID: a, Quantity: 2}
	d, err := f.svc.Prepare(f.ctx, uid, src, addrID)
	if err != nil || !d.Ready() || d.Total != 200000 {
		t.Fatalf("draft: %+v %v", d, err)
	}
	placed, err := f.svc.PlaceOrder(f.ctx, uid, PlaceRequest{Source: src, AddressID: addrID, ExpectedTotal: d.Total})
	if err != nil {
		t.Fatal(err)
	}
	if f.stock(a) != 3 {
		t.Errorf("stock = %d", f.stock(a))
	}
	c, _ := f.carts.Get(f.ctx, uid)
	if len(c.Items) != 1 || c.Items[0].ID != inCart || c.Items[0].Quantity != 2 {
		t.Errorf("Buy Now must not touch the cart: %+v", c.Items)
	}
	o, _ := f.orders.Get(f.ctx, uid, placed.OrderNumber)
	if len(o.Items) != 1 || o.Items[0].Quantity != 2 {
		t.Errorf("items: %+v", o.Items)
	}
}

func TestOwnershipIsEnforced(t *testing.T) {
	f := newFixture(t)
	alice, aliceAddr := f.user("alice@example.com")
	bob, bobAddr := f.user("bob@example.com")
	v := f.product("Kaos A", "A-1", 100000, 5)
	aliceItem := f.add(alice, v, 1)

	// Bob cannot check out Alice's cart lines.
	if _, err := f.svc.Prepare(f.ctx, bob, Source{CartItemIDs: []int64{aliceItem}}, 0); !errors.Is(err, ErrNothingToCheckout) {
		t.Errorf("prepare with another user's cart line: %v", err)
	}
	if _, err := f.svc.PlaceOrder(f.ctx, bob, PlaceRequest{Source: Source{CartItemIDs: []int64{aliceItem}}, AddressID: bobAddr}); !errors.Is(err, ErrNothingToCheckout) {
		t.Errorf("place with another user's cart line: %v", err)
	}
	// Alice cannot ship to Bob's address.
	if _, err := f.svc.PlaceOrder(f.ctx, alice, PlaceRequest{Source: Source{CartItemIDs: []int64{aliceItem}}, AddressID: bobAddr}); !errors.Is(err, ErrAddressRequired) {
		t.Errorf("another user's address: %v", err)
	}
	if f.stock(v) != 5 || f.id("SELECT count(*) FROM orders") != 0 {
		t.Error("refused checkouts must not change stock or create orders")
	}

	placed, err := f.svc.PlaceOrder(f.ctx, alice, PlaceRequest{Source: Source{CartItemIDs: []int64{aliceItem}}, AddressID: aliceAddr})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.orders.Get(f.ctx, bob, placed.OrderNumber); !errors.Is(err, order.ErrNotFound) {
		t.Errorf("bob reading alice's order: %v", err)
	}
	if page, _ := f.orders.List(f.ctx, bob, "", 1); len(page.Orders) != 0 {
		t.Errorf("bob's history shows %d orders", len(page.Orders))
	}
}

func TestSnapshotsSurviveLaterChanges(t *testing.T) {
	f := newFixture(t)
	uid, addrID := f.user("a@example.com")
	v := f.product("Kaos A", "A-1", 100000, 5)
	placed, err := f.svc.PlaceOrder(f.ctx, uid, PlaceRequest{Source: Source{VariantID: v, Quantity: 1}, AddressID: addrID})
	if err != nil {
		t.Fatal(err)
	}

	f.exec("UPDATE products SET name = 'Kaos Ganti Nama'")
	f.exec("UPDATE product_variants SET price = 999000")
	f.exec("UPDATE user_addresses SET recipient_name = 'Orang Lain', street_address = 'Jl. Baru No. 1'")
	// A variant held by an unpaid order cannot be deleted (migration 0007);
	// once the order is completed the catalog row may go.
	f.exec("UPDATE orders SET status = 'COMPLETED'")
	f.exec("DELETE FROM products")

	o, err := f.orders.Get(f.ctx, uid, placed.OrderNumber)
	if err != nil {
		t.Fatal(err)
	}
	it := o.Items[0]
	if it.ProductName != "Kaos A" || it.UnitPrice != 100000 || it.SKU != "A-1" || it.ProductID != 0 || it.VariantID != 0 {
		t.Errorf("item snapshot changed: %+v", it)
	}
	if o.Address.RecipientName != "Budi Santoso" || o.Address.Street != "Jl. Ir. H. Juanda No. 100" {
		t.Errorf("address snapshot changed: %+v", o.Address)
	}
}

func TestPriceChangeIsDetected(t *testing.T) {
	f := newFixture(t)
	uid, addrID := f.user("a@example.com")
	v := f.product("Kaos A", "A-1", 100000, 5)
	item := f.add(uid, v, 2)
	src := Source{CartItemIDs: []int64{item}}
	d, _ := f.svc.Prepare(f.ctx, uid, src, addrID)

	f.exec("UPDATE product_variants SET price = 120000")
	if _, err := f.svc.PlaceOrder(f.ctx, uid, PlaceRequest{Source: src, AddressID: addrID, ExpectedTotal: d.Total}); !errors.Is(err, ErrPriceChanged) {
		t.Fatalf("stale total: %v", err)
	}
	if f.id("SELECT count(*) FROM orders") != 0 || f.stock(v) != 5 {
		t.Error("a price change must roll back")
	}

	d, _ = f.svc.Prepare(f.ctx, uid, src, addrID)
	if d.Total != 240000 {
		t.Errorf("refreshed total = %d", d.Total)
	}
	placed, err := f.svc.PlaceOrder(f.ctx, uid, PlaceRequest{Source: src, AddressID: addrID, ExpectedTotal: d.Total})
	if err != nil || placed.Total != 240000 {
		t.Errorf("with the new total: %+v %v", placed, err)
	}
}

func TestUnavailableAndShortStockRollBack(t *testing.T) {
	f := newFixture(t)
	uid, addrID := f.user("a@example.com")
	ok := f.product("Kaos A", "A-1", 100000, 5)
	short := f.product("Kaos B", "B-1", 50000, 5)
	itemOK, itemShort := f.add(uid, ok, 1), f.add(uid, short, 4)
	src := Source{CartItemIDs: []int64{itemOK, itemShort}}

	check := func(label string, want error) {
		t.Helper()
		if _, err := f.svc.PlaceOrder(f.ctx, uid, PlaceRequest{Source: src, AddressID: addrID}); !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", label, err, want)
		}
		if f.id("SELECT count(*) FROM orders") != 0 || f.stock(ok) != 5 {
			t.Errorf("%s: nothing may be written (orders %d, stock %d)", label, f.id("SELECT count(*) FROM orders"), f.stock(ok))
		}
		if c, _ := f.carts.Get(f.ctx, uid); len(c.Items) != 2 {
			t.Errorf("%s: cart must be untouched", label)
		}
	}

	f.exec("UPDATE product_variants SET stock = 3 WHERE sku = 'B-1'")
	check("insufficient stock", ErrStockChanged)
	d, _ := f.svc.Prepare(f.ctx, uid, src, addrID)
	if d.Ready() || !d.Blocked() || d.Lines[1].Problem != ProblemInsufficientStock {
		t.Errorf("draft must flag the short line: %+v", d.Lines)
	}
	f.exec("UPDATE product_variants SET stock = 5 WHERE sku = 'B-1'")

	f.exec("UPDATE products SET active = false WHERE slug = 'kaos-b'")
	check("inactive product", ErrUnavailable)
	f.exec("UPDATE products SET active = true")

	f.exec("UPDATE product_variants SET active = false WHERE sku = 'B-1'")
	check("inactive variant", ErrUnavailable)
	f.exec("UPDATE product_variants SET active = true")

	f.exec("UPDATE categories SET active = false")
	check("inactive category", ErrUnavailable)
	f.exec("UPDATE categories SET active = true")

	f.exec("DELETE FROM products WHERE slug = 'kaos-b'") // cart line variant becomes NULL
	check("deleted product/variant", ErrUnavailable)
}

func TestLastItemOnlySellsOnce(t *testing.T) {
	f := newFixture(t)
	last := f.product("Edisi Terbatas", "LTD-1", 300000, 1)
	type buyer struct{ uid, addr int64 }
	var buyers []buyer
	for i := range 6 {
		u, a := f.user(fmt.Sprintf("buyer%d@example.com", i))
		buyers = append(buyers, buyer{u, a})
	}

	var wg sync.WaitGroup
	results := make(chan error, len(buyers))
	for _, b := range buyers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.svc.PlaceOrder(f.ctx, b.uid, PlaceRequest{Source: Source{VariantID: last, Quantity: 1}, AddressID: b.addr})
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	success, outOfStock := 0, 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrStockChanged):
			outOfStock++
			if Message(err) != "Stok produk berubah. Silakan periksa kembali pesanan Anda." {
				t.Errorf("message = %q", Message(err))
			}
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if success != 1 || outOfStock != len(buyers)-1 {
		t.Errorf("success=%d outOfStock=%d", success, outOfStock)
	}
	if f.stock(last) != 0 || f.id("SELECT count(*) FROM orders") != 1 {
		t.Errorf("stock=%d orders=%d", f.stock(last), f.id("SELECT count(*) FROM orders"))
	}
}

func TestOrderNumbersAreUnique(t *testing.T) {
	f := newFixture(t)
	v := f.product("Pouch", "P-1", 10000, 99)
	uid, addr := f.user("a@example.com")

	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := f.svc.PlaceOrder(f.ctx, uid, PlaceRequest{Source: Source{VariantID: v, Quantity: 1}, AddressID: addr})
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if seen[p.OrderNumber] || !orderNumber.MatchString(p.OrderNumber) {
				t.Errorf("bad or duplicate order number %q", p.OrderNumber)
			}
			seen[p.OrderNumber] = true
		}()
	}
	wg.Wait()
	if len(seen) != 12 || f.stock(v) != 87 {
		t.Errorf("orders %d, stock %d", len(seen), f.stock(v))
	}

	// History: newest first, paginated, filtered by tab.
	page, err := f.orders.List(f.ctx, uid, "", 1)
	if err != nil || len(page.Orders) != order.PageSize || !page.HasNext {
		t.Fatalf("page 1: %d orders, next %v, %v", len(page.Orders), page.HasNext, err)
	}
	page2, _ := f.orders.List(f.ctx, uid, "", 2)
	if len(page2.Orders) != 2 || page2.HasNext {
		t.Errorf("page 2: %d orders", len(page2.Orders))
	}
	if p, _ := f.orders.List(f.ctx, uid, "unpaid", 1); len(p.Orders) != order.PageSize || p.Tab.Label != "Belum Bayar" {
		t.Errorf("unpaid tab: %d", len(p.Orders))
	}
	if p, _ := f.orders.List(f.ctx, uid, "completed", 1); len(p.Orders) != 0 {
		t.Errorf("completed tab must be empty, got %d", len(p.Orders))
	}
	if s := page.Orders[0]; s.ItemCount != 1 || s.First.ProductName != "Pouch" || s.Status.Label() != "Belum Bayar" {
		t.Errorf("summary: %+v", s)
	}
}

func TestPrepareRequiresSomethingAndAnAddress(t *testing.T) {
	f := newFixture(t)
	v := f.product("Kaos A", "A-1", 100000, 5)
	if _, err := f.svc.Prepare(f.ctx, 1, Source{}, 0); !errors.Is(err, ErrNothingToCheckout) {
		t.Errorf("empty source: %v", err)
	}
	noAddr := testutil.CreateUser(t, f.db, "no-address@example.com")
	d, err := f.svc.Prepare(f.ctx, noAddr, Source{VariantID: v, Quantity: 1}, 0)
	if err != nil || d.Address != nil || d.Ready() {
		t.Errorf("without address: ready %v, %v", d.Ready(), err)
	}
	if _, err := f.svc.PlaceOrder(f.ctx, noAddr, PlaceRequest{Source: Source{VariantID: v, Quantity: 1}}); !errors.Is(err, ErrAddressRequired) {
		t.Errorf("place without address: %v", err)
	}
	if _, err := f.svc.PlaceOrder(f.ctx, noAddr, PlaceRequest{Source: Source{VariantID: v, Quantity: 100}, AddressID: 1}); !errors.Is(err, ErrStockChanged) {
		t.Errorf("quantity above the limit must be refused: %v", err)
	}
}
