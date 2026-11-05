package address

import (
	"context"
	"errors"
	"testing"

	"ecommerce/internal/shipping/shippingtest"
	"ecommerce/internal/testutil"
	"ecommerce/internal/validate"
)

func TestDeliveryArea(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	sh := shippingtest.New(t, db)
	svc := NewService(NewRepository(db), sh.Service)
	uid := testutil.CreateUser(t, db, "a@example.com")
	other := testutil.CreateUser(t, db, "b@example.com")

	// Search, then save one of the areas: its province, city, district and
	// postal code replace what was typed; coordinates are kept.
	area := sh.Area(t, "40132")
	in := validInput("Rumah")
	in.ShippingAreaID, in.Latitude, in.Longitude = area.ID, "-6.89", "107.61"
	a, err := svc.Create(ctx, uid, in)
	if err != nil {
		t.Fatal(err)
	}
	if !a.HasShippingArea() || a.ShippingAreaID != area.ID || a.ShippingAreaProvider != "biteship" || a.ShippingAreaLabel != area.Label ||
		a.Province != "Jawa Barat" || a.City != "Bandung" || a.District != "Coblong" || a.PostalCode != "40132" ||
		a.Latitude == nil || *a.Latitude != -6.89 || a.Village != "Senayan" {
		t.Errorf("saved area: %+v", a)
	}
	got, err := svc.Get(ctx, uid, a.ID)
	if err != nil || got.ShippingAreaID != area.ID || got.PostalCode != "40132" || got.Longitude == nil {
		t.Errorf("persisted: %+v %v", got, err)
	}

	// Only areas the server found itself: an invented id or an area of
	// another provider is refused on the field.
	field := func(label, id string) {
		t.Helper()
		in := validInput("Kantor")
		in.ShippingAreaID = id
		_, err := svc.Create(ctx, uid, in)
		var v validate.Errors
		if !errors.As(err, &v) || v["shipping_area_id"] == "" {
			t.Errorf("%s: %v", label, err)
		}
	}
	field("unknown area", "IDNP-NOT-FOUND")
	if _, err := db.Exec(ctx, "INSERT INTO shipping_areas (provider, provider_area_id, label) VALUES ('rajaongkir', 'RO-1', 'Coblong')"); err != nil {
		t.Fatal(err)
	}
	field("another provider's area", "RO-1")
	noShipping := NewService(NewRepository(db), nil)
	in2 := validInput("Kos")
	in2.ShippingAreaID = area.ID
	if _, err := noShipping.Create(ctx, uid, in2); err == nil {
		t.Error("without a shipping provider an area cannot be chosen")
	}

	// Owner isolation: another user can neither read nor change it.
	if _, err := svc.Get(ctx, other, a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("other user read: %v", err)
	}
	if _, err := svc.Update(ctx, other, a.ID, in); !errors.Is(err, ErrNotFound) {
		t.Errorf("other user update: %v", err)
	}

	// Removing the area ("" ) keeps the address, without rates.
	in.ShippingAreaID = ""
	in.PostalCode = "40132"
	if a, err = svc.Update(ctx, uid, a.ID, in); err != nil || a.HasShippingArea() {
		t.Errorf("area removed: %+v %v", a, err)
	}
}
