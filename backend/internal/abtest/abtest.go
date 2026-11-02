// Package abtest splits a campaign's recipients between two versions of its
// message, A and B.
//
// A is the draft's own content: one content for everyone, or one variant per
// language. B mirrors it: one B content for each A content, in the same
// language and on the same channels. Language comes first: Go works out a
// recipient's language as without a test (language.Settings.Resolve), and
// the recipient's arm then picks A or B within that language.
//
// The arm is a pure function of the campaign and the recipient: the same on
// SMS and email, in the approval preview and at approval, in every process.
// Approval writes it, with the rendered text, into the delivery, so a retry
// or a worker restart never changes it.
package abtest

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strconv"

	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/language"
)

// The two arms.
const (
	A = "A"
	B = "B"
)

// Test is a draft's B side: one B content per A content, keyed by the A
// content's language ("" for one content whose language is not known).
type Test struct {
	Variants []language.Variant `json:"variants"`
}

// Arm assigns a recipient of a campaign to A or B, about half to each. The
// assignment hashes the two ids, so it needs no state and no randomness,
// and another campaign draws it afresh.
func Arm(campaignID, userID int64) string {
	sum := sha256.Sum256([]byte(strconv.FormatInt(campaignID, 10) + ":" + strconv.FormatInt(userID, 10)))
	if sum[0]&1 == 0 {
		return A
	}
	return B
}

// Contents are a draft's A contents: its language variants, or its one
// content in the setup's language (s nil: a draft from before there were
// languages, whose content's language is not known).
func Contents(s *language.Settings, sms *content.SMS, email *content.Email) []language.Variant {
	if s != nil && s.Mode == language.ModePerRecipient {
		return s.Variants
	}
	lang := ""
	if s != nil {
		lang = s.Language
	}
	return []language.Variant{{Language: lang, SMS: sms, Email: email}}
}

// Variant returns the B content in lang.
func (t Test) Variant(lang string) (language.Variant, bool) {
	i := slices.IndexFunc(t.Variants, func(v language.Variant) bool { return v.Language == lang })
	if i < 0 {
		return language.Variant{}, false
	}
	return t.Variants[i], true
}

// Issue is a validation finding; Path is relative to the draft.
type Issue struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Validate checks the B side against the A contents a: one B per A content,
// in the same language and on the same channels, each valid content (the
// channel limits, the variables) and different from its A.
func Validate(t Test, a []language.Variant) []Issue {
	if len(t.Variants) == 0 {
		return []Issue{{Path: "ab_test.variants", Code: "required", Message: "A/B testinde her içerik için bir B varyantı olmalı"}}
	}
	var issues []Issue
	seen := map[string]bool{}
	for i, b := range t.Variants {
		path := fmt.Sprintf("ab_test.variants[%d]", i)
		if seen[b.Language] {
			issues = append(issues, Issue{Path: path + ".language", Code: "duplicate_variant",
				Message: "aynı içerik için birden fazla B varyantı var"})
			continue
		}
		seen[b.Language] = true
		ai := slices.IndexFunc(a, func(v language.Variant) bool { return v.Language == b.Language })
		if ai < 0 {
			issues = append(issues, Issue{Path: path + ".language", Code: "unknown_variant",
				Message: "B varyantının dili A içeriklerinde yok"})
			continue
		}
		av := a[ai]
		if (av.SMS != nil) != (b.SMS != nil) || (av.Email != nil) != (b.Email != nil) {
			issues = append(issues, Issue{Path: path, Code: "channel_mismatch", Message: "B varyantı A ile aynı kanalları kullanmalı"})
			continue
		}
		bad := false
		for _, is := range content.ValidateDraft(b.SMS, b.Email) {
			issues = append(issues, Issue{Path: path + "." + is.Path, Code: is.Code, Message: is.Message})
			bad = true
		}
		if !bad && same(av, b) {
			issues = append(issues, Issue{Path: path, Code: "same_as_a", Message: "B varyantı A ile aynı; en az bir metin farklı olmalı"})
		}
	}
	for _, av := range a {
		if seen[av.Language] {
			continue
		}
		if av.Language == "" {
			issues = append(issues, Issue{Path: "ab_test.variants", Code: "missing_variant", Message: "A içeriğinin B varyantı yok"})
		} else {
			issues = append(issues, Issue{Path: "ab_test.variants", Code: "missing_variant",
				Message: fmt.Sprintf("%s dilindeki A içeriğinin B varyantı yok", av.Language)})
		}
	}
	return issues
}

// same tells whether B would send what A sends: the same text on every
// channel, by the content hash's rule (whitespace and Unicode form aside).
func same(a, b language.Variant) bool {
	if a.SMS != nil && content.Hash("sms", "", a.SMS.Text) != content.Hash("sms", "", b.SMS.Text) {
		return false
	}
	if a.Email != nil && content.Hash("email", a.Email.Subject, a.Email.Body) != content.Hash("email", b.Email.Subject, b.Email.Body) {
		return false
	}
	return true
}
