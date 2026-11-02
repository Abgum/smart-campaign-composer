package tracking_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/testutil"
	"smart-campaign-composer/backend/internal/tracking"
)

type fixture struct {
	db     *testutil.DB
	chat   string
	people int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{db: testutil.NewDB(t)}
	if err := f.db.Pool.QueryRow(context.Background(), `INSERT INTO chats DEFAULT VALUES RETURNING id::text`).Scan(&f.chat); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.db.Pool.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// campaign inserts a campaign with an approval snapshot (nil for a draft).
func (f *fixture) campaign(t *testing.T, ordinal int, name, status string, approval *string) int64 {
	t.Helper()
	var id int64
	if err := f.db.Pool.QueryRow(context.Background(), `
		INSERT INTO campaigns (chat_id, ordinal, name, status, approval) VALUES ($1, $2, $3, $4, $5::jsonb) RETURNING id`,
		f.chat, ordinal, name, status, approval).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// deliver inserts a delivery to a new person.
func (f *fixture) deliver(t *testing.T, campaignID int64, channel, status string, due time.Time) {
	t.Helper()
	ctx := context.Background()
	f.people++
	var user int64
	if err := f.db.Pool.QueryRow(ctx, `
		INSERT INTO users (first_name, last_name, phone, email, country_code, birth_date)
		VALUES ('Ayşe', 'Yılmaz', $1, $2, 'TR', '1990-01-01') RETURNING id`,
		fmt.Sprintf("+90533%07d", f.people), fmt.Sprintf("p%d@example.com", f.people)).Scan(&user); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO deliveries (campaign_id, user_id, channel, body, content_hash, status, next_attempt_at)
		VALUES ($1, $2, $3, 'Merhaba', 'hash', $4, $5)`, campaignID, user, channel, status, due)
}

func ptr(s string) *string { return &s }

// Two approved campaigns with deliveries in every state and people left out
// at approval, a draft (not listed), a paused channel and two workers.
func TestDashboardAggregate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := time.Now()
	a := f.campaign(t, 1, "A", "sending", ptr(`{
		"sms":   {"selected": 7, "excluded_consent": 2, "excluded_duplicate": 1},
		"email": {"selected": 1, "excluded_consent": 3, "excluded_duplicate": 0}}`))
	b := f.campaign(t, 2, "B", "completed", ptr(`{"sms": {"selected": 1, "excluded_consent": 0, "excluded_duplicate": 4}}`))
	f.campaign(t, 3, "Taslak", "draft", nil)

	past, later := now.Add(-time.Minute), now.Add(time.Hour)
	for _, d := range []struct {
		campaign        int64
		channel, status string
		due             time.Time
	}{
		{a, "sms", "pending", past}, {a, "sms", "pending", past}, {a, "sms", "retry", later},
		{a, "sms", "in_flight", past}, {a, "sms", "sent", past}, {a, "sms", "failed", past},
		{a, "sms", "unknown", past}, {a, "email", "skipped_no_consent", past}, {b, "sms", "sent", past},
	} {
		f.deliver(t, d.campaign, d.channel, d.status, d.due)
	}
	f.exec(t, `INSERT INTO channel_limits (channel, calls, window_ms, paused_until)
		VALUES ('sms', 100, 1050, now() + interval '10 seconds'), ('email', 100, 1050, NULL)`)
	f.exec(t, `INSERT INTO worker_heartbeats (name, beat_at)
		VALUES ('canli', now()), ('eski', now() - interval '1 minute')`)

	d, err := tracking.Read(ctx, f.db.Pool, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Campaigns) != 2 || d.Campaigns[0].ID != b || d.Campaigns[1].ID != a {
		t.Fatalf("campaigns = %+v; want B then A, no draft", d.Campaigns)
	}
	ca := d.Campaigns[1]
	if got, want := ca.Channels["sms"], (tracking.Counts{ExcludedConsent: 2, ExcludedDuplicate: 1, Pending: 2, InFlight: 1,
		Retry: 1, Sent: 1, Failed: 1, Unknown: 1}); got != want {
		t.Errorf("A sms = %+v, want %+v", got, want)
	}
	if got, want := ca.Channels["email"], (tracking.Counts{ExcludedConsent: 3, SkippedNoConsent: 1}); got != want {
		t.Errorf("A email = %+v, want %+v", got, want)
	}
	if got, want := d.Campaigns[0].Channels["sms"], (tracking.Counts{ExcludedDuplicate: 4, Sent: 1}); got != want {
		t.Errorf("B sms = %+v, want %+v", got, want)
	}
	if got, want := d.Counts["sms"], (tracking.Counts{ExcludedConsent: 2, ExcludedDuplicate: 5, Pending: 2, InFlight: 1,
		Retry: 1, Sent: 2, Failed: 1, Unknown: 1}); got != want {
		t.Errorf("total sms = %+v, want %+v", got, want)
	}
	if got, want := d.Counts["email"], (tracking.Counts{ExcludedConsent: 3, SkippedNoConsent: 1}); got != want {
		t.Errorf("total email = %+v, want %+v", got, want)
	}
	if sms := d.Channels["sms"]; sms.Due != 2 || sms.PausedUntil == nil || sms.Calls != 100 || sms.WindowMS != 1050 {
		t.Errorf("sms channel = %+v; want 2 due (the retry is due later), paused, 100 per 1050ms", sms)
	}
	if email := d.Channels["email"]; email.Due != 0 || email.PausedUntil != nil {
		t.Errorf("email channel = %+v; want nothing due, not paused", email)
	}
	if len(d.Workers) != 2 || d.Workers[0].Name != "canli" || !d.Workers[0].Alive || d.Workers[1].Name != "eski" || d.Workers[1].Alive {
		t.Errorf("workers = %+v; want canli alive, eski not", d.Workers)
	}
}

func TestReadCampaign(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.campaign(t, 1, "A", "approved", ptr(`{"sms": {"selected": 1, "excluded_consent": 1, "excluded_duplicate": 0}}`))
	draft := f.campaign(t, 2, "Taslak", "draft", nil)
	f.deliver(t, a, "sms", "pending", time.Now())

	c, err := tracking.ReadCampaign(ctx, f.db.Pool, a)
	if err != nil || c.Status != "approved" || c.Channels["sms"] != (tracking.Counts{ExcludedConsent: 1, Pending: 1}) {
		t.Fatalf("A = %+v, err %v", c, err)
	}
	if c, err := tracking.ReadCampaign(ctx, f.db.Pool, draft); err != nil || c.Status != "draft" || len(c.Channels) != 0 {
		t.Fatalf("draft = %+v, err %v; want its status and no counts", c, err)
	}
	if _, err := tracking.ReadCampaign(ctx, f.db.Pool, 999999); !errors.Is(err, tracking.ErrNotFound) {
		t.Fatalf("unknown campaign: err %v, want ErrNotFound", err)
	}
}

// An A/B campaign's numbers split by arm as well: who each arm left out at
// approval and how each arm's deliveries stand.
func TestReadCampaign_ABVariants(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.campaign(t, 1, "A/B", "sending", ptr(`{"sms": {"selected": 3, "excluded_consent": 0, "excluded_duplicate": 2}}`))
	f.exec(t, `UPDATE campaigns SET approval_variants = '{"A": {"sms": {"selected": 1, "excluded_consent": 0, "excluded_duplicate": 2}},
		"B": {"sms": {"selected": 2, "excluded_consent": 0, "excluded_duplicate": 0}}}' WHERE id = $1`, id)
	f.deliver(t, id, "sms", "sent", time.Now())
	f.deliver(t, id, "sms", "sent", time.Now())
	f.deliver(t, id, "sms", "unknown", time.Now())
	f.exec(t, `UPDATE deliveries SET ab_variant = CASE WHEN status = 'unknown' THEN 'A' ELSE 'B' END WHERE campaign_id = $1`, id)

	c, err := tracking.ReadCampaign(ctx, f.db.Pool, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Variants["A"]["sms"]; got != (tracking.Counts{ExcludedDuplicate: 2, Unknown: 1}) {
		t.Errorf("A = %+v", got)
	}
	if got := c.Variants["B"]["sms"]; got != (tracking.Counts{Sent: 2}) {
		t.Errorf("B = %+v", got)
	}
	if got := c.Channels["sms"]; got != (tracking.Counts{ExcludedDuplicate: 2, Sent: 2, Unknown: 1}) {
		t.Errorf("sms = %+v; the per-channel numbers stay whole", got)
	}

	plain := f.campaign(t, 2, "Düz", "sending", ptr(`{"sms": {"selected": 1, "excluded_consent": 0, "excluded_duplicate": 0}}`))
	f.deliver(t, plain, "sms", "sent", time.Now())
	if c, err := tracking.ReadCampaign(ctx, f.db.Pool, plain); err != nil || c.Variants != nil {
		t.Fatalf("a campaign without an A/B test: variants %v, err %v; want none", c.Variants, err)
	}
}

var (
	approvedFirst  = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	approvedSecond = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
)

// The campaign list spans every chat: drafts, sending and completed ones,
// each with its breakdown by channel and status and the people left out at
// approval. It narrows by status and by name.
func TestListCampaigns(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	draft := f.campaign(t, 1, "Taslak kampanya", "draft", nil)
	sending := f.campaign(t, 2, "İstanbul sepet", "sending",
		ptr(`{"sms": {"selected": 2, "excluded_consent": 1, "excluded_duplicate": 3}}`))
	done := f.campaign(t, 3, "Ankara hoş geldin", "completed", ptr(`{"email": {"selected": 1, "excluded_consent": 0, "excluded_duplicate": 0}}`))
	f.exec(t, `UPDATE campaigns SET approved_at = $2 WHERE id = $1`, sending, approvedSecond)
	f.exec(t, `UPDATE campaigns SET approved_at = $2 WHERE id = $1`, done, approvedFirst)
	f.deliver(t, sending, "sms", "sent", time.Now())
	f.deliver(t, sending, "sms", "pending", time.Now())
	f.deliver(t, done, "email", "sent", time.Now())

	ids := func(filter tracking.ListFilter) []int64 {
		t.Helper()
		list, err := tracking.ListCampaigns(ctx, f.db.Pool, filter)
		if err != nil {
			t.Fatal(err)
		}
		out := []int64{}
		for _, c := range list {
			out = append(out, c.ID)
		}
		return out
	}
	// Newest first: a draft by its last change, others by their approval.
	if got := ids(tracking.ListFilter{}); fmt.Sprint(got) != fmt.Sprint([]int64{draft, sending, done}) {
		t.Fatalf("all = %v, want %v", got, []int64{draft, sending, done})
	}
	for status, want := range map[string][]int64{"draft": {draft}, "active": {sending}, "completed": {done}} {
		if got := ids(tracking.ListFilter{Status: status}); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("status %s = %v, want %v", status, got, want)
		}
	}
	if got := ids(tracking.ListFilter{Query: "ankara"}); fmt.Sprint(got) != fmt.Sprint([]int64{done}) {
		t.Errorf("name search = %v, want %v", got, []int64{done})
	}
	if got := ids(tracking.ListFilter{ID: sending}); fmt.Sprint(got) != fmt.Sprint([]int64{sending}) {
		t.Errorf("by id = %v, want %v", got, []int64{sending})
	}

	list, err := tracking.ListCampaigns(ctx, f.db.Pool, tracking.ListFilter{Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	c := list[0]
	if c.Ordinal != 2 || c.ApprovedAt == nil || !c.ApprovedAt.Equal(approvedSecond) ||
		c.Channels["sms"] != (tracking.Counts{ExcludedConsent: 1, ExcludedDuplicate: 3, Pending: 1, Sent: 1}) {
		t.Fatalf("sending campaign = %+v", c)
	}
}

// The dashboard's campaigns carry what their cards show: the approval time,
// the last send, the approved version and the model that wrote it. The
// dashboard carries a version token that changes whenever a delivery does,
// so pages know when to refetch their charts.
func TestDashboard_CardsAndVersion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.campaign(t, 1, "İstanbul sepet", "sending", ptr(`{"sms": {"selected": 1, "excluded_consent": 0, "excluded_duplicate": 0}}`))
	var version int64
	if err := f.db.Pool.QueryRow(ctx, `
		INSERT INTO campaign_versions (campaign_id, version, source, llm_provider, name, segment_rule, audience)
		VALUES ($1, 3, 'llm', 'mock', 'İstanbul sepet', '{}', '{}') RETURNING id`, id).Scan(&version); err != nil {
		t.Fatal(err)
	}
	lastSent := approvedSecond.Add(time.Minute)
	f.exec(t, `UPDATE campaigns SET approved_version_id = $2, approved_at = $3 WHERE id = $1`, id, version, approvedSecond)
	f.deliver(t, id, "sms", "sent", time.Now())
	f.exec(t, `UPDATE deliveries SET sent_at = $2 WHERE campaign_id = $1`, id, lastSent)

	d, err := tracking.Read(ctx, f.db.Pool, time.Now())
	if err != nil || len(d.Campaigns) != 1 {
		t.Fatalf("read = %+v, %v", d, err)
	}
	c := d.Campaigns[0]
	if c.Ordinal != 1 || c.ApprovedAt == nil || !c.ApprovedAt.Equal(approvedSecond) || c.LastSentAt == nil ||
		!c.LastSentAt.Equal(lastSent) || c.Version == nil || *c.Version != 3 || c.LLMProvider == nil || *c.LLMProvider != "mock" {
		t.Fatalf("card = %+v", c)
	}
	again, err := tracking.Read(ctx, f.db.Pool, time.Now())
	if err != nil || d.Version == "" || again.Version != d.Version {
		t.Fatalf("version %q then %q (%v); want a stable, non-empty token", d.Version, again.Version, err)
	}
	f.exec(t, `UPDATE deliveries SET status = 'failed' WHERE campaign_id = $1`, id)
	if changed, err := tracking.Read(ctx, f.db.Pool, time.Now()); err != nil || changed.Version == d.Version {
		t.Fatalf("version stayed %q after a delivery changed (%v)", changed.Version, err)
	}
}
