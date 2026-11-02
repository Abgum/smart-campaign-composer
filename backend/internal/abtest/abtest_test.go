package abtest_test

import (
	"strings"
	"testing"

	"smart-campaign-composer/backend/internal/abtest"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/language"
)

// The arm is a pure function of campaign and recipient: the same on every
// call, about half of the recipients on each side, and drawn afresh for
// another campaign.
func TestArm_DeterministicAndBalanced(t *testing.T) {
	if abtest.Arm(7, 42) != abtest.Arm(7, 42) {
		t.Fatal("the same recipient got two arms")
	}
	a, differ := 0, 0
	for u := int64(1); u <= 10000; u++ {
		arm := abtest.Arm(7, u)
		if arm != abtest.A && arm != abtest.B {
			t.Fatalf("arm %q", arm)
		}
		if arm == abtest.A {
			a++
		}
		if arm != abtest.Arm(8, u) {
			differ++
		}
	}
	if a < 4800 || a > 5200 {
		t.Fatalf("%d of 10000 recipients in A, want about half", a)
	}
	if differ < 4800 || differ > 5200 {
		t.Fatalf("%d of 10000 recipients changed arm between two campaigns, want about half", differ)
	}
}

func sms(text string) *content.SMS { return &content.SMS{Text: text} }

func email(subject, body string) *content.Email { return &content.Email{Subject: subject, Body: body} }

func TestValidate(t *testing.T) {
	single := []language.Variant{{Language: "tr", SMS: sms("Merhaba {{first_name}}"), Email: email("Fırsat", "Merhaba")}}
	perRecipient := []language.Variant{
		{Language: "tr", SMS: sms("Merhaba {{first_name}}")},
		{Language: "en", SMS: sms("Hi {{first_name}}")},
	}
	for _, tc := range []struct {
		name  string
		a     []language.Variant
		b     []language.Variant
		codes []string // the issues' codes, in order; none when valid
		path  string   // the first issue's path
	}{
		{"single content", single, []language.Variant{{Language: "tr", SMS: sms("Son gün {{first_name}}"), Email: email("Fırsat", "Merhaba")}}, nil, ""},
		{"one B per language", perRecipient, []language.Variant{
			{Language: "en", SMS: sms("Last day {{first_name}}")},
			{Language: "tr", SMS: sms("Son gün {{first_name}}")},
		}, nil, ""},
		{"no B at all", single, nil, []string{"required"}, "ab_test.variants"},
		{"a language without B", perRecipient, []language.Variant{{Language: "tr", SMS: sms("Son gün")}}, []string{"missing_variant"}, "ab_test.variants"},
		{"B in a language A does not have", single, []language.Variant{
			{Language: "tr", SMS: sms("Son gün"), Email: email("Fırsat", "Merhaba")},
			{Language: "fr", SMS: sms("Bonjour"), Email: email("Offre", "Bonjour")},
		}, []string{"unknown_variant"}, "ab_test.variants[1].language"},
		{"two B for one language", perRecipient, []language.Variant{
			{Language: "tr", SMS: sms("Son gün")}, {Language: "en", SMS: sms("Last day")}, {Language: "tr", SMS: sms("Bugün")},
		}, []string{"duplicate_variant"}, "ab_test.variants[2].language"},
		{"other channels than A", single, []language.Variant{{Language: "tr", SMS: sms("Son gün")}}, []string{"channel_mismatch"}, "ab_test.variants[0]"},
		{"B over the SMS limit", perRecipient, []language.Variant{
			{Language: "tr", SMS: sms(strings.Repeat("a", 161))}, {Language: "en", SMS: sms("Last day")},
		}, []string{"too_long"}, "ab_test.variants[0].sms.text"},
		{"B the same as A", perRecipient, []language.Variant{
			{Language: "tr", SMS: sms("Merhaba  {{first_name}} ")}, {Language: "en", SMS: sms("Last day")},
		}, []string{"same_as_a"}, "ab_test.variants[0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issues := abtest.Validate(abtest.Test{Variants: tc.b}, tc.a)
			var codes []string
			for _, is := range issues {
				codes = append(codes, is.Code)
				if is.Message == "" {
					t.Errorf("issue %s has no message", is.Code)
				}
			}
			if strings.Join(codes, ",") != strings.Join(tc.codes, ",") {
				t.Fatalf("codes = %v, want %v (%+v)", codes, tc.codes, issues)
			}
			if len(issues) > 0 && issues[0].Path != tc.path {
				t.Fatalf("path = %q, want %q", issues[0].Path, tc.path)
			}
		})
	}
}

// B's content in a language is found by the language.
func TestVariant(t *testing.T) {
	test := abtest.Test{Variants: []language.Variant{{Language: "en", SMS: sms("Last day")}, {Language: "tr", SMS: sms("Son gün")}}}
	if v, ok := test.Variant("tr"); !ok || v.SMS.Text != "Son gün" {
		t.Fatalf("Variant(tr) = %+v, %v", v, ok)
	}
	if _, ok := test.Variant("fr"); ok {
		t.Fatal("found a variant that is not there")
	}
}
