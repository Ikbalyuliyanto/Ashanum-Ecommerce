// Package cart owns the shopping cart: one active cart per user whose lines
// reference product variants.
//
// The cart is not a price or stock authority. Lines store only variant and
// quantity; price, stock and availability are read live from the catalog,
// nothing is reserved, and checkout (Phase 5) validates everything again.
// The Service has no HTTP dependency, so web handlers and a future API share
// it.
package cart

import (
	"errors"
	"fmt"
	"math"
)

const (
	// MaxQuantity is the most units of one variant per cart line.
	MaxQuantity = 99
	// MaxItems is the most distinct lines in one cart.
	MaxItems = 50
)

var (
	ErrItemNotFound = errors.New("cart: item not found")
	ErrUnavailable  = errors.New("cart: product not available")
	ErrCartFull     = errors.New("cart: too many items")
)

// QuantityError explains why a quantity was refused. Available is the
// current stock, InCart what this variant already has in the cart.
type QuantityError struct {
	Requested int
	Available int
	InCart    int
}

func (e *QuantityError) Error() string {
	return fmt.Sprintf("cart: quantity %d refused (stock %d, in cart %d)", e.Requested, e.Available, e.InCart)
}

// Message returns the customer-facing (Indonesian) text for a cart error,
// or "" for unexpected errors that must not be shown.
func Message(err error) string {
	var qe *QuantityError
	switch {
	case errors.As(err, &qe):
		switch {
		case qe.Requested < 1:
			return "Jumlah minimal 1."
		case qe.Available <= 0:
			return "Stok produk ini sedang habis."
		case qe.Requested > MaxQuantity && qe.Available >= MaxQuantity:
			return fmt.Sprintf("Maksimal %d per produk.", MaxQuantity)
		case qe.InCart > 0:
			return fmt.Sprintf("Sudah ada %d di keranjang Anda. Stok tersedia %d, jadi Anda hanya dapat menambah %d lagi.",
				qe.InCart, qe.Available, max(min(qe.Available, MaxQuantity)-qe.InCart, 0))
		default:
			return fmt.Sprintf("Stok tersedia hanya %d.", qe.Available)
		}
	case errors.Is(err, ErrUnavailable):
		return "Produk tidak tersedia."
	case errors.Is(err, ErrCartFull):
		return fmt.Sprintf("Keranjang penuh (maksimal %d produk berbeda). Hapus beberapa produk terlebih dahulu.", MaxItems)
	case errors.Is(err, ErrItemNotFound):
		return "Produk tidak ditemukan di keranjang Anda."
	}
	return ""
}

// Status is whether a cart line can go to checkout right now.
type Status string

const (
	StatusAvailable         Status = "available"
	StatusInsufficientStock Status = "insufficient_stock" // in stock, but less than the quantity
	StatusOutOfStock        Status = "out_of_stock"
	StatusUnavailable       Status = "unavailable" // variant/product/category inactive or deleted
)

// Item is one cart line with the catalog's current data.
type Item struct {
	ID            int64
	VariantID     int64 // 0 when the variant was deleted
	ProductID     int64 // 0 when the variant was deleted
	SKU           string
	ProductName   string
	ProductSlug   string
	ImageURL      string
	VariantLabel  string // e.g. "Hitam / XL"
	Price         int64  // current price (display only, not authoritative)
	OriginalPrice int64
	Stock         int
	Quantity      int
	Status        Status
}

func (i Item) Checkoutable() bool { return i.Status == StatusAvailable }
func (i Item) Subtotal() int64    { return i.Price * int64(i.Quantity) }

// MaxQuantity is the largest quantity this line may be set to now.
func (i Item) MaxQuantity() int { return min(i.Stock, MaxQuantity) }

func (i Item) DiscountPercent() int {
	if i.OriginalPrice <= i.Price || i.OriginalPrice <= 0 {
		return 0
	}
	return int(math.Round(float64(i.OriginalPrice-i.Price) * 100 / float64(i.OriginalPrice)))
}

// Cart is the user's active cart, newest line first.
type Cart struct {
	Items []Item
}

// Quantity is the header badge: total quantity of every line.
func (c Cart) Quantity() int {
	n := 0
	for _, it := range c.Items {
		n += it.Quantity
	}
	return n
}

// Summary totals the selected, checkout-ready lines.
type Summary struct {
	Lines    int
	Quantity int
	Subtotal int64
}

// Summarize totals the lines for which selected returns true; lines that
// are not checkout-ready never count.
func (c Cart) Summarize(selected func(itemID int64) bool) Summary {
	var s Summary
	for _, it := range c.Items {
		if it.Checkoutable() && selected(it.ID) {
			s.Lines++
			s.Quantity += it.Quantity
			s.Subtotal += it.Subtotal()
		}
	}
	return s
}

// Line is a validated variant and quantity at the current price. Add-to-cart
// uses it, and Phase 5 "Buy Now" can check out a Line directly without
// storing it in the cart.
type Line struct {
	VariantID     int64
	ProductID     int64
	ProductName   string
	ProductSlug   string
	SKU           string
	Price         int64
	OriginalPrice int64
	Stock         int
	Quantity      int
}

func (l Line) Subtotal() int64 { return l.Price * int64(l.Quantity) }

// limit is the largest quantity allowed for this variant right now.
func (l Line) limit() int { return min(l.Stock, MaxQuantity) }
