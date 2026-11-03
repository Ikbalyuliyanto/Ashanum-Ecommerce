package user

import (
	"context"
	"errors"
	"testing"

	"ecommerce/internal/testutil"
	"ecommerce/internal/validate"
)

func TestProfile(t *testing.T) {
	db := testutil.DB(t)
	svc, ctx := NewService(NewRepository(db)), context.Background()
	uid := testutil.CreateUser(t, db, "rina.putri@example.com")

	acc, err := svc.GetAccount(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if acc.DisplayName() != "rina.putri" || acc.Profile != (Profile{}) {
		t.Errorf("new account: %+v (display %q)", acc, acc.DisplayName())
	}

	p, err := svc.UpdateProfile(ctx, uid, ProfileInput{
		FirstName: "  Rina ", LastName: "Putri  Lestari", Phone: "0812-3456-7890", Gender: "PEREMPUAN",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Profile{FirstName: "Rina", LastName: "Putri Lestari", Phone: "+6281234567890", Gender: GenderFemale}
	if p != want {
		t.Errorf("saved %+v, want %+v", p, want)
	}

	acc, _ = svc.GetAccount(ctx, uid)
	if acc.Profile != want || acc.DisplayName() != "Rina Putri Lestari" {
		t.Errorf("reloaded %+v", acc.Profile)
	}

	// Clearing optional fields stores NULLs again.
	if _, err := svc.UpdateProfile(ctx, uid, ProfileInput{FirstName: "Rina"}); err != nil {
		t.Fatal(err)
	}
	acc, _ = svc.GetAccount(ctx, uid)
	if acc.Profile != (Profile{FirstName: "Rina"}) {
		t.Errorf("after clearing: %+v", acc.Profile)
	}
}

func TestProfileValidation(t *testing.T) {
	db := testutil.DB(t)
	svc, ctx := NewService(NewRepository(db)), context.Background()
	uid := testutil.CreateUser(t, db, "x@example.com")

	_, err := svc.UpdateProfile(ctx, uid, ProfileInput{FirstName: "", LastName: "B4dut", Phone: "021-555", Gender: "LAINNYA"})
	var ve validate.Errors
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want validate.Errors", err)
	}
	for _, f := range []string{"first_name", "last_name", "phone", "gender"} {
		if ve[f] == "" {
			t.Errorf("missing error for %s", f)
		}
	}

	acc, _ := svc.GetAccount(ctx, uid)
	if acc.Profile != (Profile{}) {
		t.Error("invalid input must not be saved")
	}
}
