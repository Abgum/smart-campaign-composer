package httpapi

import (
	"encoding/json"
	"regexp"
	"strings"
)

// A tool's raw streamed JSON can contain the internal numeric campaign_id.
// The browser only needs the draft's readable fields. Project those fields
// into a small snapshot, which replaces the previous preview on every delta.
var (
	previewNameRE    = regexp.MustCompile(`"name"\s*:\s*"((?:[^"\\]|\\.)*)`)
	previewSMSRE     = regexp.MustCompile(`"sms"\s*:\s*\{\s*"text"\s*:\s*"((?:[^"\\]|\\.)*)`)
	previewSubjectRE = regexp.MustCompile(`"subject"\s*:\s*"((?:[^"\\]|\\.)*)`)
	previewBodyRE    = regexp.MustCompile(`"body"\s*:\s*"((?:[^"\\]|\\.)*)`)
	previewABTestRE  = regexp.MustCompile(`"ab_test"\s*:\s*`)
	incompleteRuneRE = regexp.MustCompile(`\\u[0-9a-fA-F]{0,3}$`)
)

// withoutBSide drops the ab_test value (an A/B draft's B side) from the raw
// tool input, so the preview shows A wherever the model streams B. A value
// not finished yet is dropped to the end.
func withoutBSide(raw string) string {
	loc := previewABTestRE.FindStringIndex(raw)
	if loc == nil {
		return raw
	}
	return raw[:loc[0]] + raw[valueEnd(raw, loc[1]):]
}

// valueEnd returns the index just past the JSON value that starts at i, or
// len(s) while it is incomplete.
func valueEnd(s string, i int) int {
	depth, inString, escaped := 0, false, false
	for j := i; j < len(s); j++ {
		c := s[j]
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
				if depth == 0 {
					return j + 1
				}
			}
		case c == '"':
			inString = true
		case c == '{' || c == '[':
			depth++
		case c == '}' || c == ']':
			if depth--; depth <= 0 {
				return j + 1
			}
		case c == ',' && depth == 0:
			return j
		}
	}
	return len(s)
}

func previewString(raw string, re *regexp.Regexp) *string {
	m := re.FindStringSubmatch(raw)
	if len(m) < 2 {
		return nil
	}
	value := m[1]
	if strings.HasSuffix(value, `\`) && !strings.HasSuffix(value, `\\`) {
		value = value[:len(value)-1]
	}
	value = incompleteRuneRE.ReplaceAllString(value, "")
	var decoded string
	if json.Unmarshal([]byte(`"`+value+`"`), &decoded) == nil {
		return &decoded
	}
	return &value
}

func draftPreviewSnapshot(raw string) string {
	raw = withoutBSide(raw)
	name := previewString(raw, previewNameRE)
	sms := previewString(raw, previewSMSRE)
	subject := previewString(raw, previewSubjectRE)
	body := previewString(raw, previewBodyRE)
	type email struct {
		Subject string `json:"subject"`
		Body    string `json:"body,omitempty"`
	}
	preview := struct {
		Name *string `json:"name,omitempty"`
		SMS  *struct {
			Text string `json:"text"`
		} `json:"sms,omitempty"`
		Email *email `json:"email,omitempty"`
	}{Name: name}
	if sms != nil {
		preview.SMS = &struct {
			Text string `json:"text"`
		}{Text: *sms}
	}
	if subject != nil || body != nil {
		preview.Email = &email{}
		if subject != nil {
			preview.Email.Subject = *subject
		}
		if body != nil {
			preview.Email.Body = *body
		}
	}
	b, _ := json.Marshal(preview)
	return string(b)
}
