// Package content validates SMS/email drafts, renders them per recipient and
// computes the content hash that defines "the same message".
package content

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Channel limits from the brief.
const (
	SMSMaxRunes     = 160
	SubjectMaxRunes = 80
	BodyMaxRunes    = 5000
)

// FirstNameFallback replaces an empty first name when rendering; content in
// another language uses its own form from FirstNameFallbacks.
const FirstNameFallback = "Değerli Müşterimiz"

// FirstNameFallbacks are the forms of FirstNameFallback per content language.
var FirstNameFallbacks = map[string]string{"tr": FirstNameFallback, "en": "Valued Customer", "fr": "Cher client"}

// Variables are the supported personalization placeholders.
var Variables = []string{"first_name", "last_name", "city"}

// VariableEstimate is the length assumed for a variable at draft time; the
// exact check happens at approval with real values.
var VariableEstimate = map[string]int{"first_name": 10, "last_name": 10, "city": 10}

// SMS is an SMS draft.
type SMS struct {
	Text string `json:"text"`
}

// Email is an email draft (plain-text body).
type Email struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// Issue is a machine-readable validation finding.
type Issue struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Vars are the personalization values for one recipient. Language is the
// content's language ("" when unknown: Turkish forms).
type Vars struct {
	FirstName, LastName, City string
	Language                  string
}

var placeholderRe = regexp.MustCompile(`\{\{\s*([^{}]*?)\s*\}\}`)

// ValidateDraft checks a draft's channel content. Variables are counted at
// their estimated length; CheckRendered does the exact check at approval.
func ValidateDraft(sms *SMS, email *Email) []Issue {
	var issues []Issue
	if sms == nil && email == nil {
		return []Issue{{Code: "no_channel", Message: "en az bir kanal (sms ya da email) gerekli"}}
	}
	if sms != nil {
		issues = append(issues, checkTemplate("sms.text", sms.Text, SMSMaxRunes)...)
	}
	if email != nil {
		issues = append(issues, checkTemplate("email.subject", email.Subject, SubjectMaxRunes)...)
		issues = append(issues, checkTemplate("email.body", email.Body, BodyMaxRunes)...)
	}
	return issues
}

func checkTemplate(path, text string, maxRunes int) []Issue {
	if strings.TrimSpace(text) == "" {
		return []Issue{{Path: path, Code: "required", Message: "boş olamaz"}}
	}
	var issues []Issue
	for _, m := range placeholderRe.FindAllStringSubmatch(text, -1) {
		if !slices.Contains(Variables, m[1]) {
			issues = append(issues, Issue{Path: path, Code: "unknown_variable",
				Message: fmt.Sprintf("%q desteklenmiyor; izinli değişkenler: {{%s}}", m[0], strings.Join(Variables, "}}, {{"))})
		}
	}
	if strings.ContainsAny(placeholderRe.ReplaceAllString(text, ""), "{}") {
		issues = append(issues, Issue{Path: path, Code: "unbalanced_braces",
			Message: "süslü parantezler yalnızca {{degisken}} biçiminde kullanılabilir"})
	}
	estimated := placeholderRe.ReplaceAllStringFunc(text, func(m string) string {
		name := placeholderRe.FindStringSubmatch(m)[1]
		return strings.Repeat("x", VariableEstimate[name])
	})
	if n := utf8.RuneCountInString(estimated); n > maxRunes {
		issues = append(issues, Issue{Path: path, Code: "too_long",
			Message: fmt.Sprintf("tahmini uzunluk %d karakter, sınır %d (değişkenler ~%d karakter sayılır)", n, maxRunes, VariableEstimate["first_name"])})
	}
	return issues
}

// Render fills template variables for one recipient.
func Render(tmpl string, v Vars) string {
	return placeholderRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		switch placeholderRe.FindStringSubmatch(m)[1] {
		case "first_name":
			if v.FirstName == "" {
				if f, ok := FirstNameFallbacks[v.Language]; ok {
					return f
				}
				return FirstNameFallback
			}
			return v.FirstName
		case "last_name":
			return v.LastName
		case "city":
			return v.City
		}
		return m
	})
}

// CheckRendered checks a rendered message against the channel limits.
func CheckRendered(channel, subject, body string) []Issue {
	var issues []Issue
	switch channel {
	case "sms":
		if n := utf8.RuneCountInString(body); n > SMSMaxRunes {
			issues = append(issues, Issue{Path: "sms.text", Code: "too_long",
				Message: fmt.Sprintf("render edilmiş SMS %d karakter, sınır %d", n, SMSMaxRunes)})
		}
	case "email":
		if n := utf8.RuneCountInString(subject); n > SubjectMaxRunes {
			issues = append(issues, Issue{Path: "email.subject", Code: "too_long",
				Message: fmt.Sprintf("render edilmiş konu %d karakter, sınır %d", n, SubjectMaxRunes)})
		}
	}
	return issues
}

// Hash returns the content hash of a rendered message on a channel. Two
// messages are "the same" when channel, NFC-normalized text and whitespace-
// collapsed text match; case and every other character are significant.
func Hash(channel, subject, body string) string {
	h := sha256.New()
	h.Write([]byte(channel))
	h.Write([]byte{0})
	h.Write([]byte(normalize(subject)))
	h.Write([]byte{0})
	h.Write([]byte(normalize(body)))
	return hex.EncodeToString(h.Sum(nil))
}

func normalize(s string) string {
	return strings.Join(strings.Fields(norm.NFC.String(s)), " ")
}
