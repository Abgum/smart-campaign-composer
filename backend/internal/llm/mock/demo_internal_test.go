package mock

import (
	"slices"
	"strings"
	"testing"

	"smart-campaign-composer/backend/internal/segment"
)

func TestDemoScenarioNamesAreEnglish(t *testing.T) {
	want := []string{"default", "repair", "unknown_city", "new_campaign", "duplicate", "ambiguous", "refusal", "max_tokens", "save_then_llm_error", "error_mid_stream", "slow_stream", "english", "multilingual", "bilingual", "group_decision", "pending_to_english", "ab_test", "remove_ab_test"}
	got := make([]string, len(Scenarios))
	for i, s := range Scenarios {
		got[i] = s.Name
	}
	if !slices.Equal(got, want) {
		t.Fatalf("scenario names = %v, want %v", got, want)
	}
	if got := scenarioOf("Create a campaign #scenario:repair"); got != "repair" {
		t.Fatalf("#scenario tag = %q, want repair", got)
	}
}

func TestDemoSamples_TargetCountriesOrSettlements(t *testing.T) {
	for _, s := range samples {
		rule, err := segment.ParseRule([]byte(s.rule))
		if err != nil {
			t.Fatalf("%s: parse rule: %v", s.name, err)
		}
		for _, condition := range rule.Conditions {
			for _, place := range condition.Places {
				if place.Level != "city" && place.Level != "country" {
					t.Errorf("%s: unsupported demo place %+v", s.name, place)
				}
			}
		}
	}
}

// The demo adds its "last day" line whenever the result fits the SMS limit
// as the validator estimates it (a variable counts 10 characters).
func TestLastDay_FitsByTheValidatorsEstimate(t *testing.T) {
	head := "Merhaba {{first_name}}, "      // estimated 20 characters
	fits := head + strings.Repeat("a", 108) // estimated 128; 143 with the line
	if got, _ := lastDay(&smsText{Text: fits}); got != fits+lastDaySuffix {
		t.Fatalf("estimated 128 + 15 fits 160, but the line was not added: %q", got)
	}
	long := head + strings.Repeat("a", 130) // estimated 150; 165 with the line
	if got, _ := lastDay(&smsText{Text: long}); got != long {
		t.Fatalf("estimated 150 + 15 exceeds 160, but the text changed: %q", got)
	}
}
