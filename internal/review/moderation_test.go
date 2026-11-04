package review_test

import (
	"errors"
	"testing"

	"ecommerce/internal/review"
	"ecommerce/internal/testutil"
	"ecommerce/internal/validate"
)

func TestModeration(t *testing.T) {
	f := newFixture(t)
	admin := testutil.CreateAdmin(t, f.db, "admin@example.com")
	pid, vid := f.product("Kaos A", "A-1", 100000, 10)
	write := func(email string, rating int, comment string) (int64, int64) {
		uid, addr := f.user(email)
		_, item := f.order(uid, addr, vid, true)
		id, err := f.svc.Create(f.ctx, uid, item, review.Input{Rating: rating, Comment: comment})
		if err != nil {
			t.Fatal(err)
		}
		return uid, id
	}
	_, good := write("a@example.com", 5, "Bagus sekali")
	spammer, spam := write("b@example.com", 1, "Kunjungi situs judi kami!!!")
	if avg, n := f.rating(pid); avg != 3 || n != 2 {
		t.Fatalf("before moderation: %.2f (%d)", avg, n)
	}
	public := func() []review.PublicReview {
		p, err := f.svc.ProductPage(f.ctx, pid, 0, 1, 1) // count > 0 forces the query
		if err != nil {
			t.Fatal(err)
		}
		return p.Reviews
	}

	var v validate.Errors
	if err := f.svc.Hide(f.ctx, admin, spam, "  "); !errors.As(err, &v) {
		t.Errorf("hide without a reason: %v", err)
	}
	if err := f.svc.Hide(f.ctx, admin, spam, "Spam / promosi situs lain"); err != nil {
		t.Fatal(err)
	}
	if avg, n := f.rating(pid); avg != 5 || n != 1 {
		t.Errorf("a hidden review still counts: %.2f (%d)", avg, n)
	}
	if rs := public(); len(rs) != 1 || rs[0].ID != good {
		t.Errorf("a hidden review is public: %+v", rs)
	}
	p, _ := f.svc.ProductPage(f.ctx, pid, 5, 1, 1)
	if p.Summary.Bars[4].Count != 0 || p.Summary.Bars[0].Count != 1 { // bars run 5 → 1
		t.Errorf("distribution counts a hidden review: %+v", p.Summary.Bars)
	}
	if err := f.svc.Hide(f.ctx, admin, spam, "lagi"); !errors.Is(err, review.ErrModerated) {
		t.Errorf("hide twice: %v", err)
	}

	// The buyer still sees (and may edit) the review; it stays hidden.
	h, err := f.svc.History(f.ctx, spammer, "", 1)
	if err != nil || len(h.Reviews) != 1 || !h.Reviews[0].Hidden {
		t.Errorf("buyer history: %+v %v", h.Reviews, err)
	}
	if err := f.svc.Update(f.ctx, spammer, spam, review.Input{Rating: 2, Comment: "diubah"}); err != nil {
		t.Fatal(err)
	}
	if avg, n := f.rating(pid); avg != 5 || n != 1 {
		t.Errorf("an edit must not bring a hidden review back: %.2f (%d)", avg, n)
	}
	if r, _ := f.svc.Get(f.ctx, spammer, spam); !r.Edited() || !r.Hidden {
		t.Errorf("edited hidden review: %+v", r)
	}
	if got := f.int("SELECT count(*) FROM product_reviews"); got != 2 {
		t.Errorf("reviews are never deleted, got %d rows", got)
	}

	if err := f.svc.Unhide(f.ctx, admin, spam); err != nil {
		t.Fatal(err)
	}
	if avg, n := f.rating(pid); avg != 3.5 || n != 2 {
		t.Errorf("after unhide: %.2f (%d)", avg, n)
	}
	if rs := public(); len(rs) != 2 {
		t.Errorf("unhidden review not public: %+v", rs)
	}
	if err := f.svc.Unhide(f.ctx, admin, spam); !errors.Is(err, review.ErrModerated) {
		t.Errorf("unhide twice: %v", err)
	}
	if err := f.svc.Hide(f.ctx, admin, 999999, "tidak ada"); !errors.Is(err, review.ErrNotFound) {
		t.Errorf("unknown review: %v", err)
	}
	if got := f.int("SELECT count(*) FROM admin_audit_logs WHERE entity_type = 'REVIEW' AND entity_id = $1", spam); got != 2 {
		t.Errorf("moderation audit = %d, want 2", got)
	}

	page, err := f.svc.AdminList(f.ctx, review.AdminFilter{Rating: 2})
	if err != nil || len(page.Reviews) != 1 || page.Reviews[0].ID != spam || page.Reviews[0].Author == "b@example.com" {
		t.Errorf("admin list by rating (masked author): %+v %v", page.Reviews, err)
	}
	if n, _ := f.svc.RecalculateAll(f.ctx); n != 0 {
		t.Errorf("the cache was out of sync with visible reviews: %d repaired", n)
	}
}
