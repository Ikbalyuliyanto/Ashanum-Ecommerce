package address

import (
	"net/url"
	"strconv"
	"strings"

	"ecommerce/internal/validate"
)

// normalize validates input; errors are keyed by the form field names.
func normalize(in Input) (fields, error) {
	errs := validate.Errors{}
	f := fields{
		Label:         validate.Text(in.Label),
		RecipientName: validate.Text(in.RecipientName),
		Province:      validate.Text(in.Province),
		City:          validate.Text(in.City),
		District:      validate.Text(in.District),
		Village:       validate.Text(in.Village),
		PostalCode:    strings.TrimSpace(in.PostalCode),
		StreetAddress: validate.Text(in.StreetAddress),
		MapsURL:       strings.TrimSpace(in.MapsURL),
	}

	validate.Length(errs, "label", f.Label, "Label alamat", 1, 30)
	validate.Name(errs, "recipient_name", f.RecipientName, "Nama penerima", true)
	if phone, ok := validate.Phone(in.Phone); ok {
		f.Phone = phone
	} else {
		errs.Add("phone", "Masukkan nomor HP Indonesia yang valid, contoh 081234567890.")
	}
	validate.Length(errs, "province", f.Province, "Provinsi", 3, 60)
	validate.Length(errs, "city", f.City, "Kabupaten/Kota", 3, 60)
	validate.Length(errs, "district", f.District, "Kecamatan", 3, 60)
	validate.Length(errs, "village", f.Village, "Kelurahan/Desa", 3, 60)
	if !isPostalCode(f.PostalCode) {
		errs.Add("postal_code", "Kode pos harus 5 digit angka.")
	}
	validate.Length(errs, "street_address", f.StreetAddress, "Alamat lengkap", 10, 200)

	lat, lng := strings.TrimSpace(in.Latitude), strings.TrimSpace(in.Longitude)
	switch {
	case lat == "" && lng == "":
	case lat == "" || lng == "":
		errs.Add("latitude", "Isi latitude dan longitude bersamaan, atau kosongkan keduanya.")
	default:
		if v, ok := parseCoordinate(lat, 90); ok {
			f.Latitude = &v
		} else {
			errs.Add("latitude", "Latitude harus angka antara -90 dan 90.")
		}
		if v, ok := parseCoordinate(lng, 180); ok {
			f.Longitude = &v
		} else {
			errs.Add("longitude", "Longitude harus angka antara -180 dan 180.")
		}
	}

	if f.MapsURL != "" && !isGoogleMapsURL(f.MapsURL) {
		errs.Add("maps_url", "Gunakan tautan Google Maps (https://maps.app.goo.gl/… atau https://www.google.com/maps/…).")
	}
	return f, errs.Err()
}

func isPostalCode(s string) bool {
	if len(s) != 5 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// parseCoordinate accepts a decimal point or a single decimal comma.
func parseCoordinate(s string, limit float64) (float64, bool) {
	if !strings.Contains(s, ".") {
		s = strings.Replace(s, ",", ".", 1)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < -limit || v > limit {
		return 0, false
	}
	return v, true
}

// isGoogleMapsURL allows only https links to Google Maps, so the stored link
// can be rendered safely as a clickable anchor.
func isGoogleMapsURL(raw string) bool {
	if len(raw) > 500 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "maps.google.com", "maps.google.co.id", "maps.app.goo.gl":
		return true
	case "google.com", "www.google.com", "google.co.id", "www.google.co.id", "goo.gl":
		return strings.HasPrefix(u.Path, "/maps")
	}
	return false
}
