package delivery_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/abtest"
	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/delivery"
	"smart-campaign-composer/backend/internal/language"
	"smart-campaign-composer/backend/internal/provider"
	"smart-campaign-composer/backend/internal/segment"
	"smart-campaign-composer/backend/internal/testutil"
)

// lostFirst loses the answer to the first call of every key (the provider
// may have accepted it) and accepts the rest; it keeps what each call sent.
type lostFirst struct {
	mu   sync.Mutex
	sent map[string][]string // key → bodies, call by call
}

func (p *lostFirst) Send(_ context.Context, m provider.Message) provider.Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent[m.IdempotencyKey] = append(p.sent[m.IdempotencyKey], m.Body)
	if len(p.sent[m.IdempotencyKey]) == 1 {
		return provider.Result{Outcome: provider.Ambiguous, Status: 503}
	}
	return provider.Result{Outcome: provider.Sent, Status: 202, MessageID: m.IdempotencyKey}
}

// The arm is written with the text at approval: neither a retry nor a
// worker restart (a new store and sender) changes what a recipient gets.
func TestABTest_RetryAndRestartKeepTheArm(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	var people []person
	for i := range 8 {
		people = append(people, w.person(t, fmt.Sprintf("Kişi%d", i)))
	}
	rule := segment.Rule{Match: "all", Conditions: []segment.Condition{{Kind: "location", Operator: "in",
		Places: []segment.PlaceRef{{Level: "city", GeonameID: 745044, Country: "TR", Name: "İstanbul"}}}}}
	s, err := w.camps.Drafts(campaign.Author{ChatID: w.chatID, Source: "user"}).SaveDraft(ctx, campaign.DraftCmd{
		Name: "A/B", Rule: rule, SMS: &content.SMS{Text: "A metni"}, ChangeSummary: "ilk",
		Localization: &language.Settings{Mode: language.ModeSingle, Language: "tr"},
		ABTest:       &abtest.Test{Variants: []language.Variant{{Language: "tr", SMS: &content.SMS{Text: "B metni"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.camps.Approve(ctx, s.CampaignID, s.VersionID, segment.DefaultCatalog(), w.clk.Now()); err != nil {
		t.Fatal(err)
	}

	p := &lostFirst{sent: map[string][]string{}}
	if n := w.step(t, w.sender(p)); n != 8 {
		t.Fatalf("first round attempted %d deliveries, want 8", n)
	}
	// The worker restarts: everything it held is rebuilt from the database.
	w.store = delivery.NewStore(w.db.Pool, w.policy)
	w.clk.(*testutil.FakeClock).Advance(2 * time.Minute)
	if n := w.step(t, w.sender(p)); n != 8 {
		t.Fatalf("retry round attempted %d deliveries, want 8", n)
	}

	for _, who := range people {
		id := w.deliveryID(t, s.CampaignID, who, "sms")
		arm := abtest.Arm(s.CampaignID, who.id)
		var stored string
		if err := w.db.Pool.QueryRow(ctx, `SELECT ab_variant FROM deliveries WHERE id = $1`, id).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		want := arm + " metni"
		if got := p.sent[id]; stored != arm || len(got) != 2 || got[0] != want || got[1] != want {
			t.Fatalf("person %d (arm %s, stored %s) was sent %q, want %q twice", who.id, arm, stored, got, want)
		}
		if st := w.state(t, id); st.status != "sent" {
			t.Fatalf("person %d: %+v, want sent", who.id, st)
		}
	}
}
