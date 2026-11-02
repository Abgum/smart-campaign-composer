// Package i18n gives API messages in Turkish or English.
//
// A message in the code is written once, in either language; one with
// values is a fmt template (%d, %s, %q, %v, %w). The catalog pairs every
// such message with the other language (and, for a message the code writes
// in technical English, a Turkish and a plain English form). Translate finds
// the entry a message came from, takes its values back out and fills them
// into the wanted form. A message with no entry stays as it is, so a missing
// translation shows the original text rather than nothing.
package i18n

import (
	"net/http"
	"regexp"
	"strings"
)

// Lang is an answer language.
type Lang string

const (
	TR Lang = "tr" // the app's own language, and the default
	EN Lang = "en"
)

// FromRequest is the language a request asks for: ?lang= first, then the
// first Accept-Language tag. Anything but English gets Turkish.
func FromRequest(r *http.Request) Lang {
	if v := r.URL.Query().Get("lang"); v != "" {
		return parse(v)
	}
	return parse(r.Header.Get("Accept-Language"))
}

func parse(v string) Lang {
	first := strings.ToLower(strings.TrimSpace(strings.SplitN(strings.SplitN(v, ",", 2)[0], ";", 2)[0]))
	if first == "en" || strings.HasPrefix(first, "en-") {
		return EN
	}
	return TR
}

var verb = regexp.MustCompile(`%[dsqvw]`)

// form is one way a message is written: as it is, or as a pattern that
// matches the printed template and gives its values back.
type form struct {
	text  string
	verbs []string
	re    *regexp.Regexp // nil for a text without values
}

func compile(text string) form {
	verbs := verb.FindAllString(text, -1)
	if len(verbs) == 0 {
		return form{text: text}
	}
	parts := verb.Split(text, -1)
	var b strings.Builder
	b.WriteString("^")
	for i, part := range parts {
		b.WriteString(regexp.QuoteMeta(part))
		if i < len(verbs) {
			switch verbs[i] {
			case "%d":
				b.WriteString(`(-?\d+)`)
			case "%q":
				b.WriteString(`("(?:[^"\\]|\\.)*")`)
			default:
				b.WriteString(`(.+?)`)
			}
		}
	}
	b.WriteString("$")
	return form{text: text, verbs: verbs, re: regexp.MustCompile(b.String())}
}

// match gives the values of msg printed from this form.
func (f form) match(msg string) ([]string, bool) {
	if f.re == nil {
		return nil, msg == f.text
	}
	m := f.re.FindStringSubmatch(msg)
	if m == nil {
		return nil, false
	}
	return m[1:], true
}

// fill prints values into this form in order; a %s value (a word such as a
// place level) is itself translated.
func (f form) fill(lang Lang, values []string) string {
	i := 0
	return verb.ReplaceAllStringFunc(f.text, func(v string) string {
		if i >= len(values) {
			return v
		}
		val := values[i]
		i++
		if v == "%s" || v == "%v" {
			return Translate(lang, val)
		}
		return val
	})
}

type entry struct{ tr, en, src form }

var entries = func() []entry {
	out := make([]entry, len(catalog))
	for i, p := range catalog {
		e := entry{tr: compile(p.TR), en: compile(p.EN)}
		if p.Src != "" {
			e.src = compile(p.Src)
		}
		out[i] = e
	}
	return out
}()

var prefixed = regexp.MustCompile(`^([a-z_]+): (.+)$`)

// Translate gives msg in lang.
func Translate(lang Lang, msg string) string {
	for _, e := range entries {
		want := e.tr
		if lang == EN {
			want = e.en
		}
		for _, f := range []form{e.tr, e.en, e.src} {
			if f.text == "" {
				continue
			}
			if values, ok := f.match(msg); ok {
				return want.fill(lang, values)
			}
		}
	}
	// "code: message", as a typed error prints itself
	if m := prefixed.FindStringSubmatch(msg); m != nil {
		if rest := Translate(lang, m[2]); rest != m[2] {
			return m[1] + ": " + rest
		}
	}
	return msg
}
