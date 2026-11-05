// Package audit is the admin audit trail: who did what to which entity.
//
// Entries are written with Record inside the same transaction as the change
// they describe, so an action and its audit row commit (or roll back)
// together. The table is append-only (a database trigger refuses UPDATE and
// DELETE). Metadata holds small facts — statuses, quantities, notes — and
// must never contain passwords, tokens or provider keys.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/database/dbgen"
)

// Entity types (admin_audit_logs.entity_type).
const (
	EntityOrder    = "ORDER"
	EntityProduct  = "PRODUCT"
	EntityVariant  = "VARIANT"
	EntityCategory = "CATEGORY"
	EntityReturn   = "RETURN"
	EntityRefund   = "REFUND"
	EntityReview   = "REVIEW"
	EntityUser     = "USER"
)

// Entry is one admin action. Action is "<area>.<verb>", e.g. "order.ship".
type Entry struct {
	AdminID    int64
	Action     string
	EntityType string
	EntityID   int64 // 0 when the action has no single entity
	Label      string
	Meta       map[string]any
}

// Record writes e with q, which should belong to the transaction making
// the change.
func Record(ctx context.Context, q *dbgen.Queries, e Entry) error {
	p := dbgen.InsertAdminAuditParams{AdminUserID: e.AdminID, Action: e.Action, EntityType: e.EntityType, EntityLabel: e.Label}
	if e.EntityID != 0 {
		p.EntityID = &e.EntityID
	}
	if len(e.Meta) > 0 {
		raw, err := json.Marshal(e.Meta)
		if err != nil {
			return err
		}
		p.Metadata = raw
	}
	return q.InsertAdminAudit(ctx, p)
}

// Log is one entry as shown on the audit page.
type Log struct {
	ID         int64
	Action     string
	EntityType string
	EntityID   int64
	Label      string
	Meta       map[string]any
	AdminEmail string
	At         time.Time
}

// Pair is one metadata entry formatted for display.
type Pair struct {
	Key   string
	Value string
}

// MetaPairs returns the metadata sorted by key; lists and maps are shown as
// compact JSON.
func (l Log) MetaPairs() []Pair {
	keys := make([]string, 0, len(l.Meta))
	for k := range l.Meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Pair, 0, len(keys))
	for _, k := range keys {
		var v string
		switch x := l.Meta[k].(type) {
		case string:
			v = x
		case nil:
			v = "—"
		case map[string]any, []any:
			raw, _ := json.Marshal(x)
			v = string(raw)
		default:
			v = fmt.Sprint(x)
		}
		if v != "" {
			out = append(out, Pair{Key: k, Value: v})
		}
	}
	return out
}

// ActionLabel describes the action in Indonesian (falls back to the code).
func (l Log) ActionLabel() string {
	if s, ok := actionLabels[l.Action]; ok {
		return s
	}
	return l.Action
}

var actionLabels = map[string]string{
	"user.create_admin":        "Membuat akun admin",
	"user.grant_admin":         "Memberi peran admin",
	"order.confirm":            "Konfirmasi pesanan (diproses)",
	"order.ship":               "Kirim pesanan",
	"order.complete":           "Selesaikan pesanan",
	"order.cancel":             "Batalkan pesanan",
	"shipment.book":            "Buat pengiriman (penyedia)",
	"shipment.cancel":          "Batalkan pengiriman",
	"shipment.sync":            "Perbarui status pengiriman",
	"product.create":           "Tambah produk",
	"product.update":           "Ubah produk",
	"product.activate":         "Aktifkan produk",
	"product.deactivate":       "Nonaktifkan produk",
	"product.add_attribute":    "Tambah atribut",
	"variant.create":           "Tambah varian",
	"variant.update":           "Ubah varian",
	"variant.activate":         "Aktifkan varian",
	"variant.deactivate":       "Nonaktifkan varian",
	"stock.adjust":             "Penyesuaian stok",
	"category.create":          "Tambah kategori",
	"category.update":          "Ubah kategori",
	"return.approve":           "Setujui pengembalian",
	"return.reject":            "Tolak pengembalian",
	"return.waiting_return":    "Menunggu barang dikirim",
	"return.receive":           "Terima & periksa barang",
	"return.restock":           "Kembalikan barang ke stok",
	"return.refunded":          "Refund retur dicatat berhasil",
	"return.prepare_refund":    "Siapkan refund",
	"return.complete_exchange": "Kirim barang pengganti",
	"return.complete":          "Selesaikan pengembalian",
	"return.cancel":            "Batalkan pengembalian",
	"refund.processing":        "Refund sedang diproses",
	"refund.succeeded":         "Refund selesai (manual)",
	"refund.failed":            "Refund gagal",
	"review.hide":              "Sembunyikan ulasan",
	"review.unhide":            "Tampilkan ulasan",
}

const (
	PageSize    = 20
	MaxPageSize = 100
)

// Filter selects entries; zero values mean all.
type Filter struct {
	EntityType string
	EntityID   int64
	Page       int
	PageSize   int
}

type Page struct {
	Logs     []Log
	Page     int
	PageSize int
	HasNext  bool
}

type Service struct {
	q *dbgen.Queries
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{q: dbgen.New(pool)} }

// List returns one page of entries, newest first.
func (s *Service) List(ctx context.Context, f Filter) (Page, error) {
	f.Page = min(max(f.Page, 1), 1000)
	if f.PageSize <= 0 {
		f.PageSize = PageSize
	}
	f.PageSize = min(f.PageSize, MaxPageSize)
	p := dbgen.AdminAuditLogsParams{EntityType: f.EntityType, RowLimit: int32(f.PageSize + 1), RowOffset: int32((f.Page - 1) * f.PageSize)}
	if f.EntityID != 0 {
		p.EntityID = &f.EntityID
	}
	rows, err := s.q.AdminAuditLogs(ctx, p)
	if err != nil {
		return Page{}, err
	}
	page := Page{Page: f.Page, PageSize: f.PageSize}
	if len(rows) > f.PageSize {
		rows, page.HasNext = rows[:f.PageSize], true
	}
	for _, r := range rows {
		l := Log{ID: r.ID, Action: r.Action, EntityType: r.EntityType, Label: r.EntityLabel, AdminEmail: r.AdminEmail, At: r.CreatedAt}
		if r.EntityID != nil {
			l.EntityID = *r.EntityID
		}
		if len(r.Metadata) > 0 {
			_ = json.Unmarshal(r.Metadata, &l.Meta)
		}
		page.Logs = append(page.Logs, l)
	}
	return page, nil
}

// For returns the latest entries about one entity (detail pages).
func (s *Service) For(ctx context.Context, entityType string, id int64, limit int) ([]Log, error) {
	p, err := s.List(ctx, Filter{EntityType: entityType, EntityID: id, PageSize: limit})
	return p.Logs, err
}
