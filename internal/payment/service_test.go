package payment_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/address"
	"ecommerce/internal/cart"
	"ecommerce/internal/category"
	"ecommerce/internal/checkout"
	"ecommerce/internal/payment"
	"ecommerce/internal/product"
	"ecommerce/internal/shipping/shippingtest"
	"ecommerce/internal/testutil"
)

// fakeProvider is an in-memory payment.Provider. Notifications are
// "<provider order id>:signed"; any other signature is invalid.
type fakeProvider struct {
	mu          sync.Mutex
	tx          map[string]payment.ProviderStatus
	created     []payment.TransactionRequest
	cancelled   []string
	createErr   error
	statusErr   error
	cancelErr   error
	createDelay time.Duration
}

func newFakeProvider() *fakeProvider { return &fakeProvider{tx: map[string]payment.ProviderStatus{}} }

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) CreateTransaction(_ context.Context, req payment.TransactionRequest) (payment.Checkout, error) {
	f.mu.Lock()
	delay, err := f.createDelay, f.createErr
	f.mu.Unlock()
	time.Sleep(delay)
	if err != nil {
		return payment.Checkout{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, req)
	return payment.Checkout{Token: "tok-" + req.ProviderOrderID, RedirectURL: "https://pay.example/" + req.ProviderOrderID}, nil
}

func (f *fakeProvider) GetStatus(_ context.Context, id string) (payment.ProviderStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return payment.ProviderStatus{}, f.statusErr
	}
	return f.tx[id], nil
}

func (f *fakeProvider) Cancel(_ context.Context, id string) (payment.ProviderStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelErr != nil {
		return payment.ProviderStatus{}, f.cancelErr
	}
	f.cancelled = append(f.cancelled, id)
	st := f.tx[id]
	if st.Found && st.Status == payment.StatusPending {
		st.Status, st.Raw = payment.StatusCancelled, "cancel"
		f.tx[id] = st
	}
	return st, nil
}

func (f *fakeProvider) ParseNotification(body []byte) (payment.Notification, error) {
	id, sig, ok := strings.Cut(string(body), ":")
	if !ok || id == "" {
		return payment.Notification{}, payment.ErrInvalidNotification
	}
	if sig != "signed" {
		return payment.Notification{}, payment.ErrInvalidSignature
	}
	return payment.Notification{ProviderOrderID: id}, nil
}

func (f *fakeProvider) MethodLabel(t string) string { return "Metode " + t }

// set makes the provider report status for a transaction.
func (f *fakeProvider) set(id string, s payment.Status, raw string, amount int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := payment.ProviderStatus{Found: true, Status: s, Raw: raw, Amount: amount, PaymentType: "bank_transfer", TransactionID: "trx-" + id}
	if s == payment.StatusPaid {
		t := time.Now().Truncate(time.Second)
		st.PaidAt = &t
	}
	f.tx[id] = st
}

func (f *fakeProvider) createdFor(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.created {
		if strings.HasPrefix(r.ProviderOrderID, prefix) {
			n++
		}
	}
	return n
}

type fixture struct {
	t        *testing.T
	ctx      context.Context
	db       *pgxpool.Pool
	prov     *fakeProvider
	svc      *payment.Service
	checkout *checkout.Service
	carts    *cart.Service
	addrs    *address.Service
	catID    int64
}

// ship is the shipping cost of every test order (JNE REG, up to 1 kg in
// the fake provider): orders.total = items + ship is what is charged.
const ship = 9000

func newFixture(t *testing.T) *fixture {
	db := testutil.DB(t)
	f := &fixture{t: t, ctx: context.Background(), db: db, prov: newFakeProvider(),
		carts: cart.NewService(cart.NewRepository(db)), addrs: address.NewService(address.NewRepository(db), shippingtest.For(t, db).Service)}
	f.svc = f.newService(f.prov)
	f.checkout = checkout.NewService(checkout.NewRepository(db), f.addrs, shippingtest.For(t, db).Service)
	c, err := category.NewService(category.NewRepository(db)).Create(f.ctx, category.Input{Name: "Atasan", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	f.catID = c.ID
	return f
}

// newService is another Service on the same database, like a second
// process (e.g. the reconcile command next to the web server).
func (f *fixture) newService(p payment.Provider) *payment.Service {
	return payment.NewService(payment.NewRepository(f.db), p, payment.Options{BaseURL: "https://toko.example", Sandbox: true})
}

func (f *fixture) product(name, sku string, price int64, stock int) int64 {
	f.t.Helper()
	if _, err := product.NewService(product.NewRepository(f.db)).Create(f.ctx, product.Input{WeightGrams: 100,
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
	a, err := f.addrs.Create(f.ctx, userID, address.Input{ShippingAreaID: shippingtest.For(f.t, f.db).AreaID(f.t),
		Label: "Rumah", RecipientName: "Budi Santoso", Phone: "081234567890",
		Province: "Jawa Barat", City: "Kota Bandung", District: "Coblong", Village: "Dago",
		PostalCode: "40135", StreetAddress: "Jl. Ir. H. Juanda No. 100",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return userID, a.ID
}

// order places a Buy Now order and returns its number.
func (f *fixture) order(userID, addressID, variantID int64, qty int) string {
	f.t.Helper()
	placed, err := f.checkout.PlaceOrder(f.ctx, userID, quoted(f.t, f.checkout, userID, checkout.PlaceRequest{
		Source: checkout.Source{VariantID: variantID, Quantity: qty}, AddressID: addressID,
	}))
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

func (f *fixture) str(sql string, args ...any) string {
	f.t.Helper()
	var s *string
	if err := f.db.QueryRow(f.ctx, sql, args...).Scan(&s); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	if s == nil {
		return ""
	}
	return *s
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *fixture) stock(variantID int64) int64 {
	return f.int("SELECT stock FROM product_variants WHERE id = $1", variantID)
}

func (f *fixture) orderStatus(number string) string {
	return f.str("SELECT status FROM orders WHERE order_number = $1", number)
}

func (f *fixture) paymentStatus(providerOrderID string) string {
	return f.str("SELECT status FROM payments WHERE provider_order_id = $1", providerOrderID)
}

func (f *fixture) notify(providerOrderID string) error {
	return f.svc.HandleNotification(f.ctx, []byte(providerOrderID+":signed"))
}

func (f *fixture) orderID(number string) int64 {
	return f.int("SELECT id FROM orders WHERE order_number = $1", number)
}

// overdue moves an order's payment deadline into the past.
func (f *fixture) overdue(number string, ago time.Duration) {
	f.exec("UPDATE orders SET payment_due_at = now() - $2::interval WHERE order_number = $1", number, fmt.Sprintf("%d seconds", int(ago.Seconds())))
}

func TestPayCreatesAndReusesAttempt(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	v := f.product("Kaos A", "A-1", 129000, 10)
	number := f.order(uid, addr, v, 2)

	url, err := f.svc.Pay(f.ctx, uid, number)
	if err != nil {
		t.Fatal(err)
	}
	p1 := number + "-P1"
	if url != "https://pay.example/"+p1 {
		t.Errorf("redirect = %q", url)
	}
	req := f.prov.created[0]
	if req.ProviderOrderID != p1 || req.Amount != 258000+ship || req.Customer.Email != "a@example.com" ||
		req.FinishURL != "https://toko.example/account/orders/"+number+"/payment/finish" {
		t.Errorf("transaction request = %+v", req)
	}
	var sum int64
	for _, it := range req.Items {
		sum += it.Price * int64(it.Quantity)
	}
	if sum != req.Amount || len(req.Items) != 2 || req.Items[0].ID != "A-1" || req.Items[1].ID != "SHIPPING" || req.Items[1].Price != ship {
		t.Errorf("items %+v must add up to %d", req.Items, req.Amount)
	}
	due := f.int("SELECT extract(epoch FROM payment_due_at)::bigint FROM orders WHERE order_number = $1", number)
	if req.ExpiresAt.Unix() > due || req.ExpiresAt.Unix() < due-60 {
		t.Errorf("attempt expiry %v must end at the order deadline", req.ExpiresAt)
	}
	if got := f.str("SELECT amount::text || ' ' || status || ' ' || snap_token || ' ' || attempt_no FROM payments WHERE provider_order_id = $1", p1); got != "267000 PENDING tok-"+p1+" 1" {
		t.Errorf("payment row = %q", got)
	}

	// A second click reuses the pending attempt: no new provider transaction.
	again, err := f.svc.Pay(f.ctx, uid, number)
	if err != nil || again != url || f.prov.createdFor(number) != 1 {
		t.Errorf("second pay: %q %v (created %d)", again, err, f.prov.createdFor(number))
	}

	// Many simultaneous clicks on a fresh order still open one transaction.
	number2 := f.order(uid, addr, v, 1)
	f.prov.createDelay = 30 * time.Millisecond
	var wg sync.WaitGroup
	urls := make([]string, 6)
	errs := make([]error, 6)
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			urls[i], errs[i] = f.svc.Pay(f.ctx, uid, number2)
		}()
	}
	wg.Wait()
	for i := range 6 {
		if errs[i] != nil || urls[i] != "https://pay.example/"+number2+"-P1" {
			t.Errorf("concurrent pay %d: %q %v", i, urls[i], errs[i])
		}
	}
	if n := f.prov.createdFor(number2); n != 1 {
		t.Errorf("provider transactions = %d, want 1", n)
	}
	if n := f.int("SELECT count(*) FROM payments p JOIN orders o ON o.id = p.order_id WHERE o.order_number = $1", number2); n != 1 {
		t.Errorf("attempts = %d, want 1", n)
	}
}

func TestPayGuards(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	other, _ := f.user("b@example.com")
	v := f.product("Kaos A", "A-1", 100000, 10)
	number := f.order(uid, addr, v, 1)

	if _, err := f.svc.Pay(f.ctx, other, number); !errors.Is(err, payment.ErrNotFound) {
		t.Errorf("another user's order: %v", err)
	}
	if _, err := f.newService(nil).Pay(f.ctx, uid, number); !errors.Is(err, payment.ErrNotConfigured) {
		t.Errorf("no provider: %v", err)
	}

	// The amount always comes from the order and must match its lines.
	f.exec("UPDATE orders SET subtotal = 1000, total = 1000 + shipping_cost WHERE order_number = $1", number)
	if _, err := f.svc.Pay(f.ctx, uid, number); !errors.Is(err, payment.ErrInvalidAmount) || len(f.prov.created) != 0 {
		t.Errorf("inconsistent amounts: %v (created %d)", err, len(f.prov.created))
	}
	f.exec("UPDATE orders SET subtotal = 100000, total = 100000 + shipping_cost WHERE order_number = $1", number)

	// Too late: the deadline passed, the order is expired instead.
	f.overdue(number, time.Minute)
	if _, err := f.svc.Pay(f.ctx, uid, number); !errors.Is(err, payment.ErrOrderExpired) {
		t.Errorf("overdue: %v", err)
	}
	if s := f.orderStatus(number); s != "CANCELLED" {
		t.Errorf("overdue order = %s, want CANCELLED", s)
	}
	// A cancelled order is never revived by paying.
	if _, err := f.svc.Pay(f.ctx, uid, number); !errors.Is(err, payment.ErrNotPayable) {
		t.Errorf("pay cancelled order: %v", err)
	}
}

func TestProviderUnavailableAndRetryPolicy(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	v := f.product("Kaos A", "A-1", 100000, 10)
	number := f.order(uid, addr, v, 1)

	f.prov.createErr = fmt.Errorf("%w: timeout", payment.ErrProviderUnavailable)
	_, err := f.svc.Pay(f.ctx, uid, number)
	if !errors.Is(err, payment.ErrProviderUnavailable) || payment.Message(err) != "Pembayaran belum berhasil. Silakan coba kembali." {
		t.Fatalf("provider down: %v / %q", err, payment.Message(err))
	}
	if got := f.str("SELECT status || ' ' || provider_status FROM payments WHERE provider_order_id = $1", number+"-P1"); got != "FAILED create_failed" {
		t.Errorf("attempt 1 = %q", got)
	}
	if s := f.orderStatus(number); s != "WAITING_PAYMENT" {
		t.Errorf("order = %s", s)
	}

	f.prov.createErr = nil
	if url, err := f.svc.Pay(f.ctx, uid, number); err != nil || !strings.HasSuffix(url, number+"-P2") {
		t.Fatalf("retry: %q %v", url, err)
	}

	// A declined payment closes the attempt; a new one is only created
	// when the buyer clicks again.
	f.prov.set(number+"-P2", payment.StatusDenied, "deny", 100000+ship)
	if err := f.notify(number + "-P2"); err != nil {
		t.Fatal(err)
	}
	if s := f.paymentStatus(number + "-P2"); s != "DENIED" {
		t.Errorf("P2 = %s", s)
	}
	if n := f.int("SELECT count(*) FROM payments WHERE order_id = $1", f.orderID(number)); n != 2 {
		t.Errorf("attempts after denial = %d, want 2 (no automatic retry)", n)
	}
	if url, err := f.svc.Pay(f.ctx, uid, number); err != nil || !strings.HasSuffix(url, number+"-P3") {
		t.Errorf("pay again: %q %v", url, err)
	}
}

func TestPaidNotificationIsIdempotent(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	v := f.product("Kaos A", "A-1", 100000, 10)
	number := f.order(uid, addr, v, 3)
	if _, err := f.svc.Pay(f.ctx, uid, number); err != nil {
		t.Fatal(err)
	}
	p1 := number + "-P1"
	stockAfterOrder := f.stock(v)

	if err := f.svc.HandleNotification(f.ctx, []byte(p1+":forged")); !errors.Is(err, payment.ErrInvalidSignature) {
		t.Errorf("forged notification: %v", err)
	}
	if err := f.svc.HandleNotification(f.ctx, []byte("ASH-NOPE-P1:signed")); !errors.Is(err, payment.ErrUnknownPayment) {
		t.Errorf("unknown transaction: %v", err)
	}

	f.prov.set(p1, payment.StatusPending, "pending", 300000+ship)
	if err := f.notify(p1); err != nil || f.paymentStatus(p1) != "PENDING" || f.orderStatus(number) != "WAITING_PAYMENT" {
		t.Errorf("pending: %v %s %s", err, f.paymentStatus(p1), f.orderStatus(number))
	}

	f.prov.set(p1, payment.StatusPaid, "settlement", 300000+ship)
	for range 3 {
		if err := f.notify(p1); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.str("SELECT p.status || ' ' || o.status || ' ' || (p.paid_at IS NOT NULL) || ' ' || (o.paid_at IS NOT NULL) || ' ' || p.payment_type FROM payments p JOIN orders o ON o.id = p.order_id WHERE p.provider_order_id = $1", p1); got != "PAID WAITING_CONFIRMATION true true bank_transfer" {
		t.Errorf("after paid ×3: %q", got)
	}
	if n := f.int("SELECT count(*) FROM payment_events WHERE kind = 'paid'"); n != 1 {
		t.Errorf("paid transitions = %d, want 1", n)
	}
	if n := f.int("SELECT count(*) FROM payment_events WHERE kind = 'duplicate' AND to_status = 'PAID'"); n != 2 {
		t.Errorf("duplicates recorded = %d, want 2", n)
	}
	if s := f.stock(v); s != stockAfterOrder {
		t.Errorf("stock changed on payment: %d → %d", stockAfterOrder, s)
	}
	if _, err := f.svc.Pay(f.ctx, uid, number); !errors.Is(err, payment.ErrNotPayable) {
		t.Errorf("pay a paid order: %v", err)
	}
}

func TestNotificationStatuses(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	v := f.product("Kaos A", "A-1", 50000, 50)

	for _, tc := range []struct {
		status payment.Status
		raw    string
	}{
		{payment.StatusFailed, "failure"},
		{payment.StatusDenied, "deny"},
		{payment.StatusCancelled, "cancel"},
		{payment.StatusExpired, "expire"},
	} {
		number := f.order(uid, addr, v, 1)
		if _, err := f.svc.Pay(f.ctx, uid, number); err != nil {
			t.Fatal(err)
		}
		before := f.stock(v)
		f.prov.set(number+"-P1", tc.status, tc.raw, 50000+ship)
		if err := f.notify(number + "-P1"); err != nil {
			t.Fatal(err)
		}
		// The attempt closes; before the deadline the order stays payable.
		if s := f.paymentStatus(number + "-P1"); s != string(tc.status) {
			t.Errorf("%s: attempt = %s", tc.raw, s)
		}
		if s := f.orderStatus(number); s != "WAITING_PAYMENT" || f.stock(v) != before {
			t.Errorf("%s: order %s, stock %d → %d", tc.raw, s, before, f.stock(v))
		}
	}

	// Expiry at the deadline cancels the order and gives the stock back.
	number := f.order(uid, addr, v, 4)
	if _, err := f.svc.Pay(f.ctx, uid, number); err != nil {
		t.Fatal(err)
	}
	before := f.stock(v)
	f.overdue(number, time.Second)
	f.prov.set(number+"-P1", payment.StatusExpired, "expire", 50000*4+ship)
	for range 2 { // expire notification delivered twice
		if err := f.notify(number + "-P1"); err != nil {
			t.Fatal(err)
		}
	}
	if s, r := f.orderStatus(number), f.str("SELECT cancel_reason FROM orders WHERE order_number = $1", number); s != "CANCELLED" || r != "PAYMENT_EXPIRED" {
		t.Errorf("expired order = %s (%s)", s, r)
	}
	if got := f.stock(v); got != before+4 {
		t.Errorf("stock = %d, want %d (restored once)", got, before+4)
	}
}

func TestOutOfOrderNotifications(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	v := f.product("Kaos A", "A-1", 70000, 10)

	// PAID is final: a late "expire" or "pending" changes nothing.
	number := f.order(uid, addr, v, 1)
	f.svc.Pay(f.ctx, uid, number)
	p1 := number + "-P1"
	f.prov.set(p1, payment.StatusPaid, "settlement", 70000+ship)
	f.notify(p1)
	stock := f.stock(v)
	f.overdue(number, time.Hour)
	for _, late := range []struct {
		s   payment.Status
		raw string
	}{{payment.StatusExpired, "expire"}, {payment.StatusPending, "pending"}, {payment.StatusCancelled, "cancel"}} {
		f.prov.set(p1, late.s, late.raw, 70000+ship)
		if err := f.notify(p1); err != nil {
			t.Fatal(err)
		}
	}
	if f.paymentStatus(p1) != "PAID" || f.orderStatus(number) != "WAITING_CONFIRMATION" || f.stock(v) != stock {
		t.Errorf("paid order changed: %s %s stock %d", f.paymentStatus(p1), f.orderStatus(number), f.stock(v))
	}
	if expired, _ := f.svc.ExpireOverdue(f.ctx, f.orderID(number)); expired || f.stock(v) != stock {
		t.Error("a paid order must never be expired or restocked")
	}

	// Paid after the buyer cancelled: flagged for a person, never revived.
	number2 := f.order(uid, addr, v, 2)
	f.svc.Pay(f.ctx, uid, number2)
	q1 := number2 + "-P1"
	if err := f.svc.CancelOrder(f.ctx, uid, number2); err != nil {
		t.Fatal(err)
	}
	stock = f.stock(v)
	f.prov.set(q1, payment.StatusPending, "pending", 140000+ship) // buyer still used the old checkout
	f.prov.cancelled = nil
	if err := f.notify(q1); err != nil {
		t.Fatal(err)
	}
	if len(f.prov.cancelled) != 1 || f.prov.cancelled[0] != q1 {
		t.Errorf("a transaction opened on a closed attempt must be cancelled at the provider: %v", f.prov.cancelled)
	}
	f.prov.set(q1, payment.StatusPaid, "settlement", 140000+ship)
	if err := f.notify(q1); err != nil {
		t.Fatal(err)
	}
	review := f.str("SELECT review_reason FROM payments WHERE provider_order_id = $1", q1)
	if f.paymentStatus(q1) != "CANCELLED" || f.orderStatus(number2) != "CANCELLED" || f.stock(v) != stock || !strings.Contains(review, "refund") {
		t.Errorf("paid after cancel: %s %s stock %d review %q", f.paymentStatus(q1), f.orderStatus(number2), f.stock(v), review)
	}
}

func TestAmountMismatchNeedsReview(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	v := f.product("Kaos A", "A-1", 100000, 10)
	number := f.order(uid, addr, v, 1)
	f.svc.Pay(f.ctx, uid, number)
	p1 := number + "-P1"

	f.prov.set(p1, payment.StatusPaid, "settlement", 1000) // not the order total
	if err := f.notify(p1); err != nil {
		t.Fatal(err)
	}
	if f.paymentStatus(p1) != "PENDING" || f.orderStatus(number) != "WAITING_PAYMENT" {
		t.Errorf("mismatched amount must not mark paid: %s %s", f.paymentStatus(p1), f.orderStatus(number))
	}
	if r := f.str("SELECT review_reason FROM payments WHERE provider_order_id = $1", p1); !strings.Contains(r, "amount mismatch") {
		t.Errorf("review reason = %q", r)
	}
	rep, err := f.svc.Reconcile(f.ctx, 10)
	if err != nil || len(rep.Review) != 1 || rep.Review[0].ProviderOrderID != p1 {
		t.Errorf("reconcile review list: %+v %v", rep, err)
	}
}

func TestCancelUnpaidOrder(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	other, _ := f.user("b@example.com")
	v := f.product("Kaos A", "A-1", 80000, 10)

	number := f.order(uid, addr, v, 3)
	if s := f.stock(v); s != 7 {
		t.Fatalf("stock after order = %d", s)
	}
	if err := f.svc.CancelOrder(f.ctx, other, number); !errors.Is(err, payment.ErrNotFound) {
		t.Errorf("another user's cancel: %v", err)
	}
	if err := f.svc.CancelOrder(f.ctx, uid, number); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.CancelOrder(f.ctx, uid, number); err != nil { // double cancel
		t.Errorf("second cancel: %v", err)
	}
	if s, r := f.orderStatus(number), f.str("SELECT cancel_reason FROM orders WHERE order_number = $1", number); s != "CANCELLED" || r != "USER_CANCELLED" || f.stock(v) != 10 {
		t.Errorf("cancelled: %s %s stock %d", s, r, f.stock(v))
	}

	// With an open card transaction: cancelled at the provider first.
	number = f.order(uid, addr, v, 1)
	f.svc.Pay(f.ctx, uid, number)
	f.prov.set(number+"-P1", payment.StatusPending, "pending", 80000+ship)
	if err := f.svc.CancelOrder(f.ctx, uid, number); err != nil {
		t.Fatal(err)
	}
	if f.paymentStatus(number+"-P1") != "CANCELLED" || f.orderStatus(number) != "CANCELLED" || f.stock(v) != 10 {
		t.Errorf("cancel with pending attempt: %s %s %d", f.paymentStatus(number+"-P1"), f.orderStatus(number), f.stock(v))
	}

	// Provider unreachable: the order is kept (it might be paid meanwhile).
	number = f.order(uid, addr, v, 1)
	f.svc.Pay(f.ctx, uid, number)
	f.prov.set(number+"-P1", payment.StatusPending, "pending", 80000+ship)
	f.prov.cancelErr = payment.ErrProviderUnavailable
	if err := f.svc.CancelOrder(f.ctx, uid, number); !errors.Is(err, payment.ErrCancelUnavailable) || f.orderStatus(number) != "WAITING_PAYMENT" || f.stock(v) != 9 {
		t.Errorf("provider down: %v %s %d", err, f.orderStatus(number), f.stock(v))
	}
	f.prov.cancelErr = nil

	// The provider says it is paid: cancelling becomes the payment.
	f.prov.set(number+"-P1", payment.StatusPaid, "settlement", 80000+ship)
	if err := f.svc.CancelOrder(f.ctx, uid, number); !errors.Is(err, payment.ErrAlreadyPaid) {
		t.Errorf("cancel a paid order: %v", err)
	}
	if f.orderStatus(number) != "WAITING_CONFIRMATION" || f.stock(v) != 9 {
		t.Errorf("paid order: %s stock %d", f.orderStatus(number), f.stock(v))
	}
	if err := f.svc.CancelOrder(f.ctx, uid, number); !errors.Is(err, payment.ErrAlreadyPaid) {
		t.Errorf("cancel after paid: %v", err)
	}
}

func TestConcurrentCancelAndExpireRestoreOnce(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	v := f.product("Kaos A", "A-1", 80000, 20)
	number := f.order(uid, addr, v, 5)
	f.overdue(number, time.Minute)
	id := f.orderID(number)

	// Several "processes" race: buyer cancels, page views and the reconcile
	// command expire the same order.
	var wg sync.WaitGroup
	for i := range 8 {
		svc := f.newService(f.prov)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				svc.CancelOrder(f.ctx, uid, number)
			} else {
				svc.ExpireOverdue(f.ctx, id)
			}
		}()
	}
	wg.Wait()
	if s := f.stock(v); s != 20 {
		t.Errorf("stock = %d, want 20 (restored exactly once)", s)
	}
	if n := f.int("SELECT count(*) FROM payment_events WHERE kind = 'order_cancelled'"); n != 1 || f.orderStatus(number) != "CANCELLED" {
		t.Errorf("cancellations recorded = %d, status %s", n, f.orderStatus(number))
	}
}

func TestExpireOverdue(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	v := f.product("Kaos A", "A-1", 60000, 20)

	// No payment started: expires as soon as the deadline passes.
	number := f.order(uid, addr, v, 2)
	if done, _ := f.svc.ExpireOverdue(f.ctx, f.orderID(number)); done {
		t.Error("an order before its deadline must not expire")
	}
	f.overdue(number, time.Second)
	for range 2 {
		if _, err := f.svc.ExpireOverdue(f.ctx, f.orderID(number)); err != nil {
			t.Fatal(err)
		}
	}
	if f.orderStatus(number) != "CANCELLED" || f.stock(v) != 20 {
		t.Errorf("expired: %s stock %d", f.orderStatus(number), f.stock(v))
	}

	// An open checkout gets a grace period, then is checked at the provider.
	number = f.order(uid, addr, v, 2)
	f.svc.Pay(f.ctx, uid, number)
	p1 := number + "-P1"
	f.overdue(number, time.Minute)
	if done, _ := f.svc.ExpireOverdue(f.ctx, f.orderID(number)); done || f.orderStatus(number) != "WAITING_PAYMENT" {
		t.Error("within the grace period the order must wait")
	}
	f.overdue(number, 10*time.Minute)
	f.prov.statusErr = payment.ErrProviderUnavailable
	if done, err := f.svc.ExpireOverdue(f.ctx, f.orderID(number)); done || err != nil || f.orderStatus(number) != "WAITING_PAYMENT" {
		t.Errorf("provider down: must retry later (%v %v)", done, err)
	}
	f.prov.statusErr = nil
	// Never used at the provider (no transaction): safe to cancel.
	if done, err := f.svc.ExpireOverdue(f.ctx, f.orderID(number)); !done || err != nil {
		t.Errorf("unused checkout: %v %v", done, err)
	}
	if f.paymentStatus(p1) != "EXPIRED" || f.stock(v) != 20 {
		t.Errorf("after expiry: %s stock %d", f.paymentStatus(p1), f.stock(v))
	}

	// Still pending at the provider: cancelled there before the order goes.
	number = f.order(uid, addr, v, 1)
	f.svc.Pay(f.ctx, uid, number)
	f.prov.set(number+"-P1", payment.StatusPending, "pending", 60000+ship)
	f.overdue(number, 10*time.Minute)
	if done, err := f.svc.ExpireOverdue(f.ctx, f.orderID(number)); !done || err != nil || f.stock(v) != 20 {
		t.Errorf("pending at provider: %v %v stock %d", done, err, f.stock(v))
	}
	if f.prov.tx[number+"-P1"].Status != payment.StatusCancelled {
		t.Error("the provider transaction must be cancelled before the order")
	}
}

func TestRestockRollsBackOnError(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	v := f.product("Kaos A", "A-1", 60000, 10)
	number := f.order(uid, addr, v, 4)
	f.svc.Pay(f.ctx, uid, number)

	// Make the final order update fail inside the transaction.
	f.exec(`CREATE FUNCTION fail_cancel() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.status = 'CANCELLED' THEN RAISE EXCEPTION 'simulated failure'; END IF; RETURN NEW; END $$`)
	f.exec("CREATE TRIGGER fail_cancel BEFORE UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION fail_cancel()")
	f.prov.tx[number+"-P1"] = payment.ProviderStatus{} // provider has no transaction
	if err := f.svc.CancelOrder(f.ctx, uid, number); err == nil {
		t.Fatal("expected the simulated failure")
	}
	if f.stock(v) != 6 || f.orderStatus(number) != "WAITING_PAYMENT" || f.paymentStatus(number+"-P1") != "PENDING" {
		t.Errorf("partial cancel committed: stock %d, %s, %s", f.stock(v), f.orderStatus(number), f.paymentStatus(number+"-P1"))
	}
	if n := f.int("SELECT count(*) FROM payment_events WHERE kind = 'order_cancelled'"); n != 0 {
		t.Errorf("events of a rolled back cancel: %d", n)
	}
	f.exec("DROP TRIGGER fail_cancel ON orders")
	if err := f.svc.CancelOrder(f.ctx, uid, number); err != nil || f.stock(v) != 10 {
		t.Errorf("cancel after fix: %v stock %d", err, f.stock(v))
	}
}

func TestReservedVariantsAndMissingVariants(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	a := f.product("Kaos A", "A-1", 50000, 10)
	b := f.product("Kaos B", "B-1", 70000, 10)
	number := f.order(uid, addr, a, 1)

	// A variant held by an unpaid order cannot be deleted.
	_, err := f.db.Exec(f.ctx, "DELETE FROM products WHERE id = (SELECT product_id FROM product_variants WHERE id = $1)", a)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23001" {
		t.Errorf("delete reserved variant: %v", err)
	}
	if err := f.svc.CancelOrder(f.ctx, uid, number); err != nil {
		t.Fatal(err)
	}
	f.exec("DELETE FROM products WHERE id = (SELECT product_id FROM product_variants WHERE id = $1)", a)

	// An order line whose variant is gone (older data): the rest is
	// restored, the gap is recorded, nothing crashes.
	f.carts.AddItem(f.ctx, uid, b, 2)
	c := f.product("Kaos C", "C-1", 30000, 10)
	f.carts.AddItem(f.ctx, uid, c, 3)
	ids := []int64{
		f.int("SELECT ci.id FROM cart_items ci JOIN carts c ON c.id = ci.cart_id WHERE c.user_id = $1 AND ci.variant_id = $2", uid, b),
		f.int("SELECT ci.id FROM cart_items ci JOIN carts c ON c.id = ci.cart_id WHERE c.user_id = $1 AND ci.variant_id = $2", uid, c),
	}
	placed, err := f.checkout.PlaceOrder(f.ctx, uid, quoted(f.t, f.checkout, uid, checkout.PlaceRequest{Source: checkout.Source{CartItemIDs: ids}, AddressID: addr}))
	if err != nil {
		t.Fatal(err)
	}
	f.exec("UPDATE order_items SET variant_id = NULL WHERE variant_id = $1", c)
	if err := f.svc.CancelOrder(f.ctx, uid, placed.OrderNumber); err != nil {
		t.Fatal(err)
	}
	if f.stock(b) != 10 || f.stock(c) != 7 {
		t.Errorf("stock b=%d (want 10), c=%d (want 7, no target to restore)", f.stock(b), f.stock(c))
	}
	if d := f.str("SELECT detail FROM payment_events WHERE kind = 'restock_missing'"); !strings.Contains(d, "1 line") {
		t.Errorf("missing variant not recorded: %q", d)
	}
}

func TestReconcile(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	v := f.product("Kaos A", "A-1", 90000, 10)

	// A payment whose notification never arrived.
	paid := f.order(uid, addr, v, 1)
	f.svc.Pay(f.ctx, uid, paid)
	f.prov.set(paid+"-P1", payment.StatusPaid, "settlement", 90000+ship)
	f.exec("UPDATE payments SET created_at = now() - interval '20 minutes' WHERE provider_order_id = $1", paid+"-P1")
	// An unpaid order past its deadline.
	late := f.order(uid, addr, v, 2)
	f.overdue(late, time.Minute)

	cmd := f.newService(f.prov) // the command runs as its own process
	rep, err := cmd.Reconcile(f.ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Checked != 1 || rep.Updated != 1 || rep.Expired != 1 || rep.Errors != 0 {
		t.Errorf("report = %+v", rep)
	}
	if f.orderStatus(paid) != "WAITING_CONFIRMATION" || f.orderStatus(late) != "CANCELLED" || f.stock(v) != 9 {
		t.Errorf("after reconcile: %s %s stock %d", f.orderStatus(paid), f.orderStatus(late), f.stock(v))
	}
	if rep, _ = cmd.Reconcile(f.ctx, 100); rep.Updated != 0 || rep.Expired != 0 {
		t.Errorf("second run must change nothing: %+v", rep)
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
