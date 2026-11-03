package view

import "testing"

func TestFormatRupiah(t *testing.T) {
	for n, want := range map[int64]string{0: "Rp0", 999: "Rp999", 1000: "Rp1.000", 129000: "Rp129.000", 12500000: "Rp12.500.000", -5000: "-Rp5.000"} {
		if got := FormatRupiah(n); got != want {
			t.Errorf("FormatRupiah(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestFormatSold(t *testing.T) {
	for n, want := range map[int]string{0: "0", 950: "950", 1000: "1rb", 1250: "1,2rb", 12500: "12rb", 1_300_000: "1,3jt"} {
		if got := FormatSold(n); got != want {
			t.Errorf("FormatSold(%d) = %q, want %q", n, got, want)
		}
	}
}
