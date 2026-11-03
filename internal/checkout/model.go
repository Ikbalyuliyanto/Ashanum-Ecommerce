// Package checkout turns selected cart lines or a "Buy Now" variant into an
// order.
//
// The server is the only source of truth: lines, prices, stock and the
// address are re-read (and, when placing, row-locked) from the database;
// nothing the browser sends about price, total, names or stock is trusted.
// Placing an order is one transaction: lock cart lines, lock variants in id
// order, validate, insert order + items, decrement stock atomically and
// remove the purchased cart lines. The Service has no HTTP dependency.
package checkout

import (
	"errors"

	"ecommerce/internal/address"
)

var (
	ErrNothingToCheckout = errors.New("checkout: nothing to check out")
	ErrAddressRequired   = errors.New("checkout: shipping address required")
	ErrStockChanged      = errors.New("checkout: stock changed")
	ErrUnavailable       = errors.New("checkout: product no longer available")
	ErrPriceChanged      = errors.New("checkout: price changed")
)

// Message returns the customer-facing (Indonesian) text for a checkout
// error, or "" for unexpected errors that must not be shown.
func Message(err error) string {
	switch {
	case errors.Is(err, ErrStockChanged):
		return "Stok produk berubah. Silakan periksa kembali pesanan Anda."
	case errors.Is(err, ErrUnavailable):
		return "Ada produk yang sudah tidak tersedia. Silakan periksa kembali pesanan Anda."
	case errors.Is(err, ErrPriceChanged):
		return "Harga produk berubah sejak halaman ini dibuka. Periksa total terbaru lalu buat pesanan kembali."
	case errors.Is(err, ErrAddressRequired):
		return "Pilih alamat pengiriman terlebih dahulu."
	case errors.Is(err, ErrNothingToCheckout):
		return "Pilih produk yang ingin dibeli terlebih dahulu."
	}
	return ""
}

// Source is what is being checked out: selected cart lines, or one variant
// and quantity ("Buy Now", which never touches the cart).
type Source struct {
	CartItemIDs []int64
	VariantID   int64
	Quantity    int
}

func (s Source) BuyNow() bool { return s.VariantID > 0 }

// Problem says why a line cannot be ordered right now.
type Problem string

const (
	ProblemNone              Problem = ""
	ProblemUnavailable       Problem = "unavailable" // variant/product/category inactive or deleted
	ProblemOutOfStock        Problem = "out_of_stock"
	ProblemInsufficientStock Problem = "insufficient_stock"
	ProblemInvalidQuantity   Problem = "invalid_quantity"
)

// Line is one product to buy, with the catalog's current data.
type Line struct {
	CartItemID    int64 // 0 for Buy Now
	VariantID     int64
	ProductID     int64
	ProductName   string
	ProductSlug   string
	SKU           string
	VariantLabel  string
	ImageURL      string
	UnitPrice     int64
	OriginalPrice int64
	Quantity      int
	Stock         int
	Problem       Problem
}

func (l Line) Subtotal() int64 { return l.UnitPrice * int64(l.Quantity) }

// Draft is the checkout page: current lines and totals, the user's
// addresses and the chosen one. Shipping and discount are 0 until those
// phases exist (the UI says they are not calculated yet).
type Draft struct {
	Source       Source
	Lines        []Line
	Addresses    []address.Address
	Address      *address.Address
	Subtotal     int64
	ShippingCost int64
	Discount     int64
	Total        int64
}

// Blocked reports whether any line has a problem.
func (d Draft) Blocked() bool {
	for _, l := range d.Lines {
		if l.Problem != ProblemNone {
			return true
		}
	}
	return false
}

// Ready reports whether the order can be placed as shown.
func (d Draft) Ready() bool { return len(d.Lines) > 0 && !d.Blocked() && d.Address != nil }

// Quantity is the total number of units.
func (d Draft) Quantity() int {
	n := 0
	for _, l := range d.Lines {
		n += l.Quantity
	}
	return n
}

// PlaceRequest places an order. ExpectedTotal is the total the customer
// saw (0 = not checked); a different current total is ErrPriceChanged.
// IdempotencyKey makes a repeated submission return the same order.
type PlaceRequest struct {
	Source         Source
	AddressID      int64
	ExpectedTotal  int64
	IdempotencyKey string
}

// Placed is the result of PlaceOrder. Existing is true when the
// idempotency key matched an order created earlier.
type Placed struct {
	OrderNumber string
	Total       int64
	Existing    bool
}
