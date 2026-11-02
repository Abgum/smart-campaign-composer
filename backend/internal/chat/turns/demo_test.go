package turns_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/chat/turns"
	"smart-campaign-composer/backend/internal/llm/mock"
	"smart-campaign-composer/backend/internal/testutil"
)

// The demo model plays the router and the main model when the app runs
// without Claude. These tests run real turns with it: what it proposes goes
// through the same Go checks and is saved the same way, so a demo without an
// API key shows the real flow.

func newDemoRunner(t *testing.T, db *testutil.DB) *turns.Runner {
	t.Helper()
	demo := mock.NewDemo("test")
	demo.SlowDelay = time.Millisecond
	return newRoutedRunner(t, db, demo, demo)
}

// runFocused begins and executes a turn with a selected tab.
func runFocused(t *testing.T, r *turns.Runner, chatID, text string, focus *int64) turns.TurnResult {
	t.Helper()
	ctx := context.Background()
	begun, err := r.Begin(ctx, turns.BeginRequest{ChatID: chatID, ClientTurnID: uuid(), Text: text, FocusCampaignID: focus})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	r.Execute(ctx, begun.TurnID, &recorder{})
	res, err := r.Result(ctx, begun.TurnID)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	return res
}

type storedVersion struct {
	Version     int
	SegmentRule json.RawMessage
	SMS         json.RawMessage
	Email       json.RawMessage
}

func currentVersion(t *testing.T, db *testutil.DB, campaignID int64) storedVersion {
	t.Helper()
	var v storedVersion
	if err := db.Pool.QueryRow(context.Background(), `
		SELECT v.version, v.segment_rule, v.sms, v.email FROM campaigns c JOIN campaign_versions v ON v.id = c.current_version_id
		WHERE c.id = $1`, campaignID).Scan(&v.Version, &v.SegmentRule, &v.SMS, &v.Email); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestDemo_FirstBrief_CreatesValidDraft(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	res := runFocused(t, newDemoRunner(t, db), chatID, "İstanbul'da sepete ürün ekleyip satın almayanlara SMS gönder", nil)

	if res.Outcome != "ok" || len(res.CampaignRefs) != 1 || res.CampaignRefs[0].Action != "created" {
		t.Fatalf("result = %+v; want one created draft", res)
	}
	v := currentVersion(t, db, res.CampaignRefs[0].CampaignID)
	if !strings.Contains(string(v.SegmentRule), `"not_followed_by": "purchase"`) || len(v.SMS) == 0 {
		t.Fatalf("draft = rule %s, sms %s; want an abandoned-cart rule and an SMS", v.SegmentRule, v.SMS)
	}
	if m := res.AssistantMessage; m == nil || m.LLMProvider != "mock" || !strings.Contains(m.Text, "Kampanya 1 v1") {
		t.Fatalf("assistant = %+v; want the mock's summary of what was saved", m)
	}
}

// The demo model does not read the brief: a new campaign is the next canned
// sample, and its answer says so instead of passing it off as a reading.
func TestDemo_NewDraftSaysItIsCanned(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	res := runFocused(t, newDemoRunner(t, db), chatID, "Antalya için yaz kampanyası", nil)
	if m := res.AssistantMessage; m == nil || !strings.Contains(m.Text, "hazır demo taslağı") || !strings.Contains(m.Text, "isteğiniz yorumlanmadı") {
		t.Fatalf("assistant = %+v; want it to say the draft is a canned demo, not a reading of the request", m)
	}
}

// The demo router edits the selected tab; the edit keeps what the campaign
// already says (a manual edit included) and changes only its own part.
func TestDemo_EditsSelectedTab(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	a, b := addDraft(t, db, chatID, "İstanbul Sepet"), addDraft(t, db, chatID, "İzmir Yaz")
	res := runFocused(t, newDemoRunner(t, db), chatID, "SMS'e son gün vurgusu ekle", &b)

	if res.Outcome != "ok" || len(res.CampaignRefs) != 1 || res.CampaignRefs[0].CampaignID != b || res.CampaignRefs[0].Version != 2 {
		t.Fatalf("result = %+v; want campaign %d at v2", res, b)
	}
	if v := currentVersion(t, db, b); !strings.Contains(string(v.SMS), "İzmir Yaz SMS") {
		t.Fatalf("v2 sms = %s; the existing text must be kept", v.SMS)
	}
	if v := currentVersion(t, db, a); v.Version != 1 {
		t.Fatalf("campaign %d changed to v%d", a, v.Version)
	}
}

// With several drafts and no tab, the demo router asks instead of guessing.
func TestDemo_SeveralDraftsNoTab_Asks(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	addDraft(t, db, chatID, "İstanbul Sepet")
	addDraft(t, db, chatID, "İzmir Yaz")
	res := runFocused(t, newDemoRunner(t, db), chatID, "SMS'i kısalt", nil)

	if res.Route == nil || res.Route.Intent != "clarify" || len(res.Route.Candidates) != 2 || len(res.CampaignRefs) != 0 {
		t.Fatalf("result = %+v, route %+v; want a question with two candidates", res, res.Route)
	}
}

func TestDemo_Scenarios(t *testing.T) {
	cases := []struct {
		scenario string
		check    func(t *testing.T, db *testutil.DB, res turns.TurnResult)
	}{
		{"repair", func(t *testing.T, db *testutil.DB, res turns.TurnResult) {
			if res.Outcome != "ok" || len(res.CampaignRefs) != 1 ||
				count(t, db, `SELECT count(*) FROM llm_calls WHERE turn_id = $1 AND kind = 'main'`, res.TurnID) != 3 {
				t.Fatalf("result = %+v; want a rejected draft, its repair and a summary", res)
			}
		}},
		{"unknown_city", func(t *testing.T, _ *testutil.DB, res turns.TurnResult) {
			if res.Outcome != "ok" || len(res.CampaignRefs) != 0 || !strings.Contains(res.AssistantMessage.Text, "Atlantis") {
				t.Fatalf("result = %+v; want a question about the unknown place and nothing saved", res)
			}
		}},
		{"refusal", func(t *testing.T, _ *testutil.DB, res turns.TurnResult) {
			if res.Outcome != "failed" || res.ErrorCode != "refusal" {
				t.Fatalf("result = %+v", res)
			}
		}},
		{"max_tokens", func(t *testing.T, db *testutil.DB, res turns.TurnResult) {
			if res.Outcome != "ok" || len(res.CampaignRefs) != 1 ||
				count(t, db, `SELECT count(*) FROM llm_calls WHERE turn_id = $1 AND stop_reason = 'max_tokens'`, res.TurnID) != 1 {
				t.Fatalf("result = %+v; want one truncated answer, retried", res)
			}
		}},
		{"save_then_llm_error", func(t *testing.T, _ *testutil.DB, res turns.TurnResult) {
			if res.Outcome != "partial" || len(res.CampaignRefs) != 1 || res.ErrorCode != "llm_unavailable" {
				t.Fatalf("result = %+v; want the saved draft kept and the turn partial", res)
			}
		}},
		{"error_mid_stream", func(t *testing.T, _ *testutil.DB, res turns.TurnResult) {
			if res.Outcome != "failed" || len(res.CampaignRefs) != 0 || res.ErrorCode == "" {
				t.Fatalf("result = %+v; want a failed turn without side effects", res)
			}
		}},
		{"slow_stream", func(t *testing.T, _ *testutil.DB, res turns.TurnResult) {
			if res.Outcome != "ok" || len(res.CampaignRefs) != 1 {
				t.Fatalf("result = %+v", res)
			}
		}},
		{"unknown_scenario", func(t *testing.T, _ *testutil.DB, res turns.TurnResult) {
			if res.Outcome != "ok" || len(res.CampaignRefs) != 0 || !strings.Contains(res.AssistantMessage.Text, "repair") {
				t.Fatalf("result = %+v; an unknown scenario must list the known ones", res)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.scenario, func(t *testing.T) {
			db := testutil.NewDB(t)
			chatID := newChat(t, db)
			res := runFocused(t, newDemoRunner(t, db), chatID, "İstanbul için sepet hatırlatması #scenario:"+c.scenario, nil)
			c.check(t, db, res)
		})
	}
}

// Router scenarios: a new campaign beside existing ones, an exact copy (for
// the duplicate-content demo), and a forced question.
func TestDemo_RouterScenarios(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	a := addDraft(t, db, chatID, "İstanbul Sepet")
	r := newDemoRunner(t, db)

	res := runFocused(t, r, chatID, "Ankara için hoş geldin kampanyası #scenario:new_campaign", nil)
	if len(res.CampaignRefs) != 1 || res.CampaignRefs[0].Action != "created" || res.CampaignRefs[0].Ordinal != 2 {
		t.Fatalf("yeni: %+v; want campaign 2 created", res)
	}

	res = runFocused(t, r, chatID, "Bunun aynısından bir tane daha #scenario:duplicate", &a)
	if len(res.CampaignRefs) != 1 || res.CampaignRefs[0].Action != "created" {
		t.Fatalf("kopya: %+v; want a new campaign", res)
	}
	src, cp := currentVersion(t, db, a), currentVersion(t, db, res.CampaignRefs[0].CampaignID)
	if string(src.SMS) != string(cp.SMS) || string(src.SegmentRule) != string(cp.SegmentRule) {
		t.Fatalf("copy = %s %s; want the source's rule and content %s %s", cp.SegmentRule, cp.SMS, src.SegmentRule, src.SMS)
	}

	res = runFocused(t, r, chatID, "SMS'i kısalt #scenario:ambiguous", &a)
	if res.Route == nil || res.Route.Intent != "clarify" || len(res.CampaignRefs) != 0 {
		t.Fatalf("belirsiz: %+v; want a question", res)
	}
}

// Go's notes about a failed model call name the model that answered: with
// the demo model they must not say Claude.
func TestDemo_NotesDoNotClaimClaude(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	res := runFocused(t, newDemoRunner(t, db), chatID, "Deneme #scenario:save_then_llm_error", nil)
	if txt := res.AssistantMessage.Text; strings.Contains(txt, "Claude") || !strings.Contains(txt, "Mock modelin yanıtı tamamlanamadı") {
		t.Fatalf("partial note = %q; with the mock it must name the mock model, not Claude", txt)
	}
}
