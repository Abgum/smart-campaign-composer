package contextbuilder

import (
	"strings"
	"testing"

	"smart-campaign-composer/backend/internal/abtest"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/language"
)

// The model sees a target campaign's B side in full, so a change it writes
// can carry the A/B test on.
func TestBuild_ChatState_ABTest(t *testing.T) {
	c := draft(101, 1, "A/B", 2, "user")
	c.Current.ABTest = &abtest.Test{Variants: []language.Variant{{Language: "tr", SMS: &content.SMS{Text: "B metni"}}}}
	req := Build(Input{Campaigns: []CampaignView{c}, Targets: Targets{Intent: "edit", Source: "router", Allowed: []int64{101}},
		UserText: "SMS'i kısalt", Today: today})
	s := chatState(t, req)
	if len(s.TargetCampaigns) != 1 {
		t.Fatalf("targets = %+v", s.TargetCampaigns)
	}
	ab := s.TargetCampaigns[0].ABTest
	if ab == nil || len(ab.Variants) != 1 || ab.Variants[0].SMS.Text != "B metni" {
		t.Fatalf("target ab_test = %+v", ab)
	}
}

// The system prompt tells the model when to write an A/B test, how it
// combines with languages, that a save without it keeps it, and that only
// remove_ab_test removes it.
func TestSystemPrompt_ABTestRules(t *testing.T) {
	p := SystemPrompt()
	for _, want := range []string{"ab_test", "A/B test", "same arm", "every variant of per_recipient",
		"A save without ab_test keeps the test", "remove_ab_test", "only when the user explicitly asks"} {
		if !strings.Contains(p, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
}
