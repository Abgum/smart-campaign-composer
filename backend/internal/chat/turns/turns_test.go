package turns_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/chat/agentloop"
	"smart-campaign-composer/backend/internal/chat/router"
	"smart-campaign-composer/backend/internal/chat/turns"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/llm"
	"smart-campaign-composer/backend/internal/llm/mock"
	"smart-campaign-composer/backend/internal/metrics"
	"smart-campaign-composer/backend/internal/segment"
	"smart-campaign-composer/backend/internal/testutil"
)

const validNew = `{"name":"İstanbul sepet hatırlatma",
 "segment_rule":{"match":"all","conditions":[
   {"kind":"location","operator":"in","places":[{"level":"city","name":"İstanbul","country":"TR"}]},
   {"kind":"event","event_type":"cart_add","within_days":30,"not_followed_by":"purchase"}]},
 "sms":{"text":"Merhaba {{first_name}}, sepetin seni bekliyor!"},
 "content_language":"tr","change_summary":"İlk taslak"}`

type recorder struct {
	mu     sync.Mutex
	events []agentloop.Event
}

func (r *recorder) Emit(e agentloop.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// index returns the position of the first event of type typ, or -1.
func (r *recorder) index(typ string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, e := range r.events {
		if e.Type == typ {
			return i
		}
	}
	return -1
}

func (r *recorder) last() agentloop.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.events[len(r.events)-1]
}

type failingSaver struct{}

func (failingSaver) SaveDraft(context.Context, campaign.DraftCmd) (campaign.Saved, error) {
	return campaign.Saved{}, errors.New("connection reset by peer")
}

func (failingSaver) RemoveABTest(context.Context, int64, int, string) (campaign.Removed, error) {
	return campaign.Removed{}, errors.New("connection reset by peer")
}

func newRunner(t *testing.T, db *testutil.DB, m llm.Client) *turns.Runner {
	t.Helper()
	return turns.NewRunner(turns.Deps{
		Pool: db.Pool, LLM: m, Campaigns: campaign.NewStore(db.Pool), Catalog: segment.DefaultCatalog(),
		Clock: testutil.NewFakeClock(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)),
	})
}

func newChat(t *testing.T, db *testutil.DB) string {
	t.Helper()
	var id string
	if err := db.Pool.QueryRow(context.Background(), `INSERT INTO chats DEFAULT VALUES RETURNING id::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func uuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6], b[8] = (b[6]&0x0f)|0x40, (b[8]&0x3f)|0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// runTurn begins and executes a turn synchronously.
func runTurn(t *testing.T, r *turns.Runner, chatID, text string) (turns.TurnResult, *recorder) {
	t.Helper()
	ctx := context.Background()
	begun, err := r.Begin(ctx, turns.BeginRequest{ChatID: chatID, ClientTurnID: uuid(), Text: text})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if !begun.New {
		t.Fatal("first begin must create the turn")
	}
	rec := &recorder{}
	r.Execute(ctx, begun.TurnID, rec)
	res, err := r.Result(ctx, begun.TurnID)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	return res, rec
}

func count(t *testing.T, db *testutil.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// nextContext prepares (without executing) a follow-up turn and returns the
// request the model would receive.
func nextContext(t *testing.T, r *turns.Runner, chatID string) llm.Request {
	t.Helper()
	ctx := context.Background()
	begun, err := r.Begin(ctx, turns.BeginRequest{ChatID: chatID, ClientTurnID: uuid(), Text: "devam"})
	if err != nil {
		t.Fatalf("begin next: %v", err)
	}
	p, err := r.Prepare(ctx, begun.TurnID)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return p.Request
}

func joinedText(req llm.Request) string {
	var b strings.Builder
	for _, m := range req.Messages {
		for _, bl := range m.Blocks {
			b.WriteString(bl.Text)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func TestTurn_Success(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	m := mock.NewScripted(
		mock.Call{Text: "Hazırlıyorum.", ToolName: "save_campaign_draft", ToolInput: validNew},
		mock.Call{Text: "Kaydettim."},
	)
	res, rec := runTurn(t, newRunner(t, db, m), chatID, "İstanbul'da sepeti terk edenlere hatırlatma")

	if res.Status != "completed" || res.Outcome != "ok" || res.ErrorCode != "" {
		t.Fatalf("result = %+v", res)
	}
	if res.AssistantMessage == nil || res.AssistantMessage.Text != "Hazırlıyorum.\nKaydettim." ||
		res.AssistantMessage.LLMProvider != "mock" || res.AssistantMessage.Outcome != "ok" {
		t.Fatalf("assistant message = %+v", res.AssistantMessage)
	}
	if len(res.CampaignRefs) != 1 || res.CampaignRefs[0].Action != "created" || res.CampaignRefs[0].Ordinal != 1 {
		t.Fatalf("refs = %+v", res.CampaignRefs)
	}
	if n := count(t, db, `SELECT count(*) FROM llm_calls WHERE turn_id=$1`, res.TurnID); n != 2 {
		t.Errorf("llm_calls = %d, want 2", n)
	}
	if done := rec.last(); done.Type != turns.EventDone {
		t.Errorf("last event = %s, want done", done.Type)
	}
	if rec.index(turns.EventError) != -1 {
		t.Error("no error event on success")
	}
}

// A: a draft was saved, then the next model call failed.
func TestTurn_SaveThenLLMError(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	m := mock.NewScripted(
		mock.Call{Text: "Hazırlıyorum.", ToolName: "save_campaign_draft", ToolInput: validNew},
		mock.Call{Text: "Kaydettim, 3", Err: llm.ErrUnavailable, ErrAfterEvents: 2},
	)
	r := newRunner(t, db, m)
	res, rec := runTurn(t, r, chatID, "brief")

	if res.Status != "failed" || res.Outcome != "partial" || res.ErrorCode != "llm_unavailable" {
		t.Fatalf("result = %s/%s/%s", res.Status, res.Outcome, res.ErrorCode)
	}
	// DB: the saved version stays and is current.
	if n := count(t, db, `SELECT count(*) FROM campaigns c JOIN campaign_versions v ON v.id = c.current_version_id
		WHERE c.chat_id = $1 AND v.version = 1`, chatID); n != 1 {
		t.Fatalf("campaign with current v1 = %d", n)
	}
	if len(res.CampaignRefs) != 1 {
		t.Fatalf("refs = %+v", res.CampaignRefs)
	}
	// History: completed text + Go note; the failed iteration's text is dropped.
	text := res.AssistantMessage.Text
	for _, want := range []string{"Hazırlıyorum.", "Kampanya 1 v1 kaydedildi", "tamamlanamadı"} {
		if !strings.Contains(text, want) {
			t.Errorf("assistant text %q lacks %q", text, want)
		}
	}
	if strings.Contains(text, "Kaydettim, 3") || res.AssistantMessage.Outcome != "partial" {
		t.Errorf("partial text leaked or wrong outcome: %+v", res.AssistantMessage)
	}
	// Live events: draft.saved → error → done(partial).
	saved, errEv := rec.index(agentloop.EventDraftSaved), rec.index(turns.EventError)
	if saved == -1 || errEv == -1 || saved > errEv {
		t.Fatalf("event order: draft.saved at %d, error at %d", saved, errEv)
	}
	if done := rec.last(); done.Type != turns.EventDone || done.Data.(map[string]any)["outcome"] != "partial" {
		t.Fatalf("last event = %+v", done)
	}
	// Next turn sees v1 and the note.
	next := joinedText(nextContext(t, r, chatID))
	for _, want := range []string{`"version":1`, "Kampanya 1 v1 kaydedildi", "brief"} {
		if !strings.Contains(next, want) {
			t.Errorf("next context lacks %q", want)
		}
	}
}

// B: the stream broke before anything was saved.
func TestTurn_ErrorMidStreamBeforeSave(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	m := mock.NewScripted(mock.Call{Text: "Yarım kalan cevap", Err: llm.ErrStreamInterrupted, ErrAfterEvents: 1})
	r := newRunner(t, db, m)
	res, rec := runTurn(t, r, chatID, "başarısız istek")

	if res.Status != "failed" || res.Outcome != "failed" || res.ErrorCode != "llm_unavailable" || len(res.CampaignRefs) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if n := count(t, db, `SELECT count(*) FROM campaigns WHERE chat_id=$1`, chatID); n != 0 {
		t.Fatalf("campaigns = %d", n)
	}
	if res.AssistantMessage == nil || res.AssistantMessage.Outcome != "failed" || strings.Contains(res.AssistantMessage.Text, "Yarım") {
		t.Fatalf("assistant message = %+v", res.AssistantMessage)
	}
	if rec.index(turns.EventError) == -1 {
		t.Error("error event missing")
	}
	if strings.Contains(joinedText(nextContext(t, r, chatID)), "başarısız istek") {
		t.Fatal("a failed turn without side effects must not enter the next context")
	}
}

// C: the database write failed.
func TestTurn_DBErrorOnSave(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	m := mock.NewScripted(mock.Call{ToolName: "save_campaign_draft", ToolInput: validNew}, mock.Call{Text: "never"})
	r := turns.NewRunner(turns.Deps{
		Pool: db.Pool, LLM: m, Campaigns: campaign.NewStore(db.Pool), Catalog: segment.DefaultCatalog(),
		Clock:      testutil.NewFakeClock(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)),
		WrapDrafts: func(agentloop.DraftSaver) agentloop.DraftSaver { return failingSaver{} },
	})
	res, rec := runTurn(t, r, chatID, "brief")
	if res.Outcome != "failed" || res.ErrorCode != "db_error" {
		t.Fatalf("result = %+v", res)
	}
	if n := count(t, db, `SELECT count(*) FROM campaign_versions`); n != 0 {
		t.Fatalf("versions = %d", n)
	}
	if rec.index(agentloop.EventDraftSaved) != -1 {
		t.Fatal("draft.saved must not be emitted")
	}
}

// D: refusal after a draft was saved.
func TestTurn_RefusalAfterSave(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: validNew},
		mock.Call{Text: "Yardımcı olamam.", Stop: llm.StopRefusal},
	)
	res, _ := runTurn(t, newRunner(t, db, m), chatID, "brief")
	if res.Outcome != "partial" || res.ErrorCode != "refusal" || len(res.CampaignRefs) != 1 {
		t.Fatalf("result = %+v", res)
	}
}

// Every rejected draft is a validation failure; only the rejections sent back
// to the model are repairs. The third rejected round ends the turn instead
// of asking for another repair, so the two counts differ.
func TestMetrics_DraftRejectionsCountedApartFromRepairs(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	tooLong := `{"name":"Uzun","segment_rule":{"match":"all","conditions":[{"kind":"event","event_type":"purchase","within_days":30}]},
 "sms":{"text":"` + strings.Repeat("a", 172) + `"},"content_language":"tr","change_summary":"ilk"}`
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: `{"name":"Kesik"`},
		mock.Call{ToolName: "save_campaign_draft", ToolInput: tooLong},
		mock.Call{ToolName: "save_campaign_draft", ToolInput: tooLong},
	)
	reg := prometheus.NewRegistry()
	r := turns.NewRunner(turns.Deps{Pool: db.Pool, LLM: m, Campaigns: campaign.NewStore(db.Pool), Catalog: segment.DefaultCatalog(),
		Clock: testutil.NewFakeClock(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)), Metrics: metrics.New(reg)})
	res, _ := runTurn(t, r, chatID, "İstanbul için SMS hazırla")
	if res.Outcome != "failed" || res.ErrorCode != "repair_exhausted" {
		t.Fatalf("result = %+v; want failed after three rejected drafts", res)
	}
	for _, c := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"scc_draft_rejections_total", map[string]string{"reason": "invalid_json"}, 1},
		{"scc_draft_rejections_total", map[string]string{"reason": "validation"}, 2},
		{"scc_llm_repairs_total", nil, 2},
		{"scc_turns_total", map[string]string{"outcome": "failed"}, 1},
	} {
		if got := testutil.MetricValue(t, reg, c.name, c.labels); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.labels, got, c.want)
		}
	}
}

// If the outcome cannot be persisted, "done" must not be sent: the client
// would show a result that GET /api/turns/:id cannot return. The turn stays
// running until the reaper marks it interrupted.
func TestExecute_PersistFailure_NoDone(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	chatID := newChat(t, db)
	r := newRunner(t, db, mock.NewScripted(mock.Call{Text: "Merhaba."}))
	begun, err := r.Begin(ctx, turns.BeginRequest{ChatID: chatID, ClientTurnID: uuid(), Text: "selam"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `ALTER TABLE llm_calls RENAME TO llm_calls_broken`); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	r.Execute(ctx, begun.TurnID, rec)

	if rec.index(turns.EventDone) != -1 {
		t.Fatal("done must not be sent when the outcome was not persisted")
	}
	i := rec.index(turns.EventError)
	if i == -1 || rec.events[i].Data.(map[string]any)["code"] != "persist_failed" {
		t.Fatalf("expected error persist_failed, got %+v", rec.events)
	}
	res, err := r.Result(ctx, begun.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "running" || res.AssistantMessage != nil {
		t.Fatalf("nothing of the failed finalize may be visible: %+v", res)
	}
}

func TestHeartbeat_UpdatesWhileRunning(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	chatID := newChat(t, db)
	clk := testutil.NewFakeClock(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	m := mock.NewScripted(mock.Call{Text: strings.Repeat("yavaş ", 20), ChunkDelay: 50 * time.Millisecond})
	r := turns.NewRunner(turns.Deps{Pool: db.Pool, LLM: m, Campaigns: campaign.NewStore(db.Pool),
		Catalog: segment.DefaultCatalog(), Clock: clk})
	begun, err := r.Begin(ctx, turns.BeginRequest{ChatID: chatID, ClientTurnID: uuid(), Text: "selam"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE turns SET heartbeat_at = now() - interval '1 hour' WHERE id = $1`, begun.TurnID); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { r.Execute(ctx, begun.TurnID, &recorder{}); close(done) }()

	clk.WaitForWaiters(t, 1)
	clk.Advance(5 * time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for {
		var (
			status string
			fresh  bool
		)
		if err := db.Pool.QueryRow(ctx, `SELECT status, heartbeat_at > now() - interval '1 minute' FROM turns WHERE id=$1`,
			begun.TurnID).Scan(&status, &fresh); err != nil {
			t.Fatal(err)
		}
		if status != "running" {
			t.Fatal("turn finished before a heartbeat was observed (finalize also refreshes heartbeat_at)")
		}
		if fresh {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("heartbeat_at was not refreshed while the turn ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
	<-done
}

// gatedLLM answers like its Client, but only once release is closed;
// entered is closed when the call starts.
type gatedLLM struct {
	llm.Client
	entered, release chan struct{}
}

func (g *gatedLLM) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	close(g.entered)
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.Client.Stream(ctx, req)
}

// A router call can outlast the reaper's staleness window (a slow or retried
// model call). The turn is alive all along, so it must beat from its start,
// router call included; otherwise the reaper ends a live turn.
func TestHeartbeat_CoversSlowRouterCall(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	chatID := newChat(t, db)
	a := addDraft(t, db, chatID, "İstanbul Sepet")
	clk := testutil.NewFakeClock(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	routerLLM := &gatedLLM{Client: mock.NewScripted(routeCall(t, "edit", "", a)),
		entered: make(chan struct{}), release: make(chan struct{})}
	r := turns.NewRunner(turns.Deps{Pool: db.Pool, LLM: mock.NewScripted(mock.Call{Text: "Kısalttım."}), Router: routerLLM,
		Campaigns: campaign.NewStore(db.Pool), Catalog: segment.DefaultCatalog(), Clock: clk})
	begun, err := r.Begin(ctx, turns.BeginRequest{ChatID: chatID, ClientTurnID: uuid(), Text: "SMS'i kısalt"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { r.Execute(ctx, begun.TurnID, &recorder{}); close(done) }()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(routerLLM.release) }) }
	t.Cleanup(func() { release(); <-done })
	select {
	case <-routerLLM.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the router was never called")
	}

	// The router call has now run past the staleness window by the database
	// clock, and the turn's clock moves on too: a live turn beats meanwhile.
	if _, err := db.Pool.Exec(ctx, `UPDATE turns SET heartbeat_at = now() - interval '1 hour' WHERE id = $1`, begun.TurnID); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		clk.Advance(5 * time.Second)
		if count(t, db, `SELECT count(*) FROM turns WHERE id = $1 AND heartbeat_at > now() - interval '1 minute'`, begun.TurnID) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n, err := r.ReapStale(ctx, 30*time.Second); err != nil || n != 0 {
		t.Fatalf("the reaper ended a live turn during its router call (reaped %d, err %v)", n, err)
	}

	release()
	<-done
	if res, err := r.Result(ctx, begun.TurnID); err != nil || res.Status != "completed" || res.Outcome != "ok" {
		t.Fatalf("result = %+v, %v; want the turn completed", res, err)
	}
}

// ---- router ---------------------------------------------------------------

func newRoutedRunner(t *testing.T, db *testutil.DB, main, routerLLM llm.Client) *turns.Runner {
	t.Helper()
	return turns.NewRunner(turns.Deps{
		Pool: db.Pool, LLM: main, Router: routerLLM, Campaigns: campaign.NewStore(db.Pool),
		Catalog: segment.DefaultCatalog(), Clock: testutil.NewFakeClock(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)),
	})
}

// addDraft creates a draft campaign directly (as a manual version) and
// returns its id.
func addDraft(t *testing.T, db *testutil.DB, chatID, name string) int64 {
	t.Helper()
	s, err := campaign.NewStore(db.Pool).Drafts(campaign.Author{ChatID: chatID, Source: "user"}).SaveDraft(context.Background(),
		campaign.DraftCmd{Name: name, Rule: segment.Rule{Match: "all", Conditions: []segment.Condition{
			{Kind: "event", EventType: "purchase", WithinDays: 30, MinCount: 1}}},
			SMS: &content.SMS{Text: name + " SMS"}, ChangeSummary: "ilk"})
	if err != nil {
		t.Fatal(err)
	}
	return s.CampaignID
}

func routeCall(t *testing.T, intent, question string, ids ...int64) mock.Call {
	t.Helper()
	if ids == nil {
		ids = []int64{}
	}
	b, err := json.Marshal(map[string]any{"intent": intent, "campaign_ids": ids, "question": question, "reason": "test"})
	if err != nil {
		t.Fatal(err)
	}
	return mock.Call{ToolName: router.ToolName, ToolInput: string(b)}
}

func routeOf(t *testing.T, db *testutil.DB, turnID string) map[string]any {
	t.Helper()
	var raw []byte
	if err := db.Pool.QueryRow(context.Background(), `SELECT route FROM turns WHERE id=$1`, turnID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("route json %s: %v", raw, err)
	}
	return m
}

// "Ankara için hoş geldin mesajı hazırla" in a chat that already has
// campaigns: the router reads it as a new campaign.
func TestTurn_RouterCreate_InChatWithCampaigns(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	addDraft(t, db, chatID, "İstanbul Sepet")
	routerLLM := mock.NewScripted(routeCall(t, "create", ""))
	main := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: validNew},
		mock.Call{Text: "Ankara için hoş geldin taslağını hazırladım."},
	)
	res, _ := runTurn(t, newRoutedRunner(t, db, main, routerLLM), chatID, "Ankara için hoş geldin mesajı hazırla")

	if res.Outcome != "ok" || len(res.CampaignRefs) != 1 || res.CampaignRefs[0].Action != "created" || res.CampaignRefs[0].Ordinal != 2 {
		t.Fatalf("result = %+v", res)
	}
	if routerLLM.Calls() != 1 || main.Calls() != 2 {
		t.Fatalf("router calls=%d main calls=%d", routerLLM.Calls(), main.Calls())
	}
	state := main.Requests()[0].Messages[len(main.Requests()[0].Messages)-1].Blocks[0].Text
	if !strings.Contains(state, `"intent":"create"`) || !strings.Contains(state, `"allow_create":true`) {
		t.Errorf("main model must see the routed intent: %s", state)
	}
	if n := count(t, db, `SELECT count(*) FROM llm_calls WHERE turn_id=$1 AND kind='router'`, res.TurnID); n != 1 {
		t.Errorf("router calls audited = %d", n)
	}
	if res.Route == nil || res.Route.Intent != "create" || !res.Route.AllowCreate || len(res.Route.Targets) != 0 {
		t.Errorf("route in result = %+v", res.Route)
	}
}

// The chosen target is announced before anything streams and is part of the
// stored result, so the UI can show "Hedef: Kampanya 1 (İstanbul Sepet)".
func TestTurn_RouteTargetVisible(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	a := addDraft(t, db, chatID, "İstanbul Sepet")
	addDraft(t, db, chatID, "İzmir Yaz")
	routerLLM := mock.NewScripted(routeCall(t, "edit", "", a))
	main := mock.NewScripted(mock.Call{Text: "Kısaltıyorum."})
	res, rec := runTurn(t, newRoutedRunner(t, db, main, routerLLM), chatID, "İstanbul olanın SMS'ini kısalt")

	i := rec.index(turns.EventRoute)
	if i == -1 || (rec.index(agentloop.EventAssistantDelta) != -1 && rec.index(agentloop.EventAssistantDelta) < i) {
		t.Fatalf("route must be announced first, events = %+v", rec.events)
	}
	ev := rec.events[i].Data.(turns.RouteView)
	want := turns.TargetRef{CampaignID: a, Ordinal: 1, Name: "İstanbul Sepet"}
	if ev.Intent != "edit" || len(ev.Targets) != 1 || ev.Targets[0] != want || ev.AllowCreate {
		t.Fatalf("route event = %+v", ev)
	}
	if res.Route == nil || len(res.Route.Targets) != 1 || res.Route.Targets[0] != want {
		t.Fatalf("stored route = %+v", res.Route)
	}
}

// "SMS'i kısalt" with several possible targets: the router asks; the main
// model is never called and nothing is written.
func TestTurn_RouterClarify_SkipsMainModel(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	a, b := addDraft(t, db, chatID, "İstanbul Sepet"), addDraft(t, db, chatID, "İzmir Yaz")
	q := "Hangi kampanyanın SMS'ini kısaltayım: Kampanya 1 mi, Kampanya 2 mi?"
	routerLLM := mock.NewScripted(routeCall(t, "clarify", q, a, b))
	main := mock.NewScripted()
	res, rec := runTurn(t, newRoutedRunner(t, db, main, routerLLM), chatID, "SMS'i kısalt")

	if main.Calls() != 0 {
		t.Fatalf("main model called %d times for a clarification", main.Calls())
	}
	if res.Status != "completed" || res.Outcome != "ok" || res.AssistantMessage == nil || res.AssistantMessage.Text != q {
		t.Fatalf("result = %+v", res)
	}
	i := rec.index(agentloop.EventTargetAmbiguous)
	if i == -1 {
		t.Fatal("target.ambiguous not emitted")
	}
	cands, _ := json.Marshal(rec.events[i].Data.(map[string]any)["candidates"])
	if string(cands) != "["+itoa(a)+","+itoa(b)+"]" {
		t.Errorf("candidates = %s", cands)
	}
	if res.Route == nil || res.Route.Intent != "clarify" || len(res.Route.Candidates) != 2 || res.Route.Candidates[1].Name != "İzmir Yaz" {
		t.Errorf("route = %+v", res.Route)
	}
	if n := count(t, db, `SELECT count(*) FROM campaign_versions WHERE created_by_turn_id=$1`, res.TurnID); n != 0 {
		t.Fatalf("versions written = %d", n)
	}
}

// An approved campaign is not editable: Go turns the edit into a question.
func TestTurn_RouterEditApproved_Asks(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	a := addDraft(t, db, chatID, "İstanbul Sepet")
	if _, err := db.Pool.Exec(context.Background(), `UPDATE campaigns SET status='approved' WHERE id=$1`, a); err != nil {
		t.Fatal(err)
	}
	routerLLM := mock.NewScripted(routeCall(t, "edit", "", a))
	main := mock.NewScripted()
	res, _ := runTurn(t, newRoutedRunner(t, db, main, routerLLM), chatID, "İstanbul kampanyasının SMS'ini kısalt")

	if main.Calls() != 0 || res.Outcome != "ok" || !strings.Contains(res.AssistantMessage.Text, "onaylanmış") {
		t.Fatalf("calls=%d result=%+v", main.Calls(), res)
	}
	if r := routeOf(t, db, res.TurnID); r["intent"] != "clarify" || r["reject_reason"] != "not_editable" {
		t.Errorf("stored route = %v", r)
	}
}

// A failed router call fails the turn without side effects; it is not
// treated as ambiguity and nothing is guessed.
func TestTurn_RouterError_FailsTurn(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	addDraft(t, db, chatID, "İstanbul Sepet")
	routerLLM := mock.NewScripted(mock.Call{Err: llm.ErrUnavailable})
	main := mock.NewScripted()
	res, rec := runTurn(t, newRoutedRunner(t, db, main, routerLLM), chatID, "SMS'i kısalt")

	if main.Calls() != 0 || res.Outcome != "failed" || res.ErrorCode != "llm_unavailable" || len(res.CampaignRefs) != 0 {
		t.Fatalf("calls=%d result=%+v", main.Calls(), res)
	}
	if rec.index(turns.EventRoute) != -1 {
		t.Error("no route may be announced when routing failed")
	}
	var errCode *string
	if err := db.Pool.QueryRow(context.Background(), `SELECT error_code FROM llm_calls WHERE turn_id=$1 AND kind='router'`,
		res.TurnID).Scan(&errCode); err != nil || errCode == nil || *errCode != "llm_unavailable" {
		t.Errorf("router failure not audited: %v %v", errCode, err)
	}
	if strings.Contains(joinedText(nextContext(t, newRoutedRunner(t, db, mock.NewScripted(), nil), chatID)), "SMS'i kısalt") {
		t.Error("a failed turn without side effects must not enter the next context")
	}
}

// An edit route grants no creation, whatever the main model tries.
func TestTurn_RouterEdit_MainCannotCreate(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	a := addDraft(t, db, chatID, "İstanbul Sepet")
	addDraft(t, db, chatID, "İzmir Yaz")
	routerLLM := mock.NewScripted(routeCall(t, "edit", "", a))
	main := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: validNew},
		mock.Call{Text: "Yeni kampanya açmadım; İstanbul kampanyasını mı güncelleyeyim?"},
	)
	res, _ := runTurn(t, newRoutedRunner(t, db, main, routerLLM), chatID, "İstanbul olanı Ankara'ya uyarla")
	if len(res.CampaignRefs) != 0 {
		t.Fatalf("refs = %+v", res.CampaignRefs)
	}
	tr := main.Requests()[1].Messages[len(main.Requests()[1].Messages)-1].Blocks[0]
	if !tr.IsError || !strings.Contains(tr.Text, "create_not_allowed") {
		t.Fatalf("tool result = %+v", tr)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// ---- dedupe and reaper ------------------------------------------------------

func TestBegin_SameClientTurnID_ReturnsExisting(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	chatID := newChat(t, db)
	r := newRunner(t, db, mock.NewScripted())
	id := uuid()
	first, err := r.Begin(ctx, turns.BeginRequest{ChatID: chatID, ClientTurnID: id, Text: "merhaba"})
	if err != nil || !first.New {
		t.Fatalf("first = %+v, %v", first, err)
	}
	again, err := r.Begin(ctx, turns.BeginRequest{ChatID: chatID, ClientTurnID: id, Text: "merhaba"})
	if err != nil || again.New || again.TurnID != first.TurnID || again.Status != "running" {
		t.Fatalf("again = %+v, %v; a repeated request must map to the same turn", again, err)
	}
	if n := count(t, db, `SELECT count(*) FROM messages WHERE turn_id=$1 AND role='user'`, first.TurnID); n != 1 {
		t.Fatalf("user messages = %d", n)
	}
}

func TestBegin_SameIDDifferentText_Mismatch(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	chatID := newChat(t, db)
	r := newRunner(t, db, mock.NewScripted())
	id := uuid()
	if _, err := r.Begin(ctx, turns.BeginRequest{ChatID: chatID, ClientTurnID: id, Text: "merhaba"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Begin(ctx, turns.BeginRequest{ChatID: chatID, ClientTurnID: id, Text: "başka"}); !errors.Is(err, turns.ErrIdempotencyMismatch) {
		t.Fatalf("err = %v, want ErrIdempotencyMismatch", err)
	}
}

func TestBegin_OtherTurnRunning_InProgress(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	chatID := newChat(t, db)
	r := newRunner(t, db, mock.NewScripted())
	if _, err := r.Begin(ctx, turns.BeginRequest{ChatID: chatID, ClientTurnID: uuid(), Text: "bir"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Begin(ctx, turns.BeginRequest{ChatID: chatID, ClientTurnID: uuid(), Text: "iki"}); !errors.Is(err, turns.ErrTurnInProgress) {
		t.Fatalf("err = %v, want ErrTurnInProgress", err)
	}
}

func TestBegin_UnknownChat(t *testing.T) {
	db := testutil.NewDB(t)
	r := newRunner(t, db, mock.NewScripted())
	_, err := r.Begin(context.Background(), turns.BeginRequest{ChatID: uuid(), ClientTurnID: uuid(), Text: "x"})
	if !errors.Is(err, turns.ErrChatNotFound) {
		t.Fatalf("err = %v, want ErrChatNotFound", err)
	}
}

// A turn orphaned by a crash (stale heartbeat) is ended as interrupted and
// unlocks the chat; a live turn is left alone.
func TestReaper_StaleRunning_FailsAndUnlocks(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	chatID := newChat(t, db)
	r := newRunner(t, db, mock.NewScripted())
	stale, _ := r.Begin(ctx, turns.BeginRequest{ChatID: chatID, ClientTurnID: uuid(), Text: "eski"})

	if n, err := r.ReapStale(ctx, 30*time.Second); err != nil || n != 0 {
		t.Fatalf("fresh turn reaped: n=%d err=%v", n, err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE turns SET heartbeat_at = now() - interval '5 minutes' WHERE id=$1`, stale.TurnID); err != nil {
		t.Fatal(err)
	}
	if n, err := r.ReapStale(ctx, 30*time.Second); err != nil || n != 1 {
		t.Fatalf("reaped n=%d err=%v", n, err)
	}
	res, err := r.Result(ctx, stale.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" || res.Outcome != "failed" || res.ErrorCode != "interrupted" || res.AssistantMessage == nil {
		t.Fatalf("result = %+v", res)
	}
	if _, err := r.Begin(ctx, turns.BeginRequest{ChatID: chatID, ClientTurnID: uuid(), Text: "yeni"}); err != nil {
		t.Fatalf("chat must be unlocked after reaping: %v", err)
	}
}

// A reaped turn that had already saved a draft is partial, not failed.
func TestReaper_TurnWithSavedDraftIsPartial(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	chatID := newChat(t, db)
	r := newRunner(t, db, mock.NewScripted())
	b, _ := r.Begin(ctx, turns.BeginRequest{ChatID: chatID, ClientTurnID: uuid(), Text: "brief"})
	if _, err := campaign.NewStore(db.Pool).Drafts(campaign.Author{ChatID: chatID, TurnID: b.TurnID, Source: "llm"}).SaveDraft(ctx,
		campaign.DraftCmd{Name: "A", Rule: segment.Rule{Match: "all", Conditions: []segment.Condition{
			{Kind: "event", EventType: "purchase", WithinDays: 30, MinCount: 1}}}, SMS: &content.SMS{Text: "x"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE turns SET heartbeat_at = now() - interval '5 minutes' WHERE id=$1`, b.TurnID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReapStale(ctx, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	res, _ := r.Result(ctx, b.TurnID)
	if res.Outcome != "partial" || len(res.CampaignRefs) != 1 || !strings.Contains(res.AssistantMessage.Text, "Kampanya 1 v1") {
		t.Fatalf("result = %+v", res)
	}
}

// Go's note about a turn that failed after saving is written in the language
// of the interface the turn was started from.
func TestTurn_SaveThenLLMError_NoteInTheInterfaceLanguage(t *testing.T) {
	db := testutil.NewDB(t)
	chatID := newChat(t, db)
	m := mock.NewScripted(
		mock.Call{Text: "Hazırlıyorum.", ToolName: "save_campaign_draft", ToolInput: validNew},
		mock.Call{Text: "Kaydettim, 3", Err: llm.ErrUnavailable, ErrAfterEvents: 2},
	)
	r := newRunner(t, db, m)
	ctx := context.Background()
	begun, err := r.Begin(ctx, turns.BeginRequest{ChatID: chatID, ClientTurnID: uuid(), Text: "brief", Lang: "en"})
	if err != nil {
		t.Fatal(err)
	}
	r.Execute(ctx, begun.TurnID, &recorder{})
	res, err := r.Result(ctx, begun.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	text := res.AssistantMessage.Text
	for _, want := range []string{"Hazırlıyorum.", "Campaign 1 v1 saved", "answer could not be completed", "temporarily not responding"} {
		if !strings.Contains(text, want) {
			t.Errorf("assistant text %q lacks %q", text, want)
		}
	}
	if strings.Contains(text, "kaydedildi") {
		t.Errorf("the note is still Turkish: %q", text)
	}
}
