package httpapi_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/chat/turns"
	"smart-campaign-composer/backend/internal/clock"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/httpapi"
	"smart-campaign-composer/backend/internal/llm"
	"smart-campaign-composer/backend/internal/llm/mock"
	"smart-campaign-composer/backend/internal/segment"
	"smart-campaign-composer/backend/internal/testutil"
)

type env struct {
	db  *testutil.DB
	api *httpapi.Server
	srv *httptest.Server
}

func newEnv(t *testing.T, main llm.Client) *env {
	t.Helper()
	return newEnvWith(t, main, nil, nil)
}

// newEnvWith is newEnv with adjusted dependencies and a wrapped handler.
func newEnvWith(t *testing.T, main llm.Client, adjust func(*httpapi.Deps), wrap func(http.Handler) http.Handler) *env {
	t.Helper()
	return newEnvOn(t, main, adjust, wrap, nil)
}

// newEnvOn is newEnvWith with a hook that prepares the test server (its
// listener, its http.Server) before it starts.
func newEnvOn(t *testing.T, main llm.Client, adjust func(*httpapi.Deps), wrap func(http.Handler) http.Handler,
	prepare func(*httptest.Server)) *env {
	t.Helper()
	db := testutil.NewDB(t)
	d := httpapi.Deps{Pool: db.Pool, Campaigns: campaign.NewStore(db.Pool),
		Catalog: segment.DefaultCatalog(), Clock: clock.Real{}, LLM: main.Info(), TurnTimeout: 30 * time.Second}
	if adjust != nil {
		adjust(&d)
	}
	d.Runner = turns.NewRunner(turns.Deps{Pool: db.Pool, LLM: main, Campaigns: campaign.NewStore(db.Pool),
		Catalog: segment.DefaultCatalog(), Clock: clock.Real{}, Metrics: d.Metrics})
	api := httpapi.New(d)
	var h http.Handler = api
	if wrap != nil {
		h = wrap(api)
	}
	srv := httptest.NewUnstartedServer(h)
	if prepare != nil {
		prepare(srv)
	}
	srv.Start()
	t.Cleanup(func() {
		srv.Close()
		waitOrFail(t, api.Wait, 5*time.Second, "background turns did not finish")
	})
	return &env{db: db, api: api, srv: srv}
}

func waitOrFail(t *testing.T, f func(), d time.Duration, msg string) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal(msg)
	}
}

func uuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6], b[8] = (b[6]&0x0f)|0x40, (b[8]&0x3f)|0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func (e *env) createChat(t *testing.T) string {
	t.Helper()
	resp, err := http.Post(e.srv.URL+"/api/chats", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct{ ID string }
	if resp.StatusCode != http.StatusCreated || json.NewDecoder(resp.Body).Decode(&body) != nil || body.ID == "" {
		t.Fatalf("create chat: %d %+v", resp.StatusCode, body)
	}
	return body.ID
}

func (e *env) postTurn(t *testing.T, ctx context.Context, chatID, clientID, text string) *http.Response {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"client_turn_id": clientID, "text": text})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.srv.URL+"/api/chats/"+chatID+"/turns", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

type sseEvent struct {
	Type string
	Data json.RawMessage
}

// readEvents reads SSE events until the stream ends or stop returns true.
func readEvents(t *testing.T, r io.Reader, stop func(sseEvent) bool) []sseEvent {
	t.Helper()
	var (
		out []sseEvent
		cur sseEvent
	)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.Type = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = json.RawMessage(strings.TrimPrefix(line, "data: "))
		case line == "" && cur.Type != "":
			out = append(out, cur)
			if stop != nil && stop(cur) {
				return out
			}
			cur = sseEvent{}
		}
	}
	return out
}

func (e *env) getTurn(t *testing.T, turnID string) turns.TurnResult {
	t.Helper()
	resp, err := http.Get(e.srv.URL + "/api/turns/" + turnID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var res turns.TurnResult
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&res) != nil {
		t.Fatalf("get turn: %d", resp.StatusCode)
	}
	return res
}

// previewText joins the streamed assistant text.
func previewText(events []sseEvent) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Type == "assistant.delta" {
			var d struct{ Text string }
			_ = json.Unmarshal(ev.Data, &d)
			b.WriteString(d.Text)
		}
	}
	return b.String()
}

// indexOf returns the position of the first event of type typ, or -1.
func indexOf(events []sseEvent, typ string) int {
	for i, ev := range events {
		if ev.Type == typ {
			return i
		}
	}
	return -1
}

func turnIDOf(t *testing.T, events []sseEvent) string {
	t.Helper()
	if len(events) == 0 || events[0].Type != "turn.started" {
		t.Fatalf("stream must start with turn.started, got %+v", events)
	}
	var d struct {
		TurnID string `json:"turn_id"`
	}
	if err := json.Unmarshal(events[0].Data, &d); err != nil || d.TurnID == "" {
		t.Fatalf("turn.started data %s", events[0].Data)
	}
	return d.TurnID
}

const reply = "Merhaba! Hangi kitleye ne tür bir kampanya hazırlamak istersiniz?"

func TestPostTurn_StreamsAndStores(t *testing.T) {
	m := mock.NewScripted(mock.Call{Text: reply})
	e := newEnv(t, m)
	chatID := e.createChat(t)
	resp := e.postTurn(t, context.Background(), chatID, uuid(), "merhaba")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status=%d content-type=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	events := readEvents(t, resp.Body, nil)
	turnID := turnIDOf(t, events)

	if text := previewText(events); text != reply {
		t.Errorf("streamed text = %q", text)
	}
	if i := indexOf(events, "resync"); i != -1 {
		t.Errorf("a stream that kept up must not resync: %s", events[i].Data)
	}
	if last := events[len(events)-1]; last.Type != "done" || !strings.Contains(string(last.Data), `"outcome":"ok"`) {
		t.Fatalf("last event = %+v", last)
	}
	if res := e.getTurn(t, turnID); res.Status != "completed" || res.AssistantMessage.Text != reply {
		t.Fatalf("stored = %+v", res)
	}
}

func TestManualCampaign_CreateInNewAndExistingChat_NoLLM(t *testing.T) {
	m := mock.NewScripted()
	e := newEnv(t, m)
	post := func(chatID string) (int, map[string]any) {
		t.Helper()
		body := map[string]any{"name": "Manuel kampanya", "segment_rule": map[string]any{
			"match": "all", "conditions": []any{map[string]any{"kind": "attribute", "field": "total_orders", "operator": "gte", "values": []string{"0"}}},
		}, "sms": map[string]any{"text": "Merhaba {{first_name}}"}}
		if chatID != "" {
			body["chat_id"] = chatID
		}
		b, _ := json.Marshal(body)
		resp, err := http.Post(e.srv.URL+"/api/campaigns", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, out
	}
	code, first := post("")
	if code != 201 || !uuidReTest(first["campaign_id"]) || !uuidReTest(first["chat_id"]) || first["version"].(float64) != 1 {
		t.Fatalf("new chat: code=%d body=%v", code, first)
	}
	if _, err := e.db.Pool.Exec(context.Background(), `UPDATE chats SET updated_at='2020-01-01' WHERE id=$1`, first["chat_id"]); err != nil {
		t.Fatal(err)
	}
	code, second := post(first["chat_id"].(string))
	if code != 201 || second["chat_id"] != first["chat_id"] || second["ordinal"].(float64) != 2 {
		t.Fatalf("existing chat: code=%d body=%v", code, second)
	}
	var updated time.Time
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT updated_at FROM chats WHERE id=$1`, first["chat_id"]).Scan(&updated); err != nil || updated.Year() < 2026 {
		t.Fatalf("manual create did not update chat timestamp: %v, %v", updated, err)
	}
	resp, err := http.Get(e.srv.URL + "/api/chats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var chats struct{ Chats []struct{ ID string } }
	if err := json.NewDecoder(resp.Body).Decode(&chats); err != nil || len(chats.Chats) != 1 || chats.Chats[0].ID != first["chat_id"] {
		t.Fatalf("manual chat missing from list: %+v, %v", chats, err)
	}
	var messages int
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM messages WHERE chat_id=$1`, first["chat_id"]).Scan(&messages); err != nil || messages != 0 || m.Calls() != 0 {
		t.Fatalf("synthetic messages=%d llm calls=%d error=%v", messages, m.Calls(), err)
	}
}

func uuidReTest(v any) bool {
	s, ok := v.(string)
	return ok && len(s) == 36 && strings.Count(s, "-") == 4
}

func TestManualCampaign_CatalogHasCanonicalRules(t *testing.T) {
	e := newEnv(t, mock.NewScripted())
	resp, err := http.Get(e.srv.URL + "/api/campaign-rules/catalog")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Fields []struct {
			Name string `json:"name"`
		} `json:"fields"`
		Places []struct {
			Name, Level, Country string
			GeonameID            int64 `json:"geoname_id"`
		} `json:"places"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&body) != nil || len(body.Fields) < 5 || len(body.Places) < 10 {
		t.Fatalf("catalog status=%d body=%+v", resp.StatusCode, body)
	}
}

func TestManualCampaign_CatalogOffersCitiesWithoutRegions(t *testing.T) {
	e := newEnv(t, mock.NewScripted())
	resp, err := http.Get(e.srv.URL + "/api/campaign-rules/catalog")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Places []struct {
			Name, Level, Country string
		} `json:"places"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	istanbul := []string{}
	berlin := []string{}
	izmit := false
	paris := false
	for _, p := range body.Places {
		if p.Country == "TR" && p.Name == "İstanbul" {
			istanbul = append(istanbul, p.Level)
		}
		if p.Country == "DE" && p.Name == "Berlin" {
			berlin = append(berlin, p.Level)
		}
		if p.Country == "TR" && p.Name == "İzmit" && p.Level == "city" {
			izmit = true
		}
		if p.Country == "FR" && p.Name == "Paris" && p.Level == "city" {
			paris = true
		}
	}
	if !slices.Equal(istanbul, []string{"city"}) || !slices.Equal(berlin, []string{"city"}) || !izmit || !paris {
		t.Fatalf("İstanbul levels = %v, Berlin levels = %v, İzmit = %v, Paris = %v", istanbul, berlin, izmit, paris)
	}
}

// Two identical requests at the same time (a client retry) run the turn
// once: one gets the stream, the other a JSON status, the model one call.
func TestPostTurn_SameIDConcurrent_RunsOnce(t *testing.T) {
	m := mock.NewScripted(mock.Call{Text: reply, ChunkDelay: 20 * time.Millisecond})
	e := newEnv(t, m)
	chatID, clientID := e.createChat(t), uuid()

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		streams   int
		jsonCodes []int
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := e.postTurn(t, context.Background(), chatID, clientID, "merhaba")
			defer resp.Body.Close()
			mu.Lock()
			defer mu.Unlock()
			if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
				streams++
				mu.Unlock()
				readEvents(t, resp.Body, nil)
				mu.Lock()
				return
			}
			jsonCodes = append(jsonCodes, resp.StatusCode)
		}()
	}
	wg.Wait()
	if streams != 1 || len(jsonCodes) != 1 || (jsonCodes[0] != http.StatusAccepted && jsonCodes[0] != http.StatusOK) {
		t.Fatalf("streams=%d json=%v; want exactly one stream and one 202/200", streams, jsonCodes)
	}
	if m.Calls() != 1 {
		t.Fatalf("model calls = %d, want 1", m.Calls())
	}
}

func TestPostTurn_SameIDDifferentText_409(t *testing.T) {
	e := newEnv(t, mock.NewScripted(mock.Call{Text: reply}))
	chatID, clientID := e.createChat(t), uuid()
	resp := e.postTurn(t, context.Background(), chatID, clientID, "merhaba")
	readEvents(t, resp.Body, nil)
	resp.Body.Close()

	resp = e.postTurn(t, context.Background(), chatID, clientID, "başka bir şey")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "idempotency_mismatch") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}

func TestPostTurn_OtherTurnRunning_409(t *testing.T) {
	e := newEnv(t, mock.NewScripted(mock.Call{Text: reply, ChunkDelay: 30 * time.Millisecond}))
	chatID := e.createChat(t)
	first := e.postTurn(t, context.Background(), chatID, uuid(), "bir")
	defer first.Body.Close()
	readEvents(t, first.Body, func(ev sseEvent) bool { return ev.Type == "turn.started" })

	resp := e.postTurn(t, context.Background(), chatID, uuid(), "iki")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "turn_in_progress") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	readEvents(t, first.Body, nil)
}

func TestPostTurn_RePostAfterCompletion_ReturnsStoredResult(t *testing.T) {
	m := mock.NewScripted(mock.Call{Text: reply})
	e := newEnv(t, m)
	chatID, clientID := e.createChat(t), uuid()
	resp := e.postTurn(t, context.Background(), chatID, clientID, "merhaba")
	readEvents(t, resp.Body, nil)
	resp.Body.Close()

	resp = e.postTurn(t, context.Background(), chatID, clientID, "merhaba")
	defer resp.Body.Close()
	var res turns.TurnResult
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&res) != nil {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if res.Status != "completed" || res.AssistantMessage == nil || res.AssistantMessage.Text != reply {
		t.Fatalf("result = %+v", res)
	}
	if m.Calls() != 1 {
		t.Fatalf("model calls = %d; a stored turn must not run again", m.Calls())
	}
}

// The client goes away mid-stream; the turn still completes and GET returns
// the stored result. The runner is not blocked by the gone client.
func TestDisconnect_TurnCompletes_GetShowsFinal(t *testing.T) {
	m := mock.NewScripted(mock.Call{Text: reply, ChunkDelay: 30 * time.Millisecond})
	e := newEnv(t, m)
	chatID := e.createChat(t)
	ctx, cancel := context.WithCancel(context.Background())
	resp := e.postTurn(t, ctx, chatID, uuid(), "merhaba")
	events := readEvents(t, resp.Body, func(ev sseEvent) bool { return ev.Type == "assistant.delta" })
	turnID := turnIDOf(t, events)
	cancel()
	resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		res := e.getTurn(t, turnID)
		if res.Status != "running" {
			if res.Status != "completed" || res.AssistantMessage.Text != reply {
				t.Fatalf("result = %+v", res)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("turn did not finish after the client disconnected")
		}
		time.Sleep(50 * time.Millisecond)
	}
	waitOrFail(t, e.api.Wait, 5*time.Second, "runner blocked after the client went away")
}

// slowClient holds a turn stream back after its first event, like a client
// that has stopped reading, until release. Other responses pass untouched.
type slowClient struct {
	gate        chan struct{}
	stalled     chan struct{} // closed when the stream first waits
	stallOnce   sync.Once
	releaseOnce sync.Once
}

func newSlowClient() *slowClient {
	return &slowClient{gate: make(chan struct{}), stalled: make(chan struct{})}
}

func (c *slowClient) release() { c.releaseOnce.Do(func() { close(c.gate) }) }

func (c *slowClient) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&slowWriter{ResponseWriter: w, c: c}, r)
	})
}

type slowWriter struct {
	http.ResponseWriter
	c      *slowClient
	writes int
}

func (w *slowWriter) Write(p []byte) (int, error) {
	if strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		w.writes++
		if w.writes > 1 {
			w.c.stallOnce.Do(func() { close(w.c.stalled) })
			<-w.c.gate
		}
	}
	return w.ResponseWriter.Write(p)
}

func (w *slowWriter) Flush() { w.ResponseWriter.(http.Flusher).Flush() }

// The client stops reading after turn.started and the turn runs to the end
// meanwhile, so the stream's buffer fills up. Live events may then be
// skipped, but not silently: the stream says so with a resync pointing at
// the stored result, sends nothing live after it, and still ends with the
// turn's final state.
func TestStream_SlowClient_FullBuffer_FinalStateNotLost(t *testing.T) {
	text := strings.Repeat("Sepetinde ürün kaldı. ", 12) // ~45 deltas of 6 runes
	slow := newSlowClient()
	e := newEnvWith(t, mock.NewScripted(mock.Call{Text: text}),
		func(d *httpapi.Deps) { d.StreamBuffer = 8 }, slow.wrap)
	t.Cleanup(slow.release) // runs before the server closes

	chatID := e.createChat(t)
	resp := e.postTurn(t, context.Background(), chatID, uuid(), "merhaba")
	defer resp.Body.Close()
	turnID := turnIDOf(t, readEvents(t, resp.Body, func(ev sseEvent) bool { return ev.Type == "turn.started" }))
	select {
	case <-slow.stalled:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never waited for the slow client")
	}
	waitOrFail(t, e.api.Wait, 5*time.Second, "the turn did not finish while the client was slow")
	slow.release()
	events := readEvents(t, resp.Body, nil)

	if len(events) == 0 {
		t.Fatal("no events after turn.started")
	}
	if last := events[len(events)-1]; last.Type != "done" || !strings.Contains(string(last.Data), `"outcome":"ok"`) {
		t.Fatalf("the stream must end with the turn's final state; last of %d events = %s %s", len(events), last.Type, last.Data)
	}
	i := indexOf(events, "resync")
	if i == -1 {
		t.Fatalf("live events were dropped without telling the client: preview %q", previewText(events))
	}
	var rs struct {
		TurnID    string `json:"turn_id"`
		Reason    string `json:"reason"`
		ResultURL string `json:"result_url"`
	}
	if err := json.Unmarshal(events[i].Data, &rs); err != nil || rs.TurnID != turnID || rs.Reason != "lagged" ||
		rs.ResultURL != "/api/turns/"+turnID {
		t.Fatalf("resync = %s", events[i].Data)
	}
	if p := previewText(events[:i]); !strings.HasPrefix(text, p) || p == text {
		t.Fatalf("the preview before resync must be a gapless, incomplete prefix; got %q", p)
	}
	for _, ev := range events[i+1:] {
		if ev.Type != "error" && ev.Type != "done" {
			t.Fatalf("live event %s after resync", ev.Type)
		}
	}
	if res := e.getTurn(t, turnID); res.Status != "completed" || res.AssistantMessage == nil || res.AssistantMessage.Text != text {
		t.Fatalf("stored result = %+v", res)
	}
}

// When the turn's result cannot be stored there is no done to send; the
// stream does not stop on a bare error but tells the client to poll the
// stored result, which shows the turn still running until the reaper ends it.
func TestStream_ResultNotStored_EndsWithResync(t *testing.T) {
	e := newEnv(t, mock.NewScripted(mock.Call{Text: reply}))
	chatID := e.createChat(t)
	if _, err := e.db.Pool.Exec(context.Background(), `ALTER TABLE llm_calls RENAME TO llm_calls_broken`); err != nil {
		t.Fatal(err)
	}
	resp := e.postTurn(t, context.Background(), chatID, uuid(), "merhaba")
	defer resp.Body.Close()
	events := readEvents(t, resp.Body, nil)
	turnID := turnIDOf(t, events)

	if i := indexOf(events, "done"); i != -1 {
		t.Fatalf("done without a stored result: %s", events[i].Data)
	}
	n := len(events)
	if n < 3 || events[n-2].Type != "error" || !strings.Contains(string(events[n-2].Data), "persist_failed") {
		t.Fatalf("expected error persist_failed before the end, got %+v", events)
	}
	var rs struct {
		TurnID    string `json:"turn_id"`
		Reason    string `json:"reason"`
		ResultURL string `json:"result_url"`
	}
	if last := events[n-1]; last.Type != "resync" || json.Unmarshal(last.Data, &rs) != nil || rs.TurnID != turnID ||
		rs.Reason != "result_pending" || rs.ResultURL != "/api/turns/"+turnID {
		t.Fatalf("last event = %s %s; want resync result_pending", last.Type, last.Data)
	}
	if res := e.getTurn(t, turnID); res.Status != "running" {
		t.Fatalf("stored = %+v; the turn stays running until the reaper ends it", res)
	}
}

func TestPostTurn_Validation(t *testing.T) {
	e := newEnv(t, mock.NewScripted())
	chatID := e.createChat(t)
	cases := []struct {
		name, chat, clientID, text string
		want                       int
	}{
		{"bad client_turn_id", chatID, "not-a-uuid", "merhaba", http.StatusBadRequest},
		{"empty text", chatID, uuid(), "   ", http.StatusBadRequest},
		{"text too long", chatID, uuid(), strings.Repeat("a", 4001), http.StatusBadRequest},
		{"unknown chat", uuid(), uuid(), "merhaba", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := e.postTurn(t, context.Background(), tc.chat, tc.clientID, tc.text)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// After a reload the client reads the chat, including a running turn to poll.
func TestGetChat(t *testing.T) {
	e := newEnv(t, mock.NewScripted(mock.Call{Text: reply, ChunkDelay: 30 * time.Millisecond}))
	chatID := e.createChat(t)
	resp := e.postTurn(t, context.Background(), chatID, uuid(), "merhaba")
	defer resp.Body.Close()
	turnID := turnIDOf(t, readEvents(t, resp.Body, func(ev sseEvent) bool { return ev.Type == "turn.started" }))

	type chatView struct {
		Messages []struct {
			TurnID string `json:"turn_id"`
			Role   string
			Text   string
		}
		Campaigns     []json.RawMessage
		RunningTurnID *string `json:"running_turn_id"`
	}
	get := func() chatView {
		r, err := http.Get(e.srv.URL + "/api/chats/" + chatID)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var v chatView
		if r.StatusCode != http.StatusOK || json.NewDecoder(r.Body).Decode(&v) != nil {
			t.Fatalf("get chat: %d", r.StatusCode)
		}
		return v
	}
	if v := get(); v.RunningTurnID == nil || *v.RunningTurnID != turnID || len(v.Messages) != 1 || v.Messages[0].Role != "user" {
		t.Fatalf("while running: %+v", v)
	}
	readEvents(t, resp.Body, nil)
	if v := get(); v.RunningTurnID != nil || len(v.Messages) != 2 || v.Messages[1].Text != reply || v.Campaigns == nil {
		t.Fatalf("after: %+v", v)
	}

	r, _ := http.Get(e.srv.URL + "/api/chats/" + uuid())
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown chat: %d", r.StatusCode)
	}
}

func TestMeta_ReportsMockProvider(t *testing.T) {
	e := newEnv(t, mock.NewDemo("ANTHROPIC_API_KEY tanımlı değil"))
	r, err := http.Get(e.srv.URL + "/api/meta")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var meta struct {
		LLM       llm.Info        `json:"llm"`
		Scenarios []mock.Scenario `json:"mock_scenarios"`
	}
	if r.StatusCode != http.StatusOK || json.NewDecoder(r.Body).Decode(&meta) != nil ||
		meta.LLM.Provider != "mock" || meta.LLM.Reason == "" {
		t.Fatalf("meta = %+v (status %d)", meta, r.StatusCode)
	}
	// The UI offers the demo's scenarios only while the mock answers.
	if !slices.ContainsFunc(meta.Scenarios, func(s mock.Scenario) bool { return s.Name == "slow_stream" }) {
		t.Fatalf("mock scenarios = %+v; want the demo's list", meta.Scenarios)
	}
}

func TestMeta_ClaudeHasNoMockScenarios(t *testing.T) {
	e := newEnvWith(t, mock.NewScripted(), func(d *httpapi.Deps) {
		d.LLM = llm.Info{Provider: "anthropic", Model: "claude-opus-5-5"}
	}, nil)
	code, body := e.do(t, http.MethodGet, "/api/meta", nil)
	if _, ok := body["mock_scenarios"]; code != http.StatusOK || ok {
		t.Fatalf("meta = %d %v; no mock scenarios with Claude", code, body)
	}
}

func (e *env) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, r)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestCampaignEndpoints_EditAndApprove(t *testing.T) {
	e := newEnv(t, mock.NewScripted())
	ctx := context.Background()
	chatID := e.createChat(t)
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO users (first_name, last_name, phone, country_code, admin1_code, city_geoname_id,
		birth_date, consent_sms) VALUES ('Ayşe','Yılmaz','+905550000001','TR','TR-34',745044,'1990-01-01',true)`); err != nil {
		t.Fatal(err)
	}
	saved, err := campaign.NewStore(e.db.Pool).Drafts(campaign.Author{ChatID: chatID, Source: "user"}).SaveDraft(ctx, campaign.DraftCmd{
		Name: "İstanbul", SMS: &content.SMS{Text: "Merhaba {{first_name}}"}, ChangeSummary: "ilk",
		Rule: segment.Rule{Match: "all", Conditions: []segment.Condition{{Kind: "location", Operator: "in",
			Places: []segment.PlaceRef{{Level: "city", GeonameID: 745044, Country: "TR", Name: "İstanbul"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/campaigns/" + publicCampaignID(t, e, saved.CampaignID)

	code, body := e.do(t, http.MethodGet, base, nil)
	cur, _ := body["current_version"].(map[string]any)
	if code != http.StatusOK || body["status"] != "draft" || cur == nil || cur["version"] != float64(1) || cur["sms"] == nil {
		t.Fatalf("get campaign: %d %v", code, body)
	}

	code, body = e.do(t, http.MethodPost, base+"/versions", map[string]any{
		"base_version_id": saved.VersionID, "name": "İstanbul", "sms": map[string]string{"text": "Selam {{first_name}}"}})
	if code != http.StatusCreated || body["version"] != float64(2) {
		t.Fatalf("edit: %d %v", code, body)
	}
	v2 := int64(body["version_id"].(float64))

	code, body = e.do(t, http.MethodPost, base+"/versions", map[string]any{
		"base_version_id": saved.VersionID, "name": "x", "sms": map[string]string{"text": "y"}})
	if code != http.StatusConflict || body["error"] != "stale_version" || body["current_version"] != float64(2) {
		t.Fatalf("stale edit: %d %v", code, body)
	}
	code, body = e.do(t, http.MethodPost, base+"/versions", map[string]any{
		"base_version_id": v2, "name": "x", "sms": map[string]string{"text": strings.Repeat("a", 161)}})
	if code != http.StatusUnprocessableEntity || body["error"] != "validation_failed" || body["issues"] == nil {
		t.Fatalf("invalid edit: %d %v", code, body)
	}

	code, body = e.do(t, http.MethodPost, base+"/approve", map[string]any{"version_id": saved.VersionID})
	if code != http.StatusConflict || body["error"] != "stale_version" {
		t.Fatalf("approve old version: %d %v", code, body)
	}
	code, body = e.do(t, http.MethodPost, base+"/approve", map[string]any{"version_id": v2})
	sms, _ := body["channels"].(map[string]any)["sms"].(map[string]any)
	if code != http.StatusOK || body["status"] != "approved" || sms["selected"] != float64(1) {
		t.Fatalf("approve: %d %v", code, body)
	}
	code, body = e.do(t, http.MethodPost, base+"/approve", map[string]any{"version_id": v2})
	if code != http.StatusOK || body["already_approved"] != true {
		t.Fatalf("approve again: %d %v", code, body)
	}
	code, body = e.do(t, http.MethodPost, base+"/versions", map[string]any{
		"base_version_id": v2, "name": "x", "sms": map[string]string{"text": "y"}})
	if code != http.StatusConflict || body["error"] != "not_draft" {
		t.Fatalf("edit approved: %d %v", code, body)
	}
	if code, _ := e.do(t, http.MethodGet, "/api/campaigns/999999", nil); code != http.StatusNotFound {
		t.Fatalf("unknown campaign: %d", code)
	}
}
