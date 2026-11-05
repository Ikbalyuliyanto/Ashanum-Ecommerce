package api

// Response DTOs: the JSON contract of /api/v1. Every field is listed here on
// purpose — domain and database structs are never serialized directly, so
// nothing internal (hashes, roles, audit data, provider payloads) can leak
// and the contract stays stable when the domain changes.
//
// Money is integer rupiah; times are RFC 3339 in WIB (+07:00); IDs are JSON
// integers (orders and returns are addressed by their public numbers).

import (
	"math"
	"strings"
	"time"

	"ecommerce/internal/address"
	"ecommerce/internal/auth"
	"ecommerce/internal/cart"
	"ecommerce/internal/category"
	"ecommerce/internal/checkout"
	"ecommerce/internal/order"
	"ecommerce/internal/payment"
	"ecommerce/internal/product"
	"ecommerce/internal/returns"
	"ecommerce/internal/review"
	"ecommerce/internal/user"
)

// ---------------------------------------------------------------- account

type tokenDTO struct {
	AccessToken      string   `json:"access_token"`
	RefreshToken     string   `json:"refresh_token"`
	TokenType        string   `json:"token_type"`
	ExpiresIn        int      `json:"expires_in"`
	RefreshExpiresIn int      `json:"refresh_expires_in"`
	User             *userDTO `json:"user,omitempty"`
}

// tokens is sent only when tokens are issued (login, register, refresh).
func tokens(p auth.TokenPair) tokenDTO {
	return tokenDTO{AccessToken: p.AccessToken, RefreshToken: p.RefreshToken, TokenType: "Bearer",
		ExpiresIn:        int(math.Round(time.Until(p.AccessExpiresAt).Seconds())),
		RefreshExpiresIn: int(math.Round(time.Until(p.RefreshExpiresAt).Seconds()))}
}

type userDTO struct {
	ID        int64   `json:"id"`
	Email     string  `json:"email"`
	FirstName string  `json:"first_name"`
	LastName  string  `json:"last_name"`
	Phone     *string `json:"phone"`
	Gender    *string `json:"gender"`
	CreatedAt string  `json:"created_at"`
}

func account(a user.Account) userDTO {
	return userDTO{ID: a.UserID, Email: a.Email, FirstName: a.Profile.FirstName, LastName: a.Profile.LastName,
		Phone: nonEmpty(a.Profile.Phone), Gender: nonEmpty(string(a.Profile.Gender)), CreatedAt: ts(a.CreatedAt)}
}

type addressDTO struct {
	ID            int64    `json:"id"`
	Label         string   `json:"label"`
	RecipientName string   `json:"recipient_name"`
	Phone         string   `json:"phone"`
	Province      string   `json:"province"`
	City          string   `json:"city"`
	District      string   `json:"district"`
	Village       string   `json:"village"`
	PostalCode    string   `json:"postal_code"`
	StreetAddress string   `json:"street_address"`
	Latitude      *float64 `json:"latitude"`
	Longitude     *float64 `json:"longitude"`
	MapsURL       *string  `json:"maps_url"`
	IsPrimary     bool     `json:"is_primary"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
}

func addressOut(a address.Address) addressDTO {
	return addressDTO{ID: a.ID, Label: a.Label, RecipientName: a.RecipientName, Phone: a.Phone, Province: a.Province,
		City: a.City, District: a.District, Village: a.Village, PostalCode: a.PostalCode, StreetAddress: a.StreetAddress,
		Latitude: a.Latitude, Longitude: a.Longitude, MapsURL: nonEmpty(a.MapsURL), IsPrimary: a.IsPrimary,
		CreatedAt: ts(a.CreatedAt), UpdatedAt: ts(a.UpdatedAt)}
}

// ---------------------------------------------------------------- catalog

type categoryDTO struct {
	ID       int64   `json:"id"`
	Name     string  `json:"name"`
	Slug     string  `json:"slug"`
	ImageURL *string `json:"image_url"`
}

func categoryOut(c category.Category) categoryDTO {
	return categoryDTO{ID: c.ID, Name: c.Name, Slug: c.Slug, ImageURL: nonEmpty(img(c.ImageURL, thumbWidth))}
}

type ratingDTO struct {
	Average float64 `json:"average"`
	Count   int     `json:"count"`
}

type productCardDTO struct {
	ID              int64     `json:"id"`
	Slug            string    `json:"slug"`
	Name            string    `json:"name"`
	Brand           string    `json:"brand"`
	ImageURL        *string   `json:"image_url"`
	Price           int64     `json:"price"`
	PriceMax        int64     `json:"price_max"`
	CompareAtPrice  *int64    `json:"compare_at_price"`
	DiscountPercent int       `json:"discount_percent"`
	InStock         bool      `json:"in_stock"`
	FlashSale       bool      `json:"flash_sale"`
	SoldCount       int       `json:"sold_count"`
	Rating          ratingDTO `json:"rating"`
}

func card(c product.Card) productCardDTO {
	return productCardDTO{ID: c.ID, Slug: c.Slug, Name: c.Name, Brand: c.Brand, ImageURL: nonEmpty(img(c.ImageURL, cardWidth)),
		Price: c.Price, PriceMax: c.PriceMax, CompareAtPrice: nonZero(c.OriginalPrice), DiscountPercent: c.DiscountPercent(),
		InStock: c.InStock, FlashSale: c.FlashSale, SoldCount: c.SoldCount,
		Rating: ratingDTO{Average: c.RatingAverage, Count: c.RatingCount}}
}

func cards(cs []product.Card) []productCardDTO {
	out := make([]productCardDTO, len(cs))
	for i, c := range cs {
		out[i] = card(c)
	}
	return out
}

type imageDTO struct {
	URL          string `json:"url"`
	ThumbnailURL string `json:"thumbnail_url"`
	Alt          string `json:"alt"`
}

type valueDTO struct {
	ID    int64  `json:"id"`
	Value string `json:"value"`
}

type attributeDTO struct {
	ID     int64      `json:"id"`
	Name   string     `json:"name"`
	Values []valueDTO `json:"values"`
}

type variantDTO struct {
	ID              int64             `json:"id"`
	SKU             string            `json:"sku"`
	Attributes      map[string]string `json:"attributes"`
	ValueIDs        []int64           `json:"value_ids"`
	Price           int64             `json:"price"`
	CompareAtPrice  *int64            `json:"compare_at_price"`
	DiscountPercent int               `json:"discount_percent"`
	Stock           int               `json:"stock"`
	Available       bool              `json:"available"`
}

type priceDTO struct {
	Min            int64  `json:"min"`
	Max            int64  `json:"max"`
	CompareAtPrice *int64 `json:"compare_at_price"`
}

type productDTO struct {
	ID          int64          `json:"id"`
	Slug        string         `json:"slug"`
	Name        string         `json:"name"`
	Brand       string         `json:"brand"`
	Description string         `json:"description"`
	Category    categoryDTO    `json:"category"`
	Images      []imageDTO     `json:"images"`
	Rating      ratingDTO      `json:"rating"`
	SoldCount   int            `json:"sold_count"`
	FlashSale   bool           `json:"flash_sale"`
	Price       priceDTO       `json:"price"`
	InStock     bool           `json:"in_stock"`
	Attributes  []attributeDTO `json:"attributes"`
	Variants    []variantDTO   `json:"variants"`
}

// productOut converts the storefront detail (active variants only). A
// variant is available when it has stock; the price summary and stock
// availability come from the variants, as on the web.
func productOut(d product.Detail) productDTO {
	p := productDTO{ID: d.ID, Slug: d.Slug, Name: d.Name, Brand: d.Brand, Description: d.Description,
		Category:  categoryDTO{ID: d.CategoryID, Name: d.CategoryName, Slug: d.CategorySlug},
		Rating:    ratingDTO{Average: d.RatingAverage, Count: d.RatingCount},
		SoldCount: d.SoldCount, FlashSale: d.FlashSale,
		Images:     make([]imageDTO, 0, len(d.Images)),
		Attributes: make([]attributeDTO, 0, len(d.Attributes)),
		Variants:   make([]variantDTO, 0, len(d.Variants))}
	for _, im := range d.Images {
		p.Images = append(p.Images, imageDTO{URL: img(im.URL, detailWidth), ThumbnailURL: img(im.URL, thumbWidth), Alt: im.Alt})
	}
	names := map[int64]string{}
	attrOf := map[int64]string{}
	for _, a := range d.Attributes {
		ad := attributeDTO{ID: a.ID, Name: a.Name, Values: make([]valueDTO, 0, len(a.Values))}
		for _, v := range a.Values {
			ad.Values = append(ad.Values, valueDTO{ID: v.ID, Value: v.Value})
			names[v.ID], attrOf[v.ID] = v.Value, a.Name
		}
		p.Attributes = append(p.Attributes, ad)
	}
	for i, v := range d.Variants {
		vd := variantDTO{ID: v.ID, SKU: v.SKU, Attributes: map[string]string{}, ValueIDs: v.ValueIDs, Price: v.Price,
			CompareAtPrice: nonZero(v.OriginalPrice), DiscountPercent: v.DiscountPercent(), Stock: v.Stock, Available: v.Stock > 0}
		if vd.ValueIDs == nil {
			vd.ValueIDs = []int64{}
		}
		for _, id := range v.ValueIDs {
			vd.Attributes[attrOf[id]] = names[id]
		}
		p.Variants = append(p.Variants, vd)
		p.InStock = p.InStock || v.Stock > 0
		if i == 0 || v.Price < p.Price.Min {
			p.Price.Min, p.Price.CompareAtPrice = v.Price, nonZero(v.OriginalPrice)
		}
		p.Price.Max = max(p.Price.Max, v.Price)
	}
	return p
}

type reviewDTO struct {
	ID           int64   `json:"id"`
	Author       string  `json:"author"`
	Rating       int     `json:"rating"`
	VariantLabel *string `json:"variant_label"`
	Comment      string  `json:"comment"`
	CreatedAt    string  `json:"created_at"`
}

func publicReviews(rs []review.PublicReview) []reviewDTO {
	out := make([]reviewDTO, len(rs))
	for i, r := range rs {
		out[i] = reviewDTO{ID: r.ID, Author: r.Author, Rating: r.Rating, VariantLabel: nonEmpty(r.VariantLabel),
			Comment: r.Comment, CreatedAt: ts(r.CreatedAt)}
	}
	return out
}

type ratingSummaryDTO struct {
	Average      float64        `json:"average"`
	Count        int            `json:"count"`
	Distribution map[string]int `json:"distribution"`
}

func ratingSummary(s review.Summary) ratingSummaryDTO {
	d := ratingSummaryDTO{Average: s.Average, Count: s.Count, Distribution: map[string]int{"1": 0, "2": 0, "3": 0, "4": 0, "5": 0}}
	for _, b := range s.Bars {
		d.Distribution[string(rune('0'+b.Stars))] = b.Count
	}
	return d
}

// ---------------------------------------------------------------- cart

type cartItemDTO struct {
	ID             int64   `json:"id"`
	ProductID      *int64  `json:"product_id"`
	ProductSlug    string  `json:"product_slug"`
	ProductName    string  `json:"product_name"`
	VariantID      *int64  `json:"variant_id"`
	VariantLabel   string  `json:"variant_label"`
	SKU            string  `json:"sku"`
	ImageURL       *string `json:"image_url"`
	UnitPrice      int64   `json:"unit_price"`
	CompareAtPrice *int64  `json:"compare_at_price"`
	Quantity       int     `json:"quantity"`
	Stock          int     `json:"stock"`
	MaxQuantity    int     `json:"max_quantity"`
	Available      bool    `json:"available"`
	Status         string  `json:"status"`
	Subtotal       int64   `json:"subtotal"`
}

type cartDTO struct {
	Items   []cartItemDTO  `json:"items"`
	Summary cartSummaryDTO `json:"summary"`
}

// cartSummaryDTO totals the lines that can be checked out now.
type cartSummaryDTO struct {
	Lines    int   `json:"lines"`
	Quantity int   `json:"quantity"`
	Subtotal int64 `json:"subtotal"`
}

func cartOut(c cart.Cart) cartDTO {
	out := cartDTO{Items: make([]cartItemDTO, 0, len(c.Items))}
	for _, it := range c.Items {
		out.Items = append(out.Items, cartItemDTO{ID: it.ID, ProductID: nonZero(it.ProductID), ProductSlug: it.ProductSlug,
			ProductName: it.ProductName, VariantID: nonZero(it.VariantID), VariantLabel: it.VariantLabel, SKU: it.SKU,
			ImageURL: nonEmpty(img(it.ImageURL, thumbWidth)), UnitPrice: it.Price, CompareAtPrice: nonZero(it.OriginalPrice),
			Quantity: it.Quantity, Stock: it.Stock, MaxQuantity: max(it.MaxQuantity(), 0), Available: it.Checkoutable(),
			Status: string(it.Status), Subtotal: it.Subtotal()})
	}
	s := c.Summarize(func(int64) bool { return true })
	out.Summary = cartSummaryDTO{Lines: s.Lines, Quantity: s.Quantity, Subtotal: s.Subtotal}
	return out
}

// ---------------------------------------------------------------- checkout

// shippingDTO says honestly that shipping is not calculated yet: cost 0 is
// a placeholder, not free shipping.
type shippingDTO struct {
	Cost    int64  `json:"cost"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

var shippingPending = shippingDTO{Cost: 0, Status: "PENDING_INTEGRATION",
	Message: "Ongkos kirim belum dihitung (integrasi kurir belum tersedia). Ini bukan gratis ongkir."}

type previewLineDTO struct {
	CartItemID     *int64  `json:"cart_item_id"`
	ProductID      int64   `json:"product_id"`
	ProductSlug    string  `json:"product_slug"`
	ProductName    string  `json:"product_name"`
	VariantID      int64   `json:"variant_id"`
	VariantLabel   string  `json:"variant_label"`
	SKU            string  `json:"sku"`
	ImageURL       *string `json:"image_url"`
	UnitPrice      int64   `json:"unit_price"`
	CompareAtPrice *int64  `json:"compare_at_price"`
	Quantity       int     `json:"quantity"`
	Stock          int     `json:"stock"`
	Subtotal       int64   `json:"subtotal"`
	Problem        *string `json:"problem"`
}

type warningDTO struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	VariantID *int64 `json:"variant_id,omitempty"`
}

type previewDTO struct {
	Items    []previewLineDTO `json:"items"`
	Address  *addressDTO      `json:"address"`
	Subtotal int64            `json:"subtotal"`
	Shipping shippingDTO      `json:"shipping"`
	Discount int64            `json:"discount"`
	Total    int64            `json:"total"`
	Ready    bool             `json:"ready"`
	Warnings []warningDTO     `json:"warnings"`
	Quantity int              `json:"quantity"`
	Currency string           `json:"currency"`
	IsBuyNow bool             `json:"is_buy_now"`
}

var problemText = map[checkout.Problem]string{
	checkout.ProblemUnavailable:       "Produk sudah tidak tersedia.",
	checkout.ProblemOutOfStock:        "Stok habis.",
	checkout.ProblemInsufficientStock: "Stok tidak mencukupi jumlah yang dipilih.",
	checkout.ProblemInvalidQuantity:   "Jumlah tidak valid.",
}

func previewOut(d checkout.Draft) previewDTO {
	p := previewDTO{Items: make([]previewLineDTO, 0, len(d.Lines)), Subtotal: d.Subtotal, Shipping: shippingPending,
		Discount: d.Discount, Total: d.Total, Ready: d.Ready(), Warnings: []warningDTO{}, Quantity: d.Quantity(),
		Currency: "IDR", IsBuyNow: d.Source.BuyNow()}
	for _, l := range d.Lines {
		ld := previewLineDTO{CartItemID: nonZero(l.CartItemID), ProductID: l.ProductID, ProductSlug: l.ProductSlug,
			ProductName: l.ProductName, VariantID: l.VariantID, VariantLabel: l.VariantLabel, SKU: l.SKU,
			ImageURL: nonEmpty(img(l.ImageURL, thumbWidth)), UnitPrice: l.UnitPrice, CompareAtPrice: nonZero(l.OriginalPrice),
			Quantity: l.Quantity, Stock: l.Stock, Subtotal: l.Subtotal()}
		if l.Problem != checkout.ProblemNone {
			code := string(l.Problem)
			ld.Problem = &code
			v := l.VariantID
			p.Warnings = append(p.Warnings, warningDTO{Code: "LINE_" + strings.ToUpper(code), Message: problemText[l.Problem], VariantID: &v})
		}
		p.Items = append(p.Items, ld)
	}
	if d.Address != nil {
		a := addressOut(*d.Address)
		p.Address = &a
	} else {
		p.Warnings = append(p.Warnings, warningDTO{Code: "ADDRESS_REQUIRED", Message: "Pilih alamat pengiriman terlebih dahulu."})
	}
	p.Warnings = append(p.Warnings, warningDTO{Code: "SHIPPING_NOT_CALCULATED", Message: shippingPending.Message})
	return p
}

// ---------------------------------------------------------------- orders

type orderLineDTO struct {
	ProductName  string  `json:"product_name"`
	VariantLabel string  `json:"variant_label"`
	ImageURL     *string `json:"image_url"`
	Quantity     int     `json:"quantity"`
	UnitPrice    int64   `json:"unit_price"`
}

type orderSummaryDTO struct {
	OrderNumber  string       `json:"order_number"`
	Status       string       `json:"status"`
	StatusLabel  string       `json:"status_label"`
	Total        int64        `json:"total"`
	ItemCount    int          `json:"item_count"`
	FirstItem    orderLineDTO `json:"first_item"`
	CreatedAt    string       `json:"created_at"`
	PaymentDueAt *string      `json:"payment_due_at"`
}

func orderSummaries(os []order.Summary) []orderSummaryDTO {
	out := make([]orderSummaryDTO, len(os))
	for i, o := range os {
		out[i] = orderSummaryDTO{OrderNumber: o.Number, Status: string(o.Status), StatusLabel: o.Status.Label(), Total: o.Total,
			ItemCount: o.ItemCount, CreatedAt: ts(o.CreatedAt),
			FirstItem: orderLineDTO{ProductName: o.First.ProductName, VariantLabel: o.First.VariantLabel,
				ImageURL: nonEmpty(img(o.First.ImageURL, thumbWidth)), Quantity: o.First.Quantity, UnitPrice: o.First.UnitPrice}}
		if o.Status == order.StatusWaitingPayment {
			out[i].PaymentDueAt = tsp(&o.DueAt)
		}
	}
	return out
}

type orderItemDTO struct {
	ID                 int64   `json:"id"`
	ProductSlug        string  `json:"product_slug"`
	ProductName        string  `json:"product_name"`
	SKU                string  `json:"sku"`
	VariantLabel       string  `json:"variant_label"`
	ImageURL           *string `json:"image_url"`
	UnitPrice          int64   `json:"unit_price"`
	Quantity           int     `json:"quantity"`
	Subtotal           int64   `json:"subtotal"`
	ReviewID           *int64  `json:"review_id"`
	ReturnedQuantity   int     `json:"returned_quantity"`
	ReturnableQuantity int     `json:"returnable_quantity"`
}

type orderAddressDTO struct {
	Label         string   `json:"label"`
	RecipientName string   `json:"recipient_name"`
	Phone         string   `json:"phone"`
	Province      string   `json:"province"`
	City          string   `json:"city"`
	District      string   `json:"district"`
	Village       string   `json:"village"`
	PostalCode    string   `json:"postal_code"`
	StreetAddress string   `json:"street_address"`
	Latitude      *float64 `json:"latitude"`
	Longitude     *float64 `json:"longitude"`
}

type shipmentDTO struct {
	Method         string  `json:"method"`
	Courier        string  `json:"courier"`
	Service        *string `json:"service"`
	TrackingNumber *string `json:"tracking_number"`
	ShippedAt      string  `json:"shipped_at"`
	LiveTracking   bool    `json:"live_tracking"`
}

type paymentDTO struct {
	Status      string  `json:"status"`
	StatusLabel string  `json:"status_label"`
	Method      *string `json:"method"`
	ExpiresAt   string  `json:"expires_at"`
	PaidAt      *string `json:"paid_at"`
}

type orderReturnDTO struct {
	ReturnNumber string `json:"return_number"`
	Type         string `json:"type"`
	Status       string `json:"status"`
	StatusLabel  string `json:"status_label"`
	CreatedAt    string `json:"created_at"`
}

type orderActionsDTO struct {
	CanPay    bool `json:"can_pay"`
	CanCancel bool `json:"can_cancel"`
	CanReview bool `json:"can_review"`
	CanReturn bool `json:"can_return"`
}

type orderDTO struct {
	OrderNumber    string           `json:"order_number"`
	Status         string           `json:"status"`
	StatusLabel    string           `json:"status_label"`
	Subtotal       int64            `json:"subtotal"`
	Shipping       shippingDTO      `json:"shipping"`
	Discount       int64            `json:"discount"`
	Total          int64            `json:"total"`
	Items          []orderItemDTO   `json:"items"`
	Address        orderAddressDTO  `json:"address"`
	Shipment       *shipmentDTO     `json:"shipment"`
	LatestPayment  *paymentDTO      `json:"latest_payment"`
	PaymentEnabled bool             `json:"payment_enabled"`
	Sandbox        bool             `json:"sandbox"`
	Actions        orderActionsDTO  `json:"actions"`
	ReturnDeadline *string          `json:"return_deadline"`
	Returns        []orderReturnDTO `json:"returns"`
	CancelReason   *string          `json:"cancel_reason"`
	CreatedAt      string           `json:"created_at"`
	PaymentDueAt   string           `json:"payment_due_at"`
	PaidAt         *string          `json:"paid_at"`
	ConfirmedAt    *string          `json:"confirmed_at"`
	CancelledAt    *string          `json:"cancelled_at"`
	CompletedAt    *string          `json:"completed_at"`
}

func orderOut(o order.Order) orderDTO {
	d := orderDTO{OrderNumber: o.Number, Status: string(o.Status), StatusLabel: o.Status.Label(), Subtotal: o.Subtotal,
		Shipping: shippingDTO{Cost: o.ShippingCost, Status: shippingPending.Status, Message: shippingPending.Message},
		Discount: o.Discount, Total: o.Total, Items: make([]orderItemDTO, 0, len(o.Items)),
		Address: orderAddressDTO{Label: o.Address.Label, RecipientName: o.Address.RecipientName, Phone: o.Address.Phone,
			Province: o.Address.Province, City: o.Address.City, District: o.Address.District, Village: o.Address.Village,
			PostalCode: o.Address.PostalCode, StreetAddress: o.Address.Street, Latitude: o.Address.Latitude, Longitude: o.Address.Longitude},
		PaymentEnabled: o.PaymentEnabled, Sandbox: o.Sandbox,
		Actions:        orderActionsDTO{CanPay: o.CanPay(), CanCancel: o.CanCancel(), CanReview: o.CanReview(), CanReturn: o.Returns.Eligible},
		ReturnDeadline: tsp(o.Returns.Deadline), Returns: make([]orderReturnDTO, 0, len(o.Returns.Returns)),
		CreatedAt: ts(o.CreatedAt), PaymentDueAt: ts(o.PaymentDueAt), PaidAt: tsp(o.PaidAt), ConfirmedAt: tsp(o.ConfirmedAt),
		CancelledAt: tsp(o.CancelledAt), CompletedAt: tsp(o.CompletedAt)}
	if o.Status == order.StatusCancelled {
		d.CancelReason = nonEmpty(o.CancelReason)
	}
	for _, it := range o.Items {
		d.Items = append(d.Items, orderItemDTO{ID: it.ID, ProductSlug: it.ProductSlug, ProductName: it.ProductName, SKU: it.SKU,
			VariantLabel: it.VariantLabel, ImageURL: nonEmpty(img(it.ImageURL, thumbWidth)), UnitPrice: it.UnitPrice,
			Quantity: it.Quantity, Subtotal: it.Subtotal, ReviewID: nonZero(it.ReviewID), ReturnedQuantity: it.Returned,
			ReturnableQuantity: it.Returnable()})
	}
	if s := o.Shipment; s != nil {
		d.Shipment = &shipmentDTO{Method: "MANUAL", Courier: s.Courier, Service: nonEmpty(s.Service),
			TrackingNumber: nonEmpty(s.Tracking), ShippedAt: ts(s.ShippedAt), LiveTracking: false}
	}
	if p := o.LastPayment(); p != nil {
		d.LatestPayment = paymentOut(*p)
	}
	for _, r := range o.Returns.Returns {
		d.Returns = append(d.Returns, orderReturnDTO{ReturnNumber: r.Number, Type: string(r.Type), Status: string(r.Status),
			StatusLabel: r.Status.Label(), CreatedAt: ts(r.CreatedAt)})
	}
	return d
}

func paymentOut(p payment.Attempt) *paymentDTO {
	return &paymentDTO{Status: string(p.Status), StatusLabel: p.Status.Label(), Method: nonEmpty(p.Method),
		ExpiresAt: ts(p.ExpiresAt), PaidAt: tsp(p.PaidAt)}
}

// ---------------------------------------------------------------- reviews

type myReviewDTO struct {
	ID            int64   `json:"id"`
	OrderItemID   int64   `json:"order_item_id,omitempty"`
	Rating        int     `json:"rating"`
	Comment       string  `json:"comment"`
	ProductName   string  `json:"product_name"`
	ProductSlug   string  `json:"product_slug"`
	VariantLabel  string  `json:"variant_label"`
	ImageURL      *string `json:"image_url"`
	OrderNumber   string  `json:"order_number"`
	ProductListed bool    `json:"product_listed"`
	Hidden        bool    `json:"hidden"`
	CreatedAt     string  `json:"created_at"`
	EditedAt      *string `json:"edited_at"`
}

func myReview(r review.Review) myReviewDTO {
	return myReviewDTO{ID: r.ID, OrderItemID: r.OrderItemID, Rating: r.Rating, Comment: r.Comment, ProductName: r.ProductName,
		ProductSlug: r.ProductSlug, VariantLabel: r.VariantLabel, ImageURL: nonEmpty(img(r.ImageURL, thumbWidth)),
		OrderNumber: r.OrderNumber, ProductListed: r.ProductListed, Hidden: r.Hidden, CreatedAt: ts(r.CreatedAt),
		EditedAt: tsp(r.EditedAt)}
}

type pendingReviewDTO struct {
	OrderItemID  int64   `json:"order_item_id"`
	ProductName  string  `json:"product_name"`
	VariantLabel string  `json:"variant_label"`
	ImageURL     *string `json:"image_url"`
	OrderNumber  string  `json:"order_number"`
	CompletedAt  *string `json:"completed_at"`
}

// ---------------------------------------------------------------- returns

type returnSummaryDTO struct {
	ReturnNumber string  `json:"return_number"`
	OrderNumber  string  `json:"order_number"`
	Type         string  `json:"type"`
	TypeLabel    string  `json:"type_label"`
	Status       string  `json:"status"`
	StatusLabel  string  `json:"status_label"`
	RefundAmount int64   `json:"refund_amount"`
	ItemCount    int     `json:"item_count"`
	ProductName  string  `json:"product_name"`
	ImageURL     *string `json:"image_url"`
	CreatedAt    string  `json:"created_at"`
}

func returnSummaries(rs []returns.Summary) []returnSummaryDTO {
	out := make([]returnSummaryDTO, len(rs))
	for i, r := range rs {
		out[i] = returnSummaryDTO{ReturnNumber: r.Number, OrderNumber: r.OrderNumber, Type: string(r.Type), TypeLabel: r.Type.Label(),
			Status: string(r.Status), StatusLabel: r.Status.Label(), RefundAmount: r.RefundAmount, ItemCount: r.ItemCount,
			ProductName: r.ProductName, ImageURL: nonEmpty(img(r.ImageURL, thumbWidth)), CreatedAt: ts(r.CreatedAt)}
	}
	return out
}

type returnItemDTO struct {
	OrderItemID      int64   `json:"order_item_id"`
	ProductName      string  `json:"product_name"`
	VariantLabel     string  `json:"variant_label"`
	ImageURL         *string `json:"image_url"`
	Quantity         int     `json:"quantity"`
	ApprovedQuantity *int    `json:"approved_quantity"`
	UnitPrice        int64   `json:"unit_price"`
}

type evidenceDTO struct {
	URL  string `json:"url"`
	Kind string `json:"kind"`
}

type returnEventDTO struct {
	Status      string `json:"status"`
	StatusLabel string `json:"status_label"`
	Actor       string `json:"actor"`
	Note        string `json:"note"`
	At          string `json:"at"`
}

type returnRefundDTO struct {
	Amount      int64   `json:"amount"`
	Status      string  `json:"status"`
	StatusLabel string  `json:"status_label"`
	ProcessedAt *string `json:"processed_at"`
}

type returnDTO struct {
	returnSummaryDTO
	Reason         string           `json:"reason"`
	ReasonLabel    string           `json:"reason_label"`
	Description    string           `json:"description"`
	ResolutionNote string           `json:"resolution_note"`
	Items          []returnItemDTO  `json:"items"`
	Evidence       []evidenceDTO    `json:"evidence"`
	Timeline       []returnEventDTO `json:"timeline"`
	Refund         *returnRefundDTO `json:"refund"`
	CanCancel      bool             `json:"can_cancel"`
	ApprovedAt     *string          `json:"approved_at"`
	RejectedAt     *string          `json:"rejected_at"`
	CompletedAt    *string          `json:"completed_at"`
}

func returnOut(r returns.Return) returnDTO {
	d := returnDTO{returnSummaryDTO: returnSummaryDTO{ReturnNumber: r.Number, OrderNumber: r.OrderNumber, Type: string(r.Type),
		TypeLabel: r.Type.Label(), Status: string(r.Status), StatusLabel: r.Status.Label(), RefundAmount: r.RefundAmount,
		ItemCount: r.Quantity(), CreatedAt: ts(r.RequestedAt)},
		Reason: string(r.Reason), ReasonLabel: r.Reason.Label(), Description: r.Description, ResolutionNote: r.ResolutionNote,
		Items: make([]returnItemDTO, 0, len(r.Items)), Evidence: make([]evidenceDTO, 0, len(r.Evidence)),
		Timeline: make([]returnEventDTO, 0, len(r.Events)), CanCancel: r.CanCancel(),
		ApprovedAt: tsp(r.ApprovedAt), RejectedAt: tsp(r.RejectedAt), CompletedAt: tsp(r.CompletedAt)}
	for i, it := range r.Items {
		if i == 0 {
			d.ProductName, d.ImageURL = it.ProductName, nonEmpty(img(it.ImageURL, thumbWidth))
		}
		d.Items = append(d.Items, returnItemDTO{OrderItemID: it.OrderItemID, ProductName: it.ProductName,
			VariantLabel: it.VariantLabel, ImageURL: nonEmpty(img(it.ImageURL, thumbWidth)), Quantity: it.Quantity,
			ApprovedQuantity: it.Approved, UnitPrice: it.UnitPrice})
	}
	for _, e := range r.Evidence {
		d.Evidence = append(d.Evidence, evidenceDTO{URL: e.URL, Kind: e.Kind})
	}
	for _, e := range r.Events {
		actor := "SYSTEM"
		switch e.Actor {
		case returns.ActorUser:
			actor = "BUYER"
		case returns.ActorAdmin:
			actor = "SHOP"
		}
		d.Timeline = append(d.Timeline, returnEventDTO{Status: string(e.To), StatusLabel: e.To.Label(), Actor: actor, Note: e.Note, At: ts(e.At)})
	}
	if f := r.Refund; f != nil {
		d.Refund = &returnRefundDTO{Amount: f.Amount, Status: string(f.Status), StatusLabel: f.Status.Label(), ProcessedAt: tsp(f.ProcessedAt)}
	}
	return d
}
