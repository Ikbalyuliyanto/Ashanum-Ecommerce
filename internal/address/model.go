// Package address manages a user's shipping addresses ("Alamat Saya").
//
// An address may carry a delivery area (kecamatan + kode pos) chosen from
// the shipping provider's area search; checkout needs it to get rates.
// When an area is chosen, its province, city, district and postal code are
// saved on the address, so the two always agree.
//
// Invariant: a user with at least one address has exactly one primary
// address. The service keeps it inside transactions and the database backs it
// with a partial unique index. The Service has no HTTP dependency so a future
// /api/v1/addresses handler can reuse it.
package address

import (
	"errors"
	"strconv"
	"time"

	"ecommerce/internal/shipping"
)

// MaxPerUser bounds how many addresses one account can keep.
const MaxPerUser = 20

var (
	ErrNotFound     = errors.New("address: not found")
	ErrLimitReached = errors.New("address: limit reached")
)

type Address struct {
	ID            int64
	UserID        int64
	Label         string
	RecipientName string
	Phone         string // E.164 (+628…)
	Province      string
	City          string
	District      string
	Village       string
	PostalCode    string
	StreetAddress string
	Latitude      *float64
	Longitude     *float64
	MapsURL       string
	IsPrimary     bool
	CreatedAt     time.Time
	UpdatedAt     time.Time

	// Delivery area (provider neutral); empty for addresses saved before
	// shipping rates existed.
	ShippingAreaProvider string
	ShippingAreaID       string
	ShippingAreaLabel    string
}

// HasShippingArea reports whether checkout can get rates for the address.
func (a Address) HasShippingArea() bool { return a.ShippingAreaID != "" }

// Input is raw user input (form or JSON); the Service validates it.
type Input struct {
	Label         string
	RecipientName string
	Phone         string
	Province      string
	City          string
	District      string
	Village       string
	PostalCode    string
	StreetAddress string
	Latitude      string
	Longitude     string
	MapsURL       string
	MakePrimary   bool
	// ShippingAreaID is an area id from the area search ("" = none). Only
	// areas the server found itself are accepted.
	ShippingAreaID string
}

// InputFrom pre-fills an edit form from a stored address.
func InputFrom(a Address) Input {
	in := Input{
		Label:         a.Label,
		RecipientName: a.RecipientName,
		Phone:         a.Phone,
		Province:      a.Province,
		City:          a.City,
		District:      a.District,
		Village:       a.Village,
		PostalCode:    a.PostalCode,
		StreetAddress: a.StreetAddress,
		MapsURL:       a.MapsURL,
		MakePrimary:   a.IsPrimary,

		ShippingAreaID: a.ShippingAreaID,
	}
	if a.Latitude != nil && a.Longitude != nil {
		in.Latitude = strconv.FormatFloat(*a.Latitude, 'f', -1, 64)
		in.Longitude = strconv.FormatFloat(*a.Longitude, 'f', -1, 64)
	}
	return in
}

// fields are validated, normalized values ready to store.
type fields struct {
	Label         string
	RecipientName string
	Phone         string
	Province      string
	City          string
	District      string
	Village       string
	PostalCode    string
	StreetAddress string
	Latitude      *float64
	Longitude     *float64
	MapsURL       string
	Area          *shipping.Area
}
