package geo

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func resolveErr(t *testing.T, err error) *ResolveError {
	t.Helper()
	var re *ResolveError
	if !errors.As(err, &re) {
		t.Fatalf("expected *ResolveError, got %v", err)
	}
	return re
}

func TestCatalog_Integrity(t *testing.T) {
	c := Default()
	if got := len(c.Admin1In("TR")); got != 81 {
		t.Fatalf("Türkiye has 81 provinces, catalog has %d", got)
	}
	if c.Source() == "" || !strings.Contains(c.Source(), "GeoNames") {
		t.Fatalf("catalog must name its source, got %q", c.Source())
	}
	// Load must reject structural errors.
	bad := []string{
		`{"countries":[{"code":"TR","name":"Türkiye"}],"admin1":[{"code":"TR-34","country":"FR","name":"x"}],"cities":[]}`,
		`{"countries":[{"code":"TR","name":"Türkiye"}],"admin1":[{"code":"TR-34","country":"TR","name":"x"}],"cities":[{"geoname_id":1,"name":"a","country":"TR","admin1":"TR-99"}]}`,
		`{"countries":[{"code":"TR","name":"Türkiye"}],"admin1":[],"cities":[{"geoname_id":1,"name":"a","country":"TR"},{"geoname_id":1,"name":"b","country":"TR"}]}`,
		`{"countries":[{"code":"TUR","name":"Türkiye"}],"admin1":[],"cities":[]}`,
	}
	for i, b := range bad {
		if _, err := Load([]byte(b)); err == nil {
			t.Errorf("bad catalog %d accepted", i)
		}
	}
}

func TestPlaces_OffersCountriesAndSettlementsWithoutRegions(t *testing.T) {
	places := Default().Places()
	for _, tc := range []struct {
		country, name, level string
	}{
		{"TR", "Türkiye", "country"},
		{"TR", "İstanbul", "city"},
		{"DE", "Berlin", "city"},
		{"TR", "İzmit", "city"},
		{"FR", "Paris", "city"},
	} {
		levels := []Level{}
		for _, p := range places {
			if p.Country == tc.country && p.Name == tc.name {
				levels = append(levels, p.Level)
			}
		}
		if len(levels) != 1 || string(levels[0]) != tc.level {
			t.Errorf("%s/%s choice levels = %v, want [%s]", tc.country, tc.name, levels, tc.level)
		}
	}
	for _, p := range places {
		if p.Level == LevelAdmin1 {
			t.Errorf("region must not be offered as a new audience choice: %+v", p)
		}
	}
}

func TestResolve_TurkishProvinceAndCityAreDifferentLevels(t *testing.T) {
	c := Default()
	p, err := c.Resolve(Ref{Level: LevelAdmin1, Name: "istanbul"})
	if err != nil || p.Level != LevelAdmin1 || p.Admin1 != "TR-34" || p.Country != "TR" || p.Name != "İstanbul" || p.GeonameID != 0 {
		t.Fatalf("province = %+v, %v", p, err)
	}
	p, err = c.Resolve(Ref{Level: LevelCity, Name: "İstanbul"})
	if err != nil || p.Level != LevelCity || p.GeonameID != 745044 || p.Admin1 != "TR-34" || p.Country != "TR" {
		t.Fatalf("city = %+v, %v", p, err)
	}
	p, err = c.Resolve(Ref{Level: LevelAdmin1, Code: "TR-41"})
	if err != nil || p.Name != "Kocaeli" {
		t.Fatalf("by code = %+v, %v", p, err)
	}
}

func TestResolve_Countries(t *testing.T) {
	c := Default()
	for _, ref := range []Ref{{Level: LevelCountry, Code: "TR"}, {Level: LevelCountry, Name: "Türkiye"}, {Level: LevelCountry, Name: "turkey"}} {
		if p, err := c.Resolve(ref); err != nil || p.Country != "TR" || p.Level != LevelCountry {
			t.Errorf("%+v → %+v, %v", ref, p, err)
		}
	}
}

func TestResolve_SameCityNameInDifferentCountries(t *testing.T) {
	c := Default()
	_, err := c.Resolve(Ref{Level: LevelCity, Name: "Paris"})
	re := resolveErr(t, err)
	if re.Code != "ambiguous_place" || len(re.Candidates) != 2 {
		t.Fatalf("Paris without a country must be ambiguous, got %+v", re)
	}
	countries := re.Candidates[0].Country + "," + re.Candidates[1].Country
	if countries != "FR,US" {
		t.Fatalf("candidates = %+v", re.Candidates)
	}

	p, err := c.Resolve(Ref{Level: LevelCity, Name: "Paris", Country: "FR"})
	if err != nil || p.GeonameID != 2988507 || p.Admin1 != "FR-IDF" {
		t.Fatalf("Paris, FR = %+v, %v", p, err)
	}
	p, err = c.Resolve(Ref{Level: LevelCity, Name: "London", Country: "Kanada"})
	if err != nil || p.Country != "CA" || p.GeonameID != 6058560 {
		t.Fatalf("London, Kanada = %+v, %v", p, err)
	}
}

func TestResolve_IDsAreVerified(t *testing.T) {
	c := Default()
	if p, err := c.Resolve(Ref{Level: LevelCity, GeonameID: 2988507}); err != nil || p.Name != "Paris" || p.Country != "FR" {
		t.Fatalf("by id = %+v, %v", p, err)
	}
	cases := []struct {
		name string
		ref  Ref
		code string
	}{
		{"id not in catalog", Ref{Level: LevelCity, GeonameID: 999999999}, "unknown_place"},
		{"province code not in catalog", Ref{Level: LevelAdmin1, Code: "TR-99"}, "unknown_place"},
		{"country code not in catalog", Ref{Level: LevelCountry, Code: "XX"}, "unknown_place"},
		{"id and name disagree", Ref{Level: LevelCity, GeonameID: 745044, Name: "Ankara"}, "place_mismatch"},
		{"id and country disagree", Ref{Level: LevelCity, GeonameID: 2988507, Country: "US"}, "place_mismatch"},
		{"code and name disagree", Ref{Level: LevelAdmin1, Code: "TR-34", Name: "İzmir"}, "place_mismatch"},
		{"province of another country", Ref{Level: LevelAdmin1, Name: "İstanbul", Country: "FR"}, "unknown_place"},
		{"no identifier", Ref{Level: LevelCity}, "missing_identifier"},
		{"bad level", Ref{Level: "district", Name: "Kadıköy"}, "invalid_level"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Resolve(tc.ref)
			if re := resolveErr(t, err); re.Code != tc.code {
				t.Fatalf("code = %s, want %s (%s)", re.Code, tc.code, re.Message)
			}
		})
	}
}

func TestResolve_UnknownCitySuggests(t *testing.T) {
	c := Default()
	_, err := c.Resolve(Ref{Level: LevelCity, Name: "Gotham"})
	if re := resolveErr(t, err); re.Code != "unknown_place" || len(re.Candidates) != 0 {
		t.Fatalf("Gotham → %+v", re)
	}
	_, err = c.Resolve(Ref{Level: LevelAdmin1, Name: "Istambul"})
	re := resolveErr(t, err)
	if re.Code != "unknown_place" || !strings.Contains(re.Message, "İstanbul") {
		t.Fatalf("Istambul → %+v", re)
	}
}

func safeResolve(t *testing.T, c *Catalog, ref Ref) (p Place, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Resolve(%+v) panicked: %v", ref, r)
		}
	}()
	return c.Resolve(ref)
}

// Each level has its own identifiers: country → ISO 3166-1 code, region →
// ISO 3166-2 code, city → GeoNames id. An identifier of another level must
// be rejected, never ignored.
func TestResolve_IdentifierNotForLevel(t *testing.T) {
	c := Default()
	cases := []struct {
		name string
		ref  Ref
	}{
		{"country with only a geoname_id", Ref{Level: LevelCountry, GeonameID: 745044}},
		{"country with code and geoname_id", Ref{Level: LevelCountry, Code: "TR", GeonameID: 745044}},
		{"country with a region code", Ref{Level: LevelCountry, Code: "TR-34"}},
		{"region with only a geoname_id", Ref{Level: LevelAdmin1, GeonameID: 745044}},
		{"region with name and geoname_id", Ref{Level: LevelAdmin1, Name: "İstanbul", GeonameID: 745044}},
		{"region with a country code", Ref{Level: LevelAdmin1, Code: "TR"}},
		{"city with only a region code", Ref{Level: LevelCity, Code: "TR-34"}},
		{"city with a country code as code", Ref{Level: LevelCity, Code: "FR", Name: "Paris"}},
		{"city with code and geoname_id", Ref{Level: LevelCity, Code: "TR-34", GeonameID: 745044}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := safeResolve(t, c, tc.ref)
			if err == nil {
				t.Fatalf("resolved to %+v; an identifier of another level must be an error", p)
			}
			if re := resolveErr(t, err); re.Code != "invalid_identifier" {
				t.Fatalf("code = %s (%s), want invalid_identifier", re.Code, re.Message)
			}
		})
	}
}

// VerifyGeoNames checks catalog cities against a GeoNames dump (TSV).
func TestVerifyGeoNames(t *testing.T) {
	c, err := Load([]byte(`{"countries":[{"code":"TR","name":"Türkiye"},{"code":"FR","name":"Fransa"}],
	 "admin1":[{"code":"TR-34","country":"TR","name":"İstanbul"},{"code":"FR-IDF","country":"FR","name":"Île-de-France"}],
	 "cities":[
	  {"geoname_id":745044,"name":"İstanbul","country":"TR","admin1":"TR-34"},
	  {"geoname_id":2988507,"name":"Paris","country":"TR","admin1":"TR-34"},
	  {"geoname_id":111,"name":"Lyon","country":"FR","admin1":"FR-IDF"},
	  {"geoname_id":222,"name":"Nowhere","country":"FR","admin1":"FR-IDF"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	row := func(id, name, ascii, alts, cc string) string {
		return strings.Join([]string{id, name, ascii, alts, "0", "0", "P", "PPL", cc, "", "", "", "", "", "0", "", "0", "UTC", "2024-01-01"}, "\t")
	}
	dump := strings.Join([]string{
		row("745044", "Istanbul", "Istanbul", "Constantinople,İstanbul", "TR"),
		row("2988507", "Paris", "Paris", "", "FR"),
		row("111", "Marseille", "Marseille", "", "FR"),
	}, "\n") + "\n"

	problems, err := VerifyGeoNames(c, strings.NewReader(dump))
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]string{}
	for _, p := range problems {
		got[p.GeonameID] = p.Problem
	}
	if len(got) != 3 || got[745044] != "" {
		t.Fatalf("problems = %+v", problems)
	}
	for id, want := range map[int64]string{2988507: "country", 111: "name", 222: "missing"} {
		if !strings.Contains(got[id], want) {
			t.Errorf("geoname %d: problem %q, want it to mention %q", id, got[id], want)
		}
	}
}

// Runs only when a GeoNames dump is provided, e.g.
//
//	make verify-geonames GEONAMES_DUMP=/path/to/cities15000.txt
func TestCatalogMatchesGeoNamesDump(t *testing.T) {
	path := os.Getenv("GEONAMES_DUMP")
	if path == "" {
		t.Skip("GEONAMES_DUMP not set; catalog geonameIds are not verified in this run")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	problems, err := VerifyGeoNames(Default(), f)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("geoname %d (%s): %s", p.GeonameID, p.Name, p.Problem)
	}
}
