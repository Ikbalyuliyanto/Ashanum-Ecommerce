package returns_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/address"
	"ecommerce/internal/cart"
	"ecommerce/internal/category"
	"ecommerce/internal/checkout"
	"ecommerce/internal/product"
	"ecommerce/internal/returns"
	"ecommerce/internal/testutil"
	"ecommerce/internal/validate"
)

type fixture struct {
	t        *testing.T
	ctx      context.Context
	db       *pgxpool.Pool
	svc      *returns.Service
	checkout *checkout.Service
	carts    *cart.Service
	addrs    *address.Service
	catID    int64
}

func newFixture(t *testing.T) *fixture {
	db := testutil.DB(t)
	f := &fixture{t: t, ctx: context.Background(), db: db,
		svc:   returns.NewService(returns.NewRepository(db), returns.Options{WindowDays: 7}),
		carts: cart.NewService(cart.NewRepository(db)), addrs: address.NewService(address.NewRepository(db))}
	f.checkout = checkout.NewService(checkout.NewRepository(db), f.addrs)
	c, err := category.NewService(category.NewRepository(db)).Create(f.ctx, category.Input{Name: "Atasan", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	f.catID = c.ID
	return f
}

func (f *fixture) product(name, sku string, price int64, stock int) int64 {
	f.t.Helper()
	if _, err := product.NewService(product.NewRepository(f.db)).Create(f.ctx, product.Input{
		CategoryID: f.catID, Name: name, Active: true,
		Images:   []product.ImageInput{{URL: "data:image/svg+xml;base64,PHN2Zy8+"}},
		Variants: []product.VariantInput{{SKU: sku, Price: price, Stock: stock}},
	}); err != nil {
		f.t.Fatal(err)
	}
	return f.int("SELECT id FROM product_variants WHERE sku = $1", sku)
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

type line struct {
	variant int64
	qty     int
}

// order buys the lines through the cart and returns the order number.
func (f *fixture) order(userID, addressID int64, lines ...line) string {
	f.t.Helper()
	var ids []int64
	for _, l := range lines {
		if _, err := f.carts.AddItem(f.ctx, userID, l.variant, l.qty); err != nil {
			f.t.Fatal(err)
		}
		ids = append(ids, f.int("SELECT ci.id FROM cart_items ci JOIN carts c ON c.id = ci.cart_id WHERE c.user_id = $1 AND ci.variant_id = $2", userID, l.variant))
	}
	placed, err := f.checkout.PlaceOrder(f.ctx, userID, checkout.PlaceRequest{Source: checkout.Source{CartItemIDs: ids}, AddressID: addressID})
	if err != nil {
		f.t.Fatal(err)
	}
	return placed.OrderNumber
}

// completed is an order completed (and paid) ago.
func (f *fixture) completed(userID, addressID int64, ago time.Duration, lines ...line) string {
	number := f.order(userID, addressID, lines...)
	testutil.CompleteOrder(f.t, f.db, number, time.Now().Add(-ago))
	return number
}

func (f *fixture) item(number string, variant int64) int64 {
	return f.int("SELECT i.id FROM order_items i JOIN orders o ON o.id = i.order_id WHERE o.order_number = $1 AND i.variant_id = $2", number, variant)
}

func (f *fixture) int(sql string, args ...any) int64 {
	f.t.Helper()
	var n int64
	if err := f.db.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *fixture) id(number string) int64 {
	return f.int("SELECT id FROM returns WHERE return_number = $1", number)
}

func request(order string, typ returns.Type, items ...returns.ItemRequest) returns.Request {
	return returns.Request{OrderNumber: order, Items: items, Type: typ, Reason: returns.ReasonDamaged}
}

func isValidation(err error) bool {
	var v validate.Errors
	return errors.As(err, &v)
}

func TestReturnEligibility(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	other, _ := f.user("b@example.com")
	v := f.product("Kaos A", "A-1", 100000, 50)

	ok := f.completed(uid, addr, time.Hour, line{v, 1})
	d, err := f.svc.Draft(f.ctx, uid, ok)
	if err != nil || len(d.Items) != 1 || d.Items[0].Remaining() != 1 {
		t.Fatalf("eligible draft: %+v %v", d, err)
	}
	if want := d.Deadline.Sub(time.Now().Add(-time.Hour)); want < 7*24*time.Hour-time.Minute || want > 7*24*time.Hour+time.Minute {
		t.Errorf("deadline must be completed_at + 7 days: %v", d.Deadline)
	}
	if _, err := f.svc.Draft(f.ctx, other, ok); !errors.Is(err, returns.ErrNotFound) {
		t.Errorf("another user's order: %v", err)
	}
	if _, err := f.svc.Create(f.ctx, other, request(ok, returns.TypeRefund, returns.ItemRequest{OrderItemID: f.item(ok, v), Quantity: 1})); !errors.Is(err, returns.ErrNotFound) {
		t.Errorf("return on another user's order: %v", err)
	}

	unpaid := f.order(uid, addr, line{v, 1}) // WAITING_PAYMENT
	if _, err := f.svc.Draft(f.ctx, uid, unpaid); !errors.Is(err, returns.ErrOrderNotCompleted) {
		t.Errorf("waiting payment: %v", err)
	}
	waiting := f.order(uid, addr, line{v, 1})
	f.exec("UPDATE orders SET status = 'WAITING_CONFIRMATION', paid_at = now() WHERE order_number = $1", waiting)
	if _, err := f.svc.Draft(f.ctx, uid, waiting); !errors.Is(err, returns.ErrOrderNotCompleted) {
		t.Errorf("not completed: %v", err)
	}
	noPayment := f.order(uid, addr, line{v, 1})
	f.exec("UPDATE orders SET status = 'COMPLETED', completed_at = now() WHERE order_number = $1", noPayment)
	if _, err := f.svc.Draft(f.ctx, uid, noPayment); !errors.Is(err, returns.ErrOrderNotPaid) {
		t.Errorf("completed but never paid: %v", err)
	}
	cancelled := f.order(uid, addr, line{v, 1})
	f.exec("UPDATE orders SET status = 'CANCELLED', cancelled_at = now() WHERE order_number = $1", cancelled)
	if _, err := f.svc.Create(f.ctx, uid, request(cancelled, returns.TypeRefund, returns.ItemRequest{OrderItemID: f.item(cancelled, v), Quantity: 1})); !errors.Is(err, returns.ErrOrderNotCompleted) {
		t.Errorf("cancelled unpaid order: %v", err)
	}
	late := f.completed(uid, addr, 8*24*time.Hour, line{v, 1})
	if _, err := f.svc.Draft(f.ctx, uid, late); !errors.Is(err, returns.ErrWindowClosed) {
		t.Errorf("outside the return window: %v", err)
	}
	if _, err := f.svc.Create(f.ctx, uid, request(late, returns.TypeRefund, returns.ItemRequest{OrderItemID: f.item(late, v), Quantity: 1})); !errors.Is(err, returns.ErrWindowClosed) {
		t.Errorf("create outside window: %v", err)
	}

	info, err := f.svc.ForOrder(f.ctx, uid, ok)
	if err != nil || !info.Eligible || info.Deadline == nil {
		t.Errorf("order info: %+v %v", info, err)
	}
	if info, _ := f.svc.ForOrder(f.ctx, uid, late); info.Eligible {
		t.Error("late order must not offer a return")
	}
}

func TestReturnQuantities(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	v := f.product("Kaos A", "A-1", 100000, 50)
	number := f.completed(uid, addr, time.Hour, line{v, 3})
	item := f.item(number, v)

	if _, err := f.svc.Create(f.ctx, uid, request(number, returns.TypeRefund, returns.ItemRequest{OrderItemID: item, Quantity: 4})); !isValidation(err) {
		t.Errorf("more than purchased: %v", err)
	}
	first, err := f.svc.Create(f.ctx, uid, request(number, returns.TypeRefund, returns.ItemRequest{OrderItemID: item, Quantity: 2}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Create(f.ctx, uid, request(number, returns.TypeRefund, returns.ItemRequest{OrderItemID: item, Quantity: 2})); !isValidation(err) {
		t.Errorf("cumulative 2 + 2 > 3 must fail: %v", err)
	}
	if _, err := f.svc.Create(f.ctx, uid, request(number, returns.TypeExchange, returns.ItemRequest{OrderItemID: item, Quantity: 1})); err != nil {
		t.Fatalf("the last unit: %v", err)
	}
	if _, err := f.svc.Draft(f.ctx, uid, number); !errors.Is(err, returns.ErrNothingToReturn) {
		t.Errorf("nothing left: %v", err)
	}
	if info, _ := f.svc.ForOrder(f.ctx, uid, number); info.Eligible || len(info.Returns) != 2 {
		t.Errorf("order info after full return: %+v", info)
	}

	// Withdrawing a request frees its quantity.
	if err := f.svc.CancelByUser(f.ctx, uid, first); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.CancelByUser(f.ctx, uid, first); err != nil {
		t.Errorf("second cancel must be harmless: %v", err)
	}
	d, err := f.svc.Draft(f.ctx, uid, number)
	if err != nil || d.Items[0].Remaining() != 2 {
		t.Errorf("after cancelling: %+v %v", d, err)
	}
}

func TestConcurrentReturns(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	v := f.product("Kaos A", "A-1", 100000, 50)
	number := f.completed(uid, addr, time.Hour, line{v, 1})
	item := f.item(number, v)

	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := request(number, returns.TypeRefund, returns.ItemRequest{OrderItemID: item, Quantity: 1})
			req.IdempotencyKey = fmt.Sprintf("key-%d", i)
			_, err := f.svc.Create(f.ctx, uid, req)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else if !isValidation(err) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || f.int("SELECT coalesce(sum(quantity), 0) FROM return_items") != 1 {
		t.Errorf("successful returns = %d, units = %d", ok, f.int("SELECT coalesce(sum(quantity), 0) FROM return_items"))
	}
}

func TestReturnValidation(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	v := f.product("Kaos A", "A-1", 100000, 50)
	number := f.completed(uid, addr, time.Hour, line{v, 2})
	otherOrder := f.completed(uid, addr, time.Hour, line{v, 1})
	item := f.item(number, v)
	base := func() returns.Request {
		return request(number, returns.TypeRefund, returns.ItemRequest{OrderItemID: item, Quantity: 1})
	}

	cases := map[string]func(*returns.Request){
		"no items":              func(r *returns.Request) { r.Items = nil },
		"unknown type":          func(r *returns.Request) { r.Type = "CREDIT" },
		"unknown reason":        func(r *returns.Request) { r.Reason = "BORED" },
		"OTHER needs details":   func(r *returns.Request) { r.Reason = returns.ReasonOther; r.Description = "   " },
		"description too long":  func(r *returns.Request) { r.Description = strings.Repeat("a", 1001) },
		"item of another order": func(r *returns.Request) { r.Items[0].OrderItemID = f.item(otherOrder, v) },
		"zero quantity":         func(r *returns.Request) { r.Items[0].Quantity = 0 },
		"duplicate item":        func(r *returns.Request) { r.Items = append(r.Items, r.Items[0]) },
		"http evidence":         func(r *returns.Request) { r.Evidence = []string{"http://contoh.example/a.jpg"} },
		"file path evidence":    func(r *returns.Request) { r.Evidence = []string{"C:\\foto\\rusak.jpg"} },
		"too much evidence": func(r *returns.Request) {
			for i := range 6 {
				r.Evidence = append(r.Evidence, fmt.Sprintf("https://contoh.example/%d.jpg", i))
			}
		},
	}
	for name, mutate := range cases {
		req := base()
		mutate(&req)
		if _, err := f.svc.Create(f.ctx, uid, req); !isValidation(err) {
			t.Errorf("%s: want a validation error, got %v", name, err)
		}
	}
	if n := f.int("SELECT count(*) FROM returns"); n != 0 {
		t.Fatalf("invalid requests stored: %d", n)
	}

	req := base()
	req.Reason, req.Description = returns.ReasonOther, "Jahitan lepas di bagian bahu"
	req.Evidence = []string{"", "https://drive.google.com/file/d/abc/view", "  https://i.imgur.com/rusak.jpg  ", "https://contoh.example/unboxing.mp4"}
	req.IdempotencyKey = "same-key"
	n, err := f.svc.Create(f.ctx, uid, req)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := f.svc.Create(f.ctx, uid, req); err != nil || again != n || f.int("SELECT count(*) FROM returns") != 1 {
		t.Errorf("same idempotency key must return the same request: %q %v", again, err)
	}
	ret, err := f.svc.Get(f.ctx, uid, n)
	if err != nil {
		t.Fatal(err)
	}
	if len(ret.Evidence) != 3 || ret.Evidence[0].Kind != "LINK" || ret.Evidence[1].Kind != "IMAGE" || ret.Evidence[2].Kind != "VIDEO" ||
		ret.Description != "Jahitan lepas di bagian bahu" || ret.Status != returns.StatusRequested || len(ret.Events) != 1 {
		t.Errorf("stored return = %+v", ret)
	}
	if !strings.HasPrefix(n, "RET-"+time.Now().In(time.FixedZone("WIB", 7*3600)).Format("20060102")+"-") || len(n) != len("RET-20261104-000001") {
		t.Errorf("return number %q", n)
	}
}

func TestRefundAmountUsesSnapshotPrices(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	a := f.product("Kaos A", "A-1", 120000, 50)
	b := f.product("Kaos B", "B-1", 45000, 50)
	number := f.completed(uid, addr, time.Hour, line{a, 2}, line{b, 3})

	f.exec("UPDATE product_variants SET price = 999000") // price changes later: snapshot wins
	refund, err := f.svc.Create(f.ctx, uid, request(number, returns.TypeRefund,
		returns.ItemRequest{OrderItemID: f.item(number, a), Quantity: 1},
		returns.ItemRequest{OrderItemID: f.item(number, b), Quantity: 2}))
	if err != nil {
		t.Fatal(err)
	}
	r, _ := f.svc.Get(f.ctx, uid, refund)
	if r.RefundAmount != 120000+2*45000 || r.ItemsValue() != r.RefundAmount || r.Quantity() != 3 {
		t.Errorf("refund amount = %d, items value %d", r.RefundAmount, r.ItemsValue())
	}
	exchange, err := f.svc.Create(f.ctx, uid, request(number, returns.TypeExchange, returns.ItemRequest{OrderItemID: f.item(number, b), Quantity: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := f.svc.Get(f.ctx, uid, exchange); r.RefundAmount != 0 || r.ItemsValue() != 45000 {
		t.Errorf("exchange must not refund money: %d / %d", r.RefundAmount, r.ItemsValue())
	}
}

func TestReturnAfterProductDeleted(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	v := f.product("Kaos Lama", "OLD-1", 80000, 10)
	number := f.completed(uid, addr, time.Hour, line{v, 2})
	item := f.item(number, v)
	f.exec("DELETE FROM products") // completed orders do not hold variants

	d, err := f.svc.Draft(f.ctx, uid, number)
	if err != nil || len(d.Items) != 1 || d.Items[0].ProductName != "Kaos Lama" {
		t.Fatalf("draft after deletion: %+v %v", d, err)
	}
	n, err := f.svc.Create(f.ctx, uid, request(number, returns.TypeRefund, returns.ItemRequest{OrderItemID: item, Quantity: 2}))
	if err != nil {
		t.Fatal(err)
	}
	if r, err := f.svc.Get(f.ctx, uid, n); err != nil || r.Items[0].ProductName != "Kaos Lama" || r.RefundAmount != 160000 {
		t.Errorf("return after deletion: %+v %v", r, err)
	}
}

func TestReturnHistoryAndIsolation(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	other, _ := f.user("b@example.com")
	v := f.product("Kaos A", "A-1", 50000, 50)
	var numbers []string
	for range 11 {
		number := f.completed(uid, addr, time.Hour, line{v, 1})
		n, err := f.svc.Create(f.ctx, uid, request(number, returns.TypeRefund, returns.ItemRequest{OrderItemID: f.item(number, v), Quantity: 1}))
		if err != nil {
			t.Fatal(err)
		}
		numbers = append(numbers, n)
	}
	p1, _ := f.svc.List(f.ctx, uid, 1)
	p2, _ := f.svc.List(f.ctx, uid, 2)
	if len(p1.Returns) != 10 || !p1.HasNext || len(p2.Returns) != 1 || p2.HasNext {
		t.Errorf("pages: %d %v / %d %v", len(p1.Returns), p1.HasNext, len(p2.Returns), p2.HasNext)
	}
	if p1.Returns[0].Number != numbers[10] || p1.Returns[0].ItemCount != 1 || p1.Returns[0].RefundAmount != 50000 {
		t.Errorf("newest first: %+v", p1.Returns[0])
	}
	if p, _ := f.svc.List(f.ctx, other, 1); len(p.Returns) != 0 {
		t.Error("another user's history must be empty")
	}
	if _, err := f.svc.Get(f.ctx, other, numbers[0]); !errors.Is(err, returns.ErrNotFound) {
		t.Errorf("another user's return: %v", err)
	}
	if err := f.svc.CancelByUser(f.ctx, other, numbers[0]); !errors.Is(err, returns.ErrNotFound) {
		t.Errorf("another user's cancel: %v", err)
	}
}

func TestStatusTransitions(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	v := f.product("Kaos A", "A-1", 100000, 50)
	number := f.completed(uid, addr, time.Hour, line{v, 5})
	item := f.item(number, v)
	create := func(typ returns.Type, qty int) int64 {
		n, err := f.svc.Create(f.ctx, uid, request(number, typ, returns.ItemRequest{OrderItemID: item, Quantity: qty}))
		if err != nil {
			t.Fatal(err)
		}
		return f.id(n)
	}
	status := func(id int64) string {
		var s string
		f.db.QueryRow(f.ctx, "SELECT status FROM returns WHERE id = $1", id).Scan(&s)
		return s
	}
	invalid := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, returns.ErrInvalidTransition) {
			t.Errorf("%s: want invalid transition, got %v", name, err)
		}
	}

	// Refund: the whole path, with invalid steps refused on the way.
	r := create(returns.TypeRefund, 2)
	invalid("receive before approval", f.svc.MarkItemReceived(f.ctx, r, ""))
	if _, err := f.svc.PrepareRefund(f.ctx, r); !errors.Is(err, returns.ErrInvalidTransition) {
		t.Errorf("refund before goods are back: %v", err)
	}
	if err := f.svc.Approve(f.ctx, r, "Silakan kirim barangnya"); err != nil {
		t.Fatal(err)
	}
	invalid("approve twice", f.svc.Approve(f.ctx, r, ""))
	invalid("reject after approval", f.svc.Reject(f.ctx, r, "terlambat"))
	if err := f.svc.CancelByUser(f.ctx, uid, mustNumber(t, f, r)); !errors.Is(err, returns.ErrInvalidTransition) {
		t.Errorf("buyer cancel after approval: %v", err)
	}
	if err := f.svc.MarkWaitingReturn(f.ctx, r, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.MarkItemReceived(f.ctx, r, "Barang diterima"); err != nil {
		t.Fatal(err)
	}
	invalid("exchange on a refund return", f.svc.CompleteExchange(f.ctx, r, ""))
	refund, err := f.svc.PrepareRefund(f.ctx, r)
	if err != nil || refund.Amount != 200000 || refund.Status != returns.RefundPending {
		t.Fatalf("prepare refund: %+v %v", refund, err)
	}
	if got := f.int("SELECT count(*) FROM refunds WHERE return_id = $1 AND status = 'PENDING' AND provider IS NULL AND payment_id IS NOT NULL", r); got != 1 {
		t.Errorf("pending refund rows = %d", got)
	}
	if _, err := f.svc.PrepareRefund(f.ctx, r); !errors.Is(err, returns.ErrInvalidTransition) {
		t.Errorf("second refund: %v", err)
	}
	if err := f.svc.CompleteRefund(f.ctx, r, returns.RefundResult{}); !isValidation(err) {
		t.Errorf("refund result needs a provider: %v", err)
	}
	if err := f.svc.CompleteRefund(f.ctx, r, returns.RefundResult{Provider: "manual", Reference: "TRF-001"}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Complete(f.ctx, r, ""); err != nil {
		t.Fatal(err)
	}
	ret, _ := f.svc.Get(f.ctx, uid, mustNumber(t, f, r))
	if ret.Status != returns.StatusCompleted || ret.Refund == nil || ret.Refund.Status != returns.RefundSucceeded ||
		ret.Refund.Provider != "manual" || len(ret.Events) != 7 || ret.ResolutionNote != "Barang diterima" || ret.CompletedAt == nil {
		t.Errorf("completed refund return: %+v refund %+v", ret, ret.Refund)
	}
	invalid("anything after completion", f.svc.Cancel(f.ctx, r, ""))

	// Exchange path.
	x := create(returns.TypeExchange, 1)
	for _, step := range []func(context.Context, int64, string) error{f.svc.Approve, f.svc.MarkWaitingReturn, f.svc.MarkItemReceived} {
		if err := step(f.ctx, x, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.svc.PrepareRefund(f.ctx, x); !errors.Is(err, returns.ErrInvalidTransition) {
		t.Errorf("refund on an exchange: %v", err)
	}
	if err := f.svc.CompleteExchange(f.ctx, x, "Pengganti dikirim"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Complete(f.ctx, x, ""); err != nil || status(x) != "COMPLETED" {
		t.Errorf("exchange complete: %v %s", err, status(x))
	}

	// Reject needs a reason; a rejected request frees its quantity.
	j := create(returns.TypeRefund, 2)
	if err := f.svc.Reject(f.ctx, j, " "); !isValidation(err) {
		t.Errorf("reject without reason: %v", err)
	}
	if err := f.svc.Reject(f.ctx, j, "Foto tidak menunjukkan kerusakan"); err != nil || status(j) != "REJECTED" {
		t.Fatalf("reject: %v %s", err, status(j))
	}
	if d, _ := f.svc.Draft(f.ctx, uid, number); d.Items[0].Remaining() != 2 {
		t.Errorf("remaining after rejection = %d, want 2", d.Items[0].Remaining())
	}
	if err := f.svc.Approve(f.ctx, 999999, ""); !errors.Is(err, returns.ErrNotFound) {
		t.Errorf("unknown return: %v", err)
	}
}

func TestRefundNeverExceedsPayment(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	v := f.product("Kaos A", "A-1", 100000, 50)
	number := f.completed(uid, addr, time.Hour, line{v, 1})
	n, err := f.svc.Create(f.ctx, uid, request(number, returns.TypeRefund, returns.ItemRequest{OrderItemID: f.item(number, v), Quantity: 1}))
	if err != nil {
		t.Fatal(err)
	}
	id := f.id(n)
	for _, step := range []func(context.Context, int64, string) error{f.svc.Approve, f.svc.MarkWaitingReturn, f.svc.MarkItemReceived} {
		if err := step(f.ctx, id, ""); err != nil {
			t.Fatal(err)
		}
	}
	f.exec("UPDATE payments SET amount = 1000 WHERE status = 'PAID'") // paid less than the refund
	if _, err := f.svc.PrepareRefund(f.ctx, id); !errors.Is(err, returns.ErrRefundTooLarge) {
		t.Errorf("refund above the payment: %v", err)
	}
	if got := f.int("SELECT count(*) FROM refunds"); got != 0 {
		t.Errorf("refund rows after refusal = %d", got)
	}
}

func mustNumber(t *testing.T, f *fixture, id int64) string {
	t.Helper()
	var n string
	if err := f.db.QueryRow(f.ctx, "SELECT return_number FROM returns WHERE id = $1", id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
