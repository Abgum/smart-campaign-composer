package seed_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/geo"
	"smart-campaign-composer/backend/internal/seed"
	"smart-campaign-composer/backend/internal/segment"
	"smart-campaign-composer/backend/internal/testutil"
)

var now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func TestGenerate_Deterministic(t *testing.T) {
	a := seed.Generate(300, 42, now)
	b := seed.Generate(300, 42, now)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same seed must produce identical data")
	}
	c := seed.Generate(300, 43, now)
	if reflect.DeepEqual(a.Users, c.Users) {
		t.Fatal("different seeds should produce different users")
	}
}

func TestGenerate_Properties(t *testing.T) {
	const n = 2000
	d := seed.Generate(n, 7, now)
	if len(d.Users) != n+seed.ForeignUsers {
		t.Fatalf("users = %d, want %d Turkish + %d foreign", len(d.Users), n, seed.ForeignUsers)
	}
	geoCat := geo.Default()
	emails, phones := map[string]bool{}, map[string]bool{}
	var sms, email, noPhone, english, noCity int
	purchases := map[int]int{}
	for _, e := range d.Events {
		if e.UserIndex < 0 || e.UserIndex >= len(d.Users) {
			t.Fatalf("event points to user %d", e.UserIndex)
		}
		if e.OccurredAt.After(now) {
			t.Fatalf("event in the future: %v", e.OccurredAt)
		}
		if e.Type == "purchase" {
			purchases[e.UserIndex]++
		}
	}
	for i, u := range d.Users {
		if u.Email == "" || emails[u.Email] {
			t.Fatalf("user %d: empty or duplicate email %q", i, u.Email)
		}
		emails[u.Email] = true
		if u.Phone == nil {
			noPhone++
		} else if phones[*u.Phone] {
			t.Fatalf("user %d: duplicate phone %q", i, *u.Phone)
		} else {
			phones[*u.Phone] = true
		}
		if (i < n) != (u.Country == "TR") {
			t.Fatalf("user %d: country %q; the first %d users are Turkish, the rest foreign", i, u.Country, n)
		}
		if a, ok := geoCat.Admin1(u.Admin1); !ok || a.Country != u.Country {
			t.Fatalf("user %d: region %q not in the local catalog for %s", i, u.Admin1, u.Country)
		}
		if u.CityGeonameID == nil {
			noCity++
		} else if c, ok := geoCat.City(*u.CityGeonameID); !ok || c.Admin1 != u.Admin1 || c.Country != u.Country {
			t.Fatalf("user %d: city %d not in the catalog or not in %s/%s", i, *u.CityGeonameID, u.Country, u.Admin1)
		}
		if u.ConsentSMS {
			sms++
		}
		if u.ConsentEmail {
			email++
		}
		if u.Language == "en" {
			english++
		}
		if u.TotalOrders != purchases[i] {
			t.Fatalf("user %d: total_orders %d but %d purchase events", i, u.TotalOrders, purchases[i])
		}
	}
	between := func(name string, got, lo, hi int) {
		if got < lo || got > hi {
			t.Errorf("%s = %d, want between %d and %d", name, got, lo, hi)
		}
	}
	between("sms consent", sms, n*60/100, n*80/100)
	between("email consent", email, n*70/100, n*90/100)
	between("missing phone", noPhone, 1, n*10/100)
	between("english speakers", english, 1, n*20/100)
	between("users with a province but no city", noCity, 1, n*10/100)
}

func TestInsert_LoadsOnceAndSupportsTheBriefSegment(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	d := seed.Generate(1000, 1, now)

	inserted, err := seed.Insert(ctx, db.Pool, d)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if !inserted {
		t.Fatal("first insert into an empty database must load data")
	}
	inserted, err = seed.Insert(ctx, db.Pool, d)
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if inserted {
		t.Fatal("second insert must be a no-op on a seeded database")
	}

	var users, events int
	if err := db.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM user_events)`).Scan(&users, &events); err != nil {
		t.Fatal(err)
	}
	if users != 1000+seed.ForeignUsers || events != len(d.Events) {
		t.Fatalf("users=%d events=%d, want %d and %d", users, events, 1000+seed.ForeignUsers, len(d.Events))
	}

	// The brief ("İstanbul'daki kullanıcılar") targets the province. Before
	// the location model, the same seed selected exactly 299 İstanbul users
	// and a brief audience of 51 (SMS 32, email 43); the new model must keep
	// that audience.
	ist := 0
	for _, u := range d.Users {
		if u.Admin1 == "TR-34" {
			ist++
		}
	}
	if ist != 299 {
		t.Fatalf("İstanbul province users = %d, want 299 as before the location model", ist)
	}
	brief := segment.Rule{Match: "all", Conditions: []segment.Condition{
		{Kind: "location", Operator: "in", Places: []segment.PlaceRef{{Level: "city", Name: "İstanbul"}}},
		{Kind: "event", EventType: "cart_add", WithinDays: 30, NotFollowedBy: "purchase"},
	}}
	a, err := segment.Count(ctx, db.Pool, brief, segment.DefaultCatalog(), now)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if want := (segment.Audience{Total: 50, SMSReachable: 31, EmailReachable: 42}); a != want {
		t.Fatalf("city-targeted brief audience = %+v, want %+v", a, want)
	}
}

// A few foreign users are fixed and do not depend on the seed, so the
// Turkish random stream (and every Turkish audience) is unchanged.
func TestGenerate_ForeignUsers(t *testing.T) {
	a, b := seed.Generate(50, 1, now), seed.Generate(50, 2, now)
	foreignA, foreignB := a.Users[50:], b.Users[50:]
	if !reflect.DeepEqual(foreignA, foreignB) {
		t.Fatal("foreign users must not depend on the seed")
	}
	byCity := map[int64]int{}
	for _, u := range foreignA {
		if u.Phone == nil || strings.HasPrefix(*u.Phone, "+90") || u.CityGeonameID == nil {
			t.Fatalf("foreign user %+v", u)
		}
		byCity[*u.CityGeonameID]++
	}
	want := map[int64]int{2988507: 3, 4717560: 2, 2643743: 2, 6058560: 1}
	if !reflect.DeepEqual(byCity, want) {
		t.Fatalf("foreign users by city = %v, want %v", byCity, want)
	}
}

// Same-named cities in different countries are different audiences in a real
// count against the seeded database.
func TestInsert_SameCityNameSplitsByCountry(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	if _, err := seed.Insert(ctx, db.Pool, seed.Generate(200, 1, now)); err != nil {
		t.Fatal(err)
	}
	place := func(p segment.PlaceRef) segment.Rule {
		return segment.Rule{Match: "all", Conditions: []segment.Condition{
			{Kind: "location", Operator: "in", Places: []segment.PlaceRef{p}}}}
	}
	count := func(r segment.Rule) int {
		t.Helper()
		a, err := segment.Count(ctx, db.Pool, r, segment.DefaultCatalog(), now)
		if err != nil {
			t.Fatalf("count %+v: %v", r, err)
		}
		return a.Total
	}
	cases := []struct {
		name string
		p    segment.PlaceRef
		want int
	}{
		{"Paris, France", segment.PlaceRef{Level: "city", Name: "Paris", Country: "FR"}, 3},
		{"Paris, US", segment.PlaceRef{Level: "city", Name: "Paris", Country: "US"}, 2},
		{"France", segment.PlaceRef{Level: "country", Code: "FR"}, 3},
		{"US", segment.PlaceRef{Level: "country", Name: "ABD"}, 2},
		{"London, UK", segment.PlaceRef{Level: "city", Name: "London", Country: "GB"}, 2},
		{"London, Canada", segment.PlaceRef{Level: "city", Name: "Londra", Country: "Kanada"}, 1},
	}
	for _, tc := range cases {
		if got := count(place(tc.p)); got != tc.want {
			t.Errorf("%s: audience = %d, want %d", tc.name, got, tc.want)
		}
	}
	if _, err := segment.Count(ctx, db.Pool, place(segment.PlaceRef{Level: "city", Name: "Paris"}), segment.DefaultCatalog(), now); err == nil {
		t.Error("Paris without a country must not be counted")
	}
	if got := count(place(segment.PlaceRef{Level: "country", Code: "TR"})); got != 200 {
		t.Errorf("Türkiye audience = %d, want the 200 Turkish users only", got)
	}
}

// The language users cover each way a recipient's language is worked out:
// no preference in a country with one supported language, in a country with
// two (Canada) and in one with none (Germany); a preference that beats the
// country; an unsupported preference; a regional code.
func TestLanguageUsers_CoverEveryLanguageCase(t *testing.T) {
	users := seed.LanguageUsers()
	cat := geo.Default()
	noPreference := map[string]bool{}
	var preferenceAbroad, unsupported, regional bool
	for _, u := range users {
		if _, ok := cat.Country(u.Country); !ok {
			t.Errorf("%s: country %s not in the geo catalog", u.Email, u.Country)
		}
		if _, ok := cat.Admin1(u.Admin1); !ok {
			t.Errorf("%s: region %s not in the geo catalog", u.Email, u.Admin1)
		}
		if c, ok := cat.City(*u.CityGeonameID); !ok || c.Country != u.Country || c.Admin1 != u.Admin1 {
			t.Errorf("%s: city %d does not match %s/%s", u.Email, *u.CityGeonameID, u.Country, u.Admin1)
		}
		if !strings.HasSuffix(u.Email, "@example.com") {
			t.Errorf("%s: not an example.com address", u.Email)
		}
		switch {
		case u.Language == nil:
			noPreference[u.Country] = true
		case *u.Language == "fr" && u.Country != "FR":
			preferenceAbroad = true
		case *u.Language == "de":
			unsupported = true
		case *u.Language == "en-GB":
			regional = true
		}
	}
	for _, c := range []string{"TR", "FR", "US", "CA", "DE"} {
		if !noPreference[c] {
			t.Errorf("no user without a preference in %s", c)
		}
	}
	if !preferenceAbroad || !unsupported || !regional {
		t.Errorf("preference abroad %v, unsupported preference %v, regional code %v; want all", preferenceAbroad, unsupported, regional)
	}
}

// The language users reach a database that is already seeded, once, and
// leave every existing user as it was; they fall into the brief's segment
// (added to the cart, did not buy).
func TestInsertLanguageUsers_AddsToASeededDatabaseOnce(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	if _, err := seed.Insert(ctx, db.Pool, seed.Generate(200, 1, now)); err != nil {
		t.Fatal(err)
	}
	existing := func() string {
		t.Helper()
		var s string
		if err := db.Pool.QueryRow(ctx, `SELECT string_agg(u::text, ' | ' ORDER BY u.id) FROM users u WHERE u.id <= $1`,
			200+seed.ForeignUsers).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := existing()

	added, err := seed.InsertLanguageUsers(ctx, db.Pool, now)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if want := len(seed.LanguageUsers()); added != want {
		t.Fatalf("added %d, want %d", added, want)
	}
	if again, err := seed.InsertLanguageUsers(ctx, db.Pool, now); err != nil || again != 0 {
		t.Fatalf("second run added %d (%v), want 0", again, err)
	}
	if after := existing(); after != before {
		t.Fatal("existing users changed")
	}
	var users, noPreference int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE language IS NULL) FROM users`).Scan(&users, &noPreference); err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, u := range seed.LanguageUsers() {
		if u.Language == nil {
			want++
		}
	}
	if users != 200+seed.ForeignUsers+added || noPreference != want {
		t.Fatalf("users=%d without preference=%d, want %d and %d", users, noPreference, 200+seed.ForeignUsers+added, want)
	}

	brief := segment.Rule{Match: "all", Conditions: []segment.Condition{
		{Kind: "event", EventType: "cart_add", WithinDays: 30, NotFollowedBy: "purchase"},
		{Kind: "attribute", Field: "language", Operator: "in", Values: []string{"fr"}},
	}}
	a, err := segment.Count(ctx, db.Pool, brief, segment.DefaultCatalog(), now)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if a.Total == 0 || a.EmailReachable != a.Total {
		t.Fatalf("French-preference abandoned carts = %+v, want the language users with e-mail", a)
	}
}
