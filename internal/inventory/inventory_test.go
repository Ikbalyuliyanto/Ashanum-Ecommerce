package inventory_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"ecommerce/internal/category"
	"ecommerce/internal/inventory"
	"ecommerce/internal/product"
	"ecommerce/internal/testutil"
	"ecommerce/internal/validate"
)

func TestManualAdjustment(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	c, err := category.NewService(category.NewRepository(db)).Create(ctx, category.Input{Name: "Atasan", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := product.NewService(product.NewRepository(db)).Create(ctx, product.Input{CategoryID: c.ID, Name: "Kaos", Active: true,
		Variants: []product.VariantInput{{SKU: "K-1", Price: 50000, Stock: 5}}}); err != nil {
		t.Fatal(err)
	}
	var variant int64
	db.QueryRow(ctx, "SELECT id FROM product_variants WHERE sku = 'K-1'").Scan(&variant)
	admin := testutil.CreateAdmin(t, db, "admin@example.com")
	svc := inventory.NewService(db, 5)
	stock := func() (n int) {
		db.QueryRow(ctx, "SELECT stock FROM product_variants WHERE id = $1", variant).Scan(&n)
		return
	}
	count := func(sql string, args ...any) (n int) { db.QueryRow(ctx, sql, args...).Scan(&n); return }
	isValidation := func(err error) bool { var v validate.Errors; return errors.As(err, &v) }

	if got := count("SELECT count(*) FROM inventory_adjustments WHERE type = 'INITIAL_STOCK' AND stock_after = 5"); got != 1 {
		t.Errorf("initial stock adjustment rows = %d", got)
	}
	adj, err := svc.Adjust(ctx, admin, inventory.AdjustInput{VariantID: variant, Mode: inventory.ModeAdd, Quantity: 10, Reason: "Barang masuk dari supplier"})
	if err != nil || adj.Before != 5 || adj.After != 15 || stock() != 15 {
		t.Fatalf("add: %+v %v stock %d", adj, err, stock())
	}
	if _, err := svc.Adjust(ctx, admin, inventory.AdjustInput{VariantID: variant, Mode: inventory.ModeRemove, Quantity: 16, Reason: "Rusak"}); !isValidation(err) {
		t.Errorf("negative stock: %v", err)
	}
	if _, err := svc.Adjust(ctx, admin, inventory.AdjustInput{VariantID: variant, Mode: inventory.ModeSet, Quantity: -1, Reason: "Opname"}); !isValidation(err) {
		t.Errorf("set below zero: %v", err)
	}
	if _, err := svc.Adjust(ctx, admin, inventory.AdjustInput{VariantID: variant, Mode: inventory.ModeAdd, Quantity: 1}); !isValidation(err) {
		t.Errorf("no reason: %v", err)
	}
	if _, err := svc.Adjust(ctx, admin, inventory.AdjustInput{VariantID: variant, Mode: "hapus", Quantity: 1, Reason: "x y z"}); !isValidation(err) {
		t.Errorf("unknown mode: %v", err)
	}
	if _, err := svc.Adjust(ctx, admin, inventory.AdjustInput{VariantID: 999999, Mode: inventory.ModeAdd, Quantity: 1, Reason: "x y z"}); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("unknown variant: %v", err)
	}
	if stock() != 15 {
		t.Fatalf("refused adjustments changed stock: %d", stock())
	}
	if _, err := svc.Adjust(ctx, admin, inventory.AdjustInput{VariantID: variant, Mode: inventory.ModeSet, Quantity: 3, Reason: "Stock opname"}); err != nil || stock() != 3 {
		t.Fatalf("set: %v stock %d", err, stock())
	}

	// Concurrent removals never take stock below zero.
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Adjust(ctx, admin, inventory.AdjustInput{VariantID: variant, Mode: inventory.ModeRemove, Quantity: 1, Reason: "Hilang"}); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 3 || stock() != 0 {
		t.Errorf("concurrent removals: %d succeeded, stock %d", ok, stock())
	}

	if got := count("SELECT count(*) FROM inventory_adjustments WHERE type = 'MANUAL_ADJUSTMENT' AND admin_user_id = $1", admin); got != 5 {
		t.Errorf("manual adjustment rows = %d, want 5", got)
	}
	if got := count("SELECT count(*) FROM admin_audit_logs WHERE action = 'stock.adjust' AND entity_id = $1", variant); got != 5 {
		t.Errorf("audit rows = %d, want 5", got)
	}
	hist, err := svc.History(ctx, variant, 10)
	if err != nil || len(hist) != 6 || hist[0].After != 0 || hist[len(hist)-1].Type != inventory.TypeInitial {
		t.Errorf("history: %+v %v", hist, err)
	}
	if _, err := db.Exec(ctx, "UPDATE admin_audit_logs SET action = 'x.y'"); err == nil {
		t.Error("audit log must be append-only")
	}

	p, err := svc.List(ctx, inventory.Filter{Status: "out"})
	if err != nil || len(p.Items) != 1 || p.Items[0].SKU != "K-1" {
		t.Errorf("out of stock list: %+v %v", p.Items, err)
	}
	if p, _ := svc.List(ctx, inventory.Filter{Status: "low"}); len(p.Items) != 0 {
		t.Errorf("a variant at 0 is out, not low: %+v", p.Items)
	}
}
