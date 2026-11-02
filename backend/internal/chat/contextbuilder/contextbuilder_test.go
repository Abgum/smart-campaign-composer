package contextbuilder

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/llm"
	"smart-campaign-composer/backend/internal/segment"
)

var today = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func okTurn(i int) HistoryTurn {
	return HistoryTurn{UserText: fmt.Sprintf("istek %d", i), AssistantText: fmt.Sprintf("yanıt %d", i), Outcome: "ok"}
}

func draft(id int64, ordinal int, name string, version int, source string) CampaignView {
	return CampaignView{
		ID: id, Ordinal: ordinal, Name: name, Status: "draft", Version: version, Source: source,
		UpdatedAt: today.Add(-time.Duration(ordinal) * time.Minute),
		Summary:   name + " özeti",
		Current: &Version{
			Version: version, Source: source,
			SegmentRule: segment.Rule{Match: "all", Conditions: []segment.Condition{
				{Kind: "event", EventType: "cart_add", WithinDays: 30, NotFollowedBy: "purchase"}}},
			SMS:      &content.SMS{Text: "Merhaba {{first_name}}"},
			Audience: segment.Audience{Total: 312, SMSReachable: 220, EmailReachable: 250},
		},
	}
}

func userTexts(req llm.Request) []string {
	var out []string
	for _, m := range req.Messages {
		if m.Role == llm.RoleUser {
			out = append(out, m.Blocks[len(m.Blocks)-1].Text)
		}
	}
	return out
}

func chatState(t *testing.T, req llm.Request) State {
	t.Helper()
	last := req.Messages[len(req.Messages)-1]
	if last.Role != llm.RoleUser || len(last.Blocks) != 2 {
		t.Fatalf("final message must be user [chat_state, text], got %+v", last)
	}
	raw := last.Blocks[0].Text
	if !strings.HasPrefix(raw, "<chat_state>\n") || !strings.HasSuffix(raw, "\n</chat_state>") {
		t.Fatalf("chat_state not wrapped in tags: %q", raw)
	}
	var s State
	body := strings.TrimSuffix(strings.TrimPrefix(raw, "<chat_state>\n"), "\n</chat_state>")
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatalf("chat_state JSON: %v\n%s", err, body)
	}
	return s
}

func TestBuild_WindowLastSixTurns(t *testing.T) {
	var hist []HistoryTurn
	for i := 1; i <= 9; i++ {
		hist = append(hist, okTurn(i))
	}
	req := Build(Input{History: hist, UserText: "yeni istek", Today: today})
	if got := len(req.Messages); got != 6*2+1 {
		t.Fatalf("messages = %d, want 13", got)
	}
	if got := req.Messages[0].Blocks[0].Text; got != "istek 4" {
		t.Fatalf("window starts at %q, want istek 4", got)
	}
	for i, m := range req.Messages {
		want := llm.RoleUser
		if i%2 == 1 {
			want = llm.RoleAssistant
		}
		if m.Role != want {
			t.Fatalf("message %d role %s, want alternating user/assistant", i, m.Role)
		}
	}
}

func TestBuild_ExcludesFailedTurnsWithoutSideEffects(t *testing.T) {
	hist := []HistoryTurn{
		okTurn(1),
		{UserText: "başarısız istek", AssistantText: "Yanıt üretilemedi.", Outcome: "failed"},
		{UserText: "kısmi istek", AssistantText: "Kampanya 2 v3 kaydedildi. Claude'un yanıtı tamamlanamadı.", Outcome: "partial",
			Refs: []Ref{{Ordinal: 2, Version: 3, Action: "revised", ChangeSummary: "SMS kısaltıldı"}}},
	}
	req := Build(Input{History: hist, UserText: "devam", Today: today})
	users := userTexts(req)
	for _, u := range users {
		if u == "başarısız istek" {
			t.Fatal("a failed turn without side effects must not enter the context")
		}
	}
	if len(users) != 3 || users[1] != "kısmi istek" {
		t.Fatalf("user messages = %v", users)
	}
}

func TestBuild_RefNotes(t *testing.T) {
	hist := []HistoryTurn{{UserText: "SMS'i kısalt", AssistantText: "Kısalttım.", Outcome: "ok",
		Refs: []Ref{{Ordinal: 2, Version: 3, Action: "revised", ChangeSummary: "SMS kısaltıldı"},
			{Ordinal: 3, Version: 1, Action: "created", ChangeSummary: "İlk taslak"}}}}
	req := Build(Input{History: hist, UserText: "teşekkürler", Today: today})
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(req.Messages))
	}
	got := req.Messages[1].Blocks[0].Text
	for _, want := range []string{"Kısalttım.", "[Kampanya 2 → v3 saved: SMS kısaltıldı]", "[Kampanya 3 → v1 created: İlk taslak]"} {
		if !strings.Contains(got, want) {
			t.Errorf("assistant message %q lacks %q", got, want)
		}
	}
}

// The prompt is written in English, catalog included.
func TestSystemPrompt_English(t *testing.T) {
	sys := SystemPrompt()
	for _, want := range []string{"You are a campaign assistant", "age (Age in years)", "cart_add (Added a product to the cart)"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	for _, old := range []string{"Kurallar:", "Hedef kitle kuralı", "İçerik:", "Yanıtların", "Sepete ürün ekleme"} {
		if strings.Contains(sys, old) {
			t.Errorf("system prompt still has Turkish %q", old)
		}
	}
}

// The reply follows the language of the user's message, the content the
// language asked for (else the message's); "İngilizce hazırla" is one
// English content for everyone. Content per recipient's language follows
// the per-recipient rules, and the model asks before deciding for a group.
func TestSystemPrompt_LanguageRules(t *testing.T) {
	sys := SystemPrompt()
	for _, want := range []string{
		"Reply in the language of the user's latest message",
		"otherwise in the language of the user's message",
		"one English content for every recipient",
		"tr, en, fr",
		"name languages and countries in words",
		"per_recipient",
		"country_languages for recipients without a supported recorded preference",
		"otherwise (no recorded preference, or an unsupported one) the country mapping",
		"cannot be reached on any of the campaign's channels never wait",
		"The user may change the country mapping by hand",
		"only after the user answered",
		"the recorded preference",
		// What the language counts are, and that they are the draft's.
		"the recipients the draft can reach when it is saved",
		"each person once",
		"say they are as of the draft",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	if strings.Contains(sys, "the marketer writes in Turkish") {
		t.Error("the prompt still says every reply and content is Turkish")
	}
	// An unsupported preference no longer waits in a group of its own.
	if strings.Contains(sys, "preference:<code>") || strings.Contains(sys, "never switched to the country's language") {
		t.Error("the prompt still keeps unsupported preferences out of the country mapping")
	}
}

func TestBuild_TokenBudget(t *testing.T) {
	big := strings.Repeat("ç", 4000)
	var hist []HistoryTurn
	for i := 0; i < 6; i++ {
		hist = append(hist, HistoryTurn{UserText: big, AssistantText: big, Outcome: "ok"})
	}
	req := Build(Input{History: hist, UserText: "son", Today: today})
	total := 0
	for _, m := range req.Messages[:len(req.Messages)-1] {
		for _, b := range m.Blocks {
			total += EstimateTokens(b.Text)
		}
	}
	if total > HistoryTokenBudget {
		t.Fatalf("history uses ~%d tokens, budget %d", total, HistoryTokenBudget)
	}
	if len(req.Messages) != 2*2+1 {
		t.Fatalf("messages = %d, want the 2 most recent turns + final", len(req.Messages))
	}
}

func TestBuild_TextOnlyHistory(t *testing.T) {
	req := Build(Input{History: []HistoryTurn{okTurn(1), okTurn(2)}, UserText: "x", Today: today,
		Campaigns: []CampaignView{draft(11, 1, "A", 2, "llm")},
		Targets:   Targets{Intent: "edit", Source: "router", Allowed: []int64{11}}})
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			if b.Type != llm.BlockText || b.Provider != nil {
				t.Fatalf("context must contain plain text blocks only, got %+v", b)
			}
		}
	}
}

func TestBuild_StaticSystemAndTools(t *testing.T) {
	a := Build(Input{UserText: "a", Today: today})
	b := Build(Input{History: []HistoryTurn{okTurn(1)}, UserText: "b", Today: today.AddDate(0, 0, 3),
		Campaigns: []CampaignView{draft(11, 1, "A", 1, "llm")}, Targets: Targets{Intent: "edit", Source: "router", Allowed: []int64{11}}})
	if a.System != b.System {
		t.Fatal("system prompt must be byte-identical across turns (prompt cache)")
	}
	if !reflect.DeepEqual(a.Tools, b.Tools) || len(a.Tools) != 2 || a.Tools[0].Name != "save_campaign_draft" || a.Tools[1].Name != "remove_ab_test" {
		t.Fatalf("tools must be the same two tools on every turn: %+v", a.Tools)
	}
	for _, want := range []string{"save_campaign_draft", "not_followed_by", "location", "geoname_id", "city", "total_spent", "160", "80", "{{first_name}}", "cart_add"} {
		if !strings.Contains(a.System, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	if strings.Contains(a.System, `"admin1"`) || !strings.Contains(a.System, "Do not broaden a province or region request") || !strings.Contains(a.System, "An unqualified place name such as İstanbul means the city") {
		t.Error("prompt must disallow region targeting without silently broadening it")
	}
	if strings.Contains(a.System, "2026-10") {
		t.Error("dates belong in chat_state, not in the cached system prompt")
	}
}

func TestBuild_ChatState(t *testing.T) {
	var camps []CampaignView
	for i := 1; i <= 25; i++ {
		c := draft(int64(100+i), i, fmt.Sprintf("Kampanya adı %d", i), 1, "llm")
		c.UpdatedAt = today.Add(time.Duration(i) * time.Minute) // higher ordinal = more recent
		camps = append(camps, c)
	}
	camps[23].Source, camps[23].Current.Source = "user", "user" // ordinal 24
	tg := Targets{Intent: "edit", Source: "router", Allowed: []int64{124, 125}}
	req := Build(Input{Campaigns: camps, Targets: tg, UserText: "kampanya 24 ve 25", Today: today})
	s := chatState(t, req)

	if s.Today != "2026-10-01" {
		t.Errorf("today = %q", s.Today)
	}
	if len(s.Campaigns) != MaxIndexedCampaigns {
		t.Fatalf("index has %d campaigns, want %d", len(s.Campaigns), MaxIndexedCampaigns)
	}
	if s.Campaigns[0].Ref != "Kampanya 6" || s.Campaigns[len(s.Campaigns)-1].Ref != "Kampanya 25" {
		t.Errorf("index must keep the 20 most recently updated, ordered by ordinal: %s … %s",
			s.Campaigns[0].Ref, s.Campaigns[len(s.Campaigns)-1].Ref)
	}
	if s.Target.Mode != "router" || !reflect.DeepEqual(s.Target.AllowedCampaignIDs, []int64{124, 125}) {
		t.Errorf("target = %+v", s.Target)
	}
	if len(s.TargetCampaigns) != 2 || s.TargetCampaigns[0].ID != 124 || s.TargetCampaigns[0].Source != "user" ||
		s.TargetCampaigns[0].SegmentRule.Conditions[0].NotFollowedBy != "purchase" || s.TargetCampaigns[0].Audience.Total != 312 {
		t.Errorf("target campaigns = %+v", s.TargetCampaigns)
	}
	if last := req.Messages[len(req.Messages)-1].Blocks[1].Text; last != "kampanya 24 ve 25" {
		t.Errorf("user text must be the final block, got %q", last)
	}
}

func TestBuild_ChatState_AmbiguousAndCappedTargets(t *testing.T) {
	camps := []CampaignView{draft(1, 1, "A", 1, "llm"), draft(2, 2, "B", 1, "llm"), draft(3, 3, "C", 1, "llm"), draft(4, 4, "D", 1, "llm")}
	s := chatState(t, Build(Input{Campaigns: camps, UserText: "x", Today: today,
		Targets: Targets{Intent: "clarify", Source: "router", Candidates: []int64{1, 2}}}))
	if !s.Target.Ambiguous || !reflect.DeepEqual(s.Target.Candidates, []string{"Kampanya 1", "Kampanya 2"}) ||
		len(s.TargetCampaigns) != 0 {
		t.Fatalf("ambiguous state = %+v / %d targets", s.Target, len(s.TargetCampaigns))
	}

	s = chatState(t, Build(Input{Campaigns: camps, UserText: "x", Today: today,
		Targets: Targets{Intent: "edit", Source: "router", Allowed: []int64{1, 2, 3, 4}}}))
	if len(s.TargetCampaigns) != MaxFullTargets {
		t.Fatalf("full target versions = %d, want cap %d", len(s.TargetCampaigns), MaxFullTargets)
	}
	if len(s.Target.AllowedCampaignIDs) != MaxFullTargets {
		t.Fatalf("allowed ids = %v; only campaigns shown in full may be written", s.Target.AllowedCampaignIDs)
	}
	// The fourth explicitly named campaign must not vanish silently.
	if !reflect.DeepEqual(s.Target.Overflow, []string{"Kampanya 4"}) {
		t.Fatalf("overflow = %v, want [Kampanya 4]", s.Target.Overflow)
	}
	in := Input{Campaigns: camps, Targets: Targets{Intent: "edit", Source: "router", Allowed: []int64{1, 2, 3, 4}}}
	if got := OverflowTargets(in); !reflect.DeepEqual(got, []int64{4}) {
		t.Fatalf("OverflowTargets = %v, want [4]", got)
	}
}

func TestBuild_ChatState_AllowCreate(t *testing.T) {
	camps := []CampaignView{draft(1, 1, "A", 1, "llm"), draft(2, 2, "B", 1, "llm")}
	for _, allow := range []bool{true, false} {
		s := chatState(t, Build(Input{Campaigns: camps, UserText: "x", Today: today,
			Targets: Targets{Intent: "clarify", Source: "router", AllowCreate: allow}}))
		if s.Target.AllowCreate != allow {
			t.Fatalf("allow_create = %v, want %v", s.Target.AllowCreate, allow)
		}
	}
	sys := SystemPrompt()
	for _, want := range []string{"allow_create", "overflow"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt must explain %q", want)
		}
	}
}

func TestBuild_ChatState_IntentAndReadOnlyReferences(t *testing.T) {
	camps := []CampaignView{draft(1, 1, "A", 2, "llm"), draft(2, 2, "B", 1, "llm")}
	s := chatState(t, Build(Input{Campaigns: camps, UserText: "Kampanya 1'i Ankara için kopyala", Today: today,
		Targets: Targets{Intent: "create", Source: "router", References: []int64{1}, AllowCreate: true}}))
	if s.Target.Intent != "create" || !s.Target.AllowCreate || len(s.Target.AllowedCampaignIDs) != 0 {
		t.Fatalf("target = %+v", s.Target)
	}
	if len(s.TargetCampaigns) != 1 || s.TargetCampaigns[0].ID != 1 || !s.TargetCampaigns[0].ReadOnly {
		t.Fatalf("reference must be shown in full and marked read-only: %+v", s.TargetCampaigns)
	}
	sys := SystemPrompt()
	for _, want := range []string{"target.intent", "read_only"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt must explain %q", want)
		}
	}
}
