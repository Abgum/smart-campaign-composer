package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"smart-campaign-composer/backend/internal/llm/mock"
)

func publicCampaignID(t *testing.T, e *env, internal int64) string {
	t.Helper()
	var id string
	if err := e.db.Pool.QueryRow(context.Background(),
		`SELECT public_id::text FROM campaigns WHERE id = $1`, internal).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCampaignPublicUUIDInTurnAndLiveEvents(t *testing.T) {
	e := newEnv(t, mock.NewDemo("test"))
	chat := e.createChat(t)
	resp := e.postTurn(t, context.Background(), chat, uuid(), "İstanbul sepet kampanyası hazırla")
	events := readEvents(t, resp.Body, nil)
	resp.Body.Close()
	var turnID, liveID string
	for _, ev := range events {
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		switch ev.Type {
		case "turn.started":
			turnID, _ = data["turn_id"].(string)
		case "draft.saved":
			liveID, _ = data["campaign_id"].(string)
		}
	}
	if turnID == "" || len(liveID) != 36 {
		t.Fatalf("turn id %q, live campaign id %q; events=%v", turnID, liveID, events)
	}
	code, result := e.do(t, http.MethodGet, "/api/turns/"+turnID, nil)
	refs := result["campaign_refs"].([]any)
	if code != http.StatusOK || len(refs) != 1 || refs[0].(map[string]any)["campaign_id"] != liveID {
		t.Fatalf("turn result: %d %v", code, result)
	}
	code, body := e.do(t, http.MethodGet, "/api/requests", nil)
	requests := body["requests"].([]any)
	requestRefs := requests[0].(map[string]any)["campaigns"].([]any)
	if code != http.StatusOK || len(requestRefs) != 1 || requestRefs[0].(map[string]any)["campaign_id"] != liveID {
		t.Fatalf("requests: %d %v", code, body)
	}

	resp = e.postTurn(t, context.Background(), chat, uuid(), "SMS'i kısalt")
	events = readEvents(t, resp.Body, nil)
	resp.Body.Close()
	var routeID, ambiguousID string
	for _, ev := range events {
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		switch ev.Type {
		case "route":
			if candidates, ok := data["candidates"].([]any); ok && len(candidates) > 0 {
				routeID, _ = candidates[0].(map[string]any)["campaign_id"].(string)
			}
		case "target.ambiguous":
			if candidates, ok := data["candidates"].([]any); ok && len(candidates) > 0 {
				ambiguousID, _ = candidates[0].(string)
			}
		}
	}
	if routeID != liveID || ambiguousID != liveID {
		t.Fatalf("route id %q, ambiguous id %q; want %q", routeID, ambiguousID, liveID)
	}
	secondTurnID := turnIDOf(t, events)
	code, result = e.do(t, http.MethodGet, "/api/turns/"+secondTurnID, nil)
	storedRoute := result["route"].(map[string]any)
	storedCandidates := storedRoute["candidates"].([]any)
	if code != http.StatusOK || storedCandidates[0].(map[string]any)["campaign_id"] != liveID {
		t.Fatalf("stored route: %d %v", code, storedRoute)
	}
}

func TestCampaignPublicUUIDAtHTTPBoundary(t *testing.T) {
	e := newEnv(t, mock.NewScripted())
	chat := e.createChat(t)
	internal := e.draftIn(t, chat, "İstanbul sepet")
	public := publicCampaignID(t, e, internal)
	if len(public) != 36 {
		t.Fatalf("public id = %q", public)
	}

	code, body := e.do(t, http.MethodGet, "/api/chats/"+chat, nil)
	campaigns := body["campaigns"].([]any)
	if code != http.StatusOK || campaigns[0].(map[string]any)["id"] != public {
		t.Fatalf("chat campaign id: %d %v", code, body)
	}
	code, body = e.do(t, http.MethodGet, "/api/campaigns/"+public, nil)
	if code != http.StatusOK || body["id"] != public {
		t.Fatalf("campaign detail id: %d %v", code, body)
	}
	code, body = e.do(t, http.MethodGet, "/api/campaigns?id="+public, nil)
	list := body["campaigns"].([]any)
	if code != http.StatusOK || len(list) != 1 || list[0].(map[string]any)["id"] != public {
		t.Fatalf("campaign list id: %d %v", code, body)
	}
	if code, _ := e.do(t, http.MethodGet, "/api/campaigns/1", nil); code != http.StatusNotFound {
		t.Fatalf("numeric campaign URL = %d, want 404", code)
	}
	if code, _ := e.do(t, http.MethodGet, "/api/campaigns?id=1", nil); code != http.StatusBadRequest {
		t.Fatalf("numeric campaign filter = %d, want 400", code)
	}
	code, body = e.do(t, http.MethodGet, "/api/analytics?range=7d&campaign_id="+public, nil)
	if code != http.StatusOK {
		t.Fatalf("UUID analytics filter = %d: %v", code, body)
	}
	if code, _ := e.do(t, http.MethodGet, "/api/analytics?range=7d&campaign_id=1", nil); code != http.StatusBadRequest {
		t.Fatalf("numeric analytics filter = %d, want 400", code)
	}
}

func TestTurnFocusAcceptsOnlyCampaignUUIDFromSameChat(t *testing.T) {
	e := newEnv(t, mock.NewScripted(mock.Call{Text: "Tamam"}))
	chat := e.createChat(t)
	otherChat := e.createChat(t)
	selected := publicCampaignID(t, e, e.draftIn(t, chat, "İstanbul"))
	foreign := publicCampaignID(t, e, e.draftIn(t, otherChat, "Ankara"))
	post := func(focus any) *http.Response {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"client_turn_id": uuid(), "text": "SMS'i kısalt", "focus_campaign_id": focus})
		resp, err := http.Post(e.srv.URL+"/api/chats/"+chat+"/turns", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := post(1)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("numeric focus status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = post(foreign)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another chat's focus status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = post(selected)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("UUID focus status = %d", resp.StatusCode)
	}
	_ = readEvents(t, resp.Body, nil)
	resp.Body.Close()
}
