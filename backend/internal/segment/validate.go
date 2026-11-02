package segment

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"smart-campaign-composer/backend/internal/geo"
)

var decimalRe = regexp.MustCompile(`^\d{1,10}(\.\d{1,2})?$`)

// Validate checks r against the catalog and returns errors, non-blocking
// warnings and the normalized rule.
func Validate(r Rule, cat Catalog) Result {
	v := validator{cat: cat}
	norm := Rule{Match: r.Match, Conditions: make([]Condition, len(r.Conditions))}

	if r.Match != "all" && r.Match != "any" {
		v.err("match", "invalid_match", `"all" ya da "any" olmalı`)
	}
	switch {
	case len(r.Conditions) == 0:
		v.err("conditions", "empty_rule", "en az bir koşul gerekli")
	case len(r.Conditions) > cat.MaxConditions:
		v.err("conditions", "too_many_conditions", fmt.Sprintf("en fazla %d koşul olabilir", cat.MaxConditions))
	}
	locations := 0
	for i, c := range r.Conditions {
		norm.Conditions[i] = v.condition(fmt.Sprintf("conditions[%d]", i), c)
		if c.Kind == "location" {
			locations++
		}
	}
	if locations > 1 {
		v.err("conditions", "multiple_location_conditions",
			"bir kuralda en fazla bir location koşulu olabilir; yerleri aynı koşulun places listesine koy (ülke ve şehri ayrı koşul yapma)")
	}
	v.lint(norm)
	return Result{Errors: v.errors, Warnings: v.warnings, Normalized: norm}
}

type validator struct {
	cat      Catalog
	errors   []Issue
	warnings []Issue
}

func (v *validator) err(path, code, msg string) {
	v.errors = append(v.errors, Issue{Path: path, Code: code, Message: msg})
}

func (v *validator) condition(p string, c Condition) Condition {
	switch c.Kind {
	case "attribute":
		if len(c.Places) > 0 {
			v.err(p+".places", "not_allowed", "places yalnızca location koşulunda kullanılır")
		}
		if c.EventType != "" || c.WithinDays != 0 || c.MinCount != 0 || c.Negate || c.NotFollowedBy != "" {
			v.err(p+".event_type", "not_allowed", "attribute koşulunda olay alanları kullanılamaz")
		}
		return v.attribute(p, c)
	case "event":
		if c.Field != "" || c.Operator != "" || len(c.Values) > 0 || len(c.Places) > 0 {
			v.err(p+".field", "not_allowed", "event koşulunda field/operator/values/places kullanılamaz")
		}
		return v.event(p, c)
	case "location":
		switch {
		case c.Field != "":
			v.err(p+".field", "not_allowed", "location koşulunda field kullanılamaz; yerler places listesinde")
		case len(c.Values) > 0:
			v.err(p+".values", "not_allowed", "location koşulunda values kullanılamaz; yerler places listesinde")
		case c.EventType != "" || c.WithinDays != 0 || c.MinCount != 0 || c.Negate || c.NotFollowedBy != "":
			v.err(p+".event_type", "not_allowed", "location koşulunda olay alanları kullanılamaz")
		}
		return v.location(p, c)
	default:
		v.err(p+".kind", "unknown_kind", `"attribute", "location" ya da "event" olmalı`)
		return c
	}
}

func (v *validator) attribute(p string, c Condition) Condition {
	out := Condition{Kind: "attribute", Field: c.Field, Operator: c.Operator}
	spec, ok := v.cat.Fields[c.Field]
	if !ok {
		v.err(p+".field", "unknown_field", "izinli alanlar: "+strings.Join(v.fieldNames(), ", "))
		return c
	}
	if !slices.Contains(spec.Operators, c.Operator) {
		v.err(p+".operator", "invalid_operator", fmt.Sprintf("%s için izinli operatörler: %s", c.Field, strings.Join(spec.Operators, ", ")))
		return c
	}

	switch c.Operator {
	case "between":
		if len(c.Values) != 2 {
			v.err(p+".values", "invalid_values", "between tam olarak iki değer ister: [alt, üst]")
			return c
		}
	case "in", "not_in":
		if len(c.Values) == 0 || len(c.Values) > v.cat.MaxListValues {
			v.err(p+".values", "invalid_values", fmt.Sprintf("%s 1-%d değer ister", c.Operator, v.cat.MaxListValues))
			return c
		}
	default:
		if len(c.Values) != 1 {
			v.err(p+".values", "invalid_values", c.Operator+" tek değer ister")
			return c
		}
	}

	before := len(v.errors)
	out.Values = make([]string, len(c.Values))
	for i, raw := range c.Values {
		out.Values[i] = v.value(fmt.Sprintf("%s.values[%d]", p, i), spec, strings.TrimSpace(raw))
	}
	if c.Operator == "between" && len(v.errors) == before {
		lo, _ := strconv.ParseFloat(out.Values[0], 64)
		hi, _ := strconv.ParseFloat(out.Values[1], 64)
		if lo > hi {
			v.err(p+".values", "invalid_values", "between: alt sınır üst sınırdan büyük olamaz")
		}
	}
	return out
}

func (v *validator) value(p string, spec FieldSpec, s string) string {
	switch spec.Type {
	case TypeInt:
		n, err := strconv.Atoi(s)
		if err != nil || n < spec.Min || n > spec.Max {
			v.err(p, "invalid_value", fmt.Sprintf("%s için %d-%d arası tam sayı olmalı", spec.Name, spec.Min, spec.Max))
			return s
		}
		return strconv.Itoa(n)
	case TypeDecimal:
		if !decimalRe.MatchString(s) {
			v.err(p, "invalid_value", spec.Name+" için en fazla 2 ondalıklı pozitif sayı olmalı (örn. 250.50)")
		}
	case TypeLanguage:
		if !slices.Contains(v.cat.Languages, s) {
			v.err(p, "invalid_value", "izinli diller: "+strings.Join(v.cat.Languages, ", "))
		}
	}
	return s
}

// location resolves every place against the local catalog. Names and ids
// from the model are not trusted: a place must resolve to exactly one catalog
// entry, and only its canonical identifiers are kept.
func (v *validator) location(p string, c Condition) Condition {
	out := Condition{Kind: "location", Operator: c.Operator}
	if c.Operator != "in" && c.Operator != "not_in" {
		v.err(p+".operator", "invalid_operator", `location için operator "in" ya da "not_in" olmalı`)
	}
	if len(c.Places) == 0 || len(c.Places) > v.cat.MaxListValues {
		v.err(p+".places", "invalid_values", fmt.Sprintf("location 1-%d yer ister", v.cat.MaxListValues))
		return out
	}
	for j, pl := range c.Places {
		if pl.Level == string(geo.LevelAdmin1) {
			v.err(fmt.Sprintf("%s.places[%d]", p, j), "unsupported_place_level", "il/bölge hedeflemesi desteklenmiyor; şehir veya ülke seçin")
			continue
		}
		place, err := v.cat.Geo.Resolve(geo.Ref{Level: geo.Level(pl.Level), Name: pl.Name, Code: pl.Code,
			GeonameID: pl.GeonameID, Country: pl.Country})
		if err != nil {
			issue := Issue{Path: fmt.Sprintf("%s.places[%d]", p, j), Code: "invalid_place", Message: err.Error()}
			var re *geo.ResolveError
			if errors.As(err, &re) {
				issue.Code, issue.Message, issue.Candidates = re.Code, re.Message, re.Candidates
			}
			v.errors = append(v.errors, issue)
			continue
		}
		ref := PlaceRef{Level: string(place.Level), GeonameID: place.GeonameID, Country: place.Country, Name: place.Name}
		switch place.Level {
		case geo.LevelCountry:
			ref.Code = place.Country
		case geo.LevelAdmin1:
			ref.Code = place.Admin1
		}
		out.Places = append(out.Places, ref)
	}
	return out
}

func (v *validator) event(p string, c Condition) Condition {
	out := c
	if _, ok := v.cat.Events[c.EventType]; !ok {
		v.err(p+".event_type", "unknown_event", "izinli olaylar: "+strings.Join(v.eventNames(), ", "))
	}
	if c.WithinDays < 1 || c.WithinDays > v.cat.MaxWithinDays {
		v.err(p+".within_days", "out_of_range", fmt.Sprintf("within_days 1-%d arası olmalı", v.cat.MaxWithinDays))
	}
	if c.MinCount < 0 || c.MinCount > v.cat.MaxMinCount {
		v.err(p+".min_count", "out_of_range", fmt.Sprintf("min_count 1-%d arası olmalı", v.cat.MaxMinCount))
	}
	if out.MinCount == 0 {
		out.MinCount = 1
	}
	if c.NotFollowedBy != "" {
		if !slices.Contains(v.cat.Sequences[c.EventType], c.NotFollowedBy) {
			v.err(p+".not_followed_by", "invalid_sequence", fmt.Sprintf("%s için not_followed_by izinli değerler: %s",
				c.EventType, strings.Join(v.cat.Sequences[c.EventType], ", ")))
		}
		if c.Negate {
			v.err(p+".negate", "conflict", "not_followed_by ile negate birlikte kullanılamaz")
		}
		if c.MinCount > 1 {
			v.err(p+".min_count", "conflict", "not_followed_by ile min_count kullanılamaz")
		}
	}
	return out
}

// lint checks the "abandoned cart" pattern. A positive cart_add plus a
// negated purchase in an "all" rule, with the purchase window not longer
// than the cart window, is the brief's "sepete ekleyip almayan" written
// wrongly: it also drops people who bought *before* adding to the cart. That
// is an error. A longer no-purchase window is a different, legitimate
// audience (lapsed buyers) and only gets a hint.
func (v *validator) lint(r Rule) {
	if r.Match != "all" {
		return
	}
	var cartWindows, noPurchaseWindows []int
	for _, c := range r.Conditions {
		if c.Kind != "event" {
			continue
		}
		if c.EventType == "cart_add" && !c.Negate && c.NotFollowedBy == "" {
			cartWindows = append(cartWindows, c.WithinDays)
		}
		if c.EventType == "purchase" && c.Negate {
			noPurchaseWindows = append(noPurchaseWindows, c.WithinDays)
		}
	}
	if len(cartWindows) == 0 || len(noPurchaseWindows) == 0 {
		return
	}
	if slices.Min(noPurchaseWindows) <= slices.Max(cartWindows) {
		v.err("conditions", "use_not_followed_by",
			`"sepete ekleyip almayan" için {"kind":"event","event_type":"cart_add","within_days":N,"not_followed_by":"purchase"} kullan; `+
				`cart_add + negate purchase, sepetten önce yapılmış satın almaları da dışlar`)
		return
	}
	v.warnings = append(v.warnings, Issue{
		Path: "conditions", Code: "abandoned_cart_hint",
		Message: `bu kural "sepete ekleyip uzun süredir alışveriş yapmayan" kitleyi seçer; ` +
			`"sepete ekleyip sonra almayan" kastediliyorsa not_followed_by kullan`,
	})
}

func (v *validator) fieldNames() []string {
	names := make([]string, 0, len(v.cat.Fields))
	for n := range v.cat.Fields {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func (v *validator) eventNames() []string {
	names := make([]string, 0, len(v.cat.Events))
	for n := range v.cat.Events {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}
