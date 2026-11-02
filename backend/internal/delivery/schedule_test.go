package delivery_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/provider"
	"smart-campaign-composer/backend/internal/segment"
	"smart-campaign-composer/backend/internal/testutil"
)

// messageLog accepts every message and keeps it.
type messageLog struct {
	mu   sync.Mutex
	msgs []provider.Message
}

func (l *messageLog) Send(_ context.Context, m provider.Message) provider.Result {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, m)
	return provider.Result{Outcome: provider.Sent, Status: 202, MessageID: m.IdempotencyKey}
}

// A scheduled campaign is not claimed before its time. When the time comes,
// the worker sends what was approved, not what the recipient's data says
// by then, and checks consent again right before each call.
func TestScheduled_ClaimedOnlyWhenDue_SendsApprovedContent_RechecksConsent(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	ayse := w.person(t, "Ayşe")
	can := w.person(t, "Can")
	s := w.draft(t, &content.SMS{Text: "Merhaba {{first_name}}"}, nil)
	sendAt := w.clk.Now().Add(2 * time.Hour)
	if _, err := w.camps.ApproveWith(ctx, s.CampaignID, s.VersionID, segment.DefaultCatalog(), w.clk.Now(),
		campaign.ApproveOptions{SendAt: &sendAt}); err != nil {
		t.Fatal(err)
	}
	// After approval Ayşe's name changes and Can withdraws SMS consent.
	if _, err := w.db.Pool.Exec(ctx, `UPDATE users SET first_name = 'Ayşegül' WHERE id = $1`, ayse.id); err != nil {
		t.Fatal(err)
	}
	w.setSMSConsent(t, can, false)

	log := &messageLog{}
	snd := w.sender(log)
	fake := w.clk.(*testutil.FakeClock)
	fake.Advance(2*time.Hour - time.Second)
	if n := w.step(t, snd); n != 0 {
		t.Fatalf("attempted %d deliveries a second before the scheduled time", n)
	}
	fake.Advance(time.Second)
	if n := w.step(t, snd); n != 2 {
		t.Fatalf("attempted %d deliveries at the scheduled time, want 2", n)
	}
	if len(log.msgs) != 1 || log.msgs[0].To != ayse.phone || log.msgs[0].Body != "Merhaba Ayşe" {
		t.Fatalf("provider calls = %+v, want one call to Ayşe with the approved text", log.msgs)
	}
	if st := w.state(t, w.deliveryID(t, s.CampaignID, ayse, "sms")); st.status != "sent" {
		t.Fatalf("Ayşe's delivery = %+v, want sent", st)
	}
	if st := w.state(t, w.deliveryID(t, s.CampaignID, can, "sms")); st.status != "skipped_no_consent" || st.attempts != 0 {
		t.Fatalf("Can's delivery = %+v, want skipped_no_consent without an attempt", st)
	}
}
