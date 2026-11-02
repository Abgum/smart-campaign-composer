// Package geo is the local location catalog: countries (ISO 3166-1
// alpha-2), first-level administrative regions such as Turkish provinces
// (ISO 3166-2) and cities/settlements (GeoNames geonameId).
//
// A province and a city are different levels: "İstanbul" the province
// (TR-34) contains users who do not live in "İstanbul" the city (745044).
// Names and ids coming from the model are never trusted as is; Resolve maps
// them to exactly one catalog entry or explains why it cannot.
package geo

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"smart-campaign-composer/backend/internal/trtext"
)

// Level of a place.
type Level string

const (
	LevelCountry Level = "country"
	LevelAdmin1  Level = "admin1"
	LevelCity    Level = "city"
)

// Country is an ISO 3166-1 country.
type Country struct {
	Code     string   `json:"code"`
	Name     string   `json:"name"`
	AltNames []string `json:"alt_names,omitempty"`
}

// Admin1 is a first-level administrative region (ISO 3166-2).
type Admin1 struct {
	Code     string   `json:"code"`
	Country  string   `json:"country"`
	Name     string   `json:"name"`
	AltNames []string `json:"alt_names,omitempty"`
}

// City is a settlement identified by its GeoNames id.
type City struct {
	GeonameID int64    `json:"geoname_id"`
	Name      string   `json:"name"`
	Country   string   `json:"country"`
	Admin1    string   `json:"admin1,omitempty"`
	AltNames  []string `json:"alt_names,omitempty"`
}

// Place is a resolved location with its full hierarchy.
type Place struct {
	Level     Level  `json:"level"`
	Country   string `json:"country"`
	Admin1    string `json:"admin1,omitempty"`
	GeonameID int64  `json:"geoname_id,omitempty"`
	Name      string `json:"name"`
}

// Ref is an unresolved reference as the model sends it.
type Ref struct {
	Level     Level
	Name      string
	Code      string
	GeonameID int64
	Country   string // code or name, narrows the search
}

// ResolveError explains why a reference did not resolve to exactly one place.
// Codes: invalid_level, missing_identifier, unknown_place, ambiguous_place,
// place_mismatch.
type ResolveError struct {
	Code       string
	Message    string
	Candidates []Place
}

func (e *ResolveError) Error() string { return e.Code + ": " + e.Message }

// Catalog is the location catalog.
type Catalog struct {
	source    string
	countries map[string]Country
	admin1    map[string]Admin1
	cities    map[int64]City
	admin1s   []Admin1 // by code
	cityList  []City   // by country, id
}

//go:embed catalog.json
var catalogJSON []byte

var defaultCatalog = sync.OnceValue(func() *Catalog {
	c, err := Load(catalogJSON)
	if err != nil {
		panic("geo: embedded catalog is invalid: " + err.Error())
	}
	return c
})

// Default returns the embedded catalog. It needs no network access.
func Default() *Catalog { return defaultCatalog() }

var (
	countryCodeRe = regexp.MustCompile(`^[A-Z]{2}$`)
	admin1CodeRe  = regexp.MustCompile(`^[A-Z]{2}-[A-Z0-9]{1,3}$`)
)

// Load parses a catalog and checks its structure.
func Load(data []byte) (*Catalog, error) {
	var f struct {
		Source    string    `json:"source"`
		Countries []Country `json:"countries"`
		Admin1    []Admin1  `json:"admin1"`
		Cities    []City    `json:"cities"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("geo catalog: %w", err)
	}
	c := &Catalog{source: f.Source, countries: map[string]Country{}, admin1: map[string]Admin1{}, cities: map[int64]City{}}
	for _, co := range f.Countries {
		if !countryCodeRe.MatchString(co.Code) || co.Name == "" {
			return nil, fmt.Errorf("geo catalog: invalid country %+v", co)
		}
		if _, dup := c.countries[co.Code]; dup {
			return nil, fmt.Errorf("geo catalog: duplicate country %s", co.Code)
		}
		c.countries[co.Code] = co
	}
	for _, a := range f.Admin1 {
		if !admin1CodeRe.MatchString(a.Code) || a.Name == "" || !strings.HasPrefix(a.Code, a.Country+"-") {
			return nil, fmt.Errorf("geo catalog: invalid region %+v", a)
		}
		if _, ok := c.countries[a.Country]; !ok {
			return nil, fmt.Errorf("geo catalog: region %s has unknown country %s", a.Code, a.Country)
		}
		if _, dup := c.admin1[a.Code]; dup {
			return nil, fmt.Errorf("geo catalog: duplicate region %s", a.Code)
		}
		c.admin1[a.Code] = a
		c.admin1s = append(c.admin1s, a)
	}
	for _, ci := range f.Cities {
		if ci.GeonameID <= 0 || ci.Name == "" {
			return nil, fmt.Errorf("geo catalog: invalid city %+v", ci)
		}
		if _, ok := c.countries[ci.Country]; !ok {
			return nil, fmt.Errorf("geo catalog: city %d has unknown country %s", ci.GeonameID, ci.Country)
		}
		if ci.Admin1 != "" {
			a, ok := c.admin1[ci.Admin1]
			if !ok || a.Country != ci.Country {
				return nil, fmt.Errorf("geo catalog: city %d has invalid region %s", ci.GeonameID, ci.Admin1)
			}
		}
		if _, dup := c.cities[ci.GeonameID]; dup {
			return nil, fmt.Errorf("geo catalog: duplicate geoname id %d", ci.GeonameID)
		}
		c.cities[ci.GeonameID] = ci
		c.cityList = append(c.cityList, ci)
	}
	slices.SortFunc(c.admin1s, func(a, b Admin1) int { return strings.Compare(a.Code, b.Code) })
	slices.SortFunc(c.cityList, func(a, b City) int {
		if n := strings.Compare(a.Country, b.Country); n != 0 {
			return n
		}
		return int(a.GeonameID - b.GeonameID)
	})
	return c, nil
}

// Source names where the data comes from.
func (c *Catalog) Source() string { return c.source }

// Places returns countries and settlements in a stable order for the
// marketer's structured audience editor. Administrative regions remain in
// the catalog for existing rules and location metadata, but are not choices.
func (c *Catalog) Places() []Place {
	out := make([]Place, 0, len(c.countries)+len(c.cityList))
	codes := make([]string, 0, len(c.countries))
	for code := range c.countries {
		codes = append(codes, code)
	}
	slices.Sort(codes)
	for _, code := range codes {
		co := c.countries[code]
		out = append(out, Place{Level: LevelCountry, Country: code, Name: co.Name})
	}
	for _, ci := range c.cityList {
		out = append(out, cityPlace(ci))
	}
	return out
}

// Admin1In lists the regions of a country, by code.
func (c *Catalog) Admin1In(country string) []Admin1 {
	var out []Admin1
	for _, a := range c.admin1s {
		if a.Country == country {
			out = append(out, a)
		}
	}
	return out
}

// Country looks up a country by ISO code.
func (c *Catalog) Country(code string) (Country, bool) { co, ok := c.countries[code]; return co, ok }

// Admin1 looks up a region by ISO 3166-2 code.
func (c *Catalog) Admin1(code string) (Admin1, bool) { a, ok := c.admin1[code]; return a, ok }

// City looks up a city by GeoNames id.
func (c *Catalog) City(id int64) (City, bool) { ci, ok := c.cities[id]; return ci, ok }

// Label is a human-readable name with its hierarchy, e.g.
// "Paris (Île-de-France, FR)".
func (c *Catalog) Label(p Place) string {
	switch p.Level {
	case LevelCity:
		if a, ok := c.admin1[p.Admin1]; ok {
			return fmt.Sprintf("%s (%s, %s)", p.Name, a.Name, p.Country)
		}
		return fmt.Sprintf("%s (%s)", p.Name, p.Country)
	case LevelAdmin1:
		return fmt.Sprintf("%s (%s)", p.Name, p.Country)
	}
	return p.Name
}

var levelLabel = map[Level]string{LevelCountry: "ülke", LevelAdmin1: "il/bölge", LevelCity: "şehir"}

// Resolve maps a reference to exactly one place.
func (c *Catalog) Resolve(r Ref) (Place, error) {
	if _, ok := levelLabel[r.Level]; !ok {
		return Place{}, &ResolveError{Code: "invalid_level", Message: `level "country", "admin1" ya da "city" olmalı`}
	}
	r.Name, r.Code, r.Country = strings.TrimSpace(r.Name), strings.ToUpper(strings.TrimSpace(r.Code)), strings.TrimSpace(r.Country)
	if err := identifiersFit(r); err != nil {
		return Place{}, err
	}
	if r.Name == "" && r.Code == "" && r.GeonameID == 0 {
		return Place{}, &ResolveError{Code: "missing_identifier", Message: "yer için name, code ya da geoname_id gerekli"}
	}
	country := ""
	if r.Country != "" {
		cc, ok := c.countryCode(r.Country)
		if !ok {
			return Place{}, &ResolveError{Code: "unknown_place", Message: fmt.Sprintf("%q katalogda bilinen bir ülke değil", r.Country)}
		}
		country = cc
	}
	switch r.Level {
	case LevelCountry:
		return c.resolveCountry(r, country)
	case LevelAdmin1:
		return c.resolveAdmin1(r, country)
	}
	return c.resolveCity(r, country)
}

// identifiersFit rejects identifiers that belong to another level: a
// country takes an ISO 3166-1 alpha-2 code, a region an ISO 3166-2 code, a
// city a GeoNames id. Nothing is ignored silently.
func identifiersFit(r Ref) *ResolveError {
	bad := func(msg string) *ResolveError { return &ResolveError{Code: "invalid_identifier", Message: msg} }
	switch r.Level {
	case LevelCountry:
		if r.GeonameID != 0 {
			return bad("country düzeyinde geoname_id kullanılamaz; ISO 3166-1 alpha-2 code (örn. \"TR\") ya da name gönder")
		}
		if r.Code != "" && !countryCodeRe.MatchString(r.Code) {
			return bad(fmt.Sprintf("country code ISO 3166-1 alpha-2 olmalı (örn. \"TR\"), %q değil", r.Code))
		}
	case LevelAdmin1:
		if r.GeonameID != 0 {
			return bad("admin1 düzeyinde geoname_id kullanılamaz; ISO 3166-2 code (örn. \"TR-34\") ya da name gönder")
		}
		if r.Code != "" && !admin1CodeRe.MatchString(r.Code) {
			return bad(fmt.Sprintf("admin1 code ISO 3166-2 olmalı (örn. \"TR-34\"), %q değil", r.Code))
		}
	case LevelCity:
		if r.Code != "" {
			return bad("city düzeyinde code kullanılamaz; geoname_id ya da name gönder, ülkeyi country alanında belirt")
		}
	}
	return nil
}

// countryCode resolves a country given as ISO code or as a name ("FR",
// "Fransa", "UK").
func (c *Catalog) countryCode(s string) (string, bool) {
	if u := strings.ToUpper(s); countryCodeRe.MatchString(u) {
		if _, ok := c.countries[u]; ok {
			return u, true
		}
	}
	for _, co := range c.countries {
		if nameMatches(s, co.Name, co.AltNames) {
			return co.Code, true
		}
	}
	return "", false
}

func (c *Catalog) resolveCountry(r Ref, filter string) (Place, error) {
	var byCode, byName *Country
	if r.Code != "" {
		co, ok := c.countries[r.Code]
		if !ok {
			return Place{}, unknown(r.Code, LevelCountry, "")
		}
		byCode = &co
	}
	if r.Name != "" {
		var matches []Country
		for _, co := range c.countries {
			if nameMatches(r.Name, co.Name, co.AltNames) {
				matches = append(matches, co)
			}
		}
		if len(matches) != 1 {
			names := make([]string, 0, len(c.countries))
			for _, co := range c.countries {
				names = append(names, co.Name)
			}
			return Place{}, unknown(r.Name, LevelCountry, suggest(r.Name, names))
		}
		byName = &matches[0]
	}
	co := byCode
	if co == nil {
		co = byName
	}
	if co == nil {
		return Place{}, &ResolveError{Code: "missing_identifier", Message: "ülke için code ya da name gerekli"}
	}
	if byCode != nil && byName != nil && byCode.Code != byName.Code {
		return Place{}, mismatch(fmt.Sprintf("code %s ile ad %q farklı ülkeler", r.Code, r.Name))
	}
	if filter != "" && co.Code != filter {
		return Place{}, mismatch(fmt.Sprintf("%s, country %s ile uyuşmuyor", co.Code, filter))
	}
	return Place{Level: LevelCountry, Country: co.Code, Name: co.Name}, nil
}

func (c *Catalog) resolveAdmin1(r Ref, filter string) (Place, error) {
	var picked *Admin1
	if r.Code != "" {
		a, ok := c.admin1[r.Code]
		if !ok {
			return Place{}, unknown(r.Code, LevelAdmin1, "")
		}
		if filter != "" && a.Country != filter {
			return Place{}, mismatch(fmt.Sprintf("%s, country %s içinde değil", a.Code, filter))
		}
		if r.Name != "" && !nameMatches(r.Name, a.Name, a.AltNames) {
			return Place{}, mismatch(fmt.Sprintf("code %s (%s) ile ad %q uyuşmuyor", a.Code, a.Name, r.Name))
		}
		picked = &a
	} else {
		var matches []Admin1
		var names []string
		for _, a := range c.admin1s {
			if filter != "" && a.Country != filter {
				continue
			}
			names = append(names, a.Name)
			if nameMatches(r.Name, a.Name, a.AltNames) {
				matches = append(matches, a)
			}
		}
		switch len(matches) {
		case 0:
			return Place{}, unknown(r.Name, LevelAdmin1, suggest(r.Name, names))
		case 1:
			picked = &matches[0]
		default:
			cands := make([]Place, len(matches))
			for i, a := range matches {
				cands[i] = Place{Level: LevelAdmin1, Country: a.Country, Admin1: a.Code, Name: a.Name}
			}
			return Place{}, c.ambiguous(r.Name, cands)
		}
	}
	return Place{Level: LevelAdmin1, Country: picked.Country, Admin1: picked.Code, Name: picked.Name}, nil
}

func (c *Catalog) resolveCity(r Ref, filter string) (Place, error) {
	var picked *City
	if r.GeonameID != 0 {
		ci, ok := c.cities[r.GeonameID]
		if !ok {
			return Place{}, unknown(fmt.Sprint(r.GeonameID), LevelCity, "")
		}
		if filter != "" && ci.Country != filter {
			return Place{}, mismatch(fmt.Sprintf("geoname_id %d (%s) country %s içinde değil", ci.GeonameID, ci.Name, filter))
		}
		if r.Name != "" && !nameMatches(r.Name, ci.Name, ci.AltNames) {
			return Place{}, mismatch(fmt.Sprintf("geoname_id %d (%s) ile ad %q uyuşmuyor", ci.GeonameID, ci.Name, r.Name))
		}
		picked = &ci
	} else {
		if r.Name == "" {
			return Place{}, &ResolveError{Code: "missing_identifier", Message: "şehir için name ya da geoname_id gerekli"}
		}
		var matches []City
		var names []string
		for _, ci := range c.cityList {
			if filter != "" && ci.Country != filter {
				continue
			}
			names = append(names, ci.Name)
			if nameMatches(r.Name, ci.Name, ci.AltNames) {
				matches = append(matches, ci)
			}
		}
		switch len(matches) {
		case 0:
			return Place{}, unknown(r.Name, LevelCity, suggest(r.Name, names))
		case 1:
			picked = &matches[0]
		default:
			cands := make([]Place, len(matches))
			for i, ci := range matches {
				cands[i] = cityPlace(ci)
			}
			return Place{}, c.ambiguous(r.Name, cands)
		}
	}
	return cityPlace(*picked), nil
}

func cityPlace(ci City) Place {
	return Place{Level: LevelCity, Country: ci.Country, Admin1: ci.Admin1, GeonameID: ci.GeonameID, Name: ci.Name}
}

func nameMatches(q, name string, alts []string) bool {
	f := trtext.Fold(q)
	if f == trtext.Fold(name) {
		return true
	}
	for _, a := range alts {
		if f == trtext.Fold(a) {
			return true
		}
	}
	return false
}

func unknown(q string, level Level, suggestion string) *ResolveError {
	msg := fmt.Sprintf("%q katalogda bilinen bir %s değil", q, levelLabel[level])
	if suggestion != "" {
		msg += fmt.Sprintf("; %q mı demek istediniz?", suggestion)
	}
	return &ResolveError{Code: "unknown_place", Message: msg}
}

func mismatch(msg string) *ResolveError {
	return &ResolveError{Code: "place_mismatch", Message: msg}
}

func (c *Catalog) ambiguous(q string, cands []Place) *ResolveError {
	labels := make([]string, len(cands))
	for i, p := range cands {
		labels[i] = c.Label(p)
	}
	return &ResolveError{Code: "ambiguous_place", Candidates: cands,
		Message: fmt.Sprintf("%q birden fazla yerle eşleşiyor: %s. Kullanıcıya hangisini kastettiğini sor; birini kendin seçme.",
			q, strings.Join(labels, ", "))}
}

// suggest returns the closest name within edit distance 2.
func suggest(q string, names []string) string {
	f := trtext.Fold(q)
	best, bestDist := "", 3
	for _, n := range names {
		if d := levenshtein(f, trtext.Fold(n)); d < bestDist {
			best, bestDist = n, d
		}
	}
	return best
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// Problem is a catalog city that does not match the GeoNames dump.
type Problem struct {
	GeonameID int64
	Name      string
	Problem   string
}

// VerifyGeoNames checks every catalog city against a GeoNames dump in the
// standard tab-separated format (cities15000.txt, allCountries.txt, …): the
// id must exist, the country code must match and one of the catalog names
// must be among GeoNames' name, ASCII name or alternate names.
func VerifyGeoNames(c *Catalog, dump io.Reader) ([]Problem, error) {
	type row struct {
		country string
		names   []string
	}
	rows := map[int64]row{}
	sc := bufio.NewScanner(dump)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20) // alternate-name lists can be long
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 9 {
			return nil, fmt.Errorf("geonames dump: malformed line %q", line)
		}
		id, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("geonames dump: bad id %q", f[0])
		}
		if _, ok := c.cities[id]; !ok {
			continue
		}
		names := []string{f[1], f[2]}
		if f[3] != "" {
			names = append(names, strings.Split(f[3], ",")...)
		}
		rows[id] = row{country: f[8], names: names}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	var problems []Problem
	for _, ci := range c.cityList {
		r, ok := rows[ci.GeonameID]
		switch {
		case !ok:
			problems = append(problems, Problem{ci.GeonameID, ci.Name, "missing from the GeoNames dump"})
		case r.country != ci.Country:
			problems = append(problems, Problem{ci.GeonameID, ci.Name,
				fmt.Sprintf("country: catalog %s, GeoNames %s", ci.Country, r.country)})
		default:
			matched := false
			for _, n := range r.names {
				if nameMatches(n, ci.Name, ci.AltNames) {
					matched = true
					break
				}
			}
			if !matched {
				problems = append(problems, Problem{ci.GeonameID, ci.Name,
					fmt.Sprintf("name: catalog %q is not among GeoNames names (%s)", ci.Name, r.names[0])})
			}
		}
	}
	slices.SortFunc(problems, func(a, b Problem) int { return int(a.GeonameID - b.GeonameID) })
	return problems, nil
}
