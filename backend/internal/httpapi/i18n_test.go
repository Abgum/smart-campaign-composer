package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"smart-campaign-composer/backend/internal/llm/mock"
)

// doLang is do with an Accept-Language header.
func (e *env) doLang(t *testing.T, method, path string, body any, lang string) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Language", lang)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

var turkish = regexp.MustCompile(`[çğıöşüÇĞİÖŞÜ]`)

// Error answers speak the language the request asks for, Turkish by default;
// the error codes stay the same in both.
func TestErrors_SpeakTheRequestLanguage(t *testing.T) {
	e := newEnv(t, mock.NewScripted())
	missing := "/api/campaigns/00000000-0000-4000-8000-000000000000"
	if code, body := e.do(t, http.MethodGet, missing, nil); code != http.StatusNotFound || body["message"] != "kampanya bulunamadı" {
		t.Errorf("default: %d %v; want 404 kampanya bulunamadı", code, body)
	}
	if code, body := e.doLang(t, http.MethodGet, missing, nil, "en-US,en;q=0.9"); code != http.StatusNotFound ||
		body["error"] != "campaign_not_found" || body["message"] != "campaign not found" {
		t.Errorf("en: %d %v; want 404 campaign_not_found, campaign not found", code, body)
	}
	if _, body := e.do(t, http.MethodGet, "/api/analytics?range=nope", nil); !strings.HasPrefix(body["message"].(string), "geçersiz filtre") {
		t.Errorf("filter error in Turkish = %v", body["message"])
	}
	if _, body := e.doLang(t, http.MethodGet, "/api/analytics?range=nope", nil, "en"); !strings.HasPrefix(body["message"].(string), "invalid filter") {
		t.Errorf("filter error in English = %v", body["message"])
	}
}

func TestValidationIssues_SpeakTheRequestLanguage(t *testing.T) {
	e := newEnv(t, mock.NewScripted())
	draft := map[string]any{
		"chat_id":      e.createChat(t),
		"name":         "Dil testi",
		"segment_rule": map[string]any{"match": "all", "conditions": []any{map[string]any{"kind": "attribute", "field": "age", "operator": "gte", "values": []string{"18"}}}},
		"sms":          map[string]any{"text": "Merhaba {{ad}}"},
	}
	code, body := e.doLang(t, http.MethodPost, "/api/campaigns", draft, "en")
	issues, _ := body["issues"].([]any)
	if code != http.StatusUnprocessableEntity || len(issues) == 0 {
		t.Fatalf("POST /api/campaigns: %d %v; want 422 with issues", code, body)
	}
	msg := issues[0].(map[string]any)["message"].(string)
	if msg != `"{{ad}}" is not supported; allowed variables: {{first_name}}, {{last_name}}, {{city}}` {
		t.Errorf("issue in English = %q", msg)
	}
}

func TestTurnErrorEvent_SpeaksTheRequestLanguage(t *testing.T) {
	e := newEnv(t, mock.NewDemo("test"))
	ctx := context.Background()
	b, _ := json.Marshal(map[string]any{"client_turn_id": uuid(), "text": "SMS hazırla #scenario:error_mid_stream"})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.srv.URL+"/api/chats/"+e.createChat(t)+"/turns", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Language", "en")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var message string
	for _, ev := range readEvents(t, resp.Body, nil) {
		if ev.Type == "error" {
			var data struct{ Message string }
			_ = json.Unmarshal(ev.Data, &data)
			message = data.Message
		}
	}
	if message == "" || turkish.MatchString(message) {
		t.Errorf("error event message = %q; want it in English", message)
	}
}

// The mock's scenario list is shown in the UI, so it speaks the request's
// language too.
func TestMetaScenarios_SpeakTheRequestLanguage(t *testing.T) {
	e := newEnv(t, mock.NewDemo("test"))
	_, body := e.doLang(t, http.MethodGet, "/api/meta", nil, "en")
	list, _ := body["mock_scenarios"].([]any)
	if len(list) == 0 {
		t.Fatalf("meta = %v; want the mock scenarios", body)
	}
	for _, s := range list {
		d := s.(map[string]any)["description"].(string)
		if turkish.MatchString(d) {
			t.Errorf("scenario %v: description %q is not in English", s.(map[string]any)["name"], d)
		}
	}
}

// A turn remembers the interface language of the request that started it, so
// Go's own notes in the transcript can be written in it.
func TestPostTurn_StoresTheInterfaceLanguage(t *testing.T) {
	e := newEnv(t, mock.NewScripted(mock.Call{Text: "Merhaba"}))
	chatID := e.createChat(t)
	code, _ := e.doLang(t, http.MethodPost, "/api/chats/"+chatID+"/turns",
		map[string]any{"client_turn_id": "00000000-0000-4000-8000-000000000123", "text": "selam"}, "en-US,en;q=0.9")
	if code != http.StatusOK {
		t.Fatalf("post turn: %d", code)
	}
	var lang *string
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT lang FROM turns WHERE chat_id = $1`, chatID).Scan(&lang); err != nil {
		t.Fatal(err)
	}
	if lang == nil || *lang != "en" {
		t.Fatalf("turn language = %v, want en", lang)
	}
}
