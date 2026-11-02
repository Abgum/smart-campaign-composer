package content

import (
	"strings"
	"testing"
)

func has(issues []Issue, path, code string) bool {
	for _, i := range issues {
		if i.Path == path && i.Code == code {
			return true
		}
	}
	return false
}

func TestSMS_RuneLimit_Turkish(t *testing.T) {
	ok := strings.Repeat("ş", 160) // 320 bytes, 160 characters
	if issues := ValidateDraft(&SMS{Text: ok}, nil); len(issues) != 0 {
		t.Fatalf("160 Turkish characters must be accepted, got %+v", issues)
	}
	if issues := ValidateDraft(&SMS{Text: ok + "ğ"}, nil); !has(issues, "sms.text", "too_long") {
		t.Fatalf("161 characters must be rejected, got %+v", issues)
	}
}

func TestSMS_VariableLengthEstimate(t *testing.T) {
	placeholder := "{{first_name}}"
	fits := strings.Repeat("a", SMSMaxRunes-VariableEstimate["first_name"]) + placeholder
	if issues := ValidateDraft(&SMS{Text: fits}, nil); len(issues) != 0 {
		t.Fatalf("estimated length %d should fit, got %+v", SMSMaxRunes, issues)
	}
	over := "a" + fits
	if issues := ValidateDraft(&SMS{Text: over}, nil); !has(issues, "sms.text", "too_long") {
		t.Fatalf("estimated length %d should be rejected, got %+v", SMSMaxRunes+1, issues)
	}
}

func TestVariables(t *testing.T) {
	cases := []struct {
		text string
		code string // empty = valid
	}{
		{"Merhaba {{first_name}}!", ""},
		{"Merhaba {{ first_name }}, {{city}} için fırsat", ""},
		{"Kodun: {{coupon}}", "unknown_variable"},
		{"Merhaba {{first_name}!", "unbalanced_braces"},
		{"Merhaba {first_name}}!", "unbalanced_braces"},
		{"Merhaba {{}}", "unknown_variable"},
	}
	for _, tc := range cases {
		issues := ValidateDraft(&SMS{Text: tc.text}, nil)
		if tc.code == "" && len(issues) != 0 {
			t.Errorf("%q: unexpected %+v", tc.text, issues)
		}
		if tc.code != "" && !has(issues, "sms.text", tc.code) {
			t.Errorf("%q: expected %s, got %+v", tc.text, tc.code, issues)
		}
	}
}

func TestEmail_Limits(t *testing.T) {
	if issues := ValidateDraft(nil, &Email{Subject: strings.Repeat("ü", 80), Body: "Merhaba"}); len(issues) != 0 {
		t.Fatalf("80-character subject must pass, got %+v", issues)
	}
	issues := ValidateDraft(nil, &Email{Subject: strings.Repeat("ü", 81), Body: "  "})
	if !has(issues, "email.subject", "too_long") {
		t.Errorf("expected subject too_long, got %+v", issues)
	}
	if !has(issues, "email.body", "required") {
		t.Errorf("expected body required, got %+v", issues)
	}
	if issues := ValidateDraft(nil, &Email{Subject: "Fırsat {{x}}", Body: "ok"}); !has(issues, "email.subject", "unknown_variable") {
		t.Errorf("variables in subject must be checked, got %+v", issues)
	}
}

func TestDraft_NeedsAChannel(t *testing.T) {
	if issues := ValidateDraft(nil, nil); !has(issues, "", "no_channel") {
		t.Fatalf("expected no_channel, got %+v", issues)
	}
}

func TestRender_PerRecipient(t *testing.T) {
	vars := Vars{FirstName: "Ayşe", LastName: "Yılmaz", City: "İzmir"}
	got := Render("Merhaba {{first_name}} {{ last_name }}, {{city}} için %30!", vars)
	if want := "Merhaba Ayşe Yılmaz, İzmir için %30!"; got != want {
		t.Fatalf("render = %q, want %q", got, want)
	}
	got = Render("Merhaba {{first_name}}, fırsat seni bekliyor", Vars{})
	if want := "Merhaba " + FirstNameFallback + ", fırsat seni bekliyor"; got != want {
		t.Fatalf("fallback render = %q, want %q", got, want)
	}
}

func TestCheckRendered(t *testing.T) {
	if issues := CheckRendered("sms", "", strings.Repeat("a", 161)); !has(issues, "sms.text", "too_long") {
		t.Fatalf("rendered SMS over 160 must fail, got %+v", issues)
	}
	if issues := CheckRendered("email", strings.Repeat("a", 81), "body"); !has(issues, "email.subject", "too_long") {
		t.Fatalf("rendered subject over 80 must fail, got %+v", issues)
	}
	if issues := CheckRendered("sms", "", strings.Repeat("a", 160)); len(issues) != 0 {
		t.Fatalf("160 must pass, got %+v", issues)
	}
}

func TestContentHash_Normalization(t *testing.T) {
	base := Hash("sms", "", "Merhaba Ayşe, %30 indirim!")
	if base == "" {
		t.Fatal("hash must not be empty")
	}
	same := []string{
		"  Merhaba Ayşe, %30 indirim!\n",
		"Merhaba  Ayşe,\t%30   indirim!",
		"Merhaba Ayşe, %30 indirim!", // decomposed ş (NFD) equals composed ş
	}
	for _, s := range same {
		if Hash("sms", "", s) != base {
			t.Errorf("%q should hash like the base message", s)
		}
	}
	different := []string{
		"merhaba Ayşe, %30 indirim!", // case is preserved
		"Merhaba Ayşe, %31 indirim!", // one character differs
	}
	for _, s := range different {
		if Hash("sms", "", s) == base {
			t.Errorf("%q must hash differently", s)
		}
	}
	if Hash("email", "", "Merhaba Ayşe, %30 indirim!") == base {
		t.Error("same text on another channel is a different message")
	}
}

func TestContentHash_EmailSubjectBodySeparated(t *testing.T) {
	if Hash("email", "Yaz fırsatı", "başladı") == Hash("email", "Yaz", "fırsatı başladı") {
		t.Fatal("moving text between subject and body must change the hash")
	}
}
