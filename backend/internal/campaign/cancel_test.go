package campaign_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/testutil"
)

func TestCancelScheduled_RemovesPendingMessagesAndReleasesDuplicateHold(t *testing.T) {
	db := testutil.NewDB(t)
	addUser(t, db, recipient{first: "Ayşe", phone: true, smsOK: true})
	_, first := newDraft(t, db, &content.SMS{Text: "Merhaba {{first_name}}"}, nil)
	if _, err := approveAt(t, db, first, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM deliveries WHERE campaign_id=$1`, first.CampaignID); n != 1 {
		t.Fatalf("scheduled deliveries = %d, want 1", n)
	}
	if err := campaign.NewStore(db.Pool).CancelScheduled(context.Background(), first.CampaignID); err != nil {
		t.Fatal(err)
	}
	if status := scalar[string](t, db, `SELECT status FROM campaigns WHERE id=$1`, first.CampaignID); status != "cancelled" {
		t.Fatalf("status = %s, want cancelled", status)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM deliveries WHERE campaign_id=$1`, first.CampaignID); n != 0 {
		t.Fatalf("cancelled campaign retained %d deliveries", n)
	}
	_, second := newDraft(t, db, &content.SMS{Text: "Merhaba {{first_name}}"}, nil)
	res, err := approve(t, db, second)
	if err != nil || res.Channels["sms"].Selected != 1 {
		t.Fatalf("same content after cancel: result=%+v err=%v", res, err)
	}
}

func TestCancelScheduled_RejectsStartedOrUnscheduled(t *testing.T) {
	for _, tc := range []struct {
		name      string
		scheduled bool
		started   bool
	}{
		{"immediate approval", false, false},
		{"delivery claimed", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.NewDB(t)
			addUser(t, db, recipient{first: "Ayşe", phone: true, smsOK: true})
			_, saved := newDraft(t, db, &content.SMS{Text: "Merhaba"}, nil)
			if tc.scheduled {
				if _, err := approveAt(t, db, saved, time.Now().Add(48*time.Hour)); err != nil {
					t.Fatal(err)
				}
			} else if _, err := approve(t, db, saved); err != nil {
				t.Fatal(err)
			}
			if tc.started {
				if _, err := db.Pool.Exec(context.Background(), `UPDATE deliveries SET status='in_flight' WHERE campaign_id=$1`, saved.CampaignID); err != nil {
					t.Fatal(err)
				}
			}
			if err := campaign.NewStore(db.Pool).CancelScheduled(context.Background(), saved.CampaignID); !errors.Is(err, campaign.ErrNotCancellable) {
				t.Fatalf("cancel err = %v, want ErrNotCancellable", err)
			}
			if status := scalar[string](t, db, `SELECT status FROM campaigns WHERE id=$1`, saved.CampaignID); status != "approved" {
				t.Fatalf("status = %s, want approved", status)
			}
			if n := scalar[int](t, db, `SELECT count(*) FROM deliveries WHERE campaign_id=$1`, saved.CampaignID); n != 1 {
				t.Fatalf("deliveries = %d, want 1", n)
			}
		})
	}
}
