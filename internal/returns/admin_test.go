package returns_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"ecommerce/internal/returns"
	"ecommerce/internal/testutil"
)

func TestPartialApproval(t *testing.T) {
	f := newFixture(t)
	admin := testutil.CreateAdmin(t, f.db, "admin@example.com")
	uid, addr := f.user("a@example.com")
	v := f.product("Kaos A", "A-1", 100000, 50)
	number := f.completed(uid, addr, time.Hour, line{v, 3})
	n, err := f.svc.Create(f.ctx, uid, request(number, returns.TypeRefund, returns.ItemRequest{OrderItemID: f.item(number, v), Quantity: 3}))
	if err != nil {
		t.Fatal(err)
	}
	id := f.id(n)
	lineID := f.lineIDs(id)[0]
	approve := func(q int) error {
		return f.svc.Approve(f.ctx, admin, id, returns.ApproveInput{Quantities: map[int64]int{lineID: q}})
	}

	if err := approve(4); !isValidation(err) {
		t.Errorf("approve more than requested: %v", err)
	}
	if err := approve(-1); !isValidation(err) {
		t.Errorf("approve a negative quantity: %v", err)
	}
	if err := approve(0); !isValidation(err) {
		t.Errorf("approve nothing (must reject instead): %v", err)
	}
	if err := f.svc.Approve(f.ctx, admin, id, returns.ApproveInput{}); !isValidation(err) {
		t.Errorf("approve without quantities: %v", err)
	}
	if got := f.int("SELECT count(*) FROM returns WHERE id = $1 AND status = 'REQUESTED'", id); got != 1 {
		t.Fatal("a refused approval must change nothing")
	}

	if err := approve(2); err != nil {
		t.Fatal(err)
	}
	if got := f.int("SELECT approved_quantity FROM return_items WHERE id = $1", lineID); got != 2 {
		t.Errorf("approved_quantity = %d, want 2", got)
	}
	if got := f.int("SELECT refund_amount FROM returns WHERE id = $1", id); got != 200000 {
		t.Errorf("refund_amount = %d, want 200000 (2 approved × price)", got)
	}
	// The unit not approved is returnable again; the approved two are held.
	if d, err := f.svc.Draft(f.ctx, uid, number); err != nil || d.Items[0].Remaining() != 1 {
		t.Errorf("remaining after partial approval: %+v %v", d, err)
	}
	ret, _ := f.svc.Get(f.ctx, uid, n)
	if !ret.Items[0].Partial() || ret.Items[0].ApprovedQuantity() != 2 || ret.ApprovedQuantity() != 2 {
		t.Errorf("buyer view: %+v", ret.Items[0])
	}
	if ev := ret.Events[len(ret.Events)-1]; ev.To != returns.StatusApproved || ev.Note == "" {
		t.Errorf("timeline note for a partial approval: %+v", ev)
	}

	if err := f.svc.MarkWaitingReturn(f.ctx, admin, id, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.MarkItemReceived(f.ctx, admin, id, f.received(id, returns.ConditionRestockable)); err != nil {
		t.Fatal(err)
	}
	refund, err := f.svc.PrepareRefund(f.ctx, admin, id)
	if err != nil || refund.Amount != 200000 {
		t.Errorf("refund uses the approved quantity: %+v %v", refund, err)
	}
}

func TestInspectionAndRestock(t *testing.T) {
	f := newFixture(t)
	admin := testutil.CreateAdmin(t, f.db, "admin@example.com")
	uid, addr := f.user("a@example.com")
	ok := f.product("Kaos OK", "OK-1", 100000, 10)
	bad := f.product("Kaos Rusak", "BAD-1", 100000, 10)
	no := f.product("Kaos Bekas", "NO-1", 100000, 10)
	number := f.completed(uid, addr, time.Hour, line{ok, 2}, line{bad, 1}, line{no, 1})
	stock := func(v int64) int64 { return f.int("SELECT stock FROM product_variants WHERE id = $1", v) }
	before := [3]int64{stock(ok), stock(bad), stock(no)}

	n, err := f.svc.Create(f.ctx, uid, request(number, returns.TypeRefund,
		returns.ItemRequest{OrderItemID: f.item(number, ok), Quantity: 2},
		returns.ItemRequest{OrderItemID: f.item(number, bad), Quantity: 1},
		returns.ItemRequest{OrderItemID: f.item(number, no), Quantity: 1}))
	if err != nil {
		t.Fatal(err)
	}
	id := f.id(n)
	if err := f.svc.Approve(f.ctx, admin, id, f.approveAll(id, "")); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.MarkWaitingReturn(f.ctx, admin, id, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Restock(f.ctx, admin, id); !errors.Is(err, returns.ErrInvalidTransition) {
		t.Errorf("restock before the goods are back: %v", err)
	}
	ids := f.lineIDs(id)
	in := returns.ReceiveInput{Conditions: map[int64]returns.Condition{ids[0]: returns.ConditionRestockable, ids[1]: returns.ConditionDamaged}}
	if err := f.svc.MarkItemReceived(f.ctx, admin, id, in); !isValidation(err) {
		t.Errorf("every approved line needs a condition: %v", err)
	}
	in.Conditions[ids[2]] = returns.ConditionNonRestockable
	if err := f.svc.MarkItemReceived(f.ctx, admin, id, in); err != nil {
		t.Fatal(err)
	}
	if now := [3]int64{stock(ok), stock(bad), stock(no)}; now != before {
		t.Errorf("receiving the goods changed stock: %v → %v", before, now)
	}

	units, err := f.svc.Restock(f.ctx, admin, id)
	if err != nil || units != 2 {
		t.Fatalf("restock: %d %v", units, err)
	}
	if stock(ok) != before[0]+2 || stock(bad) != before[1] || stock(no) != before[2] {
		t.Errorf("only RESTOCKABLE goods go back: ok %d bad %d no %d (before %v)", stock(ok), stock(bad), stock(no), before)
	}
	if got := f.int("SELECT count(*) FROM inventory_adjustments WHERE type = 'RETURN_RESTOCK' AND variant_id = $1 AND quantity_delta = 2 AND reference_type = 'RETURN' AND reference_id = $2 AND admin_user_id = $3", ok, id, admin); got != 1 {
		t.Errorf("RETURN_RESTOCK adjustment rows = %d", got)
	}

	// A repeated (or concurrent) restock adds nothing.
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.svc.Restock(f.ctx, admin, id); !errors.Is(err, returns.ErrNothingToRestock) {
				t.Errorf("repeated restock: %v", err)
			}
		}()
	}
	wg.Wait()
	if stock(ok) != before[0]+2 || f.int("SELECT count(*) FROM inventory_adjustments WHERE type = 'RETURN_RESTOCK'") != 1 {
		t.Errorf("restock duplicated: stock %d", stock(ok))
	}
	if got := f.int("SELECT restocked_quantity FROM return_items WHERE id = $1", ids[0]); got != 2 {
		t.Errorf("restocked_quantity = %d", got)
	}
}

func TestExchangeStock(t *testing.T) {
	f := newFixture(t)
	admin := testutil.CreateAdmin(t, f.db, "admin@example.com")
	uid, addr := f.user("a@example.com")
	v := f.product("Kaos A", "A-M", 100000, 10)
	other := f.product("Kaos B", "B-M", 100000, 10) // another product
	number := f.completed(uid, addr, time.Hour, line{v, 2})
	// A second variant of the same product, with one unit left.
	f.exec("INSERT INTO product_variants (product_id, sku, price, stock) SELECT product_id, 'A-L', 100000, 1 FROM product_variants WHERE id = $1", v)
	large := f.int("SELECT id FROM product_variants WHERE sku = 'A-L'")

	newExchange := func(qty int) int64 {
		n, err := f.svc.Create(f.ctx, uid, request(number, returns.TypeExchange, returns.ItemRequest{OrderItemID: f.item(number, v), Quantity: qty}))
		if err != nil {
			t.Fatal(err)
		}
		id := f.id(n)
		f.toReceived(admin, id, returns.ConditionNonRestockable)
		return id
	}
	a, b := newExchange(1), newExchange(1)
	stock := func(id int64) int64 { return f.int("SELECT stock FROM product_variants WHERE id = $1", id) }

	if err := f.svc.CompleteExchange(f.ctx, admin, a, f.replaceWith(a, other, "")); !isValidation(err) {
		t.Errorf("a replacement from another product: %v", err)
	}
	if err := f.svc.CompleteExchange(f.ctx, admin, a, returns.ExchangeInput{}); !isValidation(err) {
		t.Errorf("no replacement chosen: %v", err)
	}

	// Two exchanges want the last unit of A-L at the same time: one wins.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, id := range []int64{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = f.svc.CompleteExchange(f.ctx, admin, id, f.replaceWith(id, large, "Tukar ke L"))
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !isValidation(err):
			t.Errorf("losing exchange: want insufficient stock, got %v", err)
		}
	}
	if won != 1 || stock(large) != 0 {
		t.Fatalf("last-stock exchange: %d won, stock %d", won, stock(large))
	}
	if got := f.int("SELECT count(*) FROM inventory_adjustments WHERE type = 'EXCHANGE_OUT' AND variant_id = $1 AND quantity_delta = -1 AND stock_before = 1 AND stock_after = 0", large); got != 1 {
		t.Errorf("EXCHANGE_OUT rows = %d", got)
	}
	if got := f.int("SELECT count(*) FROM return_items WHERE replacement_variant_id = $1 AND replacement_sku = 'A-L' AND replacement_quantity = 1", large); got != 1 {
		t.Errorf("replacement recorded on %d lines", got)
	}
	loser := a
	if errs[0] == nil {
		loser = b
	}
	if got := f.int("SELECT count(*) FROM returns WHERE id = $1 AND status = 'ITEM_RECEIVED'", loser); got != 1 {
		t.Error("the refused exchange must stay ITEM_RECEIVED")
	}

	// The loser can use the same variant instead; a repeated completion is
	// refused and takes no more stock.
	if err := f.svc.CompleteExchange(f.ctx, admin, loser, f.replaceWith(loser, v, "")); err != nil {
		t.Fatal(err)
	}
	before := stock(v)
	if err := f.svc.CompleteExchange(f.ctx, admin, loser, f.replaceWith(loser, v, "")); !errors.Is(err, returns.ErrInvalidTransition) {
		t.Errorf("second completion: %v", err)
	}
	if stock(v) != before || f.int("SELECT count(*) FROM inventory_adjustments WHERE type = 'EXCHANGE_OUT'") != 2 {
		t.Errorf("double completion changed stock: %d → %d", before, stock(v))
	}
	// Requesting the exchange took no stock: only completions did.
	if stock(v) != 10-2-1 {
		t.Errorf("stock of A-M = %d, want 7 (2 sold, 1 sent as replacement)", stock(v))
	}
}

func TestRefundAdministration(t *testing.T) {
	f := newFixture(t)
	admin := testutil.CreateAdmin(t, f.db, "admin@example.com")
	uid, addr := f.user("a@example.com")
	v := f.product("Kaos A", "A-1", 100000, 50)
	number := f.completed(uid, addr, time.Hour, line{v, 1})
	n, err := f.svc.Create(f.ctx, uid, request(number, returns.TypeRefund, returns.ItemRequest{OrderItemID: f.item(number, v), Quantity: 1}))
	if err != nil {
		t.Fatal(err)
	}
	id := f.id(n)
	f.toReceived(admin, id, returns.ConditionDamaged)
	first, err := f.svc.PrepareRefund(f.ctx, admin, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.MarkRefundProcessing(f.ctx, admin, first.ID, "Diproses bagian keuangan"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.FailRefund(f.ctx, admin, first.ID, ""); !isValidation(err) {
		t.Errorf("fail without a reason: %v", err)
	}
	if err := f.svc.FailRefund(f.ctx, admin, first.ID, "Rekening tujuan tidak valid"); err != nil {
		t.Fatal(err)
	}
	if got := f.int("SELECT count(*) FROM returns WHERE id = $1 AND status = 'REFUND_PENDING'", id); got != 1 {
		t.Error("a failed refund leaves the return waiting for a refund")
	}
	second, err := f.svc.PrepareRefund(f.ctx, admin, id)
	if err != nil || second.ID == first.ID || second.Amount != 100000 {
		t.Fatalf("prepare again after failure: %+v %v", second, err)
	}
	if err := f.svc.CompleteRefundByID(f.ctx, admin, second.ID, returns.RefundResult{Reference: "BCA-77881"}); err != nil {
		t.Fatal(err)
	}
	if got := f.int("SELECT count(*) FROM refunds WHERE id = $1 AND status = 'SUCCEEDED' AND processed_at IS NOT NULL AND provider = 'manual' AND provider_refund_id = 'BCA-77881'", second.ID); got != 1 {
		t.Error("manual refund result not recorded")
	}
	if got := f.int("SELECT count(*) FROM returns WHERE id = $1 AND status = 'REFUNDED'", id); got != 1 {
		t.Error("the return moves to REFUNDED with its refund")
	}
	if err := f.svc.FailRefund(f.ctx, admin, second.ID, "terlambat"); !errors.Is(err, returns.ErrInvalidTransition) {
		t.Errorf("fail a succeeded refund: %v", err)
	}
	for _, action := range []string{"refund.processing", "refund.failed", "refund.succeeded"} {
		if got := f.int("SELECT count(*) FROM admin_audit_logs WHERE action = $1 AND entity_type = 'REFUND' AND admin_user_id = $2", action, admin); got < 1 {
			t.Errorf("no audit entry for %s", action)
		}
	}
	page, err := f.svc.AdminRefunds(f.ctx, returns.AdminFilter{Tab: "failed"})
	if err != nil || len(page.Refunds) != 1 || page.Refunds[0].ID != first.ID {
		t.Errorf("failed refunds list: %+v %v", page.Refunds, err)
	}
	if _, err := f.svc.AdminRefund(f.ctx, 999999); !errors.Is(err, returns.ErrRefundNotFound) {
		t.Errorf("unknown refund: %v", err)
	}
}
