package delivery_test

import (
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/clock"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/delivery"
	"smart-campaign-composer/backend/internal/mockprovider"
	"smart-campaign-composer/backend/internal/provider"
	"smart-campaign-composer/backend/internal/segment"
	"smart-campaign-composer/backend/internal/testutil"
)

// "Aynı kişiye aynı mesaj iki kez gitmez", end to end: campaigns go through
// the real approval, deliveries through the real claim, consent check and
// provider attempt, and the mock provider counts what it accepted.

// world is a database with recipients, a chat to create campaigns in and a
// clock (fake unless a test needs real time).
type world struct {
	db     *testutil.DB
	clk    clock.Clock
	policy delivery.Policy
	camps  *campaign.Store
	store  *delivery.Store
	chatID string
	people int
}

type person struct {
	id           int64
	phone, email string
}

type state struct {
	status              string
	attempts, ambiguous int
}

func newWorld(t *testing.T) *world {
	t.Helper()
	return newWorldWith(t, testutil.NewFakeClock(t0), delivery.DefaultPolicy)
}

// newWorldWith is newWorld on another clock and retry policy.
func newWorldWith(t *testing.T, clk clock.Clock, p delivery.Policy) *world {
	t.Helper()
	db := testutil.NewDB(t)
	w := &world{db: db, clk: clk, policy: p, camps: campaign.NewStore(db.Pool), store: delivery.NewStore(db.Pool, p)}
	if err := db.Pool.QueryRow(context.Background(), `INSERT INTO chats DEFAULT VALUES RETURNING id::text`).Scan(&w.chatID); err != nil {
		t.Fatal(err)
	}
	return w
}

// person adds an İstanbul recipient with consent on both channels.
func (w *world) person(t *testing.T, first string) person {
	t.Helper()
	w.people++
	p := person{phone: fmt.Sprintf("+90532%07d", w.people), email: fmt.Sprintf("kisi%d@example.com", w.people)}
	if err := w.db.Pool.QueryRow(context.Background(), `
		INSERT INTO users (first_name, last_name, email, phone, country_code, admin1_code, city_geoname_id,
		                   birth_date, consent_sms, consent_email)
		VALUES ($1, 'Yılmaz', $2, $3, 'TR', 'TR-34', 745044, '1990-01-01', true, true) RETURNING id`,
		first, p.email, p.phone).Scan(&p.id); err != nil {
		t.Fatal(err)
	}
	return p
}

func (w *world) setSMSConsent(t *testing.T, p person, ok bool) {
	t.Helper()
	if _, err := w.db.Pool.Exec(context.Background(), `UPDATE users SET consent_sms = $2 WHERE id = $1`, p.id, ok); err != nil {
		t.Fatal(err)
	}
}

// draft creates a campaign for everyone in İstanbul with this content.
func (w *world) draft(t *testing.T, sms *content.SMS, email *content.Email) campaign.Saved {
	t.Helper()
	rule := segment.Rule{Match: "all", Conditions: []segment.Condition{{Kind: "location", Operator: "in",
		Places: []segment.PlaceRef{{Level: "city", GeonameID: 745044, Country: "TR", Name: "İstanbul"}}}}}
	s, err := w.camps.Drafts(campaign.Author{ChatID: w.chatID, Source: "user"}).SaveDraft(context.Background(),
		campaign.DraftCmd{Name: "Kampanya", Rule: rule, SMS: sms, Email: email, ChangeSummary: "ilk"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// approve creates such a campaign and approves it now.
func (w *world) approve(t *testing.T, sms *content.SMS, email *content.Email) campaign.ApproveResult {
	t.Helper()
	s := w.draft(t, sms, email)
	res, err := w.camps.Approve(context.Background(), s.CampaignID, s.VersionID, segment.DefaultCatalog(), w.clk.Now())
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	return res
}

func (w *world) deliveryID(t *testing.T, campaignID int64, p person, channel string) string {
	t.Helper()
	var id string
	if err := w.db.Pool.QueryRow(context.Background(), `
		SELECT id::text FROM deliveries WHERE campaign_id = $1 AND user_id = $2 AND channel = $3`,
		campaignID, p.id, channel).Scan(&id); err != nil {
		t.Fatalf("delivery of campaign %d to %d on %s: %v", campaignID, p.id, channel, err)
	}
	return id
}

func (w *world) state(t *testing.T, id string) state {
	t.Helper()
	var s state
	if err := w.db.Pool.QueryRow(context.Background(), `
		SELECT status, attempts, ambiguous_attempts FROM deliveries WHERE id = $1`, id).Scan(&s.status, &s.attempts, &s.ambiguous); err != nil {
		t.Fatal(err)
	}
	return s
}

func (w *world) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := w.db.Pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (w *world) sender(p delivery.Provider) *delivery.Sender {
	return &delivery.Sender{Store: w.store, Provider: p, Clock: w.clk}
}

// mockClient starts a mock provider with faults and returns it with a
// client for it. A short timeout keeps lost answers quick.
func (w *world) mockClient(t *testing.T, faults mockprovider.Faults) (*mockprovider.Provider, *provider.Client) {
	t.Helper()
	mp, err := mockprovider.Open(filepath.Join(t.TempDir(), "provider.log"), faults)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mp.Close() })
	srv := httptest.NewServer(mp)
	t.Cleanup(srv.Close)
	return mp, provider.NewClient(srv.URL, provider.Options{Timeout: 200 * time.Millisecond})
}

// provider is mockClient with a sender using the client.
func (w *world) provider(t *testing.T, faults mockprovider.Faults) (*mockprovider.Provider, *delivery.Sender) {
	t.Helper()
	mp, client := w.mockClient(t, faults)
	return mp, w.sender(client)
}

// step claims what is due now on every channel and attempts each delivery
// once; it returns how many it attempted.
func (w *world) step(t *testing.T, s *delivery.Sender) int {
	t.Helper()
	ctx := context.Background()
	n := 0
	for _, ch := range []string{"sms", "email"} {
		claimed, err := w.store.Claim(ctx, ch, 100, w.clk.Now(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range claimed {
			if _, err := s.Attempt(ctx, c); err != nil {
				t.Fatalf("attempt %s: %v", c.ID, err)
			}
			n++
		}
	}
	return n
}

// drain attempts deliveries until none is waiting, moving the clock past
// every backoff in between.
func (w *world) drain(t *testing.T, s *delivery.Sender) {
	t.Helper()
	fake, ok := w.clk.(*testutil.FakeClock)
	if !ok {
		t.Fatal("drain moves time: it needs a fake clock")
	}
	for range 30 {
		w.step(t, s)
		if w.count(t, `SELECT count(*) FROM deliveries WHERE status IN ('pending', 'retry', 'in_flight')`) == 0 {
			return
		}
		fake.Advance(2 * time.Minute)
	}
	t.Fatal("deliveries did not settle")
}

func repeat(f mockprovider.Fault, n int) []mockprovider.Fault {
	fs := make([]mockprovider.Fault, n)
	for i := range fs {
		fs[i] = f
	}
	return fs
}

// closedAddr is an address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// X's message is accepted but every answer is lost: after five attempts the
// delivery is unknown. A second campaign with the same content leaves X out,
// so the provider is never called for it again and holds one acceptance.
func TestAcceptedButResponseLost_RetriesExhausted_SecondCampaignSameContent_NotSent(t *testing.T) {
	w := newWorld(t)
	x := w.person(t, "Ayşe")
	sms := &content.SMS{Text: "Merhaba {{first_name}}, sepetin seni bekliyor!"}
	const body = "Merhaba Ayşe, sepetin seni bekliyor!"

	first := w.approve(t, sms, nil)
	d1 := w.deliveryID(t, first.CampaignID, x, "sms")
	mp, sender := w.provider(t, mockprovider.Script{d1: repeat(mockprovider.AcceptThenDrop, 5)})
	w.drain(t, sender)
	if s := w.state(t, d1); s != (state{"unknown", 5, 5}) {
		t.Fatalf("first delivery = %+v, want unknown after 5 ambiguous attempts", s)
	}
	if n := mp.AcceptedFor(x.phone, "sms", "", body); n != 1 {
		t.Fatalf("the provider accepted the message %d times, want 1", n)
	}

	second := w.approve(t, sms, nil)
	if c := second.Channels["sms"]; c.Selected != 0 || c.ExcludedDuplicate != 1 {
		t.Fatalf("second approval = %+v, want X left out as a duplicate", c)
	}
	if n := w.count(t, `SELECT count(*) FROM deliveries WHERE campaign_id = $1`, second.CampaignID); n != 0 {
		t.Fatalf("the second campaign has %d deliveries, want 0", n)
	}
	requests := mp.Stats().Requests
	w.drain(t, sender)
	if st := mp.Stats(); st.Requests != requests || mp.AcceptedFor(x.phone, "sms", "", body) != 1 {
		t.Fatalf("after the second campaign: %d requests (was %d), %d acceptances; want no call and 1 acceptance",
			st.Requests, requests, mp.AcceptedFor(x.phone, "sms", "", body))
	}
}

// The same content from another campaign is left out, whether the first
// message was already sent or is still waiting to be sent.
func TestCrossCampaignSameContent_NotSentTwice(t *testing.T) {
	for _, sentFirst := range []bool{true, false} {
		name := "first still queued"
		if sentFirst {
			name = "first already sent"
		}
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			x := w.person(t, "Ayşe")
			sms := &content.SMS{Text: "Merhaba {{first_name}}, yeni sezon başladı!"}
			mp, sender := w.provider(t, nil)
			w.approve(t, sms, nil)
			if sentFirst {
				w.drain(t, sender)
			}
			second := w.approve(t, sms, nil)
			if c := second.Channels["sms"]; c.Selected != 0 || c.ExcludedDuplicate != 1 {
				t.Fatalf("second approval = %+v, want X left out as a duplicate", c)
			}
			w.drain(t, sender)
			if n := mp.AcceptedFor(x.phone, "sms", "", "Merhaba Ayşe, yeni sezon başladı!"); n != 1 {
				t.Fatalf("the provider accepted the message %d times, want 1", n)
			}
		})
	}
}

// One character of difference makes another message.
func TestCrossCampaignDifferentContent_IsSent(t *testing.T) {
	w := newWorld(t)
	x := w.person(t, "Ayşe")
	mp, sender := w.provider(t, nil)
	w.approve(t, &content.SMS{Text: "Yeni sezon başladı!"}, nil)
	second := w.approve(t, &content.SMS{Text: "Yeni sezon başladı."}, nil)
	if c := second.Channels["sms"]; c.Selected != 1 || c.ExcludedDuplicate != 0 {
		t.Fatalf("second approval = %+v, want X included", c)
	}
	w.drain(t, sender)
	for _, body := range []string{"Yeni sezon başladı!", "Yeni sezon başladı."} {
		if n := mp.AcceptedFor(x.phone, "sms", "", body); n != 1 {
			t.Fatalf("%q accepted %d times, want 1", body, n)
		}
	}
}

// SMS and email are different messages even with the same text.
func TestSameContentDifferentChannel_BothSent(t *testing.T) {
	w := newWorld(t)
	x := w.person(t, "Ayşe")
	mp, sender := w.provider(t, nil)
	const text = "Yeni sezon başladı!"
	w.approve(t, &content.SMS{Text: text}, nil)
	second := w.approve(t, nil, &content.Email{Subject: "Yeni sezon", Body: text})
	if c := second.Channels["email"]; c.Selected != 1 || c.ExcludedDuplicate != 0 {
		t.Fatalf("email approval = %+v, want X included", c)
	}
	w.drain(t, sender)
	if sms, email := mp.AcceptedFor(x.phone, "sms", "", text), mp.AcceptedFor(x.email, "email", "Yeni sezon", text); sms != 1 || email != 1 {
		t.Fatalf("accepted: sms %d, email %d; want 1 each", sms, email)
	}
}

// The same text for different people is a different message per person.
func TestSameTemplateDifferentUsers_NoConflict(t *testing.T) {
	w := newWorld(t)
	x, y := w.person(t, "Ayşe"), w.person(t, "Ayşe")
	mp, sender := w.provider(t, nil)
	res := w.approve(t, &content.SMS{Text: "Merhaba {{first_name}}, yeni sezon başladı!"}, nil)
	if c := res.Channels["sms"]; c.Selected != 2 || c.ExcludedDuplicate != 0 {
		t.Fatalf("approval = %+v, want both people", c)
	}
	w.drain(t, sender)
	for _, p := range []person{x, y} {
		if n := mp.AcceptedFor(p.phone, "sms", "", "Merhaba Ayşe, yeni sezon başladı!"); n != 1 {
			t.Fatalf("%s accepted %d times, want 1", p.phone, n)
		}
	}
}

// Only when every attempt definitely missed the person (unreachable
// provider, rate limits, refusal) does the message become free for a later
// campaign.
func TestSameContentAfterDefiniteFailure_Allowed(t *testing.T) {
	for _, unreachable := range []bool{true, false} {
		name := "provider refuses"
		if unreachable {
			name = "provider unreachable"
		}
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			x := w.person(t, "Ayşe")
			sms := &content.SMS{Text: "Merhaba {{first_name}}, kargon yolda!"}
			const body = "Merhaba Ayşe, kargon yolda!"
			first := w.approve(t, sms, nil)
			d1 := w.deliveryID(t, first.CampaignID, x, "sms")
			mp, sender := w.provider(t, mockprovider.Script{d1: {mockprovider.Reject429, mockprovider.Reject429, mockprovider.Reject400}})
			firstSender := sender
			if unreachable {
				firstSender = w.sender(provider.NewClient("http://"+closedAddr(t), provider.Options{Timeout: time.Second}))
			}
			w.drain(t, firstSender)
			if s := w.state(t, d1); s.status != "failed" || s.ambiguous != 0 {
				t.Fatalf("first delivery = %+v, want failed with no ambiguous attempt", s)
			}
			if n := mp.AcceptedFor(x.phone, "sms", "", body); n != 0 {
				t.Fatalf("accepted %d times before the second campaign, want 0", n)
			}

			second := w.approve(t, sms, nil)
			if c := second.Channels["sms"]; c.Selected != 1 || c.ExcludedDuplicate != 0 {
				t.Fatalf("second approval = %+v, want X included again", c)
			}
			w.drain(t, sender)
			if n := mp.AcceptedFor(x.phone, "sms", "", body); n != 1 {
				t.Fatalf("accepted %d times, want 1", n)
			}
		})
	}
}

// Consent withdrawn after approval: the delivery is skipped without calling
// the provider (consent check 2). The message never went out, so a later
// campaign may send it once consent is back.
func TestSameContentAfterConsentSkip_Allowed(t *testing.T) {
	w := newWorld(t)
	x := w.person(t, "Ayşe")
	sms := &content.SMS{Text: "Merhaba {{first_name}}, kampanya seni bekliyor!"}
	const body = "Merhaba Ayşe, kampanya seni bekliyor!"
	mp, sender := w.provider(t, nil)
	first := w.approve(t, sms, nil)
	d1 := w.deliveryID(t, first.CampaignID, x, "sms")

	w.setSMSConsent(t, x, false)
	w.drain(t, sender)
	if s := w.state(t, d1); s != (state{"skipped_no_consent", 0, 0}) {
		t.Fatalf("first delivery = %+v, want skipped without an attempt", s)
	}
	if st := mp.Stats(); st.Requests != 0 {
		t.Fatalf("the provider got %d requests, want none", st.Requests)
	}

	w.setSMSConsent(t, x, true)
	second := w.approve(t, sms, nil)
	if c := second.Channels["sms"]; c.Selected != 1 || c.ExcludedDuplicate != 0 {
		t.Fatalf("second approval = %+v, want X included", c)
	}
	w.drain(t, sender)
	if n := mp.AcceptedFor(x.phone, "sms", "", body); n != 1 {
		t.Fatalf("accepted %d times, want 1", n)
	}
}

// An earlier attempt may already have delivered the message when consent is
// withdrawn. The delivery stops without another call, but it must not free
// the message: it ends unknown, and a later campaign leaves the person out.
func TestConsentWithdrawnAfterAmbiguousAttempt_KeepsMessageHeld(t *testing.T) {
	w := newWorld(t)
	x := w.person(t, "Ayşe")
	sms := &content.SMS{Text: "Merhaba {{first_name}}, son gün bugün!"}
	const body = "Merhaba Ayşe, son gün bugün!"
	first := w.approve(t, sms, nil)
	d1 := w.deliveryID(t, first.CampaignID, x, "sms")
	mp, sender := w.provider(t, mockprovider.Script{d1: {mockprovider.AcceptThenDrop}})
	if n := w.step(t, sender); n != 1 {
		t.Fatalf("attempted %d deliveries, want 1", n)
	}
	if s := w.state(t, d1); s != (state{"retry", 1, 1}) {
		t.Fatalf("after the lost answer: %+v, want retry", s)
	}

	w.setSMSConsent(t, x, false)
	w.drain(t, sender)
	if s := w.state(t, d1); s != (state{"unknown", 1, 1}) {
		t.Fatalf("after consent was withdrawn: %+v, want unknown (the first attempt may have delivered it)", s)
	}
	if st := mp.Stats(); st.Requests != 1 {
		t.Fatalf("the provider got %d requests, want only the first", st.Requests)
	}

	w.setSMSConsent(t, x, true)
	second := w.approve(t, sms, nil)
	if c := second.Channels["sms"]; c.Selected != 0 || c.ExcludedDuplicate != 1 {
		t.Fatalf("second approval = %+v, want X left out as a duplicate", c)
	}
	if n := mp.AcceptedFor(x.phone, "sms", "", body); n != 1 {
		t.Fatalf("accepted %d times, want 1", n)
	}
}

// Two campaigns with the same content approved at the same moment end with
// one delivery between them, whichever wins.
func TestConcurrentApprove_SameContent_OneDelivery(t *testing.T) {
	sms := &content.SMS{Text: "Merhaba {{first_name}}, aynı anda iki onay!"}
	for range 5 {
		w := newWorld(t)
		x := w.person(t, "Ayşe")
		drafts := []campaign.Saved{w.draft(t, sms, nil), w.draft(t, sms, nil)}
		var (
			wg      sync.WaitGroup
			start   = make(chan struct{})
			results [2]campaign.ApproveResult
			errs    [2]error
		)
		for i, s := range drafts {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i], errs[i] = w.camps.Approve(context.Background(), s.CampaignID, s.VersionID, segment.DefaultCatalog(), w.clk.Now())
			}()
		}
		close(start)
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("approve errors: %v, %v", errs[0], errs[1])
		}
		a, b := results[0].Channels["sms"], results[1].Channels["sms"]
		if a.Selected+b.Selected != 1 || a.ExcludedDuplicate+b.ExcludedDuplicate != 1 {
			t.Fatalf("approvals = %+v and %+v, want one selected and one duplicate", a, b)
		}
		if n := w.count(t, `SELECT count(*) FROM deliveries WHERE user_id = $1`, x.id); n != 1 {
			t.Fatalf("%d deliveries to X, want 1", n)
		}
	}
}
