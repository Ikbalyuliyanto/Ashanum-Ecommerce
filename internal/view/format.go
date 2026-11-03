package view

import (
	"html/template"
	"strconv"
	"strings"
	"time"

	"ecommerce/internal/media"
)

// FormatRupiah renders an amount in rupiah as "Rp129.000".
func FormatRupiah(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, d := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(d)
	}
	if neg {
		return "-Rp" + b.String()
	}
	return "Rp" + b.String()
}

// FormatSold shortens counts like marketplaces do: 950, 1,2rb, 12rb, 1,3jt.
func FormatSold(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 10_000:
		return oneDecimal(n, 1000) + "rb"
	case n < 1_000_000:
		return strconv.Itoa(n/1000) + "rb"
	default:
		return oneDecimal(n, 1_000_000) + "jt"
	}
}

func oneDecimal(n, unit int) string {
	whole, tenth := n/unit, (n%unit)*10/unit
	if tenth == 0 {
		return strconv.Itoa(whole)
	}
	return strconv.Itoa(whole) + "," + strconv.Itoa(tenth)
}

// imageURL returns a safe src for a stored image reference, resized for
// width CSS pixels (doubled for high-density screens). Anything that does
// not pass media.Valid is replaced by the placeholder image.
func (r *Renderer) imageURL(raw string, width int) template.URL {
	if !media.Valid(raw) {
		return template.URL(r.cfg.Assets.URL("img/placeholder.svg"))
	}
	// Valid only admits https Cloudinary URLs and base64 data:image URIs,
	// so marking the result as a trusted URL is safe.
	return template.URL(media.Resize(raw, width*2))
}

// wib is Western Indonesia Time (UTC+7, no daylight saving); order dates
// are shown in it.
var wib = time.FixedZone("WIB", 7*60*60)

var monthsID = [...]string{"Jan", "Feb", "Mar", "Apr", "Mei", "Jun", "Jul", "Agu", "Sep", "Okt", "Nov", "Des"}

// FormatDateTime renders "3 Nov 2026, 10:15 WIB".
func FormatDateTime(t time.Time) string {
	t = t.In(wib)
	return strconv.Itoa(t.Day()) + " " + monthsID[t.Month()-1] + " " + strconv.Itoa(t.Year()) +
		", " + t.Format("15:04") + " WIB"
}
