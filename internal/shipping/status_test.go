package shipping

import (
	"testing"
	"time"
)

func TestCurrentStatus(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	ev := func(s Status, m int) Event { return Event{Status: s, Time: at(m)} }

	cases := []struct {
		name   string
		events []Event
		want   Status
	}{
		{"no events", nil, StatusCreated},
		{"normal journey", []Event{ev(StatusCreated, 0), ev(StatusPickedUp, 10), ev(StatusInTransit, 60)}, StatusInTransit},
		// A late, repeated or out-of-order event never moves the status back.
		{"out of order", []Event{ev(StatusCreated, 0), ev(StatusOutForDelivery, 90), ev(StatusPickedUp, 95)}, StatusOutForDelivery},
		{"duplicate", []Event{ev(StatusPickedUp, 10), ev(StatusPickedUp, 10), ev(StatusPickedUp, 11)}, StatusPickedUp},
		{"exception is newest", []Event{ev(StatusPickedUp, 10), ev(StatusException, 20)}, StatusException},
		{"exception cleared by later progress", []Event{ev(StatusPickedUp, 10), ev(StatusException, 20), ev(StatusInTransit, 30)}, StatusInTransit},
		{"delivered is final", []Event{ev(StatusDelivered, 50), ev(StatusInTransit, 60), ev(StatusException, 70)}, StatusDelivered},
		{"returned wins", []Event{ev(StatusPickedUp, 10), ev(StatusException, 20), ev(StatusReturned, 30)}, StatusReturned},
		{"cancelled by provider", []Event{ev(StatusCreated, 0), ev(StatusCancelled, 5)}, StatusCancelled},
	}
	for _, c := range cases {
		if got := Current(c.events, StatusCreated); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	if !StatusPickedUp.Moving() || !StatusDelivered.Moving() || StatusCreated.Moving() || StatusException.Moving() {
		t.Error("Moving")
	}
}

func TestPackageAndKeys(t *testing.T) {
	p := Package{Items: []Item{{VariantID: 1, WeightGrams: 250, Quantity: 2}, {VariantID: 2, WeightGrams: 100, Quantity: 3}}}
	if p.WeightGrams() != 800 || !p.Valid() {
		t.Errorf("weight %d valid %v", p.WeightGrams(), p.Valid())
	}
	more := Package{Items: []Item{{VariantID: 1, WeightGrams: 250, Quantity: 3}, {VariantID: 2, WeightGrams: 100, Quantity: 3}}}
	if packageHash(p) == packageHash(more) {
		t.Error("a quantity change must change the package hash")
	}
	swapped := Package{Items: []Item{p.Items[1], p.Items[0]}}
	if packageHash(p) != packageHash(swapped) {
		t.Error("line order must not matter")
	}
	if (Package{Items: []Item{{WeightGrams: 0, Quantity: 1}}}).Valid() || (Package{}).Valid() {
		t.Error("a package without weight is not valid")
	}
	lat, lng := -6.89, 107.61
	a := Location{AreaID: "A", PostalCode: "40132"}
	b := Location{AreaID: "A", PostalCode: "40132", Latitude: &lat, Longitude: &lng}
	if a.key() == b.key() || a.key() == (Location{AreaID: "B", PostalCode: "40132"}).key() {
		t.Error("location keys must differ for another area or coordinates")
	}
	for days, want := range map[string]string{"2-3": "2–3 hari", "1": "1 hari", "0": "Hari ini", "": ""} {
		if got := EstimateLabel(days); got != want {
			t.Errorf("EstimateLabel(%q) = %q", days, got)
		}
	}
}
