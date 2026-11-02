package agentloop

import (
	"context"
	"slices"
	"strings"
	"testing"

	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/llm/mock"
	"smart-campaign-composer/backend/internal/segment"
)

const abDraft = `{"name":"Sepet A/B",
 "segment_rule":{"match":"all","conditions":[{"kind":"event","event_type":"cart_add","within_days":30,"not_followed_by":"purchase"}]},
 "sms":{"text":"Merhaba {{first_name}}, sepetin seni bekliyor!"},"content_language":"tr",
 "ab_test":{"variants":[{"language":"tr","sms":{"text":"%s"}}]},
 "change_summary":"A/B testi eklendi"}`

// A draft's B side is checked against its A contents the way Go checks the
// rest: a B equal to A goes back to the model, a valid one is saved.
func TestLoop_ABTest_CheckedThenSaved(t *testing.T) {
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: strings.Replace(abDraft, "%s", "Merhaba {{first_name}}, sepetin seni bekliyor!", 1)},
		mock.Call{ToolName: "save_campaign_draft", ToolInput: strings.Replace(abDraft, "%s", "Son gün {{first_name}}: sepetin %10 indirimli!", 1)},
		mock.Call{Text: "A/B testini kaydettim."},
	)
	store := newStore()
	res, _, _ := run(t, m, store, baseInput())
	if res.Outcome != OutcomeOK || len(res.Saved) != 1 {
		t.Fatalf("outcome=%s saved=%d", res.Outcome, len(res.Saved))
	}
	tr := lastToolResult(t, reqAt(t, m, 1))
	if !tr.IsError || !strings.Contains(tr.Text, "same_as_a") || !strings.Contains(tr.Text, "ab_test.variants[0]") {
		t.Fatalf("feedback for B equal to A = %+v", tr)
	}
	ab := store.cmds[0].ABTest
	if ab == nil || len(ab.Variants) != 1 || ab.Variants[0].Language != "tr" || !strings.HasPrefix(ab.Variants[0].SMS.Text, "Son gün") {
		t.Fatalf("saved B side = %+v", ab)
	}
}

// removal is a RemoveABTest call the fake store received.
type removal struct {
	campaignID    int64
	baseVersion   int
	changeSummary string
}

// RemoveABTest checks like the store: a draft of the chat, the current
// version, and a test to remove.
func (s *fakeStore) RemoveABTest(_ context.Context, campaignID int64, baseVersion int, changeSummary string) (campaign.Removed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.campaigns[campaignID]
	switch {
	case !ok:
		return campaign.Removed{}, campaign.ErrNotFound
	case c.status != "draft":
		return campaign.Removed{}, campaign.ErrNotDraft
	case baseVersion != c.version:
		return campaign.Removed{}, &campaign.StaleError{CurrentVersion: c.version}
	case !c.ab:
		return campaign.Removed{}, campaign.ErrNoABTest
	}
	c.version++
	c.ab = false
	s.removals = append(s.removals, removal{campaignID, baseVersion, changeSummary})
	return campaign.Removed{Saved: campaign.Saved{CampaignID: campaignID, Ordinal: c.ordinal, Version: c.version, ChangeSummary: changeSummary},
		Audience: segment.Audience{Total: 312}}, nil
}

// A change that leaves ab_test out keeps the campaign's test (the store
// does that); the tool result tells the model so.
func TestLoop_ABTest_LeftOutIsKept(t *testing.T) {
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: revise(11, 3, "Kısa SMS")},
		mock.Call{Text: "Kaydettim."},
	)
	store := newStore()
	store.campaigns[11].ab = true
	res, _, _ := run(t, m, store, baseInput(11))
	if res.Outcome != OutcomeOK || len(res.Saved) != 1 {
		t.Fatalf("outcome=%s saved=%+v", res.Outcome, res.Saved)
	}
	tr := lastToolResult(t, reqAt(t, m, 1))
	if tr.IsError || !strings.Contains(tr.Text, "ab_test_note") || !strings.Contains(tr.Text, "kept") {
		t.Fatalf("tool result = %+v; want it to say the A/B test was kept", tr)
	}
}

// When the kept B no longer fits the new A, the store refuses the save. The
// model gets a validation error that asks it to update B (and says how a
// test is removed), and the UI is told why the draft was not saved.
func TestLoop_ABTest_KeptButOutdated_AsksForB(t *testing.T) {
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: revise(11, 3, "Kısa SMS")},
		mock.Call{Text: "B'yi de güncellemem gerekiyor."},
	)
	store := newStore()
	store.failWith = &campaign.OutdatedABTestError{Issues: []campaign.Issue{
		{Path: "ab_test.variants[0]", Code: "channel_mismatch", Message: "B varyantı A ile aynı kanalları kullanmalı"}}}
	res, rec, _ := run(t, m, store, baseInput(11))
	tr := lastToolResult(t, reqAt(t, m, 1))
	for _, want := range []string{"ab_test_outdated", "channel_mismatch", "ab_test.variants[0]", "remove_ab_test"} {
		if !tr.IsError || !strings.Contains(tr.Text, want) {
			t.Errorf("tool result lacks %q: %+v", want, tr)
		}
	}
	if !rec.has(EventDraftInvalid) {
		t.Errorf("the UI must see why the draft was refused; events = %v", rec.types())
	}
	if res.Outcome != OutcomeOK || len(res.Saved) != 0 || !slices.Equal(res.Rejections, []string{RejectValidation}) {
		t.Fatalf("outcome=%s saved=%+v rejections=%v", res.Outcome, res.Saved, res.Rejections)
	}
}

// remove_ab_test is a call of its own: the store's removal writes the
// campaign's next version without the test, and the live preview hears of
// it like of a saved draft.
func TestLoop_RemoveABTest(t *testing.T) {
	m := mock.NewScripted(
		mock.Call{ToolName: "remove_ab_test", ToolInput: `{"campaign_id":11,"base_version":3,"change_summary":"A/B testi kaldırıldı"}`},
		mock.Call{Text: "A/B testini kaldırdım."},
	)
	store := newStore()
	store.campaigns[11].ab = true
	res, rec, _ := run(t, m, store, baseInput(11))
	if res.Outcome != OutcomeOK || len(res.Saved) != 1 || res.Saved[0].Version != 4 {
		t.Fatalf("outcome=%s/%s saved=%+v", res.Outcome, res.ErrorCode, res.Saved)
	}
	if len(store.removals) != 1 || store.removals[0] != (removal{11, 3, "A/B testi kaldırıldı"}) || len(store.cmds) != 0 {
		t.Fatalf("removals=%+v drafts=%d", store.removals, len(store.cmds))
	}
	if tr := lastToolResult(t, reqAt(t, m, 1)); tr.IsError || !strings.Contains(tr.Text, `"ab_test_removed":true`) {
		t.Fatalf("tool result = %+v", tr)
	}
	i := slices.IndexFunc(rec.events, func(e Event) bool { return e.Type == EventDraftSaved })
	if i < 0 {
		t.Fatalf("no draft.saved; events = %v", rec.types())
	}
	if data := rec.events[i].Data.(map[string]any); data["version"] != 4 || data["audience"] == nil {
		t.Fatalf("draft.saved = %+v; want the version and the audience it keeps", data)
	}
}

// remove_ab_test obeys the targets of the turn like a draft does, and
// needs a current version with a test to remove.
func TestLoop_RemoveABTest_Refused(t *testing.T) {
	cases := []struct {
		name, input, code string
	}{
		{"not a target", `{"campaign_id":12,"base_version":1,"change_summary":"x"}`, "target_not_allowed"},
		{"no test", `{"campaign_id":11,"base_version":3,"change_summary":"x"}`, "no_ab_test"},
		{"old version", `{"campaign_id":11,"base_version":2,"change_summary":"x"}`, "stale_version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := mock.NewScripted(mock.Call{ToolName: "remove_ab_test", ToolInput: c.input}, mock.Call{Text: "Kaldıramadım."})
			store := newStore()
			store.campaigns[11].ab = c.code != "no_ab_test"
			res, _, _ := run(t, m, store, baseInput(11))
			if tr := lastToolResult(t, reqAt(t, m, 1)); !tr.IsError || !strings.Contains(tr.Text, c.code) {
				t.Fatalf("tool result = %+v; want %s", tr, c.code)
			}
			if len(res.Saved) != 0 || len(store.removals) != 0 {
				t.Fatalf("saved=%+v removals=%+v", res.Saved, store.removals)
			}
		})
	}
}
