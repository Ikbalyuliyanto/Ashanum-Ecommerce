package review_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/address"
	"ecommerce/internal/category"
	"ecommerce/internal/checkout"
	"ecommerce/internal/product"
	"ecommerce/internal/review"
	"ecommerce/internal/shipping/shippingtest"
	"ecommerce/internal/testutil"
	"ecommerce/internal/validate"
)

type fixture struct {
	t        *testing.T
	ctx      context.Context
	db       *pgxpool.Pool
	svc      *review.Service
	checkout *checkout.Service
	addrs    *address.Service
	catID    int64
}

func newFixture(t *testing.T) *fixture {
	db := testutil.DB(t)
	f := &fixture{t: t, ctx: context.Background(), db: db, svc: review.NewService(review.NewRepository(db)),
		addrs: address.NewService(address.NewRepository(db), shippingtest.For(t, db).Service)}
	f.checkout = checkout.NewService(checkout.NewRepository(db), f.addrs, shippingtest.For(t, db).Service)
	c, err := category.NewService(category.NewRepository(db)).Create(f.ctx, category.Input{Name: "Atasan", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	f.catID = c.ID
	return f
}

func (f *fixture) product(name, sku string, price int64, stock int) (productID, variantID int64) {
	f.t.Helper()
	pid, err := product.NewService(product.NewRepository(f.db)).Create(f.ctx, product.Input{WeightGrams: 100,
		CategoryID: f.catID, Name: name, Active: true,
		Images:   []product.ImageInput{{URL: "data:image/svg+xml;base64,PHN2Zy8+"}},
		Variants: []product.VariantInput{{SKU: sku, Price: price, Stock: stock}},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return pid, f.int("SELECT id FROM product_variants WHERE sku = $1", sku)
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

// order buys one variant and returns the order number and its item id.
func (f *fixture) order(userID, addressID, variantID int64, completed bool) (string, int64) {
	f.t.Helper()
	placed, err := f.checkout.PlaceOrder(f.ctx, userID, quoted(f.t, f.checkout, userID, checkout.PlaceRequest{
		Source: checkout.Source{VariantID: variantID, Quantity: 1}, AddressID: addressID,
	}))
	if err != nil {
		f.t.Fatal(err)
	}
	if completed {
		testutil.CompleteOrder(f.t, f.db, placed.OrderNumber, time.Now())
	}
	return placed.OrderNumber, f.int("SELECT i.id FROM order_items i JOIN orders o ON o.id = i.order_id WHERE o.order_number = $1", placed.OrderNumber)
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

func (f *fixture) rating(productID int64) (float64, int) {
	f.t.Helper()
	var avg float64
	var n int
	if err := f.db.QueryRow(f.ctx, "SELECT rating_average, rating_count FROM products WHERE id = $1", productID).Scan(&avg, &n); err != nil {
		f.t.Fatal(err)
	}
	return avg, n
}

func TestReviewEligibility(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	other, _ := f.user("b@example.com")
	_, v := f.product("Kaos A", "A-1", 100000, 20)

	// Only a COMPLETED order can be reviewed.
	_, unpaid := f.order(uid, addr, v, false) // WAITING_PAYMENT
	if _, err := f.svc.Create(f.ctx, uid, unpaid, review.Input{Rating: 5}); !errors.Is(err, review.ErrNotEligible) {
		t.Errorf("unpaid order: %v", err)
	}
	for _, status := range []string{"WAITING_CONFIRMATION", "PROCESSING", "SHIPPED"} {
		number, item := f.order(uid, addr, v, false)
		f.exec("UPDATE orders SET status = $2, paid_at = now() WHERE order_number = $1", number, status)
		if _, err := f.svc.Create(f.ctx, uid, item, review.Input{Rating: 5}); !errors.Is(err, review.ErrNotEligible) {
			t.Errorf("%s order: %v", status, err)
		}
	}
	number, cancelled := f.order(uid, addr, v, false)
	f.exec("UPDATE orders SET status = 'CANCELLED', cancelled_at = now() WHERE order_number = $1", number)
	if _, err := f.svc.Target(f.ctx, uid, cancelled); !errors.Is(err, review.ErrNotEligible) {
		t.Errorf("cancelled order: %v", err)
	}

	_, item := f.order(uid, addr, v, true)
	if _, err := f.svc.Create(f.ctx, other, item, review.Input{Rating: 5}); !errors.Is(err, review.ErrNotFound) {
		t.Errorf("another user's item: %v", err)
	}
	if _, err := f.svc.Target(f.ctx, other, item); !errors.Is(err, review.ErrNotFound) {
		t.Errorf("another user's target: %v", err)
	}
	id, err := f.svc.Create(f.ctx, uid, item, review.Input{Rating: 4, Comment: "  Bahannya bagus.\r\n\r\n\r\nSesuai.  "})
	if err != nil {
		t.Fatal(err)
	}
	rv, err := f.svc.Get(f.ctx, uid, id)
	if err != nil || rv.Rating != 4 || rv.Comment != "Bahannya bagus.\n\nSesuai." || rv.ProductName != "Kaos A" {
		t.Errorf("review = %+v %v", rv, err)
	}
	if tgt, _ := f.svc.Target(f.ctx, uid, item); tgt.ReviewID != id {
		t.Errorf("target must point at the existing review: %+v", tgt)
	}
}

func TestReviewValidationAndDuplicate(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	_, v := f.product("Kaos A", "A-1", 100000, 20)
	_, item := f.order(uid, addr, v, true)

	for _, in := range []review.Input{{Rating: 0}, {Rating: 6}, {Rating: -1}, {Rating: 5, Comment: strings.Repeat("a", 1001)}} {
		_, err := f.svc.Create(f.ctx, uid, item, in)
		var verr validate.Errors
		if !errors.As(err, &verr) {
			t.Errorf("%+v must be rejected: %v", in.Rating, err)
		}
	}
	if n := f.int("SELECT count(*) FROM product_reviews"); n != 0 {
		t.Fatalf("invalid reviews stored: %d", n)
	}
	if _, err := f.svc.Create(f.ctx, uid, item, review.Input{Rating: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Create(f.ctx, uid, item, review.Input{Rating: 3}); !errors.Is(err, review.ErrAlreadyReviewed) {
		t.Errorf("duplicate review: %v", err)
	}
}

func TestRatingAggregate(t *testing.T) {
	f := newFixture(t)
	a, addrA := f.user("a@example.com")
	b, addrB := f.user("b@example.com")
	pid, v := f.product("Kaos A", "A-1", 100000, 20)

	_, itemA := f.order(a, addrA, v, true)
	_, itemB := f.order(b, addrB, v, true)
	idA, _ := f.svc.Create(f.ctx, a, itemA, review.Input{Rating: 5})
	idB, _ := f.svc.Create(f.ctx, b, itemB, review.Input{Rating: 2})
	if avg, n := f.rating(pid); avg != 3.5 || n != 2 {
		t.Errorf("aggregate = %.2f / %d, want 3.5 / 2", avg, n)
	}

	if err := f.svc.Update(f.ctx, b, idB, review.Input{Rating: 4, Comment: "Setelah dicuci tetap bagus"}); err != nil {
		t.Fatal(err)
	}
	if avg, n := f.rating(pid); avg != 4.5 || n != 2 {
		t.Errorf("aggregate after edit = %.2f / %d, want 4.5 / 2", avg, n)
	}
	if err := f.svc.Update(f.ctx, a, idB, review.Input{Rating: 1}); !errors.Is(err, review.ErrNotFound) {
		t.Errorf("editing another user's review: %v", err)
	}
	if _, err := f.svc.Get(f.ctx, b, idA); !errors.Is(err, review.ErrNotFound) {
		t.Errorf("reading another user's review: %v", err)
	}
	if err := f.svc.Update(f.ctx, a, idA, review.Input{Rating: 9}); err == nil {
		t.Error("edit must validate the rating too")
	}

	// The cache can be rebuilt from product_reviews (repair).
	f.exec("UPDATE products SET rating_average = 1, rating_count = 99 WHERE id = $1", pid)
	if err := f.svc.RecalculateProductRating(f.ctx, pid); err != nil {
		t.Fatal(err)
	}
	if avg, n := f.rating(pid); avg != 4.5 || n != 2 {
		t.Errorf("recalculated = %.2f / %d", avg, n)
	}
	f.exec("UPDATE products SET rating_average = 0, rating_count = 0")
	if fixed, err := f.svc.RecalculateAll(f.ctx); err != nil || fixed != 1 {
		t.Errorf("repair all: %d %v", fixed, err)
	}
	if fixed, _ := f.svc.RecalculateAll(f.ctx); fixed != 0 {
		t.Errorf("second repair must change nothing: %d", fixed)
	}
}

func TestReviewAfterProductDeleted(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	pid, v := f.product("Kaos Lama", "OLD-1", 100000, 5)
	_, item := f.order(uid, addr, v, true)
	_, item2 := f.order(uid, addr, v, true)
	if _, err := f.svc.Create(f.ctx, uid, item, review.Input{Rating: 5}); err != nil {
		t.Fatal(err)
	}

	f.exec("DELETE FROM products WHERE id = $1", pid) // allowed: no unpaid order holds it
	if _, err := f.svc.Create(f.ctx, uid, item2, review.Input{Rating: 3, Comment: "Masih bisa diulas"}); err != nil {
		t.Fatalf("review after product deletion: %v", err)
	}
	h, err := f.svc.History(f.ctx, uid, "", 1)
	if err != nil || len(h.Reviews) != 2 {
		t.Fatalf("history = %+v %v", h, err)
	}
	for _, r := range h.Reviews {
		if r.ProductName != "Kaos Lama" || r.ProductListed {
			t.Errorf("history must use the order item snapshot: %+v", r)
		}
	}
}

func TestConcurrentReview(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	pid, v := f.product("Kaos A", "A-1", 100000, 5)
	_, item := f.order(uid, addr, v, true)

	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, dup := 0, 0
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.svc.Create(f.ctx, uid, item, review.Input{Rating: 5})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, review.ErrAlreadyReviewed):
				dup++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || dup != 5 || f.int("SELECT count(*) FROM product_reviews") != 1 {
		t.Errorf("ok=%d duplicates=%d", ok, dup)
	}
	if _, n := f.rating(pid); n != 1 {
		t.Errorf("rating count = %d", n)
	}
}

func TestReviewHistoryAndProductPagination(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("siti.rahma@example.com")
	f.exec("INSERT INTO user_profiles (user_id, first_name, last_name) VALUES ($1, 'Siti', 'Rahmawati')", uid)
	anon, anonAddr := f.user("ikbal@example.com") // no profile: masked e-mail name
	pid, v := f.product("Kaos A", "A-1", 50000, 50)

	for i := range 12 {
		_, item := f.order(uid, addr, v, true)
		if _, err := f.svc.Create(f.ctx, uid, item, review.Input{Rating: 1 + i%5, Comment: fmt.Sprintf("Ulasan %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	_, pending := f.order(uid, addr, v, true)
	_, anonItem := f.order(anon, anonAddr, v, true)
	f.svc.Create(f.ctx, anon, anonItem, review.Input{Rating: 5})

	p1, _ := f.svc.History(f.ctx, uid, "", 1)
	p2, _ := f.svc.History(f.ctx, uid, "", 2)
	if len(p1.Reviews) != 10 || !p1.HasNext || len(p2.Reviews) != 2 || p2.HasNext || p1.PendingCount != 1 {
		t.Errorf("history pages: %d/%v, %d/%v, pending %d", len(p1.Reviews), p1.HasNext, len(p2.Reviews), p2.HasNext, p1.PendingCount)
	}
	if p1.Reviews[0].Comment != "Ulasan 11" {
		t.Errorf("newest first: %q", p1.Reviews[0].Comment)
	}
	pend, _ := f.svc.History(f.ctx, uid, "pending", 1)
	if len(pend.Pending) != 1 || pend.Pending[0].OrderItemID != pending {
		t.Errorf("pending tab = %+v", pend.Pending)
	}

	avg, count := f.rating(pid)
	pp, err := f.svc.ProductPage(f.ctx, pid, avg, count, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pp.Reviews) != review.ProductPageSize || !pp.HasNext || pp.Summary.Count != 13 || len(pp.Summary.Bars) != 5 {
		t.Errorf("product page: %d reviews, next %v, summary %+v", len(pp.Reviews), pp.HasNext, pp.Summary)
	}
	if pp.Reviews[0].Author != "i****" {
		t.Errorf("e-mail based name must be masked: %q", pp.Reviews[0].Author)
	}
	if pp.Reviews[1].Author != "S*** R********" {
		t.Errorf("profile name must be masked: %q", pp.Reviews[1].Author)
	}
	total := 0
	for _, b := range pp.Summary.Bars {
		total += b.Count
	}
	if total != 13 || math.Abs(avg-float64(1+2+3+4+5+1+2+3+4+5+1+2+5)/13) > 1e-9 {
		t.Errorf("distribution %d, average %.4f", total, avg)
	}
	last, _ := f.svc.ProductPage(f.ctx, pid, avg, count, 3)
	if len(last.Reviews) != 3 || last.HasNext {
		t.Errorf("last product page: %d %v", len(last.Reviews), last.HasNext)
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
