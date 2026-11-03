// Package order reads orders: history ("Pesanan Saya") and order detail.
// Orders are created by package checkout. Every order is a snapshot, so
// history stays correct when addresses or the catalog change later. The
// Service has no HTTP dependency, so a future API can reuse it.
package order

import (
	"errors"
	"time"
)

var ErrNotFound = errors.New("order: not found")

type Status string

const (
	StatusWaitingPayment      Status = "WAITING_PAYMENT"
	StatusWaitingConfirmation Status = "WAITING_CONFIRMATION"
	StatusProcessing          Status = "PROCESSING"
	StatusShipped             Status = "SHIPPED"
	StatusCompleted           Status = "COMPLETED"
	StatusCancelled           Status = "CANCELLED"
)

// Label is the short customer-facing status (Indonesian).
func (s Status) Label() string {
	switch s {
	case StatusWaitingPayment:
		return "Belum Bayar"
	case StatusWaitingConfirmation:
		return "Menunggu Konfirmasi"
	case StatusProcessing:
		return "Diproses"
	case StatusShipped:
		return "Dikirim"
	case StatusCompleted:
		return "Selesai"
	case StatusCancelled:
		return "Dibatalkan"
	}
	return string(s)
}

// Tab is one filter of the order history.
type Tab struct {
	Key      string
	Label    string
	Statuses []Status
}

// Tabs are the order history filters, in display order.
var Tabs = []Tab{
	{Key: "", Label: "Semua"},
	{Key: "unpaid", Label: "Belum Bayar", Statuses: []Status{StatusWaitingPayment}},
	{Key: "processing", Label: "Diproses", Statuses: []Status{StatusWaitingConfirmation, StatusProcessing}},
	{Key: "shipped", Label: "Dikirim", Statuses: []Status{StatusShipped}},
	{Key: "completed", Label: "Selesai", Statuses: []Status{StatusCompleted}},
	{Key: "cancelled", Label: "Dibatalkan", Statuses: []Status{StatusCancelled}},
}

// TabByKey returns the tab for key, or "Semua" for unknown keys.
func TabByKey(key string) Tab {
	for _, t := range Tabs {
		if t.Key == key {
			return t
		}
	}
	return Tabs[0]
}

// Address is the shipping address copied into the order.
type Address struct {
	Label         string
	RecipientName string
	Phone         string
	Province      string
	City          string
	District      string
	Village       string
	PostalCode    string
	Street        string
	Latitude      *float64
	Longitude     *float64
}

// Item is an order line snapshot.
type Item struct {
	ID           int64
	ProductID    int64 // 0 once the product was deleted from the catalog
	VariantID    int64 // 0 once the variant was deleted
	ProductName  string
	ProductSlug  string
	SKU          string
	VariantLabel string
	ImageURL     string
	UnitPrice    int64
	Quantity     int
	Subtotal     int64
}

type Order struct {
	ID           int64
	Number       string
	Status       Status
	Subtotal     int64
	ShippingCost int64
	Discount     int64
	Total        int64
	Address      Address
	Items        []Item
	CreatedAt    time.Time
}

// Quantity is the total number of units in the order.
func (o Order) Quantity() int {
	n := 0
	for _, it := range o.Items {
		n += it.Quantity
	}
	return n
}

// Summary is one order card in the history.
type Summary struct {
	Number    string
	Status    Status
	Total     int64
	CreatedAt time.Time
	ItemCount int
	First     Item // first line, shown on the card
}

// Page is one page of the order history.
type Page struct {
	Orders  []Summary
	Tab     Tab
	Page    int
	HasNext bool
}

// PageSize is the number of orders per history page.
const PageSize = 10
