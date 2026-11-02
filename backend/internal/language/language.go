// Package language works out the content language of each recipient of a
// campaign and checks a draft's language setup.
//
// A draft either sends one content to everyone (single: in the language the
// marketer asked for, or wrote in) or one variant per language
// (per_recipient). For a per-recipient draft Go, never the model, works out
// each recipient's language: a supported recorded preference first;
// otherwise (no preference, or one that is not supported) the language the
// draft maps the recipient's country to (the model proposes the mapping, the
// marketer can change it). A country without a mapping is not guessed: its
// recipients wait in country:<CC> until a decision assigns the group to a
// variant or leaves it out. A recipient whose language has no variant waits
// in variant:<code>. A recipient the campaign cannot reach on any of its
// channels gets no message, so never waits.
package language

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"smart-campaign-composer/backend/internal/content"
)

// Supported are the languages campaign content can be written in.
var Supported = []string{"tr", "en", "fr"}

// IsSupported tells whether content can be written in code.
func IsSupported(code string) bool { return slices.Contains(Supported, code) }

// Normalize gives a recorded preference as a primary language code: "en-GB"
// is "en", " FR " is "fr"; "" means none.
func Normalize(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if i := strings.IndexAny(code, "-_"); i >= 0 {
		code = code[:i]
	}
	return code
}

// Modes of a draft, and what a decision does to a group.
const (
	ModeSingle       = "single"
	ModePerRecipient = "per_recipient"

	ActionAssign  = "assign"
	ActionExclude = "exclude"
)

// Kinds of group a recipient waits in.
const (
	KindPreference = "preference" // an unsupported preference: versions before 2026-10-30 only
	KindCountry    = "country"    // no supported preference, and no language for the country
	KindVariant    = "variant"    // a language the draft has no variant for
)

// Settings is a version's language setup.
type Settings struct {
	Mode      string            `json:"mode"`
	Language  string            `json:"language,omitempty"` // single: the content's language
	Variants  []Variant         `json:"variants,omitempty"`
	Countries []CountryLanguage `json:"country_languages,omitempty"`
	Decisions []Decision        `json:"decisions,omitempty"`
}

// Variant is the content in one language.
type Variant struct {
	Language string         `json:"language"`
	SMS      *content.SMS   `json:"sms,omitempty"`
	Email    *content.Email `json:"email,omitempty"`
}

// CountryLanguage maps the recipients of a country who have no preference
// to a language.
type CountryLanguage struct {
	Country  string `json:"country"`
	Language string `json:"language"`
}

// Decision assigns a group of waiting recipients to a variant or leaves the
// group out.
type Decision struct {
	Group    string `json:"group"`
	Action   string `json:"action"`
	Language string `json:"language,omitempty"`
}

// Resolution is the content language of one recipient.
type Resolution struct {
	Language   string // the variant to send; "" when left out or waiting
	Group      string // the group a decision applied to, or the one waiting
	Excluded   bool
	Unresolved bool
}

// Variant returns the variant in lang.
func (s Settings) Variant(lang string) (Variant, bool) {
	i := slices.IndexFunc(s.Variants, func(v Variant) bool { return v.Language == lang })
	if i < 0 {
		return Variant{}, false
	}
	return s.Variants[i], true
}

func (s Settings) countryLanguage(country string) (string, bool) {
	i := slices.IndexFunc(s.Countries, func(c CountryLanguage) bool { return c.Country == country })
	if i < 0 {
		return "", false
	}
	return s.Countries[i].Language, true
}

func (s Settings) decision(group string) (Decision, bool) {
	i := slices.IndexFunc(s.Decisions, func(d Decision) bool { return d.Group == group })
	if i < 0 {
		return Decision{}, false
	}
	return s.Decisions[i], true
}

// Resolve works out the language of a recipient with this preference (nil
// or empty for none) living in country. Single content goes to everyone.
func (s Settings) Resolve(preference *string, country string) Resolution {
	if s.Mode != ModePerRecipient {
		return Resolution{Language: s.Language}
	}
	pref := ""
	if preference != nil {
		pref = Normalize(*preference)
	}
	var lang, group string
	if IsSupported(pref) {
		lang = pref
	} else if l, ok := s.countryLanguage(country); ok {
		lang = l
	} else {
		group = KindCountry + ":" + country
	}
	if lang != "" {
		if _, ok := s.Variant(lang); ok {
			return Resolution{Language: lang}
		}
		group = KindVariant + ":" + lang
	}
	d, ok := s.decision(group)
	if !ok {
		return Resolution{Group: group, Unresolved: true}
	}
	if d.Action == ActionExclude {
		return Resolution{Group: group, Excluded: true}
	}
	if _, ok := s.Variant(d.Language); !ok || d.Action != ActionAssign {
		return Resolution{Group: group, Unresolved: true}
	}
	return Resolution{Language: d.Language, Group: group}
}

// Bucket is a number of audience members with the same language facts.
type Bucket struct {
	Preference *string
	Country    string
	SMS, Email bool // reachable on the channel: consent and contact
	Count      int
}

// Reach counts the recipients a draft can reach: each once in Total, on
// however many of its channels, and per channel apart.
type Reach struct {
	Total int `json:"total"`
	SMS   int `json:"sms"`
	Email int `json:"email"`
}

// GroupCount is how many recipients are in a group.
type GroupCount struct {
	Group string `json:"group"`
	Count int    `json:"count"`
}

// Summary is how the recipients a draft can reach, when it is saved, split
// by content language; nobody it cannot reach counts anywhere. Countries
// counts, per country, those whose language the country mapping decides
// (no supported preference); it is empty, never missing, when nobody's is,
// and missing for one content for everyone.
type Summary struct {
	Mode       string           `json:"mode"`
	Languages  map[string]Reach `json:"languages"`
	Unresolved []GroupCount     `json:"unresolved,omitempty"`
	Excluded   []GroupCount     `json:"excluded,omitempty"`
	Countries  map[string]int   `json:"countries"`
}

// Channels are the channels the draft sends on; a per-recipient draft's
// variants share theirs. One content's channels are not in its settings,
// and it never leaves anyone waiting.
func (s Settings) Channels() (sms, email bool) {
	if s.Mode != ModePerRecipient || len(s.Variants) == 0 {
		return true, true
	}
	return s.Variants[0].SMS != nil, s.Variants[0].Email != nil
}

// Summarize splits the recipients a draft can reach by content language:
// those who can get a message on at least one of its channels. sms and
// email are the draft's content for everyone (nil on a channel it does not
// use, and for a per-recipient draft, whose variants carry its channels).
// A recipient the draft cannot reach waits for no decision and counts
// nowhere.
func Summarize(s Settings, sms *content.SMS, email *content.Email, buckets []Bucket) Summary {
	sum := Summary{Mode: s.Mode, Languages: map[string]Reach{}}
	unresolved, excluded := map[string]int{}, map[string]int{}
	onSMS, onEmail := sms != nil, email != nil
	if s.Mode == ModePerRecipient {
		onSMS, onEmail = s.Channels()
		sum.Countries = map[string]int{}
	}
	for _, b := range buckets {
		bySMS, byEmail := b.SMS && onSMS, b.Email && onEmail
		if !bySMS && !byEmail {
			continue
		}
		if s.Mode == ModePerRecipient && (b.Preference == nil || !IsSupported(Normalize(*b.Preference))) {
			sum.Countries[b.Country] += b.Count
		}
		r := s.Resolve(b.Preference, b.Country)
		switch {
		case r.Unresolved:
			unresolved[r.Group] += b.Count
		case r.Excluded:
			excluded[r.Group] += b.Count
		default:
			reach := sum.Languages[r.Language]
			reach.Total += b.Count
			if bySMS {
				reach.SMS += b.Count
			}
			if byEmail {
				reach.Email += b.Count
			}
			sum.Languages[r.Language] = reach
		}
	}
	sum.Unresolved, sum.Excluded = GroupCounts(unresolved), GroupCounts(excluded)
	return sum
}

// GroupCounts lists groups with their sizes, the largest first.
func GroupCounts(m map[string]int) []GroupCount {
	var out []GroupCount
	for g, n := range m {
		out = append(out, GroupCount{Group: g, Count: n})
	}
	slices.SortFunc(out, func(a, b GroupCount) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Group, b.Group))
	})
	return out
}

// KnownGroups are the groups a decision may be written for after this
// version: the ones it left waiting and the ones it decided. A group that
// appears later has not been put to the user yet.
func KnownGroups(s Settings, sum Summary) []string {
	var out []string
	for _, g := range sum.Unresolved {
		out = append(out, g.Group)
	}
	for _, d := range s.Decisions {
		out = append(out, d.Group)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Issue is a validation finding; Path is relative to the draft.
type Issue struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

var (
	countryCode    = regexp.MustCompile(`^[A-Z]{2}$`)
	preferenceCode = regexp.MustCompile(`^[a-z]{2,8}$`)
)

// Validate checks a language setup: language codes, the variants' content
// and channels, the country mapping (countryKnown tells a known country) and
// the decisions.
func Validate(s Settings, countryKnown func(string) bool) []Issue {
	supported := strings.Join(Supported, ", ")
	unsupported := func(path, code string) Issue {
		return Issue{Path: path, Code: "unsupported_language",
			Message: fmt.Sprintf("%q desteklenmiyor; desteklenen diller: %s", code, supported)}
	}
	var issues []Issue
	switch s.Mode {
	case ModeSingle:
		switch {
		case s.Language == "":
			issues = append(issues, Issue{Path: "content_language", Code: "required",
				Message: fmt.Sprintf("içerik dili gerekli; desteklenen diller: %s", supported)})
		case !IsSupported(s.Language):
			issues = append(issues, unsupported("content_language", s.Language))
		}
		return issues
	case ModePerRecipient:
	default:
		return []Issue{{Path: "language_mode", Code: "invalid_value", Message: "dil modu single ya da per_recipient olmalı"}}
	}

	if len(s.Variants) == 0 {
		issues = append(issues, Issue{Path: "per_recipient.variants", Code: "required", Message: "en az bir dil varyantı gerekli"})
	}
	seen := map[string]bool{}
	for i, v := range s.Variants {
		path := fmt.Sprintf("per_recipient.variants[%d]", i)
		switch {
		case !IsSupported(v.Language):
			issues = append(issues, unsupported(path+".language", v.Language))
		case seen[v.Language]:
			issues = append(issues, Issue{Path: path + ".language", Code: "duplicate_variant",
				Message: fmt.Sprintf("%s varyantı birden fazla", v.Language)})
		}
		seen[v.Language] = true
		for _, is := range content.ValidateDraft(v.SMS, v.Email) {
			p := path
			if is.Path != "" {
				p += "." + is.Path
			}
			issues = append(issues, Issue{Path: p, Code: is.Code, Message: is.Message})
		}
		if first := s.Variants[0]; i > 0 && ((v.SMS == nil) != (first.SMS == nil) || (v.Email == nil) != (first.Email == nil)) {
			issues = append(issues, Issue{Path: path, Code: "channel_mismatch",
				Message: fmt.Sprintf("%s varyantının kanalları ilk varyanttan farklı; her varyant aynı kanalları içermeli", v.Language)})
		}
	}

	mapped := map[string]bool{}
	for i, c := range s.Countries {
		path := fmt.Sprintf("per_recipient.country_languages[%d]", i)
		switch {
		case !countryCode.MatchString(c.Country) || !countryKnown(c.Country):
			issues = append(issues, Issue{Path: path + ".country", Code: "unknown_country",
				Message: fmt.Sprintf("bilinmeyen ülke kodu %q", c.Country)})
		case mapped[c.Country]:
			issues = append(issues, Issue{Path: path + ".country", Code: "duplicate_country",
				Message: fmt.Sprintf("%s ülkesi birden fazla eşlendi", c.Country)})
		}
		mapped[c.Country] = true
		if !IsSupported(c.Language) {
			issues = append(issues, unsupported(path+".language", c.Language))
		}
	}

	decided := map[string]bool{}
	for i, d := range s.Decisions {
		path := fmt.Sprintf("per_recipient.decisions[%d]", i)
		switch {
		case !validGroup(d.Group):
			issues = append(issues, Issue{Path: path + ".group", Code: "invalid_group",
				Message: fmt.Sprintf("geçersiz grup %q; preference:<dil>, country:<ülke> ya da variant:<dil> olmalı", d.Group)})
		case decided[d.Group]:
			issues = append(issues, Issue{Path: path + ".group", Code: "duplicate_decision",
				Message: fmt.Sprintf("%s grubu için birden fazla karar var", d.Group)})
		}
		decided[d.Group] = true
		switch d.Action {
		case ActionExclude:
		case ActionAssign:
			if !IsSupported(d.Language) {
				issues = append(issues, unsupported(path+".language", d.Language))
			} else if _, ok := s.Variant(d.Language); !ok {
				issues = append(issues, Issue{Path: path + ".language", Code: "missing_variant",
					Message: fmt.Sprintf("%s varyantı yok; grubun atandığı dilin varyantı olmalı", d.Language)})
			}
		default:
			issues = append(issues, Issue{Path: path + ".action", Code: "invalid_value",
				Message: "karar assign ya da exclude olmalı"})
		}
	}
	return issues
}

// validGroup tells a group key. Preference groups are no longer made; a
// decision for one in an older version is still a valid (unused) decision.
func validGroup(g string) bool {
	kind, value, _ := strings.Cut(g, ":")
	switch kind {
	case KindPreference:
		return preferenceCode.MatchString(value) && !IsSupported(value)
	case KindCountry:
		return countryCode.MatchString(value)
	case KindVariant:
		return IsSupported(value)
	}
	return false
}
