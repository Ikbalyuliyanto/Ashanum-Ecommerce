package product

import (
	"fmt"
	"regexp"
	"strings"

	"ecommerce/internal/media"
	"ecommerce/internal/validate"
)

const (
	maxImages          = 8
	maxAttributes      = 3
	maxValuesPerAttr   = 30
	maxVariants        = 100
	maxPrice           = 10_000_000_000 // Rp10 miliar
	maxStock           = 1_000_000
	maxDescriptionRune = 5000
	maxWeightGrams     = 100_000 // 100 kg per unit
	maxDimensionCM     = 300
)

var skuPattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9._-]{0,63}$`)

// validProduct is Input after validation and normalization.
type validProduct struct {
	CategoryID  int64
	Name        string
	Slug        string
	Description string
	Brand       string
	Active      bool
	Featured    bool
	FlashSale   bool
	Images      []ImageInput
	Attributes  []AttributeInput
	Variants    []validVariant
	WeightGrams int
	LengthCM    int
	WidthCM     int
	HeightCM    int
}

type validVariant struct {
	SKU           string
	Price         int64
	OriginalPrice int64
	Stock         int
	WeightGrams   int      // 0 = the product's weight
	Values        []string // one value per attribute, in attribute order
}

// normalizeInput checks a new product. Errors are keyed by field, e.g.
// "variants[2].sku", so an admin form or API can point at the exact input.
func normalizeInput(in Input) (validProduct, error) {
	errs := validate.Errors{}
	p := validProduct{
		CategoryID:  in.CategoryID,
		Name:        validate.Text(in.Name),
		Slug:        strings.TrimSpace(in.Slug),
		Description: strings.TrimSpace(strings.ReplaceAll(in.Description, "\r\n", "\n")),
		Brand:       validate.Text(in.Brand),
		Active:      in.Active,
		Featured:    in.Featured,
		FlashSale:   in.FlashSale,
		WeightGrams: in.WeightGrams,
		LengthCM:    in.LengthCM,
		WidthCM:     in.WidthCM,
		HeightCM:    in.HeightCM,
	}

	checkShipping(errs, p.Active, p.WeightGrams, p.LengthCM, p.WidthCM, p.HeightCM)
	if p.CategoryID <= 0 {
		errs.Add("category_id", "Pilih kategori produk.")
	}
	validate.Length(errs, "name", p.Name, "Nama produk", 3, 120)
	if p.Slug == "" {
		p.Slug = validate.Slug(p.Name)
	}
	if !validate.IsSlug(p.Slug) {
		errs.Add("slug", "Slug hanya boleh huruf kecil, angka, dan tanda hubung.")
	}
	validate.Length(errs, "brand", p.Brand, "Merek", 0, 60)
	validate.Length(errs, "description", p.Description, "Deskripsi", 0, maxDescriptionRune)

	if len(in.Images) > maxImages {
		errs.Add("images", fmt.Sprintf("Maksimal %d gambar per produk.", maxImages))
	}
	for i, img := range in.Images {
		url := strings.TrimSpace(img.URL)
		if !media.Valid(url) {
			errs.Add(fmt.Sprintf("images[%d]", i), "Gambar harus berupa URL Cloudinary (https://res.cloudinary.com/…).")
		}
		alt := validate.Text(img.Alt)
		if alt == "" {
			alt = p.Name
		}
		validate.Length(errs, fmt.Sprintf("images[%d].alt", i), alt, "Teks alternatif", 1, 120)
		p.Images = append(p.Images, ImageInput{URL: url, Alt: alt})
	}

	p.Attributes = normalizeAttributes(errs, in.Attributes)
	p.Variants = normalizeVariants(errs, p.Attributes, in.Variants)
	return p, errs.Err()
}

func normalizeAttributes(errs validate.Errors, in []AttributeInput) []AttributeInput {
	if len(in) > maxAttributes {
		errs.Add("attributes", fmt.Sprintf("Maksimal %d atribut varian.", maxAttributes))
	}
	seen := make(map[string]bool)
	out := make([]AttributeInput, 0, len(in))
	for i, a := range in {
		key := fmt.Sprintf("attributes[%d]", i)
		name := validate.Text(a.Name)
		validate.Length(errs, key+".name", name, "Nama atribut", 1, 30)
		if seen[strings.ToLower(name)] {
			errs.Add(key+".name", "Nama atribut tidak boleh ganda.")
		}
		seen[strings.ToLower(name)] = true

		if len(a.Values) == 0 || len(a.Values) > maxValuesPerAttr {
			errs.Add(key+".values", fmt.Sprintf("Atribut perlu 1–%d nilai.", maxValuesPerAttr))
		}
		seenValue := make(map[string]bool)
		values := make([]string, 0, len(a.Values))
		for j, v := range a.Values {
			v = validate.Text(v)
			validate.Length(errs, fmt.Sprintf("%s.values[%d]", key, j), v, "Nilai atribut", 1, 40)
			if seenValue[strings.ToLower(v)] {
				errs.Add(fmt.Sprintf("%s.values[%d]", key, j), "Nilai atribut tidak boleh ganda.")
			}
			seenValue[strings.ToLower(v)] = true
			values = append(values, v)
		}
		out = append(out, AttributeInput{Name: name, Values: values})
	}
	return out
}

func normalizeVariants(errs validate.Errors, attrs []AttributeInput, in []VariantInput) []validVariant {
	switch {
	case len(in) == 0:
		errs.Add("variants", "Produk perlu minimal satu varian (harga dan stok).")
	case len(in) > maxVariants:
		errs.Add("variants", fmt.Sprintf("Maksimal %d varian per produk.", maxVariants))
	case len(attrs) == 0 && len(in) > 1:
		errs.Add("variants", "Produk tanpa atribut hanya boleh memiliki satu varian.")
	}

	seenSKU := make(map[string]bool)
	seenCombo := make(map[string]bool)
	out := make([]validVariant, 0, len(in))
	for i, v := range in {
		key := fmt.Sprintf("variants[%d]", i)
		vv := validVariant{
			SKU:           strings.ToUpper(strings.TrimSpace(v.SKU)),
			Price:         v.Price,
			OriginalPrice: v.OriginalPrice,
			Stock:         v.Stock,
			WeightGrams:   v.WeightGrams,
			Values:        make([]string, len(attrs)),
		}
		if !variantWeightOK(vv.WeightGrams) {
			errs.Add(key+".weight_grams", "Berat varian 1–100.000 gram, atau kosong untuk mengikuti berat produk.")
		}

		if !skuPattern.MatchString(vv.SKU) {
			errs.Add(key+".sku", "SKU wajib diisi: huruf besar, angka, titik, garis bawah, atau tanda hubung.")
		} else if seenSKU[vv.SKU] {
			errs.Add(key+".sku", "SKU tidak boleh ganda.")
		}
		seenSKU[vv.SKU] = true

		if vv.Price <= 0 || vv.Price > maxPrice {
			errs.Add(key+".price", "Harga harus lebih dari 0.")
		}
		if vv.OriginalPrice != 0 && vv.OriginalPrice <= vv.Price {
			errs.Add(key+".original_price", "Harga coret harus lebih besar dari harga jual.")
		}
		if vv.Stock < 0 || vv.Stock > maxStock {
			errs.Add(key+".stock", "Stok tidak valid.")
		}

		if len(v.Options) != len(attrs) {
			errs.Add(key+".options", "Varian harus memilih tepat satu nilai untuk setiap atribut.")
		}
		for ai, a := range attrs {
			value, ok := option(v.Options, a.Name)
			canonical, known := canonicalValue(a.Values, value)
			if !ok || !known {
				errs.Add(key+".options", fmt.Sprintf("Pilih nilai %s yang terdaftar.", a.Name))
				continue
			}
			vv.Values[ai] = canonical
		}
		combo := strings.ToLower(strings.Join(vv.Values, "\x00"))
		if len(attrs) > 0 && seenCombo[combo] {
			errs.Add(key+".options", "Kombinasi varian ini sudah ada.")
		}
		seenCombo[combo] = true
		out = append(out, vv)
	}
	return out
}

// checkShipping validates the shipping data: an active product needs a
// weight (rates are never computed with an assumed weight); dimensions are
// optional.
func checkShipping(errs validate.Errors, active bool, weight, length, width, height int) {
	switch {
	case weight < 0 || weight > maxWeightGrams:
		errs.Add("weight_grams", "Berat produk 1–100.000 gram.")
	case weight == 0 && active:
		errs.Add("weight_grams", "Isi berat produk (gram): ongkos kirim dihitung dari berat. Produk tanpa berat tidak bisa diaktifkan.")
	}
	for _, d := range []struct {
		field string
		v     int
	}{{"length_cm", length}, {"width_cm", width}, {"height_cm", height}} {
		if d.v < 0 || d.v > maxDimensionCM {
			errs.Add(d.field, "Dimensi 1–300 cm, atau kosong.")
		}
	}
}

func variantWeightOK(w int) bool { return w >= 0 && w <= maxWeightGrams }

// option finds the value chosen for attribute name (case-insensitive).
func option(opts map[string]string, name string) (string, bool) {
	for k, v := range opts {
		if strings.EqualFold(validate.Text(k), name) {
			return validate.Text(v), true
		}
	}
	return "", false
}

func canonicalValue(values []string, v string) (string, bool) {
	for _, known := range values {
		if strings.EqualFold(known, v) {
			return known, true
		}
	}
	return "", false
}
