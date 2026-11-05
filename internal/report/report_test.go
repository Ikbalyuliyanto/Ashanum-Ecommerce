package report_test

import (
	"context"
	"testing"
	"time"

	"ecommerce/internal/address"
	"ecommerce/internal/category"
	"ecommerce/internal/checkout"
	"ecommerce/internal/inventory"
	"ecommerce/internal/order"
	"ecommerce/internal/product"
	"ecommerce/internal/report"
	"ecommerce/internal/shipping/shippingtest"
	"ecommerce/internal/testutil"
)

func TestParseRange(t *testing.T) {
	now := time.Date(2026, 11, 4, 1, 0, 0, 0, time.UTC) // 08:00 WIB
	r, err := report.ParseRange("today", "", "", now)
	if err != nil || r.FromDate != "2026-11-04" || r.ToDate != "2026-11-04" || r.Days() != 1 {
		t.Errorf("today: %+v %v", r, err)
	}
	if r, _ := report.ParseRange("", "", "", now); r.Key != "7d" || r.FromDate != "2026-10-29" || r.Days() != 7 {
		t.Errorf("default 7d: %+v", r)
	}
	if r, _ := report.ParseRange("30d", "", "", now); r.Days() != 30 {
		t.Errorf("30d: %+v", r)
	}
	r, err = report.ParseRange("custom", "2026-10-01", "2026-10-31", now)
	if err != nil || r.Days() != 31 || !r.From.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, report.WIB)) {
		t.Errorf("custom: %+v %v", r, err)
	}
	for _, bad := range [][2]string{{"2026-10-31", "2026-10-01"}, {"kemarin", "2026-10-01"}, {"2024-01-01", "2026-01-01"}} {
		if _, err := report.ParseRange("custom", bad[0], bad[1], now); err != report.ErrBadRange {
			t.Errorf("custom %v accepted", bad)
		}
	}
}

func TestReports(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	c, _ := category.NewService(category.NewRepository(db)).Create(ctx, category.Input{Name: "Atasan", Active: true})
	products := product.NewService(product.NewRepository(db))
	mk := func(name, sku string, price int64, stock int) int64 {
		if _, err := products.Create(ctx, product.Input{WeightGrams: 100, CategoryID: c.ID, Name: name, Active: true,
			Variants: []product.VariantInput{{SKU: sku, Price: price, Stock: stock}}}); err != nil {
			t.Fatal(err)
		}
		var id int64
		db.QueryRow(ctx, "SELECT id FROM product_variants WHERE sku = $1", sku).Scan(&id)
		return id
	}
	a, b := mk("Kaos A", "A-1", 100000, 50), mk("Kaos B", "B-1", 50000, 50)
	mk("Kaos C", "C-1", 10000, 3) // low stock
	mk("Kaos D", "D-1", 10000, 0) // out of stock
	addrs := address.NewService(address.NewRepository(db), shippingtest.For(t, db).Service)
	co := checkout.NewService(checkout.NewRepository(db), addrs, shippingtest.For(t, db).Service)
	uid := testutil.CreateUser(t, db, "budi@example.com")
	ad, err := addrs.Create(ctx, uid, address.Input{ShippingAreaID: shippingtest.For(t, db).AreaID(t), Label: "Rumah", RecipientName: "Budi", Phone: "081234567890", Province: "Jawa Barat",
		City: "Bandung", District: "Coblong", Village: "Dago", PostalCode: "40135", StreetAddress: "Jl. Juanda 1"})
	if err != nil {
		t.Fatal(err)
	}
	buy := func(variant int64, qty int) string {
		p, err := co.PlaceOrder(ctx, uid, quoted(t, co, uid, checkout.PlaceRequest{Source: checkout.Source{VariantID: variant, Quantity: qty}, AddressID: ad.ID}))
		if err != nil {
			t.Fatal(err)
		}
		return p.OrderNumber
	}
	exec := func(sql string, args ...any) {
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}

	buy(a, 5)              // unpaid: never revenue
	paid1 := buy(a, 2)     // 200.000
	paid2 := buy(b, 3)     // 150.000
	cancelled := buy(a, 1) // paid then cancelled
	old := buy(b, 4)       // paid 40 days ago: outside 30 days
	for _, n := range []string{paid1, paid2, cancelled, old} {
		testutil.PayOrder(t, db, n)
	}
	exec("UPDATE orders SET status = 'CANCELLED', cancelled_at = now(), cancel_reason = 'ADMIN_CANCELLED', stock_restored_at = now() WHERE order_number = $1", cancelled)
	exec("UPDATE orders SET paid_at = now() - interval '40 days', created_at = now() - interval '40 days' WHERE order_number = $1", old)

	svc := report.NewService(db, inventory.NewService(db, 5))
	r30, _ := report.ParseRange("30d", "", "", time.Now())
	rep, err := svc.Build(ctx, r30)
	if err != nil {
		t.Fatal(err)
	}
	// Revenue is what buyers paid (orders.total, shipping included: 9000 per order).
	if rep.Sales.Revenue != 350000+2*9000 || rep.Sales.ItemsValue != 350000 || rep.Sales.PaidOrders != 2 || rep.Sales.Units != 5 {
		t.Errorf("revenue: %+v (want 350000 from 2 paid orders, 5 units)", rep.Sales)
	}
	if rep.Sales.CancelledOrders != 1 || rep.Sales.CancelledAmount != 100000+9000 {
		t.Errorf("cancelled after payment: %+v", rep.Sales)
	}
	counts := map[order.Status]int{}
	for _, s := range rep.Statuses {
		counts[s.Status] = s.Orders
	}
	if counts[order.StatusWaitingPayment] != 1 || counts[order.StatusWaitingConfirmation] != 2 || counts[order.StatusCancelled] != 1 || rep.OrdersTotal != 4 {
		t.Errorf("status breakdown: %v total %d", counts, rep.OrdersTotal)
	}
	if len(rep.TopProducts) != 2 || rep.TopProducts[0].Name != "Kaos B" || rep.TopProducts[0].Units != 3 || rep.TopProducts[1].Units != 2 {
		t.Errorf("top products: %+v", rep.TopProducts)
	}
	if len(rep.LowStock) != 1 || rep.LowStock[0].SKU != "C-1" {
		t.Errorf("low stock: %+v", rep.LowStock)
	}

	// Date filtering: the order paid 40 days ago only appears in its range.
	from := time.Now().AddDate(0, 0, -41).In(report.WIB).Format(time.DateOnly)
	to := time.Now().AddDate(0, 0, -39).In(report.WIB).Format(time.DateOnly)
	rOld, err := report.ParseRange("custom", from, to, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep, _ := svc.Build(ctx, rOld); rep.Sales.Revenue != 200000+9000 || rep.Sales.PaidOrders != 1 {
		t.Errorf("old range: %+v", rep.Sales)
	}

	d, err := svc.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.WaitingPayment != 1 || d.WaitingConfirmation != 3 || d.SalesToday != 350000+2*9000 || d.PaidToday != 2 ||
		d.LowStock != 1 || d.OutOfStock != 1 || d.SalesWeek != 350000+2*9000 {
		t.Errorf("dashboard: %+v", d)
	}
}

// quoted adds the cheapest shipping quote for the request, as the checkout
// page does before the buyer presses "Buat Pesanan".
func quoted(t testing.TB, co *checkout.Service, userID int64, req checkout.PlaceRequest) checkout.PlaceRequest {
	t.Helper()
	d, err := co.Prepare(context.Background(), userID, req.Source, req.AddressID, "")
	if err == nil && d.Shipping.Selected != nil {
		req.ShippingQuoteID = d.Shipping.Selected.ID
	}
	return req
}
