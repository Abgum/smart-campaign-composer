package segment_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/segment"
	"smart-campaign-composer/backend/internal/testutil"
)

var fixedNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type fixtureUser struct {
	name         string
	country      string // ISO 3166-1 alpha-2, default TR
	admin1       string // ISO 3166-2, default TR-34
	cityID       int64  // GeoNames id, default 745044 (İstanbul); see noCity
	noCity       bool
	birth        string
	language     string // default tr
	noLanguage   bool   // no preference recorded
	totalSpent   string
	createdAt    time.Time
	consentSMS   bool
	consentEmail bool
	noPhone      bool
}

func insertUser(t *testing.T, db *testutil.DB, u fixtureUser) int64 {
	t.Helper()
	if u.country == "" {
		u.country = "TR"
	}
	if u.admin1 == "" {
		u.admin1 = "TR-34"
	}
	var cityID *int64
	if !u.noCity {
		if u.cityID == 0 {
			u.cityID = 745044
		}
		cityID = &u.cityID
	}
	if u.birth == "" {
		u.birth = "1990-01-01"
	}
	if u.language == "" {
		u.language = "tr"
	}
	var lang *string
	if !u.noLanguage {
		lang = &u.language
	}
	if u.totalSpent == "" {
		u.totalSpent = "0"
	}
	if u.createdAt.IsZero() {
		u.createdAt = fixedNow.AddDate(-1, 0, 0)
	}
	var phone *string
	if !u.noPhone {
		p := "+90555" + u.name
		phone = &p
	}
	var id int64
	err := db.Pool.QueryRow(context.Background(), `
		INSERT INTO users (first_name, last_name, email, phone, country_code, admin1_code, city_geoname_id,
		                   language, birth_date, total_spent, consent_sms, consent_email, created_at)
		VALUES ($1, 'Test', $2, $3, $4, $5, $6, $7, $8, $9::numeric, $10, $11, $12) RETURNING id`,
		u.name, u.name+"@example.com", phone, u.country, u.admin1, cityID, lang, u.birth,
		u.totalSpent, u.consentSMS, u.consentEmail, u.createdAt).Scan(&id)
	if err != nil {
		t.Fatalf("insert user %s: %v", u.name, err)
	}
	return id
}

func insertEvent(t *testing.T, db *testutil.DB, userID int64, typ string, at time.Time) {
	t.Helper()
	if _, err := db.Pool.Exec(context.Background(),
		`INSERT INTO user_events (user_id, type, occurred_at) VALUES ($1, $2, $3)`, userID, typ, at); err != nil {
		t.Fatalf("insert event: %v", err)
	}
}

func matchingNames(t *testing.T, db *testutil.DB, r segment.Rule) []string {
	t.Helper()
	c, err := segment.Compile(r, segment.DefaultCatalog(), fixedNow)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	rows, err := db.Pool.Query(context.Background(),
		"SELECT u.first_name FROM users u WHERE "+c.Where+" ORDER BY u.first_name", c.Args...)
	if err != nil {
		t.Fatalf("query: %v\nSQL: %s", err, c.Where)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

func days(n int) time.Time { return fixedNow.AddDate(0, 0, -n) }

// The brief's example "sepetine ürün ekleyip alışveriş yapmayan" must be a
// sequence: a purchase *before* the cart add must not exclude the user.
func TestAudience_AbandonedCart(t *testing.T) {
	db := testutil.NewDB(t)
	a := insertUser(t, db, fixtureUser{name: "A"})
	insertEvent(t, db, a, "cart_add", days(5))

	b := insertUser(t, db, fixtureUser{name: "B"})
	insertEvent(t, db, b, "purchase", days(25))
	insertEvent(t, db, b, "cart_add", days(5))

	c := insertUser(t, db, fixtureUser{name: "C"})
	insertEvent(t, db, c, "cart_add", days(20))
	insertEvent(t, db, c, "purchase", days(10))

	d := insertUser(t, db, fixtureUser{name: "D"})
	insertEvent(t, db, d, "cart_add", days(40))

	e := insertUser(t, db, fixtureUser{name: "E"})
	insertEvent(t, db, e, "cart_add", days(3))
	insertEvent(t, db, e, "purchase", days(3)) // same instant counts as purchased

	insertUser(t, db, fixtureUser{name: "F"}) // no events

	sequence := segment.Rule{Match: "all", Conditions: []segment.Condition{
		{Kind: "event", EventType: "cart_add", WithinDays: 30, NotFollowedBy: "purchase"},
	}}
	if got, want := matchingNames(t, db, sequence), []string{"A", "B"}; !slices.Equal(got, want) {
		t.Fatalf("not_followed_by matched %v, want %v", got, want)
	}

	// The naive reading is rejected by validation; evaluate its two halves
	// separately to show what it would have selected.
	naive := segment.Rule{Match: "all", Conditions: []segment.Condition{
		{Kind: "event", EventType: "cart_add", WithinDays: 30},
		{Kind: "event", EventType: "purchase", WithinDays: 30, Negate: true},
	}}
	if _, err := segment.Compile(naive, segment.DefaultCatalog(), fixedNow); err == nil {
		t.Fatal("the naive abandoned-cart rule must not compile")
	}
	added := matchingNames(t, db, rule("all", segment.Condition{Kind: "event", EventType: "cart_add", WithinDays: 30}))
	noPurchase := matchingNames(t, db, rule("all", segment.Condition{Kind: "event", EventType: "purchase", WithinDays: 30, Negate: true}))
	var both []string
	for _, n := range added {
		if slices.Contains(noPurchase, n) {
			both = append(both, n)
		}
	}
	if want := []string{"A"}; !slices.Equal(both, want) {
		t.Fatalf("naive reading matches %v, want %v (it loses B)", both, want)
	}
}

func TestAudience_AttributeOperators(t *testing.T) {
	db := testutil.NewDB(t)
	insertUser(t, db, fixtureUser{name: "Ist30", birth: "1996-06-01", totalSpent: "100.50"})
	insertUser(t, db, fixtureUser{name: "Izm40", admin1: "TR-35", cityID: 311046, birth: "1986-06-01", totalSpent: "100.49", language: "en"})
	insertUser(t, db, fixtureUser{name: "Ank20", admin1: "TR-06", cityID: 323786, birth: "2006-06-01", totalSpent: "500", createdAt: days(10)})
	buyer := insertUser(t, db, fixtureUser{name: "Buyer", admin1: "TR-16", cityID: 750269})
	insertEvent(t, db, buyer, "purchase", days(1))
	insertEvent(t, db, buyer, "purchase", days(2))
	insertEvent(t, db, buyer, "purchase", days(50))

	cases := []struct {
		name string
		rule segment.Rule
		want []string
	}{
		{"age between", rule("all", attr("age", "between", "25", "35")), []string{"Ist30"}},
		{"age gte", rule("all", attr("age", "gte", "36")), []string{"Buyer", "Izm40"}},
		{"total_spent gte decimal", rule("all", attr("total_spent", "gte", "100.50")), []string{"Ank20", "Ist30"}},
		{"language eq", rule("all", attr("language", "eq", "en")), []string{"Izm40"}},
		{"signup_days_ago lte", rule("all", attr("signup_days_ago", "lte", "30")), []string{"Ank20"}},
		{"match any", rule("any", attr("age", "lte", "20"), attr("language", "eq", "en")), []string{"Ank20", "Izm40"}},
		{"purchase min_count 2 in 7 days", rule("all",
			segment.Condition{Kind: "event", EventType: "purchase", WithinDays: 7, MinCount: 2}), []string{"Buyer"}},
		{"purchase min_count 3 in 7 days", rule("all",
			segment.Condition{Kind: "event", EventType: "purchase", WithinDays: 7, MinCount: 3}), nil},
		{"negated purchase", rule("all",
			segment.Condition{Kind: "event", EventType: "purchase", WithinDays: 7, Negate: true}), []string{"Ank20", "Ist30", "Izm40"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchingNames(t, db, tc.rule); !slices.Equal(got, tc.want) {
				t.Fatalf("matched %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCompile_ParamsOnly(t *testing.T) {
	r := rule("all",
		loc("in", segment.PlaceRef{Level: "city", Name: "İstanbul"}, segment.PlaceRef{Level: "city", Name: "Paris", Country: "FR"}),
		attr("total_spent", "gte", "250.75"),
		segment.Condition{Kind: "event", EventType: "cart_add", WithinDays: 30, NotFollowedBy: "purchase"},
	)
	c, err := segment.Compile(r, segment.DefaultCatalog(), fixedNow)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, literal := range []string{"İstanbul", "TR-34", "Paris", "2988507", "'FR'", "250.75", "cart_add", "purchase", "30"} {
		if strings.Contains(c.Where, literal) {
			t.Errorf("value %q was inlined into SQL; values must be bind parameters:\n%s", literal, c.Where)
		}
	}
	if len(c.Args) == 0 {
		t.Fatal("expected bind arguments")
	}
}

func TestCompile_RejectsInvalidRule(t *testing.T) {
	bad := rule("all", attr("password", "eq", "x' OR '1'='1"))
	if _, err := segment.Compile(bad, segment.DefaultCatalog(), fixedNow); err == nil {
		t.Fatal("Compile must refuse a rule that fails validation")
	}
}

func TestCount_ReachableByChannel(t *testing.T) {
	db := testutil.NewDB(t)
	insertUser(t, db, fixtureUser{name: "Both", consentSMS: true, consentEmail: true})
	insertUser(t, db, fixtureUser{name: "SmsOnly", consentSMS: true})
	insertUser(t, db, fixtureUser{name: "SmsNoPhone", consentSMS: true, noPhone: true})
	insertUser(t, db, fixtureUser{name: "EmailOnly", consentEmail: true})
	insertUser(t, db, fixtureUser{name: "None"})
	insertUser(t, db, fixtureUser{name: "OtherCity", admin1: "TR-06", cityID: 323786, consentSMS: true, consentEmail: true})

	got, err := segment.Count(context.Background(), db.Pool, rule("all", loc("in", segment.PlaceRef{Level: "city", Name: "İstanbul"})),
		segment.DefaultCatalog(), fixedNow)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	want := segment.Audience{Total: 5, SMSReachable: 2, EmailReachable: 2}
	if got != want {
		t.Fatalf("audience = %+v, want %+v", got, want)
	}
}

// The language filter reads the primary language of the recorded
// preference, as language.Normalize does when a recipient's variant is
// picked: "en-GB" is English, so it is in an English audience and gets the
// English content. No preference is in no language's audience.
func TestAudience_LanguageMatchesPrimaryTag(t *testing.T) {
	db := testutil.NewDB(t)
	insertUser(t, db, fixtureUser{name: "Ena", language: "en-GB"})
	insertUser(t, db, fixtureUser{name: "Enb", language: "en_US"})
	insertUser(t, db, fixtureUser{name: "Enc", language: " EN "})
	insertUser(t, db, fixtureUser{name: "Fra", language: "fr-CA"})
	insertUser(t, db, fixtureUser{name: "Tra", language: "tr"})
	insertUser(t, db, fixtureUser{name: "Yok", noLanguage: true})
	for _, tc := range []struct {
		name string
		rule segment.Rule
		want []string
	}{
		{"eq", rule("all", attr("language", "eq", "en")), []string{"Ena", "Enb", "Enc"}},
		{"in", rule("all", attr("language", "in", "en", "fr")), []string{"Ena", "Enb", "Enc", "Fra"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchingNames(t, db, tc.rule); !slices.Equal(got, tc.want) {
				t.Fatalf("matched %v, want %v", got, tc.want)
			}
		})
	}
}

func rule(match string, cs ...segment.Condition) segment.Rule {
	return segment.Rule{Match: match, Conditions: cs}
}

func loc(op string, places ...segment.PlaceRef) segment.Condition {
	return segment.Condition{Kind: "location", Operator: op, Places: places}
}

func attr(field, op string, values ...string) segment.Condition {
	return segment.Condition{Kind: "attribute", Field: field, Operator: op, Values: values}
}

// City (GeoNames id) and country targeting are distinct. Same-named cities
// in different countries remain distinct, and users with no known city can
// still match a country condition.
func TestAudience_Location(t *testing.T) {
	db := testutil.NewDB(t)
	insertUser(t, db, fixtureUser{name: "IstMerkez"})                                    // TR-34, İstanbul city
	insertUser(t, db, fixtureUser{name: "IstKirsal", noCity: true})                      // TR-34, no city
	insertUser(t, db, fixtureUser{name: "Izmit", admin1: "TR-41", cityID: 745028})       // Kocaeli, İzmit
	insertUser(t, db, fixtureUser{name: "KocaeliKirsal", admin1: "TR-41", noCity: true}) // Kocaeli, no city
	insertUser(t, db, fixtureUser{name: "Ankara", admin1: "TR-06", cityID: 323786})
	insertUser(t, db, fixtureUser{name: "ParisFR", country: "FR", admin1: "FR-IDF", cityID: 2988507})
	insertUser(t, db, fixtureUser{name: "ParisTX", country: "US", admin1: "US-TX", cityID: 4717560})

	p := func(level, name, country string) segment.PlaceRef {
		return segment.PlaceRef{Level: level, Name: name, Country: country}
	}
	cases := []struct {
		name string
		cond segment.Condition
		want []string
	}{
		{"city İstanbul", loc("in", p("city", "İstanbul", "")), []string{"IstMerkez"}},
		{"city İzmit", loc("in", p("city", "İzmit", "")), []string{"Izmit"}},
		{"Paris in France only", loc("in", p("city", "Paris", "FR")), []string{"ParisFR"}},
		{"Paris in the US only", loc("in", p("city", "Paris", "US")), []string{"ParisTX"}},
		{"country Türkiye", loc("in", p("country", "Türkiye", "")), []string{"Ankara", "IstKirsal", "IstMerkez", "Izmit", "KocaeliKirsal"}},
		{"set of places", loc("in", p("city", "Paris", "FR"), p("city", "Ankara", "")), []string{"Ankara", "ParisFR"}},
		{"not in city İstanbul keeps users without a city", loc("not_in", p("city", "İstanbul", "")),
			[]string{"Ankara", "IstKirsal", "Izmit", "KocaeliKirsal", "ParisFR", "ParisTX"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchingNames(t, db, rule("all", tc.cond)); !slices.Equal(got, tc.want) {
				t.Fatalf("matched %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCompile_AmbiguousOrUnknownPlaceRefused(t *testing.T) {
	for _, place := range []segment.PlaceRef{{Level: "city", Name: "Paris"}, {Level: "city", Name: "Gotham"}} {
		if _, err := segment.Compile(rule("all", loc("in", place)), segment.DefaultCatalog(), fixedNow); err == nil {
			t.Errorf("%+v must not compile", place)
		}
	}
}

// The facts a recipient's language is worked out from, counted for the
// rule's audience only: the recorded preference (none kept apart from any
// value), the country and the reach on each channel.
func TestLanguageBuckets(t *testing.T) {
	db := testutil.NewDB(t)
	insertUser(t, db, fixtureUser{name: "Tr1", consentSMS: true, consentEmail: true})
	insertUser(t, db, fixtureUser{name: "Tr2", consentSMS: true, consentEmail: true})
	insertUser(t, db, fixtureUser{name: "NoPrefTr", noLanguage: true, consentEmail: true})
	insertUser(t, db, fixtureUser{name: "NoPrefFr", noLanguage: true, country: "FR", admin1: "FR-IDF", cityID: 2988507,
		consentSMS: true, noPhone: true})
	insertUser(t, db, fixtureUser{name: "De", language: "de", consentSMS: true})
	insertUser(t, db, fixtureUser{name: "Old", language: "en", birth: "1950-01-01", consentSMS: true}) // not in the rule

	buckets, err := segment.LanguageBuckets(context.Background(), db.Pool, rule("all", attr("age", "lte", "60")),
		segment.DefaultCatalog(), fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, b := range buckets {
		pref := "-"
		if b.Preference != nil {
			pref = *b.Preference
		}
		got = append(got, fmt.Sprintf("%s %s sms=%v email=%v ×%d", pref, b.Country, b.SMS, b.Email, b.Count))
	}
	slices.Sort(got)
	want := []string{
		"- FR sms=false email=false ×1",
		"- TR sms=false email=true ×1",
		"de TR sms=true email=false ×1",
		"tr TR sms=true email=true ×2",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("buckets =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
