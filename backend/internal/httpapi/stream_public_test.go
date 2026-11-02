package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/chat/agentloop"
	"smart-campaign-composer/backend/internal/chat/turns"
)

// The live preview streams the email body like the SMS and the subject, and
// always shows A: an A/B draft's B side (ab_test) is never taken for A,
// wherever in the tool input the model streams it.
func TestDraftPreviewSnapshot_EmailBodyAndASide(t *testing.T) {
	type snapshot struct {
		SMS   *struct{ Text string }
		Email *struct{ Subject, Body string }
	}
	read := func(raw string) snapshot {
		t.Helper()
		var s snapshot
		if err := json.Unmarshal([]byte(draftPreviewSnapshot(raw)), &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	got := read(`{"campaign_id":1,"base_version":2,"name":"Sepet","segment_rule":{"match":"all","conditions":[]},` +
		`"sms":null,"email":{"subject":"Konu A","body":"Merhaba {{first_name}},\nsepetin seni bek`)
	if got.Email == nil || got.Email.Subject != "Konu A" || got.Email.Body != "Merhaba {{first_name}},\nsepetin seni bek" {
		t.Fatalf("email = %+v; want the subject and the body streamed so far", got.Email)
	}

	for name, raw := range map[string]string{
		"B streamed first": `{"ab_test":{"variants":[{"language":"tr","sms":{"text":"B SMS"},"email":{"subject":"B konu","body":"B gövde"}}]},` +
			`"name":"Sepet","sms":{"text":"A SMS"},"email":{"subject":"A konu","body":"A gövde"}}`,
		"per_recipient after B": `{"name":"Sepet","sms":null,"email":null,"content_language":"","change_summary":"c",` +
			`"ab_test":{"variants":[{"language":"tr","sms":{"text":"B SMS"},"email":{"subject":"B konu","body":"B gövde"}}]},` +
			`"per_recipient":{"variants":[{"language":"tr","sms":{"text":"A SMS"},"email":{"subject":"A konu","body":"A gövde"}}]}}`,
	} {
		got := read(raw)
		if got.SMS == nil || got.SMS.Text != "A SMS" || got.Email == nil || got.Email.Subject != "A konu" || got.Email.Body != "A gövde" {
			t.Errorf("%s: preview = sms %+v email %+v; want the A side", name, got.SMS, got.Email)
		}
	}
	if got := read(`{"name":"Sepet","sms":{"text":"A SMS"},"ab_test":{"variants":[{"language":"tr","sms":{"text":"B S`); got.SMS == nil || got.SMS.Text != "A SMS" {
		t.Errorf("while B streams: sms %+v; want A", got.SMS)
	}
}

func TestStream_DraftPreviewDoesNotExposeInternalCampaignID(t *testing.T) {
	s := &Server{d: Deps{WriteTimeout: time.Second}}
	sink := newStreamSink(10)
	sink.Emit(agentloop.Event{Type: agentloop.EventDraftStart, Data: map[string]string{"tool_use_id": "tool-1"}})
	for _, chunk := range []string{
		`{"campaign_id":47182,"name":"Se`,
		`pet","sms":{"text":"Mer`,
		`haba"},"email":{"subject":"Konu"}}`,
	} {
		sink.Emit(agentloop.Event{Type: agentloop.EventDraftDelta, Data: map[string]any{"tool_use_id": "tool-1", "partial_json": chunk}})
	}
	sink.Emit(agentloop.Event{Type: turns.EventDone, Data: map[string]string{"outcome": "ok"}})
	sink.close()
	w := httptest.NewRecorder()
	s.stream(w, httptest.NewRequest("GET", "/", nil), "turn-1", sink)
	if strings.Contains(w.Body.String(), "47182") {
		t.Fatalf("internal campaign id leaked in SSE: %s", w.Body.String())
	}
	var previews []string
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var data struct {
			PartialJSON string `json:"partial_json"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data) == nil && data.PartialJSON != "" {
			previews = append(previews, data.PartialJSON)
		}
	}
	if len(previews) != 3 || !strings.Contains(previews[0], "Se") ||
		!strings.Contains(previews[1], "Mer") || !strings.Contains(previews[2], "Merhaba") {
		t.Fatalf("safe name and SMS preview must still stream: %v", previews)
	}
}
