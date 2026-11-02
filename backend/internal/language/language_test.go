package language_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/language"
)

func ptr(s string) *string { return &s }

func variant(lang string) language.Variant {
	return language.Variant{Language: lang, SMS: &content.SMS{Text: "Merhaba {{first_name}}"}, Email: &content.Email{Subject: "Konu", Body: "Gövde"}}
}

// perRecipient has TR, EN and FR variants and a mapping for TR, FR and US.
func perRecipient(decisions ...language.Decision) language.Settings {
	return language.Settings{
		Mode:     language.ModePerRecipient,
		Variants: []language.Variant{variant("tr"), variant("en"), variant("fr")},
		Countries: []language.CountryLanguage{
			{Country: "TR", Language: "tr"}, {Country: "FR", Language: "fr"}, {Country: "US", Language: "en"},
		},
		Decisions: decisions,
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{"en-GB": "en", " FR ": "fr", "pt_BR": "pt", "tr": "tr", "": "", "  ": ""} {
		if got := language.Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
	if !slices.Equal(language.Supported, []string{"tr", "en", "fr"}) {
		t.Fatalf("supported = %v", language.Supported)
	}
	if !language.IsSupported("fr") || language.IsSupported("de") {
		t.Fatal("IsSupported")
	}
}

func TestResolve_PreferenceBeatsCountry(t *testing.T) {
	s := perRecipient()
	cases := []struct {
		pref    *string
		country string
		want    string
	}{
		{ptr("en"), "FR", "en"},    // an English preference in France
		{ptr("fr"), "TR", "fr"},    // a French preference in Türkiye
		{ptr("en-GB"), "GB", "en"}, // a regional code, in a country with no mapping
	}
	for _, tc := range cases {
		if r := s.Resolve(tc.pref, tc.country); r.Language != tc.want || r.Unresolved || r.Excluded {
			t.Errorf("Resolve(%s, %s) = %+v, want %s", *tc.pref, tc.country, r, tc.want)
		}
	}
}

func TestResolve_NoPreferenceUsesTheCountry(t *testing.T) {
	s := perRecipient()
	if r := s.Resolve(nil, "FR"); r.Language != "fr" {
		t.Fatalf("no preference in France = %+v, want fr", r)
	}
	if r := s.Resolve(ptr(""), "US"); r.Language != "en" {
		t.Fatalf("empty preference in the US = %+v, want en", r)
	}
}

// An unsupported preference counts as none: the country mapping the draft
// proposes decides, and only a country without a mapping waits.
func TestResolve_UnsupportedPreferenceFollowsTheCountry(t *testing.T) {
	s := perRecipient()
	if r := s.Resolve(ptr("de"), "TR"); r.Language != "tr" || r.Unresolved || r.Group != "" {
		t.Fatalf("German preference in Türkiye = %+v, want tr from the mapping", r)
	}
	if r := s.Resolve(ptr("de"), "DE"); !r.Unresolved || r.Group != "country:DE" {
		t.Fatalf("German preference in Germany = %+v, want unresolved country:DE", r)
	}
}

func TestResolve_CountryWithoutALanguageIsUnresolved(t *testing.T) {
	s := perRecipient()
	for _, c := range []string{"DE", "CA"} {
		if r := s.Resolve(nil, c); !r.Unresolved || r.Group != "country:"+c {
			t.Errorf("no preference in %s = %+v, want unresolved country:%s", c, r, c)
		}
	}
}

func TestResolve_LanguageWithoutAVariantIsUnresolved(t *testing.T) {
	s := perRecipient()
	s.Variants = []language.Variant{variant("tr"), variant("fr")}
	if r := s.Resolve(ptr("en"), "TR"); !r.Unresolved || r.Group != "variant:en" {
		t.Fatalf("English preference without an English variant = %+v, want unresolved variant:en", r)
	}
}

func TestResolve_DecisionsApplyToTheirGroup(t *testing.T) {
	s := perRecipient(
		language.Decision{Group: "country:DE", Action: language.ActionAssign, Language: "en"},
		language.Decision{Group: "country:CA", Action: language.ActionExclude},
	)
	if r := s.Resolve(ptr("de"), "DE"); r.Language != "en" || r.Group != "country:DE" || r.Unresolved {
		t.Fatalf("decided Germany = %+v, want en", r)
	}
	if r := s.Resolve(nil, "CA"); !r.Excluded || r.Group != "country:CA" || r.Language != "" {
		t.Fatalf("excluded Canada = %+v", r)
	}
	// A group with no decision still waits.
	if r := s.Resolve(nil, "ES"); !r.Unresolved || r.Group != "country:ES" {
		t.Fatalf("Spain without a decision = %+v", r)
	}
}

func TestResolve_SingleContentGoesToEveryone(t *testing.T) {
	s := language.Settings{Mode: language.ModeSingle, Language: "en"}
	for _, pref := range []*string{nil, ptr("tr"), ptr("de")} {
		if r := s.Resolve(pref, "TR"); r.Language != "en" || r.Unresolved || r.Excluded {
			t.Fatalf("single English content = %+v", r)
		}
	}
}

func TestSummarize(t *testing.T) {
	s := perRecipient(language.Decision{Group: "country:CA", Action: language.ActionExclude})
	got := language.Summarize(s, nil, nil, []language.Bucket{
		{Preference: ptr("tr"), Country: "TR", SMS: true, Email: true, Count: 10},
		{Preference: nil, Country: "TR", SMS: false, Email: true, Count: 2},
		{Preference: ptr("en"), Country: "FR", SMS: true, Email: false, Count: 3},
		{Preference: nil, Country: "FR", SMS: true, Email: true, Count: 1},
		{Preference: ptr("de"), Country: "TR", SMS: true, Email: true, Count: 4},
		{Preference: ptr("de"), Country: "DE", SMS: false, Email: true, Count: 1},
		{Preference: nil, Country: "DE", SMS: false, Email: true, Count: 2},
		{Preference: nil, Country: "CA", SMS: true, Email: true, Count: 1},
		{Preference: nil, Country: "ES", SMS: false, Email: false, Count: 3}, // reachable on no channel
	})
	want := language.Summary{
		Mode: language.ModePerRecipient,
		Languages: map[string]language.Reach{
			"tr": {Total: 16, SMS: 14, Email: 16},
			"en": {Total: 3, SMS: 3, Email: 0},
			"fr": {Total: 1, SMS: 1, Email: 1},
		},
		Unresolved: []language.GroupCount{{Group: "country:DE", Count: 3}},
		Excluded:   []language.GroupCount{{Group: "country:CA", Count: 1}},
		Countries:  map[string]int{"TR": 6, "FR": 1, "DE": 3, "CA": 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summary =\n%+v\nwant\n%+v", got, want)
	}
}

// A recipient the campaign cannot reach on any of its channels gets no
// message whatever the language, so they never wait for a decision.
func TestSummarize_UnreachableRecipientsDoNotWait(t *testing.T) {
	s := perRecipient()
	for i := range s.Variants {
		s.Variants[i].Email = nil // an SMS campaign
	}
	got := language.Summarize(s, nil, nil, []language.Bucket{
		{Preference: nil, Country: "ES", SMS: false, Email: true, Count: 2}, // e-mail only: not on this campaign
		{Preference: nil, Country: "IT", SMS: true, Email: false, Count: 1},
	})
	if want := []language.GroupCount{{Group: "country:IT", Count: 1}}; !slices.Equal(got.Unresolved, want) {
		t.Fatalf("waiting = %+v, want %+v", got.Unresolved, want)
	}
}

func TestValidate(t *testing.T) {
	known := func(c string) bool { return slices.Contains([]string{"TR", "FR", "US", "GB", "CA", "DE"}, c) }
	if is := language.Validate(perRecipient(), known); len(is) != 0 {
		t.Fatalf("valid setup: %+v", is)
	}
	if is := language.Validate(language.Settings{Mode: language.ModeSingle, Language: "en"}, known); len(is) != 0 {
		t.Fatalf("valid single: %+v", is)
	}
	type tc struct {
		name string
		s    language.Settings
		path string
		code string
	}
	edit := func(f func(*language.Settings)) language.Settings {
		s := perRecipient()
		f(&s)
		return s
	}
	cases := []tc{
		{"single without a language", language.Settings{Mode: language.ModeSingle}, "content_language", "required"},
		{"single in German", language.Settings{Mode: language.ModeSingle, Language: "de"}, "content_language", "unsupported_language"},
		{"unknown mode", language.Settings{Mode: "both"}, "language_mode", "invalid_value"},
		{"no variants", edit(func(s *language.Settings) { s.Variants = nil }), "per_recipient.variants", "required"},
		{"German variant", edit(func(s *language.Settings) { s.Variants[1].Language = "de" }),
			"per_recipient.variants[1].language", "unsupported_language"},
		{"two French variants", edit(func(s *language.Settings) { s.Variants[0].Language = "fr" }),
			"per_recipient.variants[2].language", "duplicate_variant"},
		{"variant SMS too long", edit(func(s *language.Settings) { s.Variants[2].SMS = &content.SMS{Text: string(make([]rune, 200))} }),
			"per_recipient.variants[2].sms.text", "too_long"},
		{"variant without e-mail", edit(func(s *language.Settings) { s.Variants[1].Email = nil }),
			"per_recipient.variants[1]", "channel_mismatch"},
		{"unknown country", edit(func(s *language.Settings) { s.Countries[0].Country = "XX" }),
			"per_recipient.country_languages[0].country", "unknown_country"},
		{"country to German", edit(func(s *language.Settings) { s.Countries[1].Language = "de" }),
			"per_recipient.country_languages[1].language", "unsupported_language"},
		{"country twice", edit(func(s *language.Settings) { s.Countries[2].Country = "TR" }),
			"per_recipient.country_languages[2].country", "duplicate_country"},
		{"bad group", edit(func(s *language.Settings) {
			s.Decisions = []language.Decision{{Group: "city:Paris", Action: language.ActionExclude}}
		}), "per_recipient.decisions[0].group", "invalid_group"},
		{"bad action", edit(func(s *language.Settings) {
			s.Decisions = []language.Decision{{Group: "preference:de", Action: "translate"}}
		}), "per_recipient.decisions[0].action", "invalid_value"},
		{"assign without a variant", edit(func(s *language.Settings) {
			s.Variants = s.Variants[:2]
			s.Decisions = []language.Decision{{Group: "preference:de", Action: language.ActionAssign, Language: "fr"}}
		}), "per_recipient.decisions[0].language", "missing_variant"},
		{"assign to German", edit(func(s *language.Settings) {
			s.Decisions = []language.Decision{{Group: "country:DE", Action: language.ActionAssign, Language: "de"}}
		}), "per_recipient.decisions[0].language", "unsupported_language"},
		{"group decided twice", edit(func(s *language.Settings) {
			s.Decisions = []language.Decision{
				{Group: "country:CA", Action: language.ActionExclude},
				{Group: "country:CA", Action: language.ActionAssign, Language: "en"},
			}
		}), "per_recipient.decisions[1].group", "duplicate_decision"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			is := language.Validate(c.s, known)
			if !slices.ContainsFunc(is, func(i language.Issue) bool { return i.Path == c.path && i.Code == c.code }) {
				t.Fatalf("issues = %+v, want %s at %s", is, c.code, c.path)
			}
			for _, i := range is {
				if i.Message == "" {
					t.Fatalf("issue without a message: %+v", i)
				}
			}
		})
	}
}

// A decision may be written only for a group the user was already asked
// about: one the base version had waiting or had decided.
func TestKnownGroups(t *testing.T) {
	s := perRecipient(language.Decision{Group: "country:CA", Action: language.ActionExclude})
	sum := language.Summary{Unresolved: []language.GroupCount{{Group: "preference:de", Count: 4}}}
	got := language.KnownGroups(s, sum)
	if want := []string{"country:CA", "preference:de"}; !slices.Equal(got, want) {
		t.Fatalf("known groups = %v, want %v", got, want)
	}
}

// Countries counts, per country, the recipients whose language the country
// mapping decides: no recorded preference, or one that is not supported. A
// supported preference is never decided by the mapping, so it is not counted.
func TestSummarize_CountriesCountWhoTheMappingDecides(t *testing.T) {
	got := language.Summarize(perRecipient(), nil, nil, []language.Bucket{
		{Preference: ptr("tr"), Country: "TR", SMS: true, Count: 50},
		{Preference: ptr("en-GB"), Country: "TR", SMS: true, Count: 4},
		{Preference: nil, Country: "TR", SMS: true, Count: 7},
		{Preference: ptr("de"), Country: "TR", SMS: true, Count: 2},
		{Preference: ptr("fr"), Country: "CA", SMS: true, Count: 3},
	})
	if want := map[string]int{"TR": 9}; !reflect.DeepEqual(got.Countries, want) {
		t.Fatalf("countries = %v, want %v", got.Countries, want)
	}
	// Counted with nobody to count is an empty map, not a missing one: the
	// interface tells "zero" from "not counted" (versions saved before).
	none := language.Summarize(perRecipient(), nil, nil, []language.Bucket{{Preference: ptr("tr"), Country: "TR", SMS: true, Count: 5}})
	if b, _ := json.Marshal(none); !strings.Contains(string(b), `"countries":{}`) {
		t.Fatalf("summary JSON = %s, want an empty countries object", b)
	}
	single := language.Summarize(language.Settings{Mode: language.ModeSingle, Language: "en"}, &content.SMS{Text: "Hi"}, nil,
		[]language.Bucket{{Preference: nil, Country: "TR", SMS: true, Count: 7}})
	if single.Countries != nil {
		t.Fatalf("one content for everyone has no mapping to count: %v", single.Countries)
	}
}

// The mapping decides only for those the campaign can message: someone
// without consent or contact on its channels gets nothing in any language,
// so (as they never wait for a decision) a mapping does not count them.
func TestSummarize_CountriesCountOnlyWhoCanBeMessaged(t *testing.T) {
	s := perRecipient()
	for i := range s.Variants {
		s.Variants[i].Email = nil // an SMS campaign
	}
	got := language.Summarize(s, nil, nil, []language.Bucket{
		{Preference: nil, Country: "TR", SMS: true, Email: true, Count: 5},
		{Preference: nil, Country: "TR", SMS: false, Email: true, Count: 3}, // e-mail only: not on this campaign
		{Preference: ptr("de"), Country: "TR", SMS: false, Email: false, Count: 2},
		{Preference: nil, Country: "FR", SMS: false, Email: false, Count: 4},
	})
	if want := map[string]int{"TR": 5}; !reflect.DeepEqual(got.Countries, want) {
		t.Fatalf("countries = %v, want %v", got.Countries, want)
	}
}

// Every count is of the recipients the draft can reach when it is saved:
// on at least one of its channels, each person once in a language's total
// however many of them reach the person, and per channel apart. One
// audience under an SMS draft, an e-mail draft and a draft on both; one
// content for everyone counts by its own channels.
func TestSummarize_CountsWhoTheDraftCanReach(t *testing.T) {
	audience := []language.Bucket{
		{Preference: ptr("tr"), Country: "TR", SMS: true, Email: true, Count: 10},
		{Preference: ptr("tr"), Country: "TR", SMS: true, Count: 4},
		{Preference: ptr("tr"), Country: "TR", Email: true, Count: 3},
		{Preference: ptr("tr"), Country: "TR", Count: 5}, // no consent or contact on either channel
		{Preference: nil, Country: "FR", SMS: true, Count: 2},
		{Preference: nil, Country: "FR", Email: true, Count: 1},
		{Preference: nil, Country: "CA", SMS: true, Email: true, Count: 1}, // no mapping: waits
		{Preference: nil, Country: "CA", Count: 2},
		{Preference: nil, Country: "DE", SMS: true, Count: 2}, // left out by a decision
		{Preference: nil, Country: "DE", Count: 3},
	}
	on := func(sms, email bool) language.Settings {
		s := perRecipient(language.Decision{Group: "country:DE", Action: language.ActionExclude})
		for i := range s.Variants {
			if !sms {
				s.Variants[i].SMS = nil
			}
			if !email {
				s.Variants[i].Email = nil
			}
		}
		return s
	}
	waitCA := []language.GroupCount{{Group: "country:CA", Count: 1}}
	outDE := []language.GroupCount{{Group: "country:DE", Count: 2}}
	cases := []struct {
		name      string
		got, want language.Summary
	}{
		{"SMS only", language.Summarize(on(true, false), nil, nil, audience), language.Summary{
			Mode:       language.ModePerRecipient,
			Languages:  map[string]language.Reach{"tr": {Total: 14, SMS: 14}, "fr": {Total: 2, SMS: 2}},
			Unresolved: waitCA, Excluded: outDE, Countries: map[string]int{"FR": 2, "CA": 1, "DE": 2},
		}},
		{"e-mail only", language.Summarize(on(false, true), nil, nil, audience), language.Summary{
			Mode:       language.ModePerRecipient,
			Languages:  map[string]language.Reach{"tr": {Total: 13, Email: 13}, "fr": {Total: 1, Email: 1}},
			Unresolved: waitCA, Countries: map[string]int{"FR": 1, "CA": 1},
		}},
		{"both channels, someone on both counted once", language.Summarize(on(true, true), nil, nil, audience), language.Summary{
			Mode:       language.ModePerRecipient,
			Languages:  map[string]language.Reach{"tr": {Total: 17, SMS: 14, Email: 13}, "fr": {Total: 3, SMS: 2, Email: 1}},
			Unresolved: waitCA, Excluded: outDE, Countries: map[string]int{"FR": 3, "CA": 1, "DE": 2},
		}},
		{"one English content for everyone, by SMS", language.Summarize(language.Settings{Mode: language.ModeSingle, Language: "en"},
			&content.SMS{Text: "Hi"}, nil, audience), language.Summary{
			Mode:      language.ModeSingle,
			Languages: map[string]language.Reach{"en": {Total: 19, SMS: 19}},
		}},
	}
	for _, c := range cases {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, c.got, c.want)
		}
	}
}
