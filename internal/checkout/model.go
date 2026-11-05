// Package checkout turns selected cart lines or a "Buy Now" variant into an
// order.
//
// The server is the only source of truth: lines, prices, stock, weights and
// the address are re-read (and, when placing, row-locked) from the
// database; nothing the browser or app sends about price, total, shipping
// cost, weight, names or stock is trusted. Shipping comes from quotes the
// server stored (package shipping): the client only names a quote id.
// Placing an order is one transaction: lock cart lines, lock variants in id
// order, validate, lock and check the shipping quote, insert order + items
// with the shipping snapshot, decrement stock atomically, remove the
// purchased cart lines and mark the quote used. The Service has no HTTP
// dependency.
package checkout

import (
	"errors"

	"ecommerce/internal/address"
	"ecommerce/internal/shipping"
)

var (
	ErrNothingToCheckout = errors.New("checkout: nothing to check out")
	ErrAddressRequired   = errors.New("checkout: shipping address required")
	ErrStockChanged      = errors.New("checkout: stock changed")
	ErrUnavailable       = errors.New("checkout: product no longer available")
	ErrPriceChanged      = errors.New("checkout: price changed")
	// ErrNotShippable: a line has no weight, so no shipping cost exists.
	ErrNotShippable = errors.New("checkout: product has no shipping weight")
	// ErrShippingRequired: no shipping quote was chosen.
	ErrShippingRequired = errors.New("checkout: shipping not chosen")
	// ErrAreaRequired: the address has no delivery area for rates.
	ErrAreaRequired = errors.New("checkout: address has no delivery area")
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
	case errors.Is(err, ErrNotShippable):
		return "Ada produk yang belum bisa dikirim (berat belum diatur penjual). Hapus produk tersebut dari pesanan."
	case errors.Is(err, ErrShippingRequired):
		return "Pilih pengiriman terlebih dahulu."
	case errors.Is(err, ErrAreaRequired):
		return "Lengkapi lokasi pengiriman (kecamatan/kode pos) pada alamat ini terlebih dahulu."
	}
	return shipping.Message(err)
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
	ProblemNoWeight          Problem = "no_weight" // cannot be shipped: weight not set
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
	// Shipping data from the catalog: weight per unit (variant override or
	// product default) and optional dimensions.
	WeightGrams int
	LengthCM    int
	WidthCM     int
	HeightCM    int
}

func (l Line) Subtotal() int64 { return l.UnitPrice * int64(l.Quantity) }

// packageOf is what ships: weights from the catalog, never from the client.
func packageOf(lines []Line) shipping.Package {
	p := shipping.Package{Items: make([]shipping.Item, len(lines))}
	for i, l := range lines {
		name := l.ProductName
		if l.VariantLabel != "" {
			name += " - " + l.VariantLabel
		}
		p.Items[i] = shipping.Item{VariantID: l.VariantID, Name: name, SKU: l.SKU, Value: l.UnitPrice,
			WeightGrams: l.WeightGrams, Quantity: l.Quantity, LengthCM: l.LengthCM, WidthCM: l.WidthCM, HeightCM: l.HeightCM}
	}
	return p
}

// destinationOf is where the package goes: the address's delivery area.
func destinationOf(a address.Address) shipping.Destination {
	return shipping.Destination{Provider: a.ShippingAreaProvider, AreaID: a.ShippingAreaID, Label: a.ShippingAreaLabel,
		Postal: a.PostalCode, Lat: a.Latitude, Lng: a.Longitude}
}

// ShippingState says whether shipping options could be shown.
type ShippingState string

const (
	ShippingReady         ShippingState = "READY"
	ShippingNeedAddress   ShippingState = "ADDRESS_REQUIRED"
	ShippingNeedArea      ShippingState = "AREA_REQUIRED"
	ShippingBlocked       ShippingState = "ITEMS_BLOCKED"
	ShippingNoCourier     ShippingState = "NO_COURIER"
	ShippingUnavailable   ShippingState = "PROVIDER_UNAVAILABLE"
	ShippingNotConfigured ShippingState = "NOT_CONFIGURED"
)

// Shipping is the shipping part of the checkout page: the quotes offered
// (cheapest first) and the chosen one.
type Shipping struct {
	State    ShippingState
	Options  []shipping.Quote
	Selected *shipping.Quote
	// Changed: the quote the client named is no longer offered (expired,
	// used, changed route or package): another one was selected.
	Changed bool
}

// Message explains a state that has no options.
func (s Shipping) Message() string {
	switch s.State {
	case ShippingNeedAddress:
		return "Pilih alamat pengiriman untuk melihat ongkos kirim."
	case ShippingNeedArea:
		return "Alamat ini belum punya lokasi pengiriman. Lengkapi kecamatan/kode pos untuk menghitung ongkos kirim."
	case ShippingBlocked:
		return "Ongkos kirim dihitung setelah semua produk dapat dipesan."
	case ShippingNoCourier:
		return "Belum ada kurir yang melayani alamat ini."
	case ShippingUnavailable, ShippingNotConfigured:
		return "Pengiriman sementara tidak dapat dihitung. Silakan coba lagi."
	}
	return ""
}

// Draft is the checkout page: current lines and totals, the user's
// addresses and the chosen one, and the shipping options for it. Total
// includes the selected shipping cost; discount is 0 (no vouchers yet).
type Draft struct {
	Source       Source
	Lines        []Line
	Addresses    []address.Address
	Address      *address.Address
	Shipping     Shipping
	Subtotal     int64
	ShippingCost int64
	Discount     int64
	Total        int64
}

// WeightGrams is the total weight of the lines.
func (d Draft) WeightGrams() int { return packageOf(d.Lines).WeightGrams() }

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
func (d Draft) Ready() bool {
	return len(d.Lines) > 0 && !d.Blocked() && d.Address != nil && d.Shipping.Selected != nil
}

// Quantity is the total number of units.
func (d Draft) Quantity() int {
	n := 0
	for _, l := range d.Lines {
		n += l.Quantity
	}
	return n
}

// PlaceRequest places an order. ShippingQuoteID is the shipping option the
// customer chose (a quote the server stored; never a cost). ExpectedTotal
// is the total the customer saw, shipping included (0 = not checked); a
// different current total is ErrPriceChanged. IdempotencyKey makes a
// repeated submission return the same order.
type PlaceRequest struct {
	Source          Source
	AddressID       int64
	ShippingQuoteID string
	ExpectedTotal   int64
	IdempotencyKey  string
}

// Placed is the result of PlaceOrder. Existing is true when the
// idempotency key matched an order created earlier.
type Placed struct {
	OrderNumber string
	Total       int64
	Existing    bool
}
