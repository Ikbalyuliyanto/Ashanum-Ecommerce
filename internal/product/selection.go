package product

import "sort"

// Selection is the state of the option picker after the customer chose some
// attribute values. It is pure domain logic: web pages and a future API
// render the same result.
type Selection struct {
	Groups []OptionGroup
	// Variant is set when the choice identifies one variant (always, for a
	// product without attributes).
	Variant *Variant
	// Complete is true when every attribute has a chosen value.
	Complete bool
	// Price range, strike-through price and stock across the variants that
	// still match the choice (or of Variant when set).
	PriceMin      int64
	PriceMax      int64
	OriginalPrice int64
	Stock         int
}

type OptionGroup struct {
	AttributeID int64
	Name        string
	Chosen      string // chosen value name, "" if none
	Options     []Option
}

type Option struct {
	ValueID int64
	Value   string
	// Selected marks the chosen value of this attribute.
	Selected bool
	// Available is false when no in-stock variant has this value together
	// with the other chosen values.
	Available bool
	// Next is the full choice after clicking this option (choosing it, or
	// clearing it when already selected), sorted, for building links.
	Next []int64
}

func (s Selection) DiscountPercent() int { return discountPercent(s.PriceMin, s.OriginalPrice) }

// ChosenIDs returns the chosen value IDs, sorted.
func (s Selection) ChosenIDs() []int64 {
	var ids []int64
	for _, g := range s.Groups {
		for _, o := range g.Options {
			if o.Selected {
				ids = append(ids, o.ValueID)
			}
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// Missing lists the attributes that still need a choice.
func (s Selection) Missing() []string {
	var names []string
	for _, g := range s.Groups {
		if g.Chosen == "" {
			names = append(names, g.Name)
		}
	}
	return names
}

// Select resolves chosen value IDs against d. Unknown IDs are ignored; for
// an attribute chosen twice, the last ID wins.
func (d Detail) Select(chosen []int64) Selection {
	attrOf := make(map[int64]int64) // value ID → attribute ID
	for _, a := range d.Attributes {
		for _, v := range a.Values {
			attrOf[v.ID] = a.ID
		}
	}
	picked := make(map[int64]int64) // attribute ID → value ID
	for _, id := range chosen {
		if a, ok := attrOf[id]; ok {
			picked[a] = id
		}
	}

	// matches reports whether v agrees with every pick except attribute skip.
	matches := func(v Variant, skip int64) bool {
		for a, val := range picked {
			if a != skip && !v.has(val) {
				return false
			}
		}
		return true
	}

	var sel Selection
	sel.Complete = len(picked) == len(d.Attributes)

	for _, a := range d.Attributes {
		g := OptionGroup{AttributeID: a.ID, Name: a.Name}
		for _, val := range a.Values {
			o := Option{ValueID: val.ID, Value: val.Value, Selected: picked[a.ID] == val.ID}
			if o.Selected {
				g.Chosen = val.Value
			}
			for _, v := range d.Variants {
				if v.Stock > 0 && v.has(val.ID) && matches(v, a.ID) {
					o.Available = true
					break
				}
			}
			o.Next = nextChoice(picked, a.ID, val.ID, o.Selected)
			g.Options = append(g.Options, o)
		}
		sel.Groups = append(sel.Groups, g)
	}

	var candidates []Variant
	for _, v := range d.Variants {
		if matches(v, 0) {
			candidates = append(candidates, v)
		}
	}
	if sel.Complete && len(candidates) == 1 {
		sel.Variant = &candidates[0]
	}
	if len(candidates) == 0 {
		candidates = d.Variants
	}

	for i, v := range candidates {
		if i == 0 || v.Price < sel.PriceMin {
			sel.PriceMin, sel.OriginalPrice = v.Price, v.OriginalPrice
		}
		if v.Price > sel.PriceMax {
			sel.PriceMax = v.Price
		}
		sel.Stock += v.Stock
	}
	if sel.Variant != nil {
		v := sel.Variant
		sel.PriceMin, sel.PriceMax, sel.OriginalPrice, sel.Stock = v.Price, v.Price, v.OriginalPrice, v.Stock
	}
	return sel
}

func nextChoice(picked map[int64]int64, attr, value int64, clear bool) []int64 {
	next := make([]int64, 0, len(picked)+1)
	for a, v := range picked {
		if a != attr {
			next = append(next, v)
		}
	}
	if !clear {
		next = append(next, value)
	}
	sort.Slice(next, func(i, j int) bool { return next[i] < next[j] })
	return next
}
