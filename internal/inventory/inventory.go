// Package inventory changes stock outside checkout and keeps its audit
// trail.
//
// product_variants.stock is the only stock figure. Every change made here —
// a manual adjustment by an admin, a return restock, an exchange
// replacement, an order cancellation — writes an inventory_adjustments row
// in the same transaction as the stock update. The adjustments are a trail,
// never summed to compute stock. Sales are traced by order_items.
//
// Give, Take and Log run inside a caller's transaction (returns, product
// creation); Service.Adjust is the admin's manual change.
package inventory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/audit"
	"ecommerce/internal/database"
	"ecommerce/internal/database/dbgen"
	"ecommerce/internal/validate"
)

type Type string

const (
	TypeInitial       Type = "INITIAL_STOCK"
	TypeManual        Type = "MANUAL_ADJUSTMENT"
	TypeReturnRestock Type = "RETURN_RESTOCK"
	TypeExchangeOut   Type = "EXCHANGE_OUT"
	TypeOrderRestore  Type = "ORDER_RESTORE"
)

func (t Type) Label() string {
	switch t {
	case TypeInitial:
		return "Stok awal"
	case TypeManual:
		return "Penyesuaian manual"
	case TypeReturnRestock:
		return "Restock retur"
	case TypeExchangeOut:
		return "Barang pengganti (tukar)"
	case TypeOrderRestore:
		return "Pembatalan pesanan"
	}
	return string(t)
}

// Reference types.
const (
	RefOrder  = "ORDER"
	RefReturn = "RETURN"
)

// MaxStock bounds a variant's stock (as product validation does).
const MaxStock = 1_000_000

var (
	ErrNotFound          = errors.New("inventory: variant not found")
	ErrInsufficientStock = errors.New("inventory: not enough stock")
)

// Change is one stock change to record.
type Change struct {
	VariantID int64
	SKU       string
	Type      Type
	Delta     int
	Before    int
	After     int
	Reason    string
	RefType   string // RefOrder | RefReturn | ""
	RefID     int64
	AdminID   int64 // 0 when the system made the change
}

// Log records c with q (the caller's transaction, which also changed the
// stock).
func Log(ctx context.Context, q *dbgen.Queries, c Change) error {
	p := dbgen.AddInventoryAdjustmentParams{
		VariantID: &c.VariantID, Sku: c.SKU, Type: string(c.Type), QuantityDelta: int32(c.Delta),
		StockBefore: int32(c.Before), StockAfter: int32(c.After), Reason: c.Reason,
	}
	if c.RefType != "" {
		p.ReferenceType, p.ReferenceID = &c.RefType, &c.RefID
	}
	if c.AdminID != 0 {
		p.AdminUserID = &c.AdminID
	}
	return q.AddInventoryAdjustment(ctx, p)
}

// Give adds quantity units to a variant and records it (e.g. a return
// restock). It runs in the caller's transaction.
func Give(ctx context.Context, q *dbgen.Queries, c Change, quantity int) (Change, error) {
	if quantity <= 0 {
		return c, fmt.Errorf("inventory: give %d units", quantity)
	}
	row, err := q.GiveVariantStock(ctx, dbgen.GiveVariantStockParams{ID: c.VariantID, Quantity: int32(quantity)})
	if database.IsNoRows(err) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.SKU, c.Delta, c.After, c.Before = row.Sku, quantity, int(row.Stock), int(row.Stock)-quantity
	return c, Log(ctx, q, c)
}

// Take removes quantity units only if that many are in stock, atomically
// (UPDATE … WHERE stock >= quantity), and records it. Too little stock is
// ErrInsufficientStock and changes nothing.
func Take(ctx context.Context, q *dbgen.Queries, c Change, quantity int) (Change, error) {
	if quantity <= 0 {
		return c, fmt.Errorf("inventory: take %d units", quantity)
	}
	row, err := q.TakeVariantStock(ctx, dbgen.TakeVariantStockParams{ID: c.VariantID, Quantity: int32(quantity)})
	if database.IsNoRows(err) {
		return c, ErrInsufficientStock
	}
	if err != nil {
		return c, err
	}
	c.SKU, c.Delta, c.After, c.Before = row.Sku, -quantity, int(row.Stock), int(row.Stock)+quantity
	return c, Log(ctx, q, c)
}

// Adjustment is one recorded stock change.
type Adjustment struct {
	ID         int64
	SKU        string
	Type       Type
	Delta      int
	Before     int
	After      int
	Reason     string
	RefType    string
	RefID      int64
	AdminEmail string
	At         time.Time
}

// Item is a variant on the stock page.
type Item struct {
	VariantID     int64
	SKU           string
	Stock         int
	Active        bool
	Price         int64
	ProductID     int64
	ProductName   string
	ProductActive bool
	VariantLabel  string
}

// Low reports stock at or below the threshold but not zero.
func (i Item) Low(threshold int) bool { return i.Stock > 0 && i.Stock <= threshold }

// Modes of a manual adjustment.
const (
	ModeSet    = "set"    // set the stock to Quantity
	ModeAdd    = "add"    // add Quantity units
	ModeRemove = "remove" // remove Quantity units
)

// AdjustInput is an admin's manual stock change. Only these fields are
// read from the form.
type AdjustInput struct {
	VariantID int64
	Mode      string
	Quantity  int
	Reason    string
}

const (
	PageSize    = 20
	MaxPageSize = 100
)

// Filter selects the stock list: Status "" (all), "low" or "out".
type Filter struct {
	Status   string
	Query    string
	Page     int
	PageSize int
}

type Page struct {
	Items    []Item
	Filter   Filter
	HasNext  bool
	LowStock int // threshold
}

type Service struct {
	pool     *pgxpool.Pool
	q        *dbgen.Queries
	lowStock int
}

// NewService: lowStock is LOW_STOCK_THRESHOLD (dashboard and reports only).
func NewService(pool *pgxpool.Pool, lowStock int) *Service {
	return &Service{pool: pool, q: dbgen.New(pool), lowStock: lowStock}
}

// LowStockThreshold is the configured "stok rendah" limit.
func (s *Service) LowStockThreshold() int { return s.lowStock }

// Adjust changes a variant's stock by an admin decision in one transaction:
//
//	lock variant → compute and validate the new stock (>= 0) → update
//	→ inventory adjustment → admin audit entry
func (s *Service) Adjust(ctx context.Context, adminID int64, in AdjustInput) (Adjustment, error) {
	errs := validate.Errors{}
	in.Reason = validate.Text(in.Reason)
	validate.Length(errs, "reason", in.Reason, "Alasan", 3, 200)
	switch in.Mode {
	case ModeSet:
		if in.Quantity < 0 || in.Quantity > MaxStock {
			errs.Add("quantity", "Stok baru harus antara 0 dan 1.000.000.")
		}
	case ModeAdd, ModeRemove:
		if in.Quantity < 1 || in.Quantity > MaxStock {
			errs.Add("quantity", "Jumlah harus lebih dari 0.")
		}
	default:
		errs.Add("mode", "Pilih jenis perubahan stok.")
	}
	if err := errs.Err(); err != nil {
		return Adjustment{}, err
	}

	var out Adjustment
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		v, err := q.LockVariantStock(ctx, in.VariantID)
		if database.IsNoRows(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		before := int(v.Stock)
		after := before
		switch in.Mode {
		case ModeSet:
			after = in.Quantity
		case ModeAdd:
			after = before + in.Quantity
		case ModeRemove:
			after = before - in.Quantity
		}
		switch {
		case after < 0:
			return validate.Errors{"quantity": fmt.Sprintf("Stok tidak boleh negatif (stok saat ini %d).", before)}
		case after > MaxStock:
			return validate.Errors{"quantity": "Stok maksimal 1.000.000."}
		case after == before:
			return validate.Errors{"quantity": "Stok baru sama dengan stok saat ini."}
		}
		if err := q.SetVariantStock(ctx, dbgen.SetVariantStockParams{ID: v.ID, Stock: int32(after)}); err != nil {
			return err
		}
		c := Change{VariantID: v.ID, SKU: v.Sku, Type: TypeManual, Delta: after - before, Before: before, After: after,
			Reason: in.Reason, AdminID: adminID}
		if err := Log(ctx, q, c); err != nil {
			return err
		}
		out = Adjustment{SKU: v.Sku, Type: TypeManual, Delta: c.Delta, Before: before, After: after, Reason: in.Reason, At: time.Now()}
		return audit.Record(ctx, q, audit.Entry{AdminID: adminID, Action: "stock.adjust", EntityType: audit.EntityVariant,
			EntityID: v.ID, Label: v.Sku, Meta: map[string]any{"before": before, "after": after, "delta": c.Delta, "reason": in.Reason}})
	})
	return out, err
}

// Variant returns one variant for the stock form.
func (s *Service) Variant(ctx context.Context, id int64) (Item, error) {
	row, err := s.q.StockVariant(ctx, id)
	if database.IsNoRows(err) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, err
	}
	return Item{VariantID: row.ID, SKU: row.Sku, Stock: int(row.Stock), Active: row.Active, Price: row.Price,
		ProductID: row.ProductID, ProductName: row.ProductName, ProductActive: row.ProductActive, VariantLabel: row.VariantLabel}, nil
}

// History returns the latest adjustments of a variant.
func (s *Service) History(ctx context.Context, variantID int64, limit int) ([]Adjustment, error) {
	rows, err := s.q.VariantAdjustments(ctx, dbgen.VariantAdjustmentsParams{VariantID: &variantID, RowLimit: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]Adjustment, len(rows))
	for i, r := range rows {
		out[i] = Adjustment{ID: r.ID, Type: Type(r.Type), Delta: int(r.QuantityDelta), Before: int(r.StockBefore),
			After: int(r.StockAfter), Reason: r.Reason, AdminEmail: r.AdminEmail, At: r.CreatedAt}
		if r.ReferenceType != nil && r.ReferenceID != nil {
			out[i].RefType, out[i].RefID = *r.ReferenceType, *r.ReferenceID
		}
	}
	return out, nil
}

// ProductHistory returns the latest adjustments of a product's variants.
func (s *Service) ProductHistory(ctx context.Context, productID int64, limit int) ([]Adjustment, error) {
	rows, err := s.q.ProductAdjustments(ctx, dbgen.ProductAdjustmentsParams{ProductID: productID, RowLimit: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]Adjustment, len(rows))
	for i, r := range rows {
		out[i] = Adjustment{ID: r.ID, SKU: r.Sku, Type: Type(r.Type), Delta: int(r.QuantityDelta), Before: int(r.StockBefore),
			After: int(r.StockAfter), Reason: r.Reason, AdminEmail: r.AdminEmail, At: r.CreatedAt}
	}
	return out, nil
}

// List returns one page of the stock list.
func (s *Service) List(ctx context.Context, f Filter) (Page, error) {
	f.Page = min(max(f.Page, 1), 1000)
	if f.PageSize <= 0 {
		f.PageSize = PageSize
	}
	f.PageSize = min(f.PageSize, MaxPageSize)
	if f.Status != "low" && f.Status != "out" {
		f.Status = ""
	}
	f.Query = validate.SearchText(f.Query)
	rows, err := s.q.StockList(ctx, dbgen.StockListParams{Filter: f.Status, Threshold: int32(s.lowStock), Q: database.Like(f.Query),
		RowLimit: int32(f.PageSize + 1), RowOffset: int32((f.Page - 1) * f.PageSize)})
	if err != nil {
		return Page{}, err
	}
	p := Page{Filter: f, LowStock: s.lowStock}
	if len(rows) > f.PageSize {
		rows, p.HasNext = rows[:f.PageSize], true
	}
	for _, r := range rows {
		p.Items = append(p.Items, Item{VariantID: r.ID, SKU: r.Sku, Stock: int(r.Stock), Active: r.Active, Price: r.Price,
			ProductID: r.ProductID, ProductName: r.ProductName, ProductActive: r.ProductActive, VariantLabel: r.VariantLabel})
	}
	return p, nil
}
