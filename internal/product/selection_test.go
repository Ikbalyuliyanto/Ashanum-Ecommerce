package product

import (
	"reflect"
	"testing"
)

// kaos: Warna {Hitam=1, Putih=2} × Ukuran {S=3, M=4}; Putih-M is sold out
// and Hitam-S has a different price.
func kaos() Detail {
	return Detail{
		Attributes: []Attribute{
			{ID: 10, Name: "Warna", Values: []AttributeValue{{1, "Hitam"}, {2, "Putih"}}},
			{ID: 11, Name: "Ukuran", Values: []AttributeValue{{3, "S"}, {4, "M"}}},
		},
		Variants: []Variant{
			{ID: 100, SKU: "ASH-BLK-S", Price: 119000, Stock: 5, ValueIDs: []int64{1, 3}},
			{ID: 101, SKU: "ASH-BLK-M", Price: 129000, OriginalPrice: 159000, Stock: 10, ValueIDs: []int64{1, 4}},
			{ID: 102, SKU: "ASH-WHT-S", Price: 129000, Stock: 3, ValueIDs: []int64{2, 3}},
			{ID: 103, SKU: "ASH-WHT-M", Price: 129000, Stock: 0, ValueIDs: []int64{2, 4}},
		},
	}
}

func TestSelectNothingChosen(t *testing.T) {
	s := kaos().Select(nil)
	if s.Variant != nil || s.Complete {
		t.Fatalf("no variant expected: %+v", s)
	}
	if s.PriceMin != 119000 || s.PriceMax != 129000 || s.Stock != 18 {
		t.Errorf("range = %d–%d stock %d", s.PriceMin, s.PriceMax, s.Stock)
	}
	for _, g := range s.Groups {
		for _, o := range g.Options {
			if !o.Available {
				t.Errorf("%s %s should be available with nothing chosen", g.Name, o.Value)
			}
		}
	}
}

func TestSelectCompleteVariant(t *testing.T) {
	s := kaos().Select([]int64{4, 1}) // order does not matter
	if s.Variant == nil || s.Variant.SKU != "ASH-BLK-M" {
		t.Fatalf("want ASH-BLK-M, got %+v", s.Variant)
	}
	if s.PriceMin != 129000 || s.OriginalPrice != 159000 || s.DiscountPercent() != 19 || s.Stock != 10 {
		t.Errorf("price %d orig %d disc %d stock %d", s.PriceMin, s.OriginalPrice, s.DiscountPercent(), s.Stock)
	}
	warna := s.Groups[0]
	if warna.Chosen != "Hitam" || !warna.Options[0].Selected {
		t.Errorf("Hitam should be chosen: %+v", warna)
	}
	// Clicking the chosen value clears it; clicking another swaps it.
	if got := warna.Options[0].Next; !reflect.DeepEqual(got, []int64{4}) {
		t.Errorf("clear Hitam → %v", got)
	}
	if got := warna.Options[1].Next; !reflect.DeepEqual(got, []int64{2, 4}) {
		t.Errorf("switch to Putih → %v", got)
	}
}

func TestSelectMarksSoldOutCombination(t *testing.T) {
	s := kaos().Select([]int64{4}) // Ukuran M
	putih := s.Groups[0].Options[1]
	if putih.Available {
		t.Error("Putih must be unavailable with M chosen (Putih-M has no stock)")
	}
	if !s.Groups[0].Options[0].Available {
		t.Error("Hitam-M is in stock")
	}

	s = kaos().Select([]int64{2, 4}) // Putih M chosen anyway
	if s.Variant == nil || s.Variant.SKU != "ASH-WHT-M" || s.Stock != 0 {
		t.Errorf("sold-out variant should still resolve with stock 0: %+v", s.Variant)
	}
}

func TestSelectIgnoresUnknownIDsAndSingleVariant(t *testing.T) {
	s := kaos().Select([]int64{999, 1})
	if s.Groups[0].Chosen != "Hitam" || s.Complete {
		t.Errorf("unknown id must be ignored: %+v", s.Groups[0])
	}

	plain := Detail{Variants: []Variant{{ID: 1, SKU: "PSG", Price: 39000, Stock: 7}}}
	s = plain.Select(nil)
	if s.Variant == nil || s.Stock != 7 || !s.Complete || len(s.Groups) != 0 {
		t.Errorf("product without attributes resolves to its only variant: %+v", s)
	}
}

func TestTSQuery(t *testing.T) {
	for in, want := range map[string]string{
		"Kaos Hitam":                "kaos:* & hitam:*",
		"  gamis   ":                "gamis:*",
		"t-shirt!":                  "t:* & shirt:*",
		"'; DROP TABLE products --": "drop:* & table:* & products:*",
		"!!! ???":                   "",
		"a b c d e f g h":           "a:* & b:* & c:* & d:* & e:* & f:*",
		"Khimar Syar'i":             "khimar:* & syar:* & i:*",
	} {
		if got := tsQuery(in); got != want {
			t.Errorf("tsQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestListParamsNormalized(t *testing.T) {
	p := ListParams{Query: " kaos ", MinPrice: 200000, MaxPrice: 50000, Page: 999, PageSize: 500, Sort: "bogus"}.normalized()
	if p.Query != "kaos" || p.MinPrice != 50000 || p.MaxPrice != 200000 || p.Page != MaxPage || p.PageSize != MaxPageSize || p.Sort != SortRelevance {
		t.Errorf("normalized = %+v", p)
	}
	if p := (ListParams{Sort: SortRelevance}).normalized(); p.Sort != SortNewest || p.Page != 1 || p.PageSize != DefaultPageSize {
		t.Errorf("relevance without query must fall back to newest: %+v", p)
	}
}

func TestSelectionMissing(t *testing.T) {
	if got := kaos().Select([]int64{4}).Missing(); len(got) != 1 || got[0] != "Warna" {
		t.Errorf("Missing = %v", got)
	}
}
