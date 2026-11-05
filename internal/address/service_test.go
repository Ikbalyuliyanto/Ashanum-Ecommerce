package address

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/shipping/shippingtest"
	"ecommerce/internal/testutil"
	"ecommerce/internal/validate"
)

func validInput(label string) Input {
	return Input{
		Label:         label,
		RecipientName: "Budi Santoso",
		Phone:         "081234567890",
		Province:      "DKI Jakarta",
		City:          "Kota Jakarta Selatan",
		District:      "Kebayoran Baru",
		Village:       "Senayan",
		PostalCode:    "12190",
		StreetAddress: "Jl. Asia Afrika No. 8, RT 01/RW 02",
	}
}

func setup(t *testing.T) (*Service, *pgxpool.Pool, context.Context) {
	db := testutil.DB(t)
	return NewService(NewRepository(db), shippingtest.New(t, db).Service), db, context.Background()
}

func primaryIDs(t *testing.T, svc *Service, userID int64) []int64 {
	t.Helper()
	list, err := svc.List(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, a := range list {
		if a.IsPrimary {
			ids = append(ids, a.ID)
		}
	}
	return ids
}

func TestCreateUpdateAndPrimaryRules(t *testing.T) {
	svc, db, ctx := setup(t)
	uid := testutil.CreateUser(t, db, "a@example.com")

	home, err := svc.Create(ctx, uid, validInput("Rumah"))
	if err != nil {
		t.Fatal(err)
	}
	if !home.IsPrimary || home.Phone != "+6281234567890" {
		t.Errorf("first address must be primary with normalized phone: %+v", home)
	}

	office, err := svc.Create(ctx, uid, validInput("Kantor"))
	if err != nil {
		t.Fatal(err)
	}
	if office.IsPrimary {
		t.Error("second address must not become primary by default")
	}

	in := validInput("Kos")
	in.MakePrimary = true
	kos, err := svc.Create(ctx, uid, in)
	if err != nil {
		t.Fatal(err)
	}
	if got := primaryIDs(t, svc, uid); len(got) != 1 || got[0] != kos.ID {
		t.Errorf("primary = %v, want [%d]", got, kos.ID)
	}

	// Update fields; the primary flag survives an edit without the switch.
	upd := validInput("Kos Baru")
	upd.Latitude, upd.Longitude = "-6,2251", "106.7999"
	upd.MapsURL = "https://maps.app.goo.gl/abc123"
	kos, err = svc.Update(ctx, uid, kos.ID, upd)
	if err != nil {
		t.Fatal(err)
	}
	if kos.Label != "Kos Baru" || !kos.IsPrimary || kos.Latitude == nil || *kos.Latitude != -6.2251 || kos.MapsURL == "" {
		t.Errorf("after update: %+v", kos)
	}

	// Set primary moves the flag.
	if err := svc.SetPrimary(ctx, uid, office.ID); err != nil {
		t.Fatal(err)
	}
	if got := primaryIDs(t, svc, uid); len(got) != 1 || got[0] != office.ID {
		t.Errorf("primary = %v, want [%d]", got, office.ID)
	}
	if err := svc.SetPrimary(ctx, uid, office.ID); err != nil {
		t.Errorf("setting the current primary again must be a no-op: %v", err)
	}

	// Deleting the primary promotes the newest remaining address.
	if err := svc.Delete(ctx, uid, office.ID); err != nil {
		t.Fatal(err)
	}
	if got := primaryIDs(t, svc, uid); len(got) != 1 || got[0] != kos.ID {
		t.Errorf("after deleting primary: primary = %v, want [%d]", got, kos.ID)
	}

	// Deleting a non-primary address leaves the primary alone.
	if err := svc.Delete(ctx, uid, home.ID); err != nil {
		t.Fatal(err)
	}
	if got := primaryIDs(t, svc, uid); len(got) != 1 || got[0] != kos.ID {
		t.Errorf("primary = %v, want [%d]", got, kos.ID)
	}

	if err := svc.Delete(ctx, uid, kos.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := svc.List(ctx, uid); len(list) != 0 {
		t.Errorf("expected no addresses, got %d", len(list))
	}
	if err := svc.Delete(ctx, uid, kos.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting twice: err = %v", err)
	}
}

func TestOtherUsersAddressesAreInvisible(t *testing.T) {
	svc, db, ctx := setup(t)
	alice := testutil.CreateUser(t, db, "alice@example.com")
	bob := testutil.CreateUser(t, db, "bob@example.com")

	a, err := svc.Create(ctx, alice, validInput("Rumah"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, bob, validInput("Rumah Bob")); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Get(ctx, bob, a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get: err = %v", err)
	}
	if _, err := svc.Update(ctx, bob, a.ID, validInput("Dibajak")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update: err = %v", err)
	}
	if err := svc.SetPrimary(ctx, bob, a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetPrimary: err = %v", err)
	}
	if err := svc.Delete(ctx, bob, a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete: err = %v", err)
	}

	list, _ := svc.List(ctx, bob)
	if len(list) != 1 || list[0].Label != "Rumah Bob" {
		t.Errorf("bob sees %+v", list)
	}
	got, err := svc.Get(ctx, alice, a.ID)
	if err != nil || got.Label != "Rumah" || !got.IsPrimary {
		t.Errorf("alice's address changed: %+v, %v", got, err)
	}
	if ids := primaryIDs(t, svc, bob); len(ids) != 1 {
		t.Errorf("bob must keep exactly one primary, got %v", ids)
	}
}

func TestValidation(t *testing.T) {
	svc, db, ctx := setup(t)
	uid := testutil.CreateUser(t, db, "v@example.com")

	bad := Input{
		Label:         "",
		RecipientName: "B",
		Phone:         "12345",
		Province:      "",
		City:          "X",
		District:      "",
		Village:       "",
		PostalCode:    "1234a",
		StreetAddress: "pendek",
		Latitude:      "-6.2",
		MapsURL:       "javascript:alert(1)",
	}
	_, err := svc.Create(ctx, uid, bad)
	var ve validate.Errors
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want validate.Errors", err)
	}
	for _, f := range []string{"label", "phone", "province", "city", "district", "village", "postal_code", "street_address", "latitude", "maps_url"} {
		if ve[f] == "" {
			t.Errorf("missing error for %s", f)
		}
	}
	if list, _ := svc.List(ctx, uid); len(list) != 0 {
		t.Error("invalid address must not be stored")
	}
}

func TestLimit(t *testing.T) {
	svc, db, ctx := setup(t)
	uid := testutil.CreateUser(t, db, "many@example.com")
	for i := range MaxPerUser {
		if _, err := svc.Create(ctx, uid, validInput("Alamat "+strconv.Itoa(i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Create(ctx, uid, validInput("Satu lagi")); !errors.Is(err, ErrLimitReached) {
		t.Errorf("err = %v, want ErrLimitReached", err)
	}
}

func TestConcurrentSetPrimaryKeepsOnePrimary(t *testing.T) {
	svc, db, ctx := setup(t)
	uid := testutil.CreateUser(t, db, "race@example.com")
	var ids []int64
	for i := range 4 {
		a, err := svc.Create(ctx, uid, validInput("Alamat "+strconv.Itoa(i)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.ID)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for i := range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- svc.SetPrimary(ctx, uid, ids[i%len(ids)])
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("SetPrimary: %v", err)
		}
	}
	if got := primaryIDs(t, svc, uid); len(got) != 1 {
		t.Errorf("want exactly one primary, got %v", got)
	}
}

func TestGoogleMapsURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://maps.app.goo.gl/AbC123":            true,
		"https://www.google.com/maps/place/Monas":   true,
		"https://maps.google.com/?q=-6.17,106.82":   true,
		"https://www.google.co.id/maps/@-6.2,106.8": true,
		"http://maps.app.goo.gl/AbC123":             false,
		"https://www.google.com/search?q=maps":      false,
		"https://evil.example/maps":                 false,
		"https://maps.app.goo.gl.evil.example/x":    false,
		"javascript:alert(1)":                       false,
	} {
		if got := isGoogleMapsURL(raw); got != want {
			t.Errorf("isGoogleMapsURL(%q) = %v, want %v", raw, got, want)
		}
	}
}
