package segment

import (
	"testing"
)

func abandonedCart(city string) Rule {
	return Rule{Match: "all", Conditions: []Condition{
		{Kind: "location", Operator: "in", Places: []PlaceRef{{Level: "city", Name: city}}},
		{Kind: "event", EventType: "cart_add", WithinDays: 30, NotFollowedBy: "purchase"},
	}}
}

func codes(issues []Issue) []string {
	out := make([]string, 0, len(issues))
	for _, i := range issues {
		out = append(out, i.Path+":"+i.Code)
	}
	return out
}

func hasIssue(issues []Issue, path, code string) bool {
	for _, i := range issues {
		if i.Path == path && i.Code == code {
			return true
		}
	}
	return false
}

func TestValidateRule_RejectsRegionTargeting(t *testing.T) {
	rule := Rule{Match: "all", Conditions: []Condition{{Kind: "location", Operator: "in", Places: []PlaceRef{{Level: "admin1", Code: "TR-34"}}}}}
	result := Validate(rule, DefaultCatalog())
	if !hasIssue(result.Errors, "conditions[0].places[0]", "unsupported_place_level") {
		t.Fatalf("region targeting should be rejected, got %v", codes(result.Errors))
	}
}

func TestValidateRule_ValidAbandonedCart(t *testing.T) {
	res := Validate(abandonedCart("İstanbul"), DefaultCatalog())
	if len(res.Errors) != 0 {
		t.Fatalf("expected no errors, got %v", codes(res.Errors))
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", codes(res.Warnings))
	}
}

func TestValidateRule_Errors(t *testing.T) {
	cases := []struct {
		name string
		rule Rule
		path string
		code string
	}{
		{"empty rule", Rule{Match: "all"}, "conditions", "empty_rule"},
		{"bad match", Rule{Match: "some", Conditions: abandonedCart("İzmir").Conditions}, "match", "invalid_match"},
		{"unknown kind", Rule{Match: "all", Conditions: []Condition{{Kind: "sql"}}}, "conditions[0].kind", "unknown_kind"},
		{"unknown field", Rule{Match: "all", Conditions: []Condition{
			{Kind: "attribute", Field: "password", Operator: "eq", Values: []string{"x"}}}}, "conditions[0].field", "unknown_field"},
		{"operator not allowed for field", Rule{Match: "all", Conditions: []Condition{
			{Kind: "attribute", Field: "age", Operator: "in", Values: []string{"30"}}}}, "conditions[0].operator", "invalid_operator"},
		{"between needs two values", Rule{Match: "all", Conditions: []Condition{
			{Kind: "attribute", Field: "age", Operator: "between", Values: []string{"18"}}}}, "conditions[0].values", "invalid_values"},
		{"between reversed", Rule{Match: "all", Conditions: []Condition{
			{Kind: "attribute", Field: "age", Operator: "between", Values: []string{"40", "18"}}}}, "conditions[0].values", "invalid_values"},
		{"non integer age", Rule{Match: "all", Conditions: []Condition{
			{Kind: "attribute", Field: "age", Operator: "gte", Values: []string{"otuz"}}}}, "conditions[0].values[0]", "invalid_value"},
		{"injection in numeric value", Rule{Match: "all", Conditions: []Condition{
			{Kind: "attribute", Field: "total_spent", Operator: "gte", Values: []string{"1; DROP TABLE users"}}}}, "conditions[0].values[0]", "invalid_value"},
		{"city is no longer an attribute", Rule{Match: "all", Conditions: []Condition{
			{Kind: "attribute", Field: "city", Operator: "eq", Values: []string{"İstanbul"}}}}, "conditions[0].field", "unknown_field"},
		{"location needs in/not_in", Rule{Match: "all", Conditions: []Condition{
			{Kind: "location", Operator: "eq", Places: []PlaceRef{{Level: "admin1", Name: "İzmir"}}}}}, "conditions[0].operator", "invalid_operator"},
		{"location without places", Rule{Match: "all", Conditions: []Condition{
			{Kind: "location", Operator: "in"}}}, "conditions[0].places", "invalid_values"},
		{"attribute fields on location", Rule{Match: "all", Conditions: []Condition{
			{Kind: "location", Operator: "in", Values: []string{"İzmir"}, Places: []PlaceRef{{Level: "admin1", Name: "İzmir"}}}}}, "conditions[0].values", "not_allowed"},
		{"unknown language", Rule{Match: "all", Conditions: []Condition{
			{Kind: "attribute", Field: "language", Operator: "eq", Values: []string{"xx"}}}}, "conditions[0].values[0]", "invalid_value"},
		{"unknown event", Rule{Match: "all", Conditions: []Condition{
			{Kind: "event", EventType: "login", WithinDays: 7}}}, "conditions[0].event_type", "unknown_event"},
		{"within_days zero", Rule{Match: "all", Conditions: []Condition{
			{Kind: "event", EventType: "purchase", WithinDays: 0}}}, "conditions[0].within_days", "out_of_range"},
		{"within_days too large", Rule{Match: "all", Conditions: []Condition{
			{Kind: "event", EventType: "purchase", WithinDays: 400}}}, "conditions[0].within_days", "out_of_range"},
		{"min_count too large", Rule{Match: "all", Conditions: []Condition{
			{Kind: "event", EventType: "purchase", WithinDays: 30, MinCount: 1000}}}, "conditions[0].min_count", "out_of_range"},
		{"sequence not allowed", Rule{Match: "all", Conditions: []Condition{
			{Kind: "event", EventType: "purchase", WithinDays: 30, NotFollowedBy: "cart_add"}}}, "conditions[0].not_followed_by", "invalid_sequence"},
		{"negate with sequence", Rule{Match: "all", Conditions: []Condition{
			{Kind: "event", EventType: "cart_add", WithinDays: 30, NotFollowedBy: "purchase", Negate: true}}}, "conditions[0].negate", "conflict"},
		{"min_count with sequence", Rule{Match: "all", Conditions: []Condition{
			{Kind: "event", EventType: "cart_add", WithinDays: 30, NotFollowedBy: "purchase", MinCount: 2}}}, "conditions[0].min_count", "conflict"},
		{"attribute fields on event", Rule{Match: "all", Conditions: []Condition{
			{Kind: "event", EventType: "purchase", WithinDays: 30, Field: "city"}}}, "conditions[0].field", "not_allowed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Validate(tc.rule, DefaultCatalog())
			if !hasIssue(res.Errors, tc.path, tc.code) {
				t.Fatalf("expected %s:%s, got %v", tc.path, tc.code, codes(res.Errors))
			}
		})
	}
}

func TestValidateRule_TooManyConditions(t *testing.T) {
	r := Rule{Match: "any"}
	for i := 0; i < 11; i++ {
		r.Conditions = append(r.Conditions, Condition{Kind: "event", EventType: "purchase", WithinDays: 30})
	}
	if res := Validate(r, DefaultCatalog()); !hasIssue(res.Errors, "conditions", "too_many_conditions") {
		t.Fatalf("expected too_many_conditions, got %v", codes(res.Errors))
	}
}

// Place names and ids from Claude are resolved against the local catalog;
// only canonical codes are kept.
func TestValidateRule_Location_NormalizesToCodes(t *testing.T) {
	r := Rule{Match: "all", Conditions: []Condition{{Kind: "location", Operator: "in", Places: []PlaceRef{
		{Level: "city", Name: "istanbul"},
		{Level: "city", Name: "Paris", Country: "Fransa"},
		{Level: "country", Code: "tr"},
	}}}}
	res := Validate(r, DefaultCatalog())
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors %v", codes(res.Errors))
	}
	want := []PlaceRef{
		{Level: "city", GeonameID: 745044, Country: "TR", Name: "İstanbul"},
		{Level: "city", GeonameID: 2988507, Country: "FR", Name: "Paris"},
		{Level: "country", Code: "TR", Country: "TR", Name: "Türkiye"},
	}
	for i, w := range want {
		if got := res.Normalized.Conditions[0].Places[i]; got != w {
			t.Errorf("place %d = %+v, want %+v", i, got, w)
		}
	}
}

func TestValidateRule_Location_Errors(t *testing.T) {
	cases := []struct {
		name  string
		place PlaceRef
		code  string
	}{
		{"same city name in two countries", PlaceRef{Level: "city", Name: "Paris"}, "ambiguous_place"},
		{"unknown city", PlaceRef{Level: "city", Name: "Gotham"}, "unknown_place"},
		{"id not in catalog", PlaceRef{Level: "city", GeonameID: 999999999}, "unknown_place"},
		{"id and name disagree", PlaceRef{Level: "city", GeonameID: 745044, Name: "Ankara"}, "place_mismatch"},
		{"bad level", PlaceRef{Level: "district", Name: "Kadıköy"}, "invalid_level"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Rule{Match: "all", Conditions: []Condition{{Kind: "location", Operator: "in", Places: []PlaceRef{tc.place}}}}
			res := Validate(r, DefaultCatalog())
			if !hasIssue(res.Errors, "conditions[0].places[0]", tc.code) {
				t.Fatalf("expected %s, got %v", tc.code, codes(res.Errors))
			}
		})
	}
	r := Rule{Match: "all", Conditions: []Condition{{Kind: "location", Operator: "in", Places: []PlaceRef{{Level: "city", Name: "Paris"}}}}}
	for _, e := range Validate(r, DefaultCatalog()).Errors {
		if e.Code == "ambiguous_place" && len(e.Candidates) != 2 {
			t.Fatalf("ambiguity must list the candidates, got %+v", e.Candidates)
		}
	}
}

// Country and city must not become separate OR'ed conditions: a rule has at
// most one location condition, whose places are each matched along their
// full hierarchy.
func TestValidateRule_SingleLocationCondition(t *testing.T) {
	r := Rule{Match: "any", Conditions: []Condition{
		{Kind: "location", Operator: "in", Places: []PlaceRef{{Level: "country", Code: "FR"}}},
		{Kind: "location", Operator: "in", Places: []PlaceRef{{Level: "city", Name: "Paris", Country: "FR"}}},
	}}
	if res := Validate(r, DefaultCatalog()); !hasIssue(res.Errors, "conditions", "multiple_location_conditions") {
		t.Fatalf("expected multiple_location_conditions, got %v", codes(res.Errors))
	}
}

func TestValidateRule_DefaultsMinCount(t *testing.T) {
	r := Rule{Match: "all", Conditions: []Condition{{Kind: "event", EventType: "purchase", WithinDays: 30}}}
	res := Validate(r, DefaultCatalog())
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors %v", codes(res.Errors))
	}
	if res.Normalized.Conditions[0].MinCount != 1 {
		t.Fatalf("min_count default = %d, want 1", res.Normalized.Conditions[0].MinCount)
	}
}

// "Sepete ekleyip almayan" written as cart_add + negated purchase over the
// same (or a shorter) window silently excludes people who bought *before*
// adding to the cart. It must not be saved.
func TestValidateRule_NaiveAbandonedCart_Blocked(t *testing.T) {
	for _, purchaseDays := range []int{30, 14} {
		r := Rule{Match: "all", Conditions: []Condition{
			{Kind: "event", EventType: "cart_add", WithinDays: 30},
			{Kind: "event", EventType: "purchase", WithinDays: purchaseDays, Negate: true},
		}}
		if res := Validate(r, DefaultCatalog()); !hasIssue(res.Errors, "conditions", "use_not_followed_by") {
			t.Fatalf("purchase window %d: expected use_not_followed_by error, got errors %v", purchaseDays, codes(res.Errors))
		}
	}
}

// A longer "no purchase" window is a different audience (lapsed buyers who
// still add to cart) that the sequence form cannot express; it is allowed
// with a hint.
func TestValidateRule_LongerNoPurchaseWindow_AllowedWithHint(t *testing.T) {
	r := Rule{Match: "all", Conditions: []Condition{
		{Kind: "event", EventType: "cart_add", WithinDays: 30},
		{Kind: "event", EventType: "purchase", WithinDays: 90, Negate: true},
	}}
	res := Validate(r, DefaultCatalog())
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors %v", codes(res.Errors))
	}
	if !hasIssue(res.Warnings, "conditions", "abandoned_cart_hint") {
		t.Fatalf("expected abandoned_cart_hint warning, got %v", codes(res.Warnings))
	}
	any := Rule{Match: "any", Conditions: r.Conditions}
	if res := Validate(any, DefaultCatalog()); len(res.Errors) != 0 {
		t.Fatalf("match any is not the abandoned-cart pattern, got %v", codes(res.Errors))
	}
}

func TestParseRule_RejectsUnknownKeys(t *testing.T) {
	_, err := ParseRule([]byte(`{"match":"all","conditions":[],"sql":"DELETE FROM users"}`))
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
	_, err = ParseRule([]byte(`{"match":"all","conditions":[{"kind":"event","event_type":"purchase","within_days":30,"raw":"x"}]}`))
	if err == nil {
		t.Fatal("expected error for unknown condition key")
	}
	if _, err := ParseRule([]byte(`{"match":"all","conditions":[]}`)); err != nil {
		t.Fatalf("valid shape rejected: %v", err)
	}
}

// A place with an identifier of another level (a GeoNames id on a country)
// is a validation error, not a crash and not silently ignored.
func TestValidateRule_Location_IdentifierOfAnotherLevel(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Validate panicked: %v", r)
		}
	}()
	for _, pl := range []PlaceRef{
		{Level: "country", GeonameID: 745044},
		{Level: "city", Name: "Paris", Code: "FR"},
	} {
		r := Rule{Match: "all", Conditions: []Condition{{Kind: "location", Operator: "in", Places: []PlaceRef{pl}}}}
		if res := Validate(r, DefaultCatalog()); !hasIssue(res.Errors, "conditions[0].places[0]", "invalid_identifier") {
			t.Errorf("%+v: expected invalid_identifier, got %v", pl, codes(res.Errors))
		}
	}
}
