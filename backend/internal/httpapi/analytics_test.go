package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/llm/mock"
	"smart-campaign-composer/backend/internal/segment"
)

func TestAnalyticsEndpoint(t *testing.T) {
	e := newEnv(t, mock.NewScripted())
	e.approveOne(t)

	code, body := e.do(t, http.MethodGet, "/api/analytics?range=7d", nil)
	window, _ := body["window"].(map[string]any)
	kpis, _ := body["kpis"].(map[string]any)
	queued, _ := kpis["queued"].(map[string]any)
	series, _ := body["series"].([]any)
	if code != http.StatusOK || window["range"] != "7d" || window["tz"] != "Europe/Istanbul" || queued["total"] != float64(1) || len(series) != 7 {
		t.Fatalf("GET /api/analytics: %d %v", code, body)
	}
	for _, bad := range []string{"range=yesterday", "channel=fax", "cities=0"} {
		if code, body := e.do(t, http.MethodGet, "/api/analytics?"+bad, nil); code != http.StatusBadRequest || body["error"] != "invalid_filter" {
			t.Errorf("?%s: %d %v; want 400 invalid_filter", bad, code, body)
		}
	}
}

// draftIn saves a draft campaign named name in chat.
func (e *env) draftIn(t *testing.T, chat, name string) int64 {
	t.Helper()
	saved, err := campaign.NewStore(e.db.Pool).Drafts(campaign.Author{ChatID: chat, Source: "user"}).SaveDraft(context.Background(),
		campaign.DraftCmd{Name: name, SMS: &content.SMS{Text: "Merhaba {{first_name}}"}, ChangeSummary: "ilk",
			Rule: segment.Rule{Match: "all", Conditions: []segment.Condition{{Kind: "location", Operator: "in",
				Places: []segment.PlaceRef{{Level: "city", GeonameID: 745044, Country: "TR", Name: "İstanbul"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	return saved.CampaignID
}

// Campaigns are listed across chats, and narrow by status.
func TestCampaignsList(t *testing.T) {
	e := newEnv(t, mock.NewScripted())
	approved := e.approveOne(t)
	draft := e.draftIn(t, e.createChat(t), "Taslak")

	code, body := e.do(t, http.MethodGet, "/api/campaigns", nil)
	if list, _ := body["campaigns"].([]any); code != http.StatusOK || len(list) != 2 {
		t.Fatalf("GET /api/campaigns: %d %v; want both campaigns", code, body)
	}
	for status, internal := range map[string]int64{"draft": draft, "active": approved} {
		want := publicCampaignID(t, e, internal)
		code, body := e.do(t, http.MethodGet, "/api/campaigns?status="+status, nil)
		list, _ := body["campaigns"].([]any)
		if code != http.StatusOK || len(list) != 1 || list[0].(map[string]any)["id"] != want {
			t.Errorf("?status=%s: %d %v; want campaign %s", status, code, body, want)
		}
	}
	if code, body := e.do(t, http.MethodGet, "/api/campaigns?status=nope", nil); code != http.StatusBadRequest || body["error"] != "invalid_filter" {
		t.Errorf("?status=nope: %d %v; want 400 invalid_filter", code, body)
	}
	// one campaign by id, as its page asks for it
	publicDraft := publicCampaignID(t, e, draft)
	code, body = e.do(t, http.MethodGet, "/api/campaigns?id="+publicDraft, nil)
	if list, _ := body["campaigns"].([]any); code != http.StatusOK || len(list) != 1 || list[0].(map[string]any)["id"] != publicDraft {
		t.Errorf("?id=%s: %d %v; want only that campaign", publicDraft, code, body)
	}
	if code, body := e.do(t, http.MethodGet, "/api/campaigns?id=abc", nil); code != http.StatusBadRequest || body["error"] != "invalid_filter" {
		t.Errorf("?id=abc: %d %v; want 400 invalid_filter", code, body)
	}
}

// The sidebar lists recent chats the user has written in, named after
// their first campaign (or first message); chats never written in are not
// listed.
func TestChatsList(t *testing.T) {
	e := newEnv(t, mock.NewScripted(mock.Call{Text: reply}, mock.Call{Text: reply}))
	ctx := context.Background()
	talked := e.createChat(t)
	resp := e.postTurn(t, ctx, talked, uuid(), "Merhaba, kampanya hazırlayalım")
	readEvents(t, resp.Body, nil)
	resp.Body.Close()
	e.createChat(t) // opened, never written in

	code, body := e.do(t, http.MethodGet, "/api/chats", nil)
	chats, _ := body["chats"].([]any)
	if code != http.StatusOK || len(chats) != 1 {
		t.Fatalf("GET /api/chats: %d %v; want the one chat written in", code, body)
	}
	if c := chats[0].(map[string]any); c["id"] != talked || c["title"] != "Merhaba, kampanya hazırlayalım" {
		t.Fatalf("chat = %v", c)
	}
	e.draftIn(t, talked, "İstanbul sepet")
	_, body = e.do(t, http.MethodGet, "/api/chats", nil)
	if c := body["chats"].([]any)[0].(map[string]any); c["title"] != "İstanbul sepet" {
		t.Fatalf("title = %v; want the first campaign's name", c["title"])
	}
}

// The overview shows marketers' latest requests: what they asked, when, and
// what came of it (the campaign it created or changed, or a question back),
// newest first.
func TestRequestsList(t *testing.T) {
	e := newEnv(t, mock.NewDemo("test"))
	ctx := context.Background()
	chat := e.createChat(t)
	brief := "İstanbul'da sepete ürün ekleyip satın almayanlara SMS hazırla"
	for _, text := range []string{brief, "SMS'i kısalt"} {
		resp := e.postTurn(t, ctx, chat, uuid(), text)
		readEvents(t, resp.Body, nil)
		resp.Body.Close()
	}

	code, body := e.do(t, http.MethodGet, "/api/requests?limit=5", nil)
	list, _ := body["requests"].([]any)
	if code != http.StatusOK || len(list) != 2 {
		t.Fatalf("GET /api/requests: %d %v; want both requests", code, body)
	}
	newest, oldest := list[0].(map[string]any), list[1].(map[string]any)
	if newest["text"] != "SMS'i kısalt" || newest["intent"] != "clarify" || len(newest["campaigns"].([]any)) != 0 {
		t.Errorf("newest = %v; want the question back, no campaign", newest)
	}
	refs, _ := oldest["campaigns"].([]any)
	if oldest["text"] != brief || oldest["chat_id"] != chat || oldest["intent"] != "create" || oldest["status"] != "completed" || len(refs) != 1 {
		t.Fatalf("oldest = %v; want the brief and the campaign it created", oldest)
	}
	if ref := refs[0].(map[string]any); ref["name"] != "İstanbul sepet hatırlatma" || ref["action"] != "created" || ref["version"] != float64(1) {
		t.Errorf("campaign = %v", ref)
	}
	// the mock plays a canned draft; the UI must be able to tell it apart from a model's reading
	if oldest["llm_provider"] != "mock" || newest["llm_provider"] != "mock" {
		t.Errorf("llm_provider = %v, %v; want mock for both", oldest["llm_provider"], newest["llm_provider"])
	}
	if code, body := e.do(t, http.MethodGet, "/api/requests?limit=0", nil); code != http.StatusBadRequest || body["error"] != "invalid_filter" {
		t.Errorf("?limit=0: %d %v; want 400 invalid_filter", code, body)
	}
}
