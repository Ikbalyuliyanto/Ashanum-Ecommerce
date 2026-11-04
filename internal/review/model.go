// Package review holds product reviews and ratings.
//
// A review belongs to one purchased order item and can only be written
// once the order is COMPLETED. product_reviews is the source of truth for
// ratings; products.rating_average / rating_count are a cache rebuilt in
// the same transaction as every review change (RecalculateProductRating).
//
// Moderation: an admin may hide a review (a reason is required) and show it
// again. A hidden review is kept — the buyer still sees and may edit it —
// but it is not public and does not count in the product rating; hiding and
// showing recalculate the rating in the same transaction. Reviews are never
// hard-deleted. The Service has no HTTP dependency, so a future API can
// reuse it.
package review

import (
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrNotFound        = errors.New("review: not found")
	ErrNotEligible     = errors.New("review: order item cannot be reviewed")
	ErrAlreadyReviewed = errors.New("review: order item already reviewed")
	ErrModerated       = errors.New("review: moderation status already changed")
)

// Message is the customer-facing text for a known error, or "".
func Message(err error) string {
	switch {
	case errors.Is(err, ErrNotEligible):
		return "Ulasan hanya bisa diberikan untuk pesanan yang sudah selesai."
	case errors.Is(err, ErrAlreadyReviewed):
		return "Produk ini sudah Anda ulas. Anda bisa mengubah ulasannya."
	}
	return ""
}

const (
	PageSize        = 10 // review history and pending list
	ProductPageSize = 5  // reviews per load on the product page
	MaxComment      = 1000
)

// Input is what the buyer sends: a rating and an optional comment.
type Input struct {
	Rating  int
	Comment string
}

// Target is a purchased item the buyer may review (snapshot data).
type Target struct {
	OrderItemID  int64
	OrderNumber  string
	ProductName  string
	ProductSlug  string
	VariantLabel string
	ImageURL     string
	ReviewID     int64 // 0 while not reviewed
}

// Review is one of the buyer's own reviews, with the item snapshot.
type Review struct {
	ID            int64
	OrderItemID   int64
	Rating        int
	Comment       string
	CreatedAt     time.Time
	EditedAt      *time.Time // the buyer's last change
	Hidden        bool       // hidden by moderation: not public
	ProductName   string
	ProductSlug   string
	VariantLabel  string
	ImageURL      string
	OrderNumber   string
	ProductListed bool // the product is still in the catalog
}

// Edited reports whether the buyer changed the review after writing it.
func (r Review) Edited() bool { return r.EditedAt != nil }

// Target returns the review's item, e.g. for the edit form.
func (r Review) Target() Target {
	return Target{OrderItemID: r.OrderItemID, OrderNumber: r.OrderNumber, ProductName: r.ProductName,
		ProductSlug: r.ProductSlug, VariantLabel: r.VariantLabel, ImageURL: r.ImageURL, ReviewID: r.ID}
}

// PendingItem is a purchased item still waiting for a review.
type PendingItem struct {
	OrderItemID  int64
	ProductName  string
	VariantLabel string
	ImageURL     string
	OrderNumber  string
	CompletedAt  *time.Time
}

// HistoryPage is one page of "Ulasan Saya" (reviews or pending items).
type HistoryPage struct {
	Tab          string // "" (reviews) or "pending"
	Reviews      []Review
	Pending      []PendingItem
	PendingCount int
	Page         int
	HasNext      bool
}

// PublicReview is a review as shown on the product page.
type PublicReview struct {
	ID           int64
	Author       string // masked name, e.g. "I**** Y********"
	Rating       int
	VariantLabel string
	Comment      string
	CreatedAt    time.Time
}

// Bar is one row of the rating distribution.
type Bar struct {
	Stars int
	Count int
}

// Summary is the rating summary of a product.
type Summary struct {
	Average float64
	Count   int
	Bars    []Bar // 5 stars down to 1
}

// Stars is the average rounded to whole stars, for display.
func (s Summary) Stars() int { return int(math.Round(s.Average)) }

// ProductPage is one page of a product's reviews.
type ProductPage struct {
	ProductID   int64
	ProductName string
	ProductSlug string
	Summary     Summary
	Reviews     []PublicReview
	Page        int
	HasNext     bool
}

// AdminReview is a review in the moderation list. The buyer is shown only
// masked, with the order number as identifier.
type AdminReview struct {
	ID           int64
	ProductID    int64 // 0 once the product was deleted
	ProductName  string
	VariantLabel string
	OrderNumber  string
	Author       string // masked name
	Rating       int
	Comment      string
	CreatedAt    time.Time
	EditedAt     *time.Time
	HiddenAt     *time.Time
	Reason       string // moderation reason while hidden
}

func (r AdminReview) Hidden() bool { return r.HiddenAt != nil }

const (
	AdminPageSize    = 20
	MaxAdminPageSize = 100
)

// AdminFilter selects the moderation list: Status "" | "visible" | "hidden",
// Rating 0 (all) or 1–5, Query on the product name.
type AdminFilter struct {
	Status   string
	Rating   int
	Query    string
	Page     int
	PageSize int
}

type AdminPage struct {
	Reviews []AdminReview
	Filter  AdminFilter
	HasNext bool
}

// MaskName hides a reviewer's identity: every word keeps its first letter
// ("Ikbal Yuliyanto" → "I**** Y********"). Without a profile name the
// e-mail's local part is masked the same way; the domain is never shown.
func MaskName(first, last, email string) string {
	name := strings.TrimSpace(first + " " + last)
	if name == "" {
		name, _, _ = strings.Cut(email, "@")
	}
	words := strings.Fields(name)
	if len(words) == 0 {
		return "Pembeli"
	}
	for i, w := range words {
		r, size := utf8.DecodeRuneInString(w)
		words[i] = string(r) + strings.Repeat("*", utf8.RuneCountInString(w[size:]))
	}
	return strings.Join(words, " ")
}

// cleanComment normalizes line breaks and trims the comment.
func cleanComment(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(strings.TrimSpace(s), "\n")
	out := lines[:0]
	blank := 0
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			if blank++; blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}
