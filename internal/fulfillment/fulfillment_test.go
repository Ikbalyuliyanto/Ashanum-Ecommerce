package fulfillment_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/address"
	"ecommerce/internal/category"
	"ecommerce/internal/checkout"
	"ecommerce/internal/fulfillment"
	"ecommerce/internal/payment"
	"ecommerce/internal/product"
	"ecommerce/internal/returns"
	"ecommerce/internal/review"
	"ecommerce/internal/testutil"
	"ecommerce/internal/validate"
)

type fixture struct {
	t        *testing.T
	ctx      context.Context
	db       *pgxpool.Pool
	svc      *fulfillment.Service
	checkout *checkout.Service
	admin    int64
	buyer    int64
	address  int64
	variant  int64
}

func newFixture(t *testing.T) *fixture {
	db := testutil.DB(t)
	ctx := context.Background()
	pay := payment.NewService(payment.NewRepository(db), nil, payment.Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	f := &fixture{t: t, ctx: ctx, db: db, svc: fulfillment.NewService(db, pay)}
	addrs := address.NewService(address.NewRepository(db))
	f.checkout = checkout.NewService(checkout.NewRepository(db), addrs)
	c, err := category.NewService(category.NewRepository(db)).Create(ctx, category.Input{Name: "Atasan", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := product.NewService(product.NewRepository(db)).Create(ctx, product.Input{
		CategoryID: c.ID, Name: "Kaos Basic", Active: true,
		Images:   []product.ImageInput{{URL: "data:image/svg+xml;base64,PHN2Zy8+"}},
		Variants: []product.VariantInput{{SKU: "KB-1", Price: 100000, Stock: 10}},
	}); err != nil {
		t.Fatal(err)
	}
	f.variant = f.int("SELECT id FROM product_variants WHERE sku = 'KB-1'")
	f.admin = testutil.CreateAdmin(t, db, "admin@example.com")
	f.buyer = testutil.CreateUser(t, db, "budi@example.com")
	a, err := addrs.Create(ctx, f.buyer, address.Input{Label: "Rumah", RecipientName: "Budi Santoso", Phone: "081234567890",
		Province: "Jawa Barat", City: "Kota Bandung", District: "Coblong", Village: "Dago", PostalCode: "40135",
		StreetAddress: "Jl. Ir. H. Juanda No. 100"})
	if err != nil {
		t.Fatal(err)
	}
	f.address = a.ID
	return f
}

// order places a buy-now order of qty units (stock is taken at checkout).
func (f *fixture) order(qty int) string {
	f.t.Helper()
	placed, err := f.checkout.PlaceOrder(f.ctx, f.buyer, checkout.PlaceRequest{
		Source: checkout.Source{VariantID: f.variant, Quantity: qty}, AddressID: f.address})
	if err != nil {
		f.t.Fatal(err)
	}
	return placed.OrderNumber
}

func (f *fixture) int(sql string, args ...any) int64 {
	f.t.Helper()
	var n int64
	if err := f.db.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (f *fixture) status(number string) string {
	var s string
	f.db.QueryRow(f.ctx, "SELECT status FROM orders WHERE order_number = $1", number).Scan(&s)
	return s
}

func (f *fixture) stock() int64 {
	return f.int("SELECT stock FROM product_variants WHERE id = $1", f.variant)
}

var ship = fulfillment.ShipInput{Courier: "JNE", Service: "REG", Tracking: "JNE123456789"}

func TestNormalFlowReachesCompleted(t *testing.T) {
	f := newFixture(t)
	number := f.order(2)

	// Unpaid: nothing can be processed, and the database refuses it too.
	if err := f.svc.Confirm(f.ctx, f.admin, number); !errors.Is(err, fulfillment.ErrNotPaid) {
		t.Errorf("confirm unpaid: %v", err)
	}
	if err := f.svc.Ship(f.ctx, f.admin, number, ship); !errors.Is(err, fulfillment.ErrNotPaid) {
		t.Errorf("ship unpaid: %v", err)
	}
	if _, err := f.db.Exec(f.ctx, "UPDATE orders SET status = 'PROCESSING' WHERE order_number = $1", number); err == nil {
		t.Error("the database lets an unpaid order be PROCESSING")
	}

	testutil.PayOrder(t, f.db, number) // as a provider-confirmed payment does
	if err := f.svc.Ship(f.ctx, f.admin, number, ship); !errors.Is(err, fulfillment.ErrInvalidTransition) {
		t.Errorf("ship before confirm: %v", err)
	}
	if err := f.svc.Complete(f.ctx, f.admin, number); !errors.Is(err, fulfillment.ErrInvalidTransition) {
		t.Errorf("complete before ship: %v", err)
	}
	if err := f.svc.Confirm(f.ctx, f.admin, number); err != nil || f.status(number) != "PROCESSING" {
		t.Fatalf("confirm: %v %s", err, f.status(number))
	}
	if err := f.svc.Confirm(f.ctx, f.admin, number); !errors.Is(err, fulfillment.ErrInvalidTransition) {
		t.Errorf("confirm twice: %v", err)
	}
	if err := f.svc.Ship(f.ctx, f.admin, number, fulfillment.ShipInput{}); err == nil {
		t.Error("ship without a courier")
	} else if _, ok := err.(validate.Errors); !ok {
		t.Errorf("ship without courier: %v", err)
	}
	if err := f.svc.Ship(f.ctx, f.admin, number, ship); err != nil || f.status(number) != "SHIPPED" {
		t.Fatalf("ship: %v %s", err, f.status(number))
	}
	if got := f.int("SELECT count(*) FROM shipments s JOIN orders o ON o.id = s.order_id WHERE o.order_number = $1 AND s.method = 'MANUAL' AND s.tracking_number = 'JNE123456789'", number); got != 1 {
		t.Errorf("shipment rows = %d", got)
	}
	if _, err := f.svc.Cancel(f.ctx, f.admin, number, "salah kirim"); !errors.Is(err, fulfillment.ErrNotCancellable) {
		t.Errorf("cancel a shipped order: %v", err)
	}
	if err := f.svc.Complete(f.ctx, f.admin, number); err != nil || f.status(number) != "COMPLETED" {
		t.Fatalf("complete: %v %s", err, f.status(number))
	}
	if f.int("SELECT count(*) FROM orders WHERE order_number = $1 AND completed_at IS NOT NULL AND confirmed_at IS NOT NULL", number) != 1 {
		t.Error("completed_at / confirmed_at not set")
	}
	if err := f.svc.Complete(f.ctx, f.admin, number); !errors.Is(err, fulfillment.ErrInvalidTransition) {
		t.Errorf("complete twice: %v", err)
	}
	if got := f.int("SELECT count(*) FROM admin_audit_logs WHERE entity_type = 'ORDER' AND action IN ('order.confirm', 'order.ship', 'order.complete')"); got != 3 {
		t.Errorf("audit entries = %d, want 3", got)
	}

	// The normal flow, no fixture: the order is now reviewable and returnable.
	item := f.int("SELECT i.id FROM order_items i JOIN orders o ON o.id = i.order_id WHERE o.order_number = $1", number)
	if _, err := review.NewService(review.NewRepository(f.db)).Target(f.ctx, f.buyer, item); err != nil {
		t.Errorf("review after completion: %v", err)
	}
	rs := returns.NewService(returns.NewRepository(f.db), returns.Options{WindowDays: 7})
	if d, err := rs.Draft(f.ctx, f.buyer, number); err != nil || len(d.Items) != 1 || d.Items[0].Remaining() != 2 {
		t.Errorf("return after completion: %+v %v", d, err)
	}

	d, err := f.svc.Get(f.ctx, number)
	if err != nil || d.Shipment == nil || d.Shipment.Courier != "JNE" || d.Buyer.Email != "budi@example.com" || len(d.Payments) != 1 {
		t.Errorf("detail: %+v %v", d, err)
	}
}

func TestCancelPaidOrderRecordsRefundAndRestoresStockOnce(t *testing.T) {
	f := newFixture(t)
	number := f.order(3)
	if f.stock() != 7 {
		t.Fatalf("stock after checkout = %d", f.stock())
	}
	testutil.PayOrder(t, f.db, number)
	if err := f.svc.Confirm(f.ctx, f.admin, number); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Cancel(f.ctx, f.admin, number, " "); err == nil {
		t.Error("cancel without a reason")
	}

	// Two admins cancel at the same time: one cancellation, one restock.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = f.svc.Cancel(f.ctx, f.admin, number, "Stok rusak di gudang")
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Errorf("concurrent cancel: %v", err)
		}
	}
	if f.status(number) != "CANCELLED" || f.stock() != 10 {
		t.Fatalf("after cancel: %s stock %d (want 10)", f.status(number), f.stock())
	}
	if got := f.int("SELECT count(*) FROM orders WHERE order_number = $1 AND cancel_reason = 'ADMIN_CANCELLED' AND stock_restored_at IS NOT NULL", number); got != 1 {
		t.Error("cancel reason / stock marker")
	}
	if got := f.int("SELECT count(*) FROM refunds WHERE source = 'ORDER_CANCELLATION' AND status = 'PENDING' AND amount = 300000 AND return_id IS NULL AND provider IS NULL"); got != 1 {
		t.Errorf("cancellation refunds = %d, want 1", got)
	}
	if got := f.int("SELECT count(*) FROM inventory_adjustments WHERE type = 'ORDER_RESTORE' AND quantity_delta = 3 AND stock_before = 7 AND stock_after = 10 AND admin_user_id = $1", f.admin); got != 1 {
		t.Errorf("ORDER_RESTORE adjustments = %d", got)
	}
	if got := f.int("SELECT count(*) FROM payments WHERE status = 'PAID'"); got != 1 {
		t.Error("the payment record must stay PAID (money was taken)")
	}
	if _, err := f.svc.Cancel(f.ctx, f.admin, number, "lagi"); err != nil || f.stock() != 10 {
		t.Errorf("cancel again: %v, stock %d", err, f.stock())
	}
}

func TestCancelUnpaidOrderByAdmin(t *testing.T) {
	f := newFixture(t)
	number := f.order(2)
	res, err := f.svc.Cancel(f.ctx, f.admin, number, "Pesanan ganda")
	if err != nil || res.Paid || f.status(number) != "CANCELLED" || f.stock() != 10 {
		t.Fatalf("cancel unpaid: %+v %v %s stock %d", res, err, f.status(number), f.stock())
	}
	if got := f.int("SELECT count(*) FROM refunds"); got != 0 {
		t.Errorf("an unpaid cancellation needs no refund, got %d", got)
	}
	if got := f.int("SELECT count(*) FROM admin_audit_logs WHERE action = 'order.cancel'"); got != 1 {
		t.Errorf("audit = %d", got)
	}
	if _, err := f.svc.Cancel(f.ctx, f.admin, "ASH-TIDAK-ADA", "x y z"); !errors.Is(err, fulfillment.ErrNotFound) {
		t.Errorf("unknown order: %v", err)
	}
}

func TestListAndSearch(t *testing.T) {
	f := newFixture(t)
	a, b := f.order(1), f.order(1)
	testutil.PayOrder(t, f.db, b)
	p, err := f.svc.List(f.ctx, fulfillment.Filter{Tab: "confirm"})
	if err != nil || len(p.Orders) != 1 || p.Orders[0].Number != b {
		t.Errorf("confirm tab: %+v %v", p.Orders, err)
	}
	p, _ = f.svc.List(f.ctx, fulfillment.Filter{Query: a[len(a)-6:]})
	if len(p.Orders) != 1 || p.Orders[0].Number != a {
		t.Errorf("search by number: %+v", p.Orders)
	}
	p, _ = f.svc.List(f.ctx, fulfillment.Filter{Query: "%"})
	if len(p.Orders) != 0 {
		t.Errorf("%% must match itself, got %d", len(p.Orders))
	}
	p, _ = f.svc.List(f.ctx, fulfillment.Filter{PageSize: 1})
	if len(p.Orders) != 1 || !p.HasNext {
		t.Errorf("pagination: %d %v", len(p.Orders), p.HasNext)
	}
	p, _ = f.svc.List(f.ctx, fulfillment.Filter{PageSize: 1000})
	if p.Filter.PageSize != fulfillment.MaxPageSize {
		t.Errorf("page size cap: %d", p.Filter.PageSize)
	}
}
