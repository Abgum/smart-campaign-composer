package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/clock"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/httpapi"
	"smart-campaign-composer/backend/internal/llm"
	"smart-campaign-composer/backend/internal/llm/mock"
	"smart-campaign-composer/backend/internal/metrics"
	"smart-campaign-composer/backend/internal/segment"
)

func fastStreams(d *httpapi.Deps) { d.StreamEvery = 50 * time.Millisecond }

// approveOne approves an SMS campaign for two people in İstanbul, one of
// whom has no SMS consent; it returns the campaign id.
func (e *env) approveOne(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	chatID := e.createChat(t)
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO users (first_name, last_name, phone, country_code, admin1_code, city_geoname_id,
		birth_date, consent_sms) VALUES ('Ayşe','Yılmaz','+905550000001','TR','TR-34',745044,'1990-01-01',true),
		('Mehmet','Kaya','+905550000002','TR','TR-34',745044,'1990-01-01',false)`); err != nil {
		t.Fatal(err)
	}
	store := campaign.NewStore(e.db.Pool)
	saved, err := store.Drafts(campaign.Author{ChatID: chatID, Source: "user"}).SaveDraft(ctx, campaign.DraftCmd{
		Name: "İstanbul", SMS: &content.SMS{Text: "Merhaba {{first_name}}"}, ChangeSummary: "ilk",
		Rule: segment.Rule{Match: "all", Conditions: []segment.Condition{{Kind: "location", Operator: "in",
			Places: []segment.PlaceRef{{Level: "city", GeonameID: 745044, Country: "TR", Name: "İstanbul"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Approve(ctx, saved.CampaignID, saved.VersionID, segment.DefaultCatalog(), time.Now()); err != nil {
		t.Fatal(err)
	}
	return saved.CampaignID
}

type dashboardView struct {
	Campaigns []struct {
		ID       string
		Status   string
		Channels map[string]map[string]int
	}
	Totals   map[string]map[string]int
	Channels map[string]struct{ Due int }
}

func TestDashboardEndpoints(t *testing.T) {
	e := newEnvWith(t, mock.NewScripted(), fastStreams, nil)
	id := e.approveOne(t)
	publicID := publicCampaignID(t, e, id)

	resp, err := http.Get(e.srv.URL + "/api/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	var d dashboardView
	err = json.NewDecoder(resp.Body).Decode(&d)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || len(d.Campaigns) != 1 || d.Campaigns[0].ID != publicID {
		t.Fatalf("GET /api/dashboard: %d %+v, err %v", resp.StatusCode, d, err)
	}
	if sms := d.Campaigns[0].Channels["sms"]; sms["pending"] != 1 || sms["excluded_consent"] != 1 {
		t.Fatalf("campaign sms = %v; want 1 pending, 1 left out for consent", sms)
	}
	if d.Totals["sms"]["pending"] != 1 || d.Channels["sms"].Due != 1 {
		t.Fatalf("totals = %v, channels = %v; want 1 pending and due", d.Totals, d.Channels)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.srv.URL+"/api/dashboard/stream", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream content type %q", resp.Header.Get("Content-Type"))
	}
	start := time.Now()
	events := readEvents(t, resp.Body, func() func(sseEvent) bool {
		n := 0
		return func(sseEvent) bool { n++; return n == 3 }
	}())
	if len(events) != 3 || time.Since(start) < 90*time.Millisecond {
		t.Fatalf("got %d events in %v; want 3, a tick apart", len(events), time.Since(start))
	}
	for _, ev := range events {
		var d dashboardView
		if ev.Type != "dashboard" || json.Unmarshal(ev.Data, &d) != nil || len(d.Campaigns) != 1 {
			t.Fatalf("event %s %s", ev.Type, ev.Data)
		}
	}
}

// A campaign's stream reports its progress until the campaign completes,
// then ends.
func TestCampaignStream_EndsWhenCompleted(t *testing.T) {
	e := newEnvWith(t, mock.NewScripted(), fastStreams, nil)
	id := e.approveOne(t)
	publicID := publicCampaignID(t, e, id)
	resp, err := http.Get(e.srv.URL + "/api/campaigns/" + publicID + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first := readEvents(t, resp.Body, func(sseEvent) bool { return true })
	var c struct {
		ID       string
		Status   string
		Channels map[string]map[string]int
	}
	if len(first) != 1 || first[0].Type != "campaign" || json.Unmarshal(first[0].Data, &c) != nil ||
		c.ID != publicID || c.Status != "approved" || c.Channels["sms"]["pending"] != 1 {
		t.Fatalf("first event = %+v", first)
	}

	ctx := context.Background()
	if _, err := e.db.Pool.Exec(ctx, `UPDATE deliveries SET status = 'sent' WHERE campaign_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE campaigns SET status = 'completed' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	done := make(chan []sseEvent, 1)
	go func() { done <- readEvents(t, resp.Body, nil) }()
	select {
	case rest := <-done:
		last := rest[len(rest)-1]
		if json.Unmarshal(last.Data, &c) != nil || c.Status != "completed" || c.Channels["sms"]["sent"] != 1 {
			t.Fatalf("last event = %s %s; want the completed campaign", last.Type, last.Data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end after the campaign completed")
	}

	if resp, err := http.Get(e.srv.URL + "/api/campaigns/999999/stream"); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown campaign: %v %v", resp.StatusCode, err)
	}
}

// /readyz tells whether the API can serve (the database answers) and which
// LLM serves the chat, with the reason when it is the mock.
func TestReadyz_ReportsProvider(t *testing.T) {
	info := llm.Info{Provider: "mock", Model: "mock-demo", Reason: "ANTHROPIC_API_KEY tanımlı değil"}
	e := newEnvWith(t, mock.NewScripted(), func(d *httpapi.Deps) { d.LLM = info }, nil)
	code, body := e.do(t, http.MethodGet, "/readyz", nil)
	llmBody, _ := body["llm"].(map[string]any)
	if code != http.StatusOK || body["status"] != "ready" || body["db"] != "ok" ||
		llmBody["provider"] != "mock" || llmBody["reason"] != info.Reason {
		t.Fatalf("ready: %d %v", code, body)
	}

	bad, err := pgxpool.New(context.Background(), "postgres://scc@127.0.0.1:1/scc?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{Pool: bad, LLM: info, Clock: clock.Real{}}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if llmOut, _ := out["llm"].(map[string]any); resp.StatusCode != http.StatusServiceUnavailable ||
		out["status"] != "unavailable" || llmOut["provider"] != "mock" {
		t.Fatalf("database down: %d %v", resp.StatusCode, out)
	}
}

// /readyz checks the migration record against the migrations this build
// has, not whether some table exists: tables can be there while the record
// is behind (a migration half done, or run by hand). The check only reads;
// it never creates the version table.
// latestMigration is the newest migration of this build (00007: A/B test).
const latestMigration = 7

func TestReadyz_ChecksMigrationVersion(t *testing.T) {
	ready := func(t *testing.T, e *env) (int, map[string]any, map[string]any) {
		t.Helper()
		code, body := e.do(t, http.MethodGet, "/readyz", nil)
		schema, _ := body["schema"].(map[string]any)
		return code, body, schema
	}
	t.Run("record behind the tables", func(t *testing.T) {
		e := newEnv(t, mock.NewScripted())
		ctx := context.Background()
		if _, err := e.db.Pool.Exec(ctx, `DELETE FROM goose_db_version WHERE version_id > 0`); err != nil {
			t.Fatal(err)
		}
		if code, body, schema := ready(t, e); code != http.StatusServiceUnavailable || body["db"] != "not_migrated" ||
			schema["version"] != float64(0) || schema["latest"] != float64(latestMigration) {
			t.Fatalf("no migration recorded: %d %v; want 503 not_migrated at version 0 of %d", code, body, latestMigration)
		}
		if _, err := e.db.Pool.Exec(ctx, `DROP TABLE goose_db_version`); err != nil {
			t.Fatal(err)
		}
		if code, body, _ := ready(t, e); code != http.StatusServiceUnavailable || body["db"] != "not_migrated" {
			t.Fatalf("no version table: %d %v; want 503 not_migrated", code, body)
		}
		var created bool
		if err := e.db.Pool.QueryRow(ctx, `SELECT to_regclass('goose_db_version') IS NOT NULL`).Scan(&created); err != nil || created {
			t.Fatalf("readyz created the version table (%v, %v); it must only read", created, err)
		}
	})
	t.Run("up to date", func(t *testing.T) {
		e := newEnv(t, mock.NewScripted())
		if code, body, schema := ready(t, e); code != http.StatusOK || body["db"] != "ok" ||
			schema["version"] != float64(latestMigration) || schema["latest"] != float64(latestMigration) {
			t.Fatalf("migrated: %d %v; want 200 at version %d of %d", code, body, latestMigration, latestMigration)
		}
	})
}

// /metrics exposes the process's counters and the database gauges.
func TestMetricsExposed(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	e := newEnvWith(t, mock.NewScripted(mock.Call{Text: reply}), func(d *httpapi.Deps) {
		d.Metrics, d.Gatherer = m, reg
	}, nil)
	reg.MustRegister(metrics.NewDBCollector(e.db.Pool, clock.Real{}))

	chatID, clientID := e.createChat(t), uuid()
	resp := e.postTurn(t, context.Background(), chatID, clientID, "merhaba")
	readEvents(t, resp.Body, nil)
	resp.Body.Close()
	resp = e.postTurn(t, context.Background(), chatID, clientID, "merhaba") // a repeat: answered from the stored turn
	resp.Body.Close()
	e.approveOne(t)

	resp, err := http.Get(e.srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		`scc_turns_total{outcome="ok"} 1`,
		`scc_llm_requests_total{kind="main",outcome="ok",provider="mock"} 1`,
		`scc_turn_dedup_total 1`,
		`scc_deliveries{channel="sms",status="pending"} 1`,
		`scc_deliveries_excluded{channel="sms",reason="consent"} 1`,
		`scc_queue_due{channel="sms"} 1`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
	if t.Failed() {
		t.Logf("/metrics (%d):\n%s", resp.StatusCode, body)
	}
}
