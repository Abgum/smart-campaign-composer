package seed_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/analytics"
	"smart-campaign-composer/backend/internal/clock"
	"smart-campaign-composer/backend/internal/delivery"
	"smart-campaign-composer/backend/internal/geo"
	"smart-campaign-composer/backend/internal/mockprovider"
	"smart-campaign-composer/backend/internal/provider"
	"smart-campaign-composer/backend/internal/seed"
	"smart-campaign-composer/backend/internal/testutil"
)

func verifyHistoryAcceptances(statusByID map[string]string, acceptedByID map[string]int) error {
	for id, status := range statusByID {
		accepted := acceptedByID[id]
		switch status {
		case "sent":
			if accepted != 1 {
				return fmt.Errorf("delivery %s is sent but provider accepted it %d times", id, accepted)
			}
		case "failed", "skipped_no_consent":
			if accepted != 0 {
				return fmt.Errorf("delivery %s is %s but provider accepted it %d times", id, status, accepted)
			}
		case "unknown":
			if accepted > 1 {
				return fmt.Errorf("delivery %s is unknown but provider accepted it %d times", id, accepted)
			}
		default:
			return fmt.Errorf("delivery %s has nonterminal status %s", id, status)
		}
	}
	for id := range acceptedByID {
		if _, ok := statusByID[id]; !ok {
			return fmt.Errorf("provider accepted delivery %s absent from history", id)
		}
	}
	return nil
}

func TestHistoryAcceptanceCheck_RejectsMatchingTotalsWithWrongDeliveries(t *testing.T) {
	statuses := map[string]string{"sent-id": "sent", "failed-id": "failed"}
	accepted := map[string]int{"failed-id": 1}
	if err := verifyHistoryAcceptances(statuses, accepted); err == nil {
		t.Fatal("equal totals hid a sent delivery without provider acceptance and an accepted failed delivery")
	}
}

// History adds weeks of campaigns to a seeded database: chats with the
// marketers' requests and campaigns approved on many past days. It sends
// nothing itself: the worker sends through the mock provider as for any
// campaign, then ShiftHistory moves the recorded times to each approval
// day. It runs once.
func TestHistory_SentByTheWorkerThroughTheMockProvider(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	istanbul, _ := time.LoadLocation("Europe/Istanbul")
	if _, err := seed.Insert(ctx, db.Pool, seed.Generate(1500, 1, now)); err != nil {
		t.Fatal(err)
	}

	added, err := seed.InsertHistory(ctx, db.Pool, istanbul, now)
	if err != nil || !added {
		t.Fatalf("history = %v, %v; want added", added, err)
	}
	again, err := seed.InsertHistory(ctx, db.Pool, istanbul, now)
	if err != nil || again {
		t.Fatalf("second run = %v, %v; want a no-op", again, err)
	}

	var due, ended, drafts, days int
	if err := db.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM deliveries WHERE status = 'pending'),
		       (SELECT count(*) FROM deliveries WHERE status <> 'pending'),
		       (SELECT count(*) FROM campaigns WHERE status = 'draft'),
		       (SELECT count(DISTINCT (approved_at AT TIME ZONE 'Europe/Istanbul')::date) FROM campaigns WHERE approved_at IS NOT NULL)`).
		Scan(&due, &ended, &drafts, &days); err != nil {
		t.Fatal(err)
	}
	if due == 0 || ended != 0 || drafts < 1 || days < 10 {
		t.Fatalf("after loading: due=%d ended=%d drafts=%d approval days=%d; want everything left to the worker", due, ended, drafts, days)
	}

	// The worker sends through the mock provider, with the shared limit.
	faults, err := mockprovider.NewRandom(42, map[mockprovider.Fault]float64{
		mockprovider.Reject400: 0.03, mockprovider.Fail503: 0.03, mockprovider.AcceptThenDrop: 0.02, mockprovider.Reject429: 0.01,
	})
	if err != nil {
		t.Fatal(err)
	}
	mock, err := mockprovider.Open(filepath.Join(t.TempDir(), "provider.log"), faults)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mock.Close() })
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	limit := delivery.Rate{Calls: 500, Window: time.Second}
	wk := &delivery.Worker{
		Store:    delivery.NewStore(db.Pool, delivery.Policy{MaxAttempts: 3, BaseDelay: 10 * time.Millisecond, MaxDelay: 40 * time.Millisecond}),
		Provider: provider.NewClient(srv.URL, provider.Options{Timeout: 150 * time.Millisecond}),
		Channels: map[string]delivery.Rate{"sms": limit, "email": limit}, Clock: clock.Real{},
		Config: delivery.Config{Lease: 2 * time.Second, CallBudget: time.Second, Batch: 100, Concurrency: 16,
			Idle: 20 * time.Millisecond, ReapEvery: 50 * time.Millisecond},
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- wk.Run(runCtx) }()
	deadline := time.Now().Add(90 * time.Second)
	for {
		left, err := seed.HistoryLeft(ctx, db.Pool)
		if err != nil {
			t.Fatal(err)
		}
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d history deliveries or campaigns still open", left)
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	recorded := map[string]time.Time{}
	rows, err := db.Pool.Query(ctx, `SELECT id::text, finished_at FROM deliveries`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var (
			id string
			at time.Time
		)
		if err := rows.Scan(&id, &at); err != nil {
			t.Fatal(err)
		}
		recorded[id] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	shifted, err := seed.ShiftHistory(ctx, db.Pool)
	if err != nil || shifted < 10 {
		t.Fatalf("shifted %d campaigns, %v", shifted, err)
	}
	if again, err := seed.ShiftHistory(ctx, db.Pool); err != nil || again != 0 {
		t.Fatalf("second shift moved %d campaigns, %v; want none", again, err)
	}

	// Each campaign moved by one offset: its first recorded terminal result
	// is 3 s after approval, with gaps between terminal results preserved.
	rows, err = db.Pool.Query(ctx, `
		SELECT d.id::text, d.campaign_id, d.finished_at, c.approved_at, d.status, d.sent_at IS NOT NULL
		FROM deliveries d JOIN campaigns c ON c.id = d.campaign_id`)
	if err != nil {
		t.Fatal(err)
	}
	offsets := map[int64]time.Duration{}
	first := map[int64]time.Time{}
	approved := map[int64]time.Time{}
	statusByID := map[string]string{}
	var sent, failed int
	for rows.Next() {
		var (
			id           string
			campaignID   int64
			at, approval time.Time
			status       string
			hasSentAt    bool
		)
		if err := rows.Scan(&id, &campaignID, &at, &approval, &status, &hasSentAt); err != nil {
			t.Fatal(err)
		}
		off := at.Sub(recorded[id])
		if prev, ok := offsets[campaignID]; ok && prev != off {
			t.Fatalf("campaign %d moved by %v and by %v", campaignID, prev, off)
		}
		offsets[campaignID] = off
		statusByID[id] = status
		if f, ok := first[campaignID]; !ok || at.Before(f) {
			first[campaignID] = at
		}
		approved[campaignID] = approval
		if !sameDay(at, approval, istanbul) {
			t.Errorf("delivery %s ended %v, not on its approval day %v", id, at, approval)
		}
		if (status == "sent") != hasSentAt {
			t.Errorf("delivery %s is %s with sent_at %v", id, status, hasSentAt)
		}
		switch status {
		case "sent":
			sent++
		case "failed":
			failed++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for id, f := range first {
		if want := approved[id].Add(3 * time.Second); !f.Equal(want) {
			t.Errorf("campaign %d: first terminal result at %v, want %v", id, f, want)
		}
	}

	// The provider took every sent message once; the definite refusals ended failed.
	stats := mock.Stats()
	for _, m := range stats.ByMessage {
		if m.Accepted > 1 {
			t.Errorf("%s accepted %d times", m.To, m.Accepted)
		}
	}
	if err := verifyHistoryAcceptances(statusByID, stats.ByKey); err != nil {
		t.Fatal(err)
	}
	if sent == 0 || failed == 0 {
		t.Fatalf("sent=%d failed=%d; want both outcomes in the test history", sent, failed)
	}

	// Each campaign came from a request in a chat, answered by the demo model.
	var turns, messages, unlabelled, orphans int
	if err := db.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM turns WHERE status = 'completed' AND outcome = 'ok'),
		       (SELECT count(*) FROM messages),
		       (SELECT count(*) FROM messages WHERE role = 'assistant' AND llm_provider IS DISTINCT FROM 'mock'),
		       (SELECT count(*) FROM campaigns c WHERE NOT EXISTS (
		           SELECT 1 FROM turn_campaign_refs r WHERE r.campaign_id = c.id AND r.action = 'created'))`).
		Scan(&turns, &messages, &unlabelled, &orphans); err != nil {
		t.Fatal(err)
	}
	if turns == 0 || messages != 2*turns || unlabelled != 0 || orphans != 0 {
		t.Fatalf("turns=%d messages=%d unlabelled=%d orphans=%d", turns, messages, unlabelled, orphans)
	}

	// The overview has something to show on many days.
	o, err := analytics.Read(ctx, db.Pool, geo.Default(), istanbul, now, analytics.Filter{Range: "30d"})
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, p := range o.Series {
		if p.SMS+p.Email > 0 {
			active++
		}
	}
	if active < 6 || len(o.Cities.Top) < 3 {
		t.Fatalf("30 days: %d active days, %d cities", active, len(o.Cities.Top))
	}
}

func sameDay(a, b time.Time, loc *time.Location) bool {
	y1, m1, d1 := a.In(loc).Date()
	y2, m2, d2 := b.In(loc).Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}

// History needs the recipients first.
func TestInsertHistory_NeedsRecipients(t *testing.T) {
	db := testutil.NewDB(t)
	istanbul, _ := time.LoadLocation("Europe/Istanbul")
	if _, err := seed.InsertHistory(context.Background(), db.Pool, istanbul, now); err == nil {
		t.Fatal("history on an empty database must fail")
	}
}
