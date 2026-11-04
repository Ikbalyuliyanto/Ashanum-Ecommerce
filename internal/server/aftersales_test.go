package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"ecommerce/internal/testutil"
)

func TestReviewFlow(t *testing.T) {
	base, db := newApp(t)
	seedCatalog(t, db)
	b := newBrowser(t, base)
	b.register("siti@example.com")
	b.addAddress()
	itemOf := func(number string) int64 {
		return queryID(t, db, "SELECT i.id FROM order_items i JOIN orders o ON o.id = i.order_id WHERE o.order_number = $1", number)
	}

	// Not completed: no review or return actions, and the server refuses.
	unpaid := buyNow(t, b, db)
	if d := b.get("/account/orders/" + unpaid); strings.Contains(d.body, "Beri Ulasan") || strings.Contains(d.body, "Ajukan Pengembalian") {
		t.Error("an unpaid order offers no review or return")
	}
	res := b.get(fmt.Sprintf("/account/reviews/new?item=%d", itemOf(unpaid)))
	if res.status != http.StatusSeeOther {
		t.Fatalf("review form for an unpaid order: %d", res.status)
	}
	expect(t, b.get(res.location), http.StatusOK, "Ulasan hanya bisa diberikan untuk pesanan yang sudah selesai.")
	if res := b.post("/account/reviews", url.Values{"item": {fmt.Sprint(itemOf(unpaid))}, "rating": {"5"}}); res.status != http.StatusSeeOther ||
		queryID(t, db, "SELECT count(*) FROM product_reviews") != 0 {
		t.Errorf("posting a review for an unpaid order: %d", res.status)
	}

	number := buyNow(t, b, db)
	testutil.CompleteOrder(t, db, number, time.Now())
	item := itemOf(number)
	orderPath := "/account/orders/" + number
	expect(t, b.get(orderPath), http.StatusOK, "Pesanan selesai", "Beri Ulasan", "Ajukan Pengembalian", "Pengembalian bisa diajukan sampai")

	form := b.get(fmt.Sprintf("/account/reviews/new?item=%d&return_to=%s", item, url.QueryEscape(orderPath)))
	expect(t, form, http.StatusOK, "Kaos Ashanum Basic", `name="rating"`, "Kirim Ulasan")
	expect(t, b.post("/account/reviews", url.Values{"item": {fmt.Sprint(item)}, "rating": {"0"}}),
		http.StatusUnprocessableEntity, "Pilih rating 1 sampai 5 bintang.")

	res = b.post("/account/reviews", url.Values{"item": {fmt.Sprint(item)}, "rating": {"5"}, "comment": {"Bahannya adem, ukurannya pas."}, "return_to": {orderPath}})
	if res.status != http.StatusSeeOther || res.location != orderPath {
		t.Fatalf("create review: %d %q", res.status, res.location)
	}
	detail := b.get(orderPath)
	expect(t, detail, http.StatusOK, "Terima kasih! Ulasan Anda sudah terkirim.", "Ubah Ulasan")
	if strings.Contains(detail.body, ">Beri Ulasan<") {
		t.Error("a reviewed order offers no second review")
	}
	reviewID := queryID(t, db, "SELECT id FROM product_reviews")
	if res := b.post("/account/reviews", url.Values{"item": {fmt.Sprint(item)}, "rating": {"1"}}); res.status != http.StatusSeeOther ||
		!strings.HasPrefix(res.location, fmt.Sprintf("/account/reviews/%d/edit", reviewID)) {
		t.Errorf("a second review must lead to editing the first: %d %q", res.status, res.location)
	}

	// Product page: masked buyer, variant, comment; no e-mail.
	page := b.get("/products/kaos-ashanum-basic")
	expect(t, page, http.StatusOK, "Ulasan Pembeli", "Bahannya adem, ukurannya pas.", "s***", "Varian: Hitam / M", "5.0", "1 ulasan")
	if strings.Contains(page.body, "siti@example.com") {
		t.Error("the product page must not show the reviewer's e-mail")
	}
	expect(t, b.get("/products/kaos-ashanum-basic/reviews"), http.StatusOK, "Ulasan Pembeli", "Bahannya adem")
	frag := b.doWithTarget("/products/kaos-ashanum-basic/reviews?page=1", "reviews-more")
	if frag.status != http.StatusOK || strings.Contains(frag.body, "<html") || !strings.Contains(frag.body, "Bahannya adem") {
		t.Errorf("reviews load-more fragment: %d %q", frag.status, frag.body)
	}
	expect(t, b.get("/products/tidak-ada/reviews"), http.StatusNotFound)

	// History and edit.
	expect(t, b.get("/account/reviews"), http.StatusOK, "Ulasan Saya", "Bahannya adem", "Edit")
	editPath := fmt.Sprintf("/account/reviews/%d", reviewID)
	expect(t, b.get(editPath+"/edit"), http.StatusOK, "Ubah Ulasan", "Bahannya adem", `value="5" checked`)
	if res := b.post(editPath, url.Values{"rating": {"3"}, "comment": {"Setelah dicuci agak menyusut."}}); res.status != http.StatusSeeOther {
		t.Fatalf("edit review: %d", res.status)
	}
	expect(t, b.get("/products/kaos-ashanum-basic"), http.StatusOK, "3.0", "Setelah dicuci agak menyusut.")

	other := newBrowser(t, base)
	other.register("budi@example.com")
	expect(t, other.get(editPath+"/edit"), http.StatusNotFound)
	expect(t, other.post(editPath, url.Values{"rating": {"1"}}), http.StatusNotFound)
	expect(t, other.get(fmt.Sprintf("/account/reviews/new?item=%d", item)), http.StatusNotFound)
	expect(t, other.post("/account/reviews", url.Values{"item": {fmt.Sprint(item)}, "rating": {"1"}}), http.StatusNotFound)
	if queryString(t, db, "SELECT comment FROM product_reviews WHERE id = $1", reviewID) != "Setelah dicuci agak menyusut." {
		t.Error("another user changed the review")
	}
}

func TestReturnFlow(t *testing.T) {
	base, db := newApp(t)
	seedCatalog(t, db)
	b := newBrowser(t, base)
	b.register("siti@example.com")
	b.addAddress()
	number := buyNow(t, b, db)
	testutil.CompleteOrder(t, db, number, time.Now())
	item := queryID(t, db, "SELECT i.id FROM order_items i JOIN orders o ON o.id = i.order_id WHERE o.order_number = $1", number)
	newPath := "/account/returns/new?order=" + number

	form := b.get(newPath)
	expect(t, form, http.StatusOK, "Ajukan Pengembalian", "Pengembalian Dana", "Tukar Barang", "Barang rusak", "Masalah ukuran/varian", "Kirim Pengajuan")
	key := hiddenValue(t, form.body, "key")

	summary := b.doWithTarget(fmt.Sprintf("%s&item=%d&qty_%d=1&type=REFUND", newPath, item, item), "return-summary")
	if summary.status != http.StatusOK || strings.Contains(summary.body, "<html") || !strings.Contains(summary.body, "Estimasi pengembalian dana") ||
		!strings.Contains(summary.body, "Rp129.000") {
		t.Errorf("summary fragment: %d %s", summary.status, summary.body)
	}

	expect(t, b.post("/account/returns", url.Values{"order": {number}, "key": {key}, "type": {"REFUND"}}),
		http.StatusUnprocessableEntity, "Pilih minimal satu barang", "Pilih alasan pengembalian.")
	valid := url.Values{"order": {number}, "key": {key}, "item": {fmt.Sprint(item)}, fmt.Sprintf("qty_%d", item): {"1"},
		"type": {"REFUND"}, "reason": {"DAMAGED"}, "description": {"Jahitan lepas"},
		"evidence": {"https://drive.google.com/file/d/abc/view\nhttp://tidak-aman.example/a.jpg"}}
	expect(t, b.post("/account/returns", valid), http.StatusUnprocessableEntity, "Tautan bukti ke-2")

	valid.Set("evidence", "https://drive.google.com/file/d/abc/view")
	res := b.post("/account/returns", valid)
	if res.status != http.StatusSeeOther || !strings.HasPrefix(res.location, "/account/returns/RET-") {
		t.Fatalf("create return: %d %q\n%s", res.status, res.location, res.body)
	}
	detailPath := res.location
	retNumber := strings.TrimPrefix(detailPath, "/account/returns/")
	if again := b.post("/account/returns", valid); again.location != detailPath {
		t.Errorf("double submit: %q", again.location)
	}
	expect(t, b.get(detailPath), http.StatusOK, "Pengajuan pengembalian terkirim.", retNumber, "Menunggu Persetujuan",
		"Pengembalian Dana", "Rp129.000", "Barang rusak", "Jahitan lepas", "Bukti 1", number, "Batalkan Pengajuan")
	expect(t, b.get("/account/returns"), http.StatusOK, retNumber, "Menunggu Persetujuan", "Rp129.000")
	orderPage := b.get("/account/orders/" + number)
	expect(t, orderPage, http.StatusOK, retNumber)
	if strings.Contains(orderPage.body, "Ajukan Pengembalian") {
		t.Error("every unit is in a return: no new return offered")
	}

	// Isolation, and no approval route for buyers.
	other := newBrowser(t, base)
	other.register("budi@example.com")
	expect(t, other.get(detailPath), http.StatusNotFound)
	expect(t, other.get(newPath), http.StatusNotFound)
	expect(t, other.post("/account/returns", valid), http.StatusNotFound)
	expect(t, other.post(detailPath+"/cancel", nil), http.StatusNotFound)
	for _, p := range []string{"/approve", "/reject", "/receive", "/refund", "/complete"} {
		if res := b.post(detailPath+p, nil); res.status < 400 {
			t.Errorf("POST %s must not exist: %d", p, res.status)
		}
	}
	if s := queryString(t, db, "SELECT status FROM returns WHERE return_number = $1", retNumber); s != "REQUESTED" {
		t.Errorf("status = %s", s)
	}

	// The buyer withdraws the request.
	if res := b.post(detailPath+"/cancel", nil); res.status != http.StatusSeeOther {
		t.Fatalf("cancel: %d", res.status)
	}
	expect(t, b.get(detailPath), http.StatusOK, "Pengajuan pengembalian dibatalkan.", "Dibatalkan", "Dibatalkan oleh pembeli")
	expect(t, b.get("/account/orders/"+number), http.StatusOK, "Ajukan Pengembalian")

	expect(t, b.get("/account"), http.StatusOK, `href="/account/reviews"`, `href="/account/returns"`)
}
