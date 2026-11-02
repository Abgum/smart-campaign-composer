package delivery_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/delivery"
	"smart-campaign-composer/backend/internal/mockprovider"
	"smart-campaign-composer/backend/internal/provider"
	"smart-campaign-composer/backend/internal/testutil"
)

type fixture struct {
	db       *testutil.DB
	store    *delivery.Store
	campaign int64
	people   int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := testutil.NewDB(t)
	f := &fixture{db: db, store: delivery.NewStore(db.Pool, policy(0))}
	ctx := context.Background()
	var chat string
	if err := db.Pool.QueryRow(ctx, `INSERT INTO chats DEFAULT VALUES RETURNING id::text`).Scan(&chat); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `INSERT INTO campaigns (chat_id, ordinal, name, status) VALUES ($1, 1, 'A', 'sending') RETURNING id`,
		chat).Scan(&f.campaign); err != nil {
		t.Fatal(err)
	}
	return f
}

// inFlight inserts a delivery to a new person, claimed by a worker after
// made attempts (ambiguous of them), its lease ending at until.
func (f *fixture) inFlight(t *testing.T, made, ambiguous int, until time.Time) (delivery.Lease, string) {
	t.Helper()
	ctx := context.Background()
	f.people++
	phone := fmt.Sprintf("+90555%07d", f.people)
	var user int64
	if err := f.db.Pool.QueryRow(ctx, `
		INSERT INTO users (first_name, last_name, phone, country_code, birth_date, consent_sms)
		VALUES ('Ayşe', 'Yılmaz', $1, 'TR', '1990-01-01', true) RETURNING id`, phone).Scan(&user); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := f.db.Pool.QueryRow(ctx, `
		INSERT INTO deliveries (campaign_id, user_id, channel, body, content_hash, status, attempts, ambiguous_attempts, locked_until)
		VALUES ($1, $2, 'sms', 'Merhaba Ayşe', 'hash', 'in_flight', $3, $4, $5) RETURNING id::text`,
		f.campaign, user, made, ambiguous, until).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return delivery.Lease{ID: id, Attempts: made}, phone
}

// claim puts a queued delivery back in flight, as the worker's claim does.
func (f *fixture) claim(t *testing.T, id string, until time.Time) delivery.Lease {
	t.Helper()
	l := delivery.Lease{ID: id}
	if err := f.db.Pool.QueryRow(context.Background(), `
		UPDATE deliveries SET status = 'in_flight', locked_until = $2 WHERE id = $1 AND status = 'retry'
		RETURNING attempts`, id, until).Scan(&l.Attempts); err != nil {
		t.Fatalf("claim: %v", err)
	}
	return l
}

type row struct {
	Status            string
	Attempts          int
	Ambiguous         int
	NextAttemptAt     time.Time
	LockedUntil       *time.Time
	LastError         *string
	ProviderMessageID *string
	SentAt            *time.Time
}

func (f *fixture) row(t *testing.T, id string) row {
	t.Helper()
	var r row
	if err := f.db.Pool.QueryRow(context.Background(), `
		SELECT status, attempts, ambiguous_attempts, next_attempt_at, locked_until, last_error, provider_message_id, sent_at
		FROM deliveries WHERE id = $1`, id).Scan(&r.Status, &r.Attempts, &r.Ambiguous, &r.NextAttemptAt, &r.LockedUntil,
		&r.LastError, &r.ProviderMessageID, &r.SentAt); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRecord_RetryableAttempt_Requeued(t *testing.T) {
	f := newFixture(t)
	l, _ := f.inFlight(t, 0, 0, t0.Add(30*time.Second))
	d, err := f.store.Record(context.Background(), l,
		provider.Result{Outcome: provider.NotSentRetryable, Status: 429, RetryAfter: 3 * time.Second}, t0)
	if err != nil || d.Status != delivery.StatusRetry {
		t.Fatalf("decision = %+v, err = %v", d, err)
	}
	r := f.row(t, l.ID)
	if r.Status != "retry" || r.Attempts != 1 || r.Ambiguous != 0 || r.LockedUntil != nil ||
		!r.NextAttemptAt.Equal(t0.Add(3*time.Second)) || r.LastError == nil || !strings.Contains(*r.LastError, "429") {
		t.Fatalf("row = %+v", r)
	}
}

func TestRecord_Sent(t *testing.T) {
	f := newFixture(t)
	l, _ := f.inFlight(t, 1, 1, t0.Add(30*time.Second))
	d, err := f.store.Record(context.Background(), l, provider.Result{Outcome: provider.Sent, Status: 200, MessageID: "msg_9"}, t0)
	if err != nil || d.Status != delivery.StatusSent {
		t.Fatalf("decision = %+v, err = %v", d, err)
	}
	r := f.row(t, l.ID)
	if r.Status != "sent" || r.Attempts != 2 || r.Ambiguous != 1 || r.LockedUntil != nil || r.LastError != nil ||
		r.ProviderMessageID == nil || *r.ProviderMessageID != "msg_9" || r.SentAt == nil || !r.SentAt.Equal(t0) {
		t.Fatalf("row = %+v", r)
	}
}

// A worker whose lease was lost (the reaper reclaimed the delivery) cannot
// overwrite what happened since.
func TestRecord_LostLease_Rejected(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	l, _ := f.inFlight(t, 0, 0, t0.Add(30*time.Second))
	if _, err := f.db.Pool.Exec(ctx, `
		UPDATE deliveries SET status = 'retry', attempts = 1, ambiguous_attempts = 1, locked_until = NULL WHERE id = $1`, l.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Record(ctx, l, provider.Result{Outcome: provider.NotSentPermanent, Status: 400}, t0); !errors.Is(err, delivery.ErrLeaseLost) {
		t.Fatalf("err = %v, want ErrLeaseLost", err)
	}
	if r := f.row(t, l.ID); r.Status != "retry" || r.Attempts != 1 || r.Ambiguous != 1 {
		t.Fatalf("row changed: %+v", r)
	}
}

// A worker that died or stalled mid-attempt may have reached the provider:
// reclaiming its delivery counts as an ambiguous attempt.
func TestReaperReclaim_CountsAsAmbiguous(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	expired, _ := f.inFlight(t, 0, 0, t0.Add(-time.Second))
	live, _ := f.inFlight(t, 0, 0, t0.Add(10*time.Second))
	lastTry, _ := f.inFlight(t, 4, 0, t0.Add(-time.Second))

	n, err := f.store.ReapExpired(ctx, t0)
	if err != nil || n != 2 {
		t.Fatalf("reaped %d, err %v; want 2", n, err)
	}
	if r := f.row(t, expired.ID); r.Status != "retry" || r.Attempts != 1 || r.Ambiguous != 1 || r.LockedUntil != nil ||
		!r.NextAttemptAt.After(t0) || r.LastError == nil {
		t.Fatalf("expired lease: %+v, want a retry counted as ambiguous", r)
	}
	if r := f.row(t, live.ID); r.Status != "in_flight" || r.Attempts != 0 {
		t.Fatalf("live lease touched: %+v", r)
	}
	// Four definite misses and a lost fifth attempt: it may have been
	// accepted, so the delivery is unknown, not failed.
	if r := f.row(t, lastTry.ID); r.Status != "unknown" || r.Attempts != 5 || r.Ambiguous != 1 {
		t.Fatalf("lost last attempt: %+v, want unknown", r)
	}
	if _, err := f.store.Record(ctx, expired, provider.Result{Outcome: provider.NotSentPermanent, Status: 400}, t0); !errors.Is(err, delivery.ErrLeaseLost) {
		t.Fatalf("late record after the reclaim: err = %v, want ErrLeaseLost", err)
	}
}

func startProvider(t *testing.T, path string, faults mockprovider.Faults) (*mockprovider.Provider, *httptest.Server) {
	t.Helper()
	mp, err := mockprovider.Open(path, faults)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mp.Close() })
	return mp, httptest.NewServer(mp)
}

// The provider accepts the first attempt, its answer is lost, and the
// provider restarts before the retry. The retry carries the same
// idempotency key, so the provider answers with that first acceptance and
// the person gets the message once.
func TestAmbiguousThenRetry_SameKey_Sent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	l, phone := f.inFlight(t, 0, 0, t0.Add(30*time.Second))
	logPath := filepath.Join(t.TempDir(), "provider.log")
	m := provider.Message{IdempotencyKey: l.ID, Channel: "sms", To: phone, Body: "Merhaba Ayşe"}

	mp, srv := startProvider(t, logPath, mockprovider.Script{l.ID: {mockprovider.AcceptThenDrop}})
	first := provider.NewClient(srv.URL, provider.Options{Timeout: 300 * time.Millisecond}).Send(ctx, m)
	if first.Outcome != provider.Ambiguous {
		t.Fatalf("first attempt = %+v, want ambiguous (accepted, answer lost)", first)
	}
	if d, err := f.store.Record(ctx, l, first, t0); err != nil || d.Status != delivery.StatusRetry {
		t.Fatalf("after the lost answer: %+v, err %v", d, err)
	}
	srv.Close()
	if err := mp.Close(); err != nil {
		t.Fatal(err)
	}

	mp, srv = startProvider(t, logPath, nil)
	defer srv.Close()
	l = f.claim(t, l.ID, t0.Add(40*time.Second))
	second := provider.NewClient(srv.URL, provider.Options{Timeout: time.Second}).Send(ctx, m)
	if second.Outcome != provider.Sent || second.Status != http.StatusOK || second.MessageID == "" {
		t.Fatalf("retry after the restart = %+v, want 200 replaying the first acceptance", second)
	}
	if d, err := f.store.Record(ctx, l, second, t0.Add(2*time.Second)); err != nil || d.Status != delivery.StatusSent {
		t.Fatalf("after the retry: %+v, err %v", d, err)
	}
	r := f.row(t, l.ID)
	if r.Status != "sent" || r.Attempts != 2 || r.Ambiguous != 1 || r.ProviderMessageID == nil || *r.ProviderMessageID != second.MessageID {
		t.Fatalf("row = %+v", r)
	}
	if st := mp.Stats(); st.Accepted != 1 || mp.AcceptedFor(phone, "sms", "", "Merhaba Ayşe") != 1 {
		t.Fatalf("the provider holds %d acceptances, want 1", st.Accepted)
	}
}

// A delivery that ends (sent, failed, unknown) records when; one that will be
// retried does not. The overview counts finished deliveries by that time.
func TestRecord_StampsFinishedAt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	finished := func(id string) *time.Time {
		t.Helper()
		var v *time.Time
		if err := f.db.Pool.QueryRow(ctx, `SELECT finished_at FROM deliveries WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	record := func(r provider.Result) string {
		t.Helper()
		l, _ := f.inFlight(t, 0, 0, t0.Add(time.Minute))
		if _, err := f.store.Record(ctx, l, r, t0); err != nil {
			t.Fatal(err)
		}
		return l.ID
	}
	if v := finished(record(provider.Result{Outcome: provider.Sent, Status: 202})); v == nil || !v.Equal(t0) {
		t.Errorf("sent: finished_at = %v, want %v", v, t0)
	}
	if v := finished(record(provider.Result{Outcome: provider.NotSentPermanent, Status: 400})); v == nil || !v.Equal(t0) {
		t.Errorf("failed: finished_at = %v, want %v", v, t0)
	}
	if v := finished(record(provider.Result{Outcome: provider.NotSentRetryable, Status: 429})); v != nil {
		t.Errorf("retry: finished_at = %v, want none", v)
	}
	lastTry, _ := f.inFlight(t, 4, 0, t0.Add(-time.Second))
	if _, err := f.store.ReapExpired(ctx, t0); err != nil {
		t.Fatal(err)
	}
	if v := finished(lastTry.ID); v == nil || !v.Equal(t0) {
		t.Errorf("unknown after the reaper: finished_at = %v, want %v", v, t0)
	}
}
