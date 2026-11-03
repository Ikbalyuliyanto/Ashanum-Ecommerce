// Package category owns product categories. The Service has no HTTP
// dependency: storefront pages use List/BySlug, and the admin phase (and a
// future API) will use Create/Update unchanged.
package category

import (
	"errors"
	"time"
)

var ErrNotFound = errors.New("category: not found")

type Category struct {
	ID           int64
	Name         string
	Slug         string
	ImageURL     string // optional icon/image; see package media
	Active       bool
	DisplayOrder int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Input is raw admin input; the Service validates it. An empty Slug is
// derived from Name.
type Input struct {
	Name         string
	Slug         string
	ImageURL     string
	Active       bool
	DisplayOrder int
}
