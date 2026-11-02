package campaign_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/segment"
	"smart-campaign-composer/backend/internal/testutil"
)

func approveAt(t *testing.T, db *testutil.DB, s campaign.Saved, sendAt time.Time) (campaign.ApproveResult, error) {
	t.Helper()
	return campaign.NewStore(db.Pool).ApproveWith(context.Background(), s.CampaignID, s.VersionID, segment.DefaultCatalog(),
		approveNow, campaign.ApproveOptions{SendAt: &sendAt})
}

// A scheduled approval queues every delivery for the chosen moment and keeps
// that moment, in UTC, with the campaign.
func TestApprove_Scheduled_StoresUTCAndQueuesForThatTime(t *testing.T) {
	db := testutil.NewDB(t)
	addUser(t, db, recipient{first: "Ayşe", phone: true, email: true, smsOK: true, emailOK: true})
	_, s := newDraft(t, db, &content.SMS{Text: "Merhaba {{first_name}}"}, &content.Email{Subject: "Fırsat", Body: "Merhaba"})
	sendAt := time.Date(2026, 10, 3, 9, 30, 0, 0, time.FixedZone("TRT", 3*60*60)) // 06:30 UTC

	res, err := approveAt(t, db, s, sendAt)
	if err != nil {
		t.Fatal(err)
	}
	if res.ScheduledAt == nil || !res.ScheduledAt.Equal(sendAt) || res.ScheduledAt.Location() != time.UTC {
		t.Fatalf("scheduled_at = %v, want %v in UTC", res.ScheduledAt, sendAt.UTC())
	}
	if got := scalar[string](t, db, `SELECT to_char(scheduled_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI') FROM campaigns WHERE id=$1`,
		s.CampaignID); got != "2026-10-03 06:30" {
		t.Fatalf("stored scheduled_at in UTC = %s", got)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM deliveries WHERE campaign_id=$1 AND status='pending' AND next_attempt_at = $2`,
		s.CampaignID, sendAt); n != 2 {
		t.Fatalf("deliveries due at the scheduled time = %d, want 2", n)
	}
	if approved := scalar[time.Time](t, db, `SELECT approved_at FROM campaigns WHERE id=$1`, s.CampaignID); !approved.Equal(approveNow) {
		t.Fatalf("approved_at = %v, want the approval time %v", approved, approveNow)
	}

	again, err := approve(t, db, s)
	if err != nil {
		t.Fatal(err)
	}
	if !again.AlreadyApproved || again.ScheduledAt == nil || !again.ScheduledAt.Equal(sendAt) {
		t.Fatalf("approve again = %+v, want the stored schedule %v", again, sendAt)
	}
}

// Sending now stays the default.
func TestApprove_WithoutSchedule_SendsNow(t *testing.T) {
	db := testutil.NewDB(t)
	addUser(t, db, recipient{first: "Ayşe", phone: true, smsOK: true})
	_, s := newDraft(t, db, &content.SMS{Text: "Merhaba {{first_name}}"}, nil)

	res, err := approve(t, db, s)
	if err != nil {
		t.Fatal(err)
	}
	if res.ScheduledAt != nil {
		t.Fatalf("scheduled_at = %v, want none", res.ScheduledAt)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM campaigns WHERE id=$1 AND scheduled_at IS NULL`, s.CampaignID); n != 1 {
		t.Fatal("an immediate campaign has a stored schedule")
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM deliveries WHERE campaign_id=$1 AND next_attempt_at = $2`, s.CampaignID, approveNow); n != 1 {
		t.Fatalf("deliveries due at approval = %d, want 1", n)
	}
}

// A schedule must lie in the future and at most a year ahead; a refused one
// queues nothing and leaves the draft as it was.
func TestApprove_ScheduleMustLieAhead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sendAt time.Time
		code   string
	}{
		{"past", approveNow.Add(-time.Minute), "schedule_in_past"},
		{"now", approveNow, "schedule_in_past"},
		{"too far", approveNow.Add(campaign.MaxScheduleAhead + time.Minute), "schedule_too_far"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.NewDB(t)
			addUser(t, db, recipient{first: "Ayşe", phone: true, smsOK: true})
			_, s := newDraft(t, db, &content.SMS{Text: "Merhaba"}, nil)

			_, err := approveAt(t, db, s, tc.sendAt)
			var ve *campaign.ValidationError
			if !errors.As(err, &ve) || len(ve.Issues) != 1 || ve.Issues[0].Path != "scheduled_at" || ve.Issues[0].Code != tc.code {
				t.Fatalf("err = %v, want a %s issue on scheduled_at", err, tc.code)
			}
			if n := scalar[int](t, db, `SELECT count(*) FROM deliveries WHERE campaign_id=$1`, s.CampaignID); n != 0 {
				t.Fatalf("deliveries = %d, want none", n)
			}
			if st := scalar[string](t, db, `SELECT status FROM campaigns WHERE id=$1`, s.CampaignID); st != "draft" {
				t.Fatalf("status = %s, want draft", st)
			}
		})
	}
	// A schedule exactly a year ahead is accepted.
	db := testutil.NewDB(t)
	addUser(t, db, recipient{first: "Ayşe", phone: true, smsOK: true})
	_, s := newDraft(t, db, &content.SMS{Text: "Merhaba"}, nil)
	if _, err := approveAt(t, db, s, approveNow.Add(campaign.MaxScheduleAhead)); err != nil {
		t.Fatalf("a year ahead: %v", err)
	}
}
