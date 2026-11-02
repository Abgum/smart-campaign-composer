package anthropic_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"smart-campaign-composer/backend/internal/llm"
	"smart-campaign-composer/backend/internal/llm/anthropic"
)

// ---- SSE fixture builders -------------------------------------------------

type ev = map[string]any

func sse(events ...ev) string {
	var b strings.Builder
	for _, e := range events {
		data, _ := json.Marshal(e)
		typ := e["type"].(string)
		b.WriteString("event: " + typ + "\ndata: " + string(data) + "\n\n")
	}
	return b.String()
}

func messageStart() ev {
	return ev{"type": "message_start", "message": ev{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5-5",
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": ev{"input_tokens": 120, "output_tokens": 1},
	}}
}
func blockStart(i int, block ev) ev {
	return ev{"type": "content_block_start", "index": i, "content_block": block}
}
func textDelta(i int, s string) ev {
	return ev{"type": "content_block_delta", "index": i, "delta": ev{"type": "text_delta", "text": s}}
}
func jsonDelta(i int, s string) ev {
	return ev{"type": "content_block_delta", "index": i, "delta": ev{"type": "input_json_delta", "partial_json": s}}
}
func sigDelta(i int, s string) ev {
	return ev{"type": "content_block_delta", "index": i, "delta": ev{"type": "signature_delta", "signature": s}}
}
func blockStop(i int) ev { return ev{"type": "content_block_stop", "index": i} }
func messageDelta(stop string, out int) ev {
	return ev{"type": "message_delta", "delta": ev{"stop_reason": stop, "stop_sequence": nil}, "usage": ev{"output_tokens": out}}
}
func messageStop() ev { return ev{"type": "message_stop"} }

func toolStart(i int) ev {
	return blockStart(i, ev{"type": "tool_use", "id": "toolu_1", "name": "save_campaign_draft", "input": ev{}})
}

// thinking + text + tool_use, stop_reason tool_use.
func textThenToolFixture() string {
	return sse(
		messageStart(),
		blockStart(0, ev{"type": "thinking", "thinking": "", "signature": ""}),
		sigDelta(0, "sig-abc"),
		blockStop(0),
		blockStart(1, ev{"type": "text", "text": ""}),
		textDelta(1, "Taslağı "),
		textDelta(1, "hazırlıyorum."),
		blockStop(1),
		toolStart(2),
		jsonDelta(2, `{"name": "Sepet`),
		jsonDelta(2, ` hatırlatma", "sms": {"text": "Merhaba`),
		jsonDelta(2, ` {{first_name}}"}}`),
		blockStop(2),
		messageDelta("tool_use", 85),
		messageStop(),
	)
}

// ---- test server and client -----------------------------------------------

type capture struct {
	mu      sync.Mutex
	headers []http.Header
	bodies  []map[string]any
	raws    [][]byte
	count   atomic.Int32
}

func (c *capture) record(r *http.Request) {
	c.count.Add(1)
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	c.mu.Lock()
	c.headers = append(c.headers, r.Header.Clone())
	c.bodies = append(c.bodies, body)
	c.raws = append(c.raws, raw)
	c.mu.Unlock()
}

func serve(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, n int)) (*httptest.Server, *capture) {
	t.Helper()
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.record(r)
		handler(w, r, int(c.count.Load()))
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func serveSSE(t *testing.T, bodies ...string) (*httptest.Server, *capture) {
	return serve(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, bodies[min(n, len(bodies))-1])
	})
}

func newClient(t *testing.T, url string, mutate ...func(*anthropic.Config)) *anthropic.Client {
	t.Helper()
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	cfg := anthropic.Config{
		APIKey: "test", BaseURL: url, Model: "claude-opus-5-5", Effort: "medium",
		MaxRetries: 2, HTTPClient: &http.Client{Transport: tr},
	}
	for _, m := range mutate {
		m(&cfg)
	}
	c, err := anthropic.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var saveTool = llm.Tool{
	Name: "save_campaign_draft", Description: "Kampanya taslağını kaydeder", Strict: true, EagerInputStreaming: true,
	InputSchema: map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"name": map[string]any{"type": "string"}},
		"required":             []string{"name"},
		"additionalProperties": false,
	},
}

func baseRequest(msgs ...llm.Message) llm.Request {
	if len(msgs) == 0 {
		msgs = []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "Kampanya hazırla"}}}}
	}
	return llm.Request{System: "SYS", Tools: []llm.Tool{saveTool}, Messages: msgs, MaxTokens: 16000}
}

func collect(t *testing.T, c *anthropic.Client, req llm.Request) ([]llm.Event, error) {
	t.Helper()
	s, err := c.Stream(context.Background(), req)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	var events []llm.Event
	for s.Next() {
		events = append(events, s.Event())
	}
	return events, s.Err()
}

func lastEnd(t *testing.T, events []llm.Event) llm.MessageEnd {
	t.Helper()
	for i := len(events) - 1; i >= 0; i-- {
		if e, ok := events[i].(llm.MessageEnd); ok {
			return e
		}
	}
	t.Fatalf("no MessageEnd in %#v", events)
	return llm.MessageEnd{}
}

func toolEnd(t *testing.T, events []llm.Event) llm.ToolUseEnd {
	t.Helper()
	for _, e := range events {
		if te, ok := e.(llm.ToolUseEnd); ok {
			return te
		}
	}
	t.Fatalf("no ToolUseEnd in %#v", events)
	return llm.ToolUseEnd{}
}

// ---- tests ---------------------------------------------------------------

func TestNew_RequiresAPIKey(t *testing.T) {
	if _, err := anthropic.New(anthropic.Config{Model: "claude-opus-5-5"}); err == nil {
		t.Fatal("New without an API key must fail")
	}
}

func TestStream_TextThenTool(t *testing.T) {
	srv, _ := serveSSE(t, textThenToolFixture())
	events, err := collect(t, newClient(t, srv.URL), baseRequest())
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	var text strings.Builder
	var deltas []string
	var starts []llm.ToolUseStart
	for _, e := range events {
		switch e := e.(type) {
		case llm.TextDelta:
			text.WriteString(e.Text)
		case llm.ToolInputDelta:
			if e.ID != "toolu_1" {
				t.Errorf("input delta id = %q", e.ID)
			}
			deltas = append(deltas, e.PartialJSON)
		case llm.ToolUseStart:
			starts = append(starts, e)
		}
	}
	if text.String() != "Taslağı hazırlıyorum." {
		t.Errorf("text = %q", text.String())
	}
	if len(starts) != 1 || starts[0] != (llm.ToolUseStart{ID: "toolu_1", Name: "save_campaign_draft"}) {
		t.Errorf("tool starts = %+v", starts)
	}
	if len(deltas) != 3 {
		t.Errorf("want 3 input deltas forwarded as they stream, got %d", len(deltas))
	}
	end := toolEnd(t, events)
	if want := `{"name": "Sepet hatırlatma", "sms": {"text": "Merhaba {{first_name}}"}}`; string(end.RawInput) != want {
		t.Errorf("raw input = %s, want %s", end.RawInput, want)
	}
	me := lastEnd(t, events)
	if me.StopReason != llm.StopToolUse {
		t.Errorf("stop = %q", me.StopReason)
	}
	if me.Usage.InputTokens != 120 || me.Usage.OutputTokens != 85 {
		t.Errorf("usage = %+v", me.Usage)
	}
	if got := []llm.BlockType{me.Content[0].Type, me.Content[1].Type, me.Content[2].Type}; got[0] != llm.BlockOpaque ||
		got[1] != llm.BlockText || got[2] != llm.BlockToolUse {
		t.Errorf("content types = %v, want [opaque text tool_use]", got)
	}
}

func TestStream_SplitEscapes_StrictParse(t *testing.T) {
	srv, _ := serveSSE(t, sse(
		messageStart(), toolStart(0),
		jsonDelta(0, `{"sms": {"text": "Merhaba \u00`),
		jsonDelta(0, `e7ok \"g`),
		jsonDelta(0, `üzel\" ş"}}`),
		blockStop(0), messageDelta("tool_use", 10), messageStop(),
	))
	events, err := collect(t, newClient(t, srv.URL), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	var in struct {
		SMS struct {
			Text string `json:"text"`
		} `json:"sms"`
	}
	raw := toolEnd(t, events).RawInput
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatalf("strict parse of %s: %v", raw, err)
	}
	if in.SMS.Text != `Merhaba çok "güzel" ş` {
		t.Fatalf("text = %q", in.SMS.Text)
	}
}

func TestStream_TruncatedToolInput_MaxTokens(t *testing.T) {
	srv, _ := serveSSE(t, sse(
		messageStart(), toolStart(0),
		jsonDelta(0, `{"name": "Sepet hatırla`),
		blockStop(0), messageDelta("max_tokens", 16000), messageStop(),
	))
	events, err := collect(t, newClient(t, srv.URL), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if raw := toolEnd(t, events).RawInput; json.Valid(raw) {
		t.Fatalf("truncated input must be passed through unrepaired, got valid JSON %s", raw)
	}
	if got := lastEnd(t, events).StopReason; got != llm.StopMaxTokens {
		t.Fatalf("stop = %q, want max_tokens", got)
	}
}

func TestStream_InvalidJSONToolInput(t *testing.T) {
	srv, _ := serveSSE(t, sse(
		messageStart(), toolStart(0),
		jsonDelta(0, `{"name": "x",}`),
		blockStop(0), messageDelta("tool_use", 10), messageStop(),
	))
	events, err := collect(t, newClient(t, srv.URL), baseRequest())
	if err != nil {
		t.Fatalf("adapter must not fail on invalid tool JSON; the loop handles it: %v", err)
	}
	if raw := toolEnd(t, events).RawInput; string(raw) != `{"name": "x",}` {
		t.Fatalf("raw input = %s", raw)
	}
}

func TestStream_RefusalMidTool(t *testing.T) {
	srv, _ := serveSSE(t, sse(
		messageStart(), toolStart(0),
		jsonDelta(0, `{"name": "Ka`),
		blockStop(0), messageDelta("refusal", 5), messageStop(),
	))
	events, err := collect(t, newClient(t, srv.URL), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if got := lastEnd(t, events).StopReason; got != llm.StopRefusal {
		t.Fatalf("stop = %q, want refusal", got)
	}
}

func TestStream_ErrorEventMidStream(t *testing.T) {
	cases := []struct {
		errType string
		want    error
	}{
		{"overloaded_error", llm.ErrOverloaded},
		{"api_error", llm.ErrStreamInterrupted},
	}
	for _, tc := range cases {
		t.Run(tc.errType, func(t *testing.T) {
			body := sse(messageStart(), blockStart(0, ev{"type": "text", "text": ""}), textDelta(0, "Yarım")) +
				"event: error\ndata: " + `{"type":"error","error":{"type":"` + tc.errType + `","message":"boom"}}` + "\n\n"
			srv, _ := serveSSE(t, body)
			events, err := collect(t, newClient(t, srv.URL), baseRequest())
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			for _, e := range events {
				if _, ok := e.(llm.MessageEnd); ok {
					t.Fatal("an interrupted stream must not produce MessageEnd")
				}
			}
		})
	}
}

func TestStream_HTTPErrors(t *testing.T) {
	cases := []struct {
		status       int
		errType      string
		want         error
		wantRequests int32
	}{
		{429, "rate_limit_error", llm.ErrRateLimited, 3},
		{500, "api_error", llm.ErrUnavailable, 3},
		{529, "overloaded_error", llm.ErrOverloaded, 3},
		{400, "invalid_request_error", llm.ErrBadRequest, 1},
		{401, "authentication_error", llm.ErrAuth, 1},
	}
	for _, tc := range cases {
		t.Run(tc.errType, func(t *testing.T) {
			srv, c := serve(t, func(w http.ResponseWriter, r *http.Request, n int) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After-Ms", "1") // keep SDK retries fast
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"`+tc.errType+`","message":"x"}}`)
			})
			_, err := collect(t, newClient(t, srv.URL), baseRequest())
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if got := c.count.Load(); got != tc.wantRequests {
				t.Fatalf("requests = %d, want %d (MaxRetries=2)", got, tc.wantRequests)
			}
			var le *llm.Error
			if !errors.As(err, &le) || le.Status != tc.status {
				t.Fatalf("status not preserved: %#v", err)
			}
			if tc.status == 429 && le.RetryAfter != 7*time.Second {
				t.Fatalf("retry after = %v, want 7s", le.RetryAfter)
			}
		})
	}
}

func TestStream_Timeout_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	c, err := anthropic.New(anthropic.Config{
		APIKey: "test", BaseURL: srv.URL, Model: "claude-opus-5-5", MaxRetries: 0,
		Timeout: 100 * time.Millisecond, HTTPClient: &http.Client{Transport: tr},
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Stream(context.Background(), baseRequest())
	if err == nil {
		for s.Next() {
		}
		err = s.Err()
		s.Close()
	}
	if !errors.Is(err, llm.ErrTimeout) {
		t.Fatalf("err = %v, want timeout", err)
	}
}

func TestStream_RequestShape(t *testing.T) {
	srv, c := serveSSE(t, textThenToolFixture())
	history := []llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "Önceki istek"}}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "Önceki yanıt [Kampanya 1 → v2]"}}},
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "<chat_state>…</chat_state>"}, {Type: llm.BlockText, Text: "SMS'i kısalt"}}},
	}
	if _, err := collect(t, newClient(t, srv.URL), baseRequest(history...)); err != nil {
		t.Fatal(err)
	}
	if len(c.bodies) != 1 {
		t.Fatalf("server received %d requests, want 1", len(c.bodies))
	}
	h, body := c.headers[0], c.bodies[0]
	if got := h.Get("Authorization"); got != "Bearer test" {
		t.Errorf("Authorization = %q, want Bearer test", got)
	}
	if got := h.Get("Anthropic-Version"); got != "2026-10-21" {
		t.Errorf("anthropic-version = %q", got)
	}
	if got := h.Get("Anthropic-Beta"); got != "" {
		t.Errorf("core path must not send beta headers, got %q", got)
	}
	if body["model"] != "claude-opus-5-5" || body["max_tokens"] != float64(16000) || body["stream"] != true {
		t.Errorf("model/max_tokens/stream = %v/%v/%v", body["model"], body["max_tokens"], body["stream"])
	}
	for _, k := range []string{"temperature", "top_p", "top_k", "tool_choice"} {
		if _, ok := body[k]; ok {
			t.Errorf("request must not set %s", k)
		}
	}
	if th, _ := body["thinking"].(map[string]any); th["type"] != "adaptive" {
		t.Errorf("thinking = %v, want adaptive", body["thinking"])
	}
	if oc, _ := body["output_config"].(map[string]any); oc["effort"] != "medium" {
		t.Errorf("output_config = %v, want effort medium", body["output_config"])
	}
	sys, _ := body["system"].([]any)
	if len(sys) != 1 {
		t.Fatalf("system = %v", body["system"])
	}
	s0 := sys[0].(map[string]any)
	if s0["text"] != "SYS" || s0["cache_control"].(map[string]any)["type"] != "ephemeral" {
		t.Errorf("system block = %v, want cached SYS", s0)
	}
	tool := body["tools"].([]any)[0].(map[string]any)
	if tool["strict"] != true || tool["eager_input_streaming"] != true {
		t.Errorf("tool flags = strict:%v eager:%v", tool["strict"], tool["eager_input_streaming"])
	}
	schema := tool["input_schema"].(map[string]any)
	if schema["additionalProperties"] != false || schema["type"] != "object" {
		t.Errorf("schema = %v", schema)
	}
	msgs, _ := json.Marshal(body["messages"])
	if strings.Contains(string(msgs), `"thinking"`) {
		t.Errorf("history must carry no thinking blocks: %s", msgs)
	}
	if n := len(body["messages"].([]any)); n != 3 {
		t.Errorf("messages = %d, want 3", n)
	}
}

// keys returns the keys of the JSON object raw, in order.
func keys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %s", raw)
	}
	var out []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// Strict mode writes properties in schema order, and a Go map would go out
// sorted. Every object's properties are sent in its required order, then its
// optional ones: the model writes campaign_id before base_version, and an
// optional field cannot come first only by the alphabet.
func TestStream_StrictSchemaInRequiredOrder(t *testing.T) {
	srv, c := serveSSE(t, textThenToolFixture())
	str := map[string]any{"type": "string"}
	condition := map[string]any{"type": "object", "additionalProperties": false,
		"properties": map[string]any{"within_days": str, "kind": str, "negate": str, "field": str},
		"required":   []string{"kind", "field", "within_days"}}
	tool := llm.Tool{Name: "save_campaign_draft", Strict: true, InputSchema: map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"ab_test": str, "per_recipient": str, "base_version": str, "campaign_id": str,
			"rule": map[string]any{"type": "object", "additionalProperties": false,
				"properties": map[string]any{"match": str, "conditions": map[string]any{"type": "array", "items": condition}},
				"required":   []string{"match", "conditions"}},
		},
		"required": []string{"campaign_id", "base_version", "rule"},
	}}
	req := baseRequest()
	req.Tools = []llm.Tool{tool}
	if _, err := collect(t, newClient(t, srv.URL), req); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Tools []struct {
			InputSchema struct {
				Properties json.RawMessage `json:"properties"`
			} `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(c.raws[0], &body); err != nil {
		t.Fatal(err)
	}
	props := body.Tools[0].InputSchema.Properties
	if got, want := keys(t, props), []string{"campaign_id", "base_version", "rule", "ab_test", "per_recipient"}; !slices.Equal(got, want) {
		t.Errorf("properties order = %v, want %v", got, want)
	}
	var top map[string]json.RawMessage
	_ = json.Unmarshal(props, &top)
	var rule struct {
		Properties json.RawMessage `json:"properties"`
	}
	_ = json.Unmarshal(top["rule"], &rule)
	if got, want := keys(t, rule.Properties), []string{"match", "conditions"}; !slices.Equal(got, want) {
		t.Errorf("rule order = %v, want %v", got, want)
	}
	var ruleProps map[string]json.RawMessage
	_ = json.Unmarshal(rule.Properties, &ruleProps)
	var conditions struct {
		Items struct {
			Properties json.RawMessage `json:"properties"`
		} `json:"items"`
	}
	_ = json.Unmarshal(ruleProps["conditions"], &conditions)
	if got, want := keys(t, conditions.Items.Properties), []string{"kind", "field", "within_days", "negate"}; !slices.Equal(got, want) {
		t.Errorf("condition order = %v, want %v", got, want)
	}
}

func TestStream_ReplaysAssistantContentVerbatim(t *testing.T) {
	srv, c := serveSSE(t, textThenToolFixture(), sse(
		messageStart(), blockStart(0, ev{"type": "text", "text": ""}), textDelta(0, "Kaydettim."), blockStop(0),
		messageDelta("end_turn", 5), messageStop(),
	))
	client := newClient(t, srv.URL)
	events, err := collect(t, client, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	assistant := llm.Message{Role: llm.RoleAssistant, Blocks: lastEnd(t, events).Content}
	result := llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
		{Type: llm.BlockToolResult, ToolUseID: "toolu_1", Text: `{"errors":[{"path":"sms.text","code":"too_long"}]}`, IsError: true},
	}}
	req := baseRequest()
	req.Messages = append(req.Messages, assistant, result)
	if _, err := collect(t, client, req); err != nil {
		t.Fatal(err)
	}

	if len(c.bodies) != 2 {
		t.Fatalf("server received %d requests, want 2", len(c.bodies))
	}
	msgs := c.bodies[1]["messages"].([]any)
	content := msgs[1].(map[string]any)["content"].([]any)
	thinking := content[0].(map[string]any)
	if thinking["type"] != "thinking" || thinking["signature"] != "sig-abc" {
		t.Errorf("thinking block not replayed verbatim: %v", thinking)
	}
	toolUse := content[2].(map[string]any)
	if toolUse["type"] != "tool_use" || toolUse["id"] != "toolu_1" ||
		toolUse["input"].(map[string]any)["name"] != "Sepet hatırlatma" {
		t.Errorf("tool_use not replayed: %v", toolUse)
	}
	tr := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "toolu_1" || tr["is_error"] != true {
		t.Errorf("tool_result = %v", tr)
	}
}
