package turns_test

import (
	"context"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"smart-campaign-composer/backend/internal/abtest"
	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/chat/turns"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/language"
	"smart-campaign-composer/backend/internal/llm"
	"smart-campaign-composer/backend/internal/llm/mock"
	"smart-campaign-composer/backend/internal/segment"
	"smart-campaign-composer/backend/internal/testutil"
)

// A change to an A/B campaign from the chat, with the model's calls
// scripted, through real turns and the real store: a model that leaves
// ab_test out keeps the test, a kept B that no longer fits sends the model
// back to update it, and only the separate remove_ab_test call removes it.

var abB = &abtest.Test{Variants: []language.Variant{{Language: "tr",
	SMS:   &content.SMS{Text: "Son gün {{first_name}}! Sepetin seni bekliyor."},
	Email: &content.Email{Subject: "Son gün: sepetin seni bekliyor", Body: "Ürünlerin hâlâ sepetinde."}}}}

// addABDraft creates version 1 of an A/B draft in chat: a Turkish A on SMS
// and email, and abB.
func addABDraft(t *testing.T, db *testutil.DB, chatID string) int64 {
	t.Helper()
	s, err := campaign.NewStore(db.Pool).Drafts(campaign.Author{ChatID: chatID, Source: "user"}).SaveDraft(context.Background(),
		campaign.DraftCmd{Name: "Sepet A/B", Rule: segment.Rule{Match: "all", Conditions: []segment.Condition{
			{Kind: "event", EventType: "cart_add", WithinDays: 30, MinCount: 1, NotFollowedBy: "purchase"}}},
			SMS:          &content.SMS{Text: "Merhaba {{first_name}}, sepetin seni bekliyor!"},
			Email:        &content.Email{Subject: "Sepetin seni bekliyor", Body: "Ürünlerin hâlâ sepetinde."},
			Localization: &language.Settings{Mode: language.ModeSingle, Language: "tr"}, ABTest: abB, ChangeSummary: "ilk"})
	if err != nil {
		t.Fatal(err)
	}
	return s.CampaignID
}

// abRevision is the model's change to the draft id at base: the whole A
// side in lang, and ab_test only when ab is not empty.
func abRevision(id int64, base int, lang, sms, subject, body, ab string) string {
	in := `{"campaign_id":` + itoa(id) + `,"base_version":` + strconv.Itoa(base) + `,"name":"Sepet A/B",
 "segment_rule":{"match":"all","conditions":[{"kind":"event","event_type":"cart_add","within_days":30,"not_followed_by":"purchase"}]},
 "sms":{"text":"` + sms + `"},"email":{"subject":"` + subject + `","body":"` + body + `"},
 "content_language":"` + lang + `","change_summary":"İçerik güncellendi"`
	if ab != "" {
		in += `,"ab_test":` + ab
	}
	return in + "}"
}

// toolResultIn is the tool result the model got back in req.
func toolResultIn(t *testing.T, req llm.Request) llm.Block {
	t.Helper()
	last := req.Messages[len(req.Messages)-1]
	if len(last.Blocks) == 0 || last.Blocks[0].Type != llm.BlockToolResult {
		t.Fatalf("last message is not a tool result: %+v", last)
	}
	return last.Blocks[0]
}

// editRunner routes every turn to campaign id as an edit.
func editRunner(t *testing.T, db *testutil.DB, main llm.Client, id int64) *turns.Runner {
	t.Helper()
	return newRoutedRunner(t, db, main, mock.NewScripted(routeCall(t, "edit", "", id)))
}

// Case 1: the model changes the A side and leaves ab_test out. The new
// version keeps the campaign's B side as it was, and the model hears so.
func TestTurn_ABTest_LeftOutIsKept(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	id := addABDraft(t, db, chatID)
	main := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: abRevision(id, 1, "tr",
			"Merhaba {{first_name}}, sepetini unutma!", "Sepetin seni bekliyor", "Ürünlerin hâlâ sepetinde.", "")},
		mock.Call{Text: "SMS'i değiştirdim."},
	)
	res, _ := runTurn(t, editRunner(t, db, main, id), chatID, "SMS'i değiştir")
	if res.Outcome != "ok" || len(res.CampaignRefs) != 1 {
		t.Fatalf("turn = %+v", res)
	}
	if v := currentVersion(t, db, id); v.Version != 2 || !strings.Contains(string(v.SMS), "sepetini unutma") {
		t.Fatalf("version %d, sms %s; want the change saved as version 2", v.Version, v.SMS)
	}
	if _, ab := currentABTest(t, db, id); !reflect.DeepEqual(ab, abB) {
		t.Fatalf("B side after the change = %+v; want it kept as it was", ab)
	}
	if tr := toolResultIn(t, main.Requests()[1]); tr.IsError || !strings.Contains(tr.Text, "ab_test_note") {
		t.Fatalf("tool result = %+v; want the save confirmed with a note that the test was kept", tr)
	}
}

// Case 2: the model turns the content into English and leaves ab_test out.
// The kept Turkish B no longer fits the new A, so the save is refused with
// a validation error that asks for an updated B. The model sends one, and
// only that draft is written.
func TestTurn_ABTest_LeftOutButNoLongerFits_ModelUpdatesB(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	id := addABDraft(t, db, chatID)
	enB := `{"variants":[{"language":"en","sms":{"text":"Last day {{first_name}}! Your cart is waiting."},` +
		`"email":{"subject":"Last day: your cart is waiting","body":"Your items are still in your cart."}}]}`
	english := func(ab string) string {
		return abRevision(id, 1, "en", "Hi {{first_name}}, your cart is waiting!", "Your cart is waiting", "Your items are still in your cart.", ab)
	}
	main := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: english("")},
		mock.Call{ToolName: "save_campaign_draft", ToolInput: english(enB)},
		mock.Call{Text: "İçeriği İngilizceye çevirdim, B'yi de güncelledim."},
	)
	res, rec := runTurn(t, editRunner(t, db, main, id), chatID, "İngilizce yap")
	if res.Outcome != "ok" || len(res.CampaignRefs) != 1 {
		t.Fatalf("turn = %+v", res)
	}
	refused := toolResultIn(t, main.Requests()[1])
	for _, want := range []string{"ab_test_outdated", "missing_variant", "update", "remove_ab_test"} {
		if !refused.IsError || !strings.Contains(refused.Text, want) {
			t.Errorf("first tool result lacks %q: %+v", want, refused)
		}
	}
	if rec.index("draft.invalid") < 0 {
		t.Error("the live preview must show why the first draft was refused")
	}
	if n := count(t, db, `SELECT count(*) FROM campaign_versions WHERE campaign_id = $1`, id); n != 2 {
		t.Fatalf("%d versions; the refused draft must not be written", n)
	}
	if _, ab := currentABTest(t, db, id); ab == nil || len(ab.Variants) != 1 || ab.Variants[0].Language != "en" ||
		!strings.HasPrefix(ab.Variants[0].SMS.Text, "Last day") {
		t.Fatalf("B side = %+v; want the model's updated English B", ab)
	}
}

// Case 3: the user asks to remove the test and the model makes the separate
// remove_ab_test call. The new version is the old one without its B side;
// nothing else changes.
func TestTurn_ABTest_RemovedOnlyByItsOwnCall(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	id := addABDraft(t, db, chatID)
	before := currentVersion(t, db, id)
	main := mock.NewScripted(
		mock.Call{ToolName: "remove_ab_test", ToolInput: `{"campaign_id":` + itoa(id) + `,"base_version":1,"change_summary":"A/B testi kaldırıldı"}`},
		mock.Call{Text: "A/B testini kaldırdım; herkes A metnini alacak."},
	)
	res, rec := runTurn(t, editRunner(t, db, main, id), chatID, "A/B testini kaldır")
	if res.Outcome != "ok" || len(res.CampaignRefs) != 1 || res.CampaignRefs[0].Action != "revised" {
		t.Fatalf("turn = %+v", res)
	}
	if _, ab := currentABTest(t, db, id); ab != nil {
		t.Fatalf("B side = %+v; want the test removed", ab)
	}
	after := currentVersion(t, db, id)
	if after.Version != 2 || string(after.SMS) != string(before.SMS) || string(after.Email) != string(before.Email) ||
		string(after.SegmentRule) != string(before.SegmentRule) {
		t.Fatalf("version %d = %+v; want version 2 with the content and rule of version 1", after.Version, after)
	}
	if tr := toolResultIn(t, main.Requests()[1]); tr.IsError || !strings.Contains(tr.Text, "ab_test_removed") {
		t.Fatalf("tool result = %+v", tr)
	}
	if rec.index("draft.saved") < 0 {
		t.Error("the live preview must show the written version")
	}
	if tools := main.Requests()[0].Tools; !slices.ContainsFunc(tools, func(tl llm.Tool) bool { return tl.Name == "remove_ab_test" }) {
		t.Fatalf("tools offered = %+v; want remove_ab_test among them", tools)
	}
}
