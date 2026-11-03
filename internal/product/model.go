// Package product owns the catalog: products, images, attributes and
// variants, plus listing, search and the home sections.
//
// Price and stock belong to variants. A product's price shown in listings is
// the lowest active variant price, read from summary columns the database
// keeps in sync (see migration 0004). The Service has no HTTP dependency, so
// web handlers and a future REST API share it.
package product

import (
	"errors"
	"math"
)

var ErrNotFound = errors.New("product: not found")

// Card is the compact listing view of a product.
type Card struct {
	ID            int64
	Name          string
	Slug          string
	Brand         string
	Price         int64 // lowest active variant price
	PriceMax      int64
	OriginalPrice int64 // of the cheapest variant; 0 when not discounted
	InStock       bool
	FlashSale     bool
	SoldCount     int
	RatingAverage float64
	RatingCount   int
	ImageURL      string
}

// DiscountPercent is derived from prices (never stored), rounded.
func (c Card) DiscountPercent() int { return discountPercent(c.Price, c.OriginalPrice) }

type Image struct {
	URL string `json:"url"`
	Alt string `json:"alt"`
}

type AttributeValue struct {
	ID    int64  `json:"id"`
	Value string `json:"value"`
}

type Attribute struct {
	ID     int64            `json:"id"`
	Name   string           `json:"name"`
	Values []AttributeValue `json:"values"`
}

type Variant struct {
	ID            int64   `json:"id"`
	SKU           string  `json:"sku"`
	Price         int64   `json:"price"`
	OriginalPrice int64   `json:"original_price"`
	Stock         int     `json:"stock"`
	ValueIDs      []int64 `json:"value_ids"`
}

func (v Variant) DiscountPercent() int { return discountPercent(v.Price, v.OriginalPrice) }

func (v Variant) has(valueID int64) bool {
	for _, id := range v.ValueIDs {
		if id == valueID {
			return true
		}
	}
	return false
}

// Detail is everything the product page needs, loaded in one query.
type Detail struct {
	ID            int64
	CategoryID    int64
	CategoryName  string
	CategorySlug  string
	Name          string
	Slug          string
	Description   string
	Brand         string
	Featured      bool
	FlashSale     bool
	SoldCount     int
	RatingAverage float64
	RatingCount   int
	Images        []Image
	Attributes    []Attribute
	Variants      []Variant // active variants only
}

// Sort orders a listing.
type Sort string

const (
	SortRelevance   Sort = "relevance" // only with a search query
	SortNewest      Sort = "newest"
	SortBestSelling Sort = "bestselling"
	SortPriceAsc    Sort = "price_asc"
	SortPriceDesc   Sort = "price_desc"
)

const (
	DefaultPageSize = 20
	MaxPageSize     = 60
	MaxPage         = 50 // deep OFFSET pages are refused to keep queries cheap
)

// ListParams filters a listing. Zero values mean "no filter".
type ListParams struct {
	Query         string
	CategoryID    int64
	MinPrice      int64
	MaxPrice      int64
	FlashSaleOnly bool
	Sort          Sort
	Page          int
	PageSize      int
}

// Page is one page of a listing.
type Page struct {
	Items    []Card
	Total    int
	Page     int
	PageSize int
	Sort     Sort
}

func (p Page) HasNext() bool { return p.Page < MaxPage && p.Page*p.PageSize < p.Total }
func (p Page) HasPrev() bool { return p.Page > 1 }

// Home holds the storefront home sections.
type Home struct {
	FlashSale   []Card
	BestSellers []Card
	Recommended []Card
}

// Input is admin input for a new product; the Service validates it.
type Input struct {
	CategoryID  int64
	Name        string
	Slug        string // derived from Name when empty
	Description string
	Brand       string
	Active      bool
	Featured    bool
	FlashSale   bool
	Images      []ImageInput
	Attributes  []AttributeInput
	Variants    []VariantInput
}

type ImageInput struct {
	URL string
	Alt string
}

type AttributeInput struct {
	Name   string   // e.g. "Warna"
	Values []string // e.g. "Hitam", "Putih"
}

// VariantInput maps every attribute name to one of its values. A product
// without attributes has exactly one variant with no options.
type VariantInput struct {
	SKU           string
	Price         int64
	OriginalPrice int64 // 0 = no strike-through price
	Stock         int
	Options       map[string]string
}

func discountPercent(price, original int64) int {
	if original <= price || original <= 0 {
		return 0
	}
	return int(math.Round(float64(original-price) * 100 / float64(original)))
}
