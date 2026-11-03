package category

import (
	"context"
	"errors"
	"testing"
	"time"

	"ecommerce/internal/testutil"
	"ecommerce/internal/validate"
)

func TestListBySlugAndValidation(t *testing.T) {
	db := testutil.DB(t)
	svc, ctx := NewService(NewRepository(db)), context.Background()

	for _, in := range []Input{
		{Name: "Tas", Active: true, DisplayOrder: 2},
		{Name: "Gamis & Dress", Active: true, DisplayOrder: 1},
		{Name: "Arsip Lama", Active: false, DisplayOrder: 0},
	} {
		if _, err := svc.Create(ctx, in); err != nil {
			t.Fatal(err)
		}
	}

	list, err := svc.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Slug != "gamis-dress" || list[1].Name != "Tas" {
		t.Fatalf("active categories in display order: %+v", list)
	}

	if c, err := svc.BySlug(ctx, "gamis-dress"); err != nil || c.Name != "Gamis & Dress" {
		t.Errorf("BySlug = %+v, %v", c, err)
	}
	for _, slug := range []string{"arsip-lama", "tidak-ada"} {
		if _, err := svc.BySlug(ctx, slug); !errors.Is(err, ErrNotFound) {
			t.Errorf("BySlug(%s): err = %v", slug, err)
		}
	}

	for name, in := range map[string]Input{
		"duplicate slug": {Name: "Tas"},
		"bad slug":       {Name: "Sepatu", Slug: "Sepatu Pria"},
		"short name":     {Name: "X"},
		"bad image":      {Name: "Sepatu", ImageURL: "http://example.com/x.png"},
	} {
		var ve validate.Errors
		if _, err := svc.Create(ctx, in); !errors.As(err, &ve) {
			t.Errorf("%s: want validation error, got %v", name, err)
		}
	}
}

func TestCacheIsInvalidatedOnWrite(t *testing.T) {
	db := testutil.DB(t)
	svc, ctx := NewService(NewRepository(db)), context.Background()
	tas, err := svc.Create(ctx, Input{Name: "Tas", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := svc.List(ctx); len(list) != 1 {
		t.Fatalf("list = %+v", list)
	}

	// A change made outside the service is served from cache until expiry.
	if _, err := db.Exec(ctx, "INSERT INTO categories (name, slug) VALUES ('Hijab', 'hijab')"); err != nil {
		t.Fatal(err)
	}
	if list, _ := svc.List(ctx); len(list) != 1 {
		t.Errorf("cached list expected, got %d", len(list))
	}
	svc.now = func() time.Time { return time.Now().Add(cacheTTL + time.Second) }
	if list, _ := svc.List(ctx); len(list) != 2 {
		t.Errorf("after TTL the list must be reloaded, got %d", len(list))
	}
	svc.now = time.Now

	// Writes through the service are visible immediately.
	if _, err := svc.Update(ctx, tas.ID, Input{Name: "Tas & Dompet", Active: false}); err != nil {
		t.Fatal(err)
	}
	list, _ := svc.List(ctx)
	if len(list) != 1 || list[0].Slug != "hijab" {
		t.Errorf("deactivated category must disappear at once: %+v", list)
	}
}
