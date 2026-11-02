package analytics_test

import (
	"context"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/analytics"
)

// Deliveries of a campaign whose scheduled time has not come are not in the
// queue; they are counted apart. Once the time has come they are queued
// like any other.
func TestRead_ScheduledApartFromQueued(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	var later, due int64
	for _, c := range []struct {
		id  *int64
		ord int
		at  time.Time
	}{{&later, 3, now.Add(24 * time.Hour)}, {&due, 4, now.Add(-time.Hour)}} {
		if err := f.db.Pool.QueryRow(ctx, `
			INSERT INTO campaigns (chat_id, ordinal, name, status, scheduled_at)
			SELECT chat_id, $2, 'Zamanlı', 'approved', $3 FROM campaigns WHERE id = $1 RETURNING id`,
			f.a, c.ord, c.at).Scan(c.id); err != nil {
			t.Fatal(err)
		}
	}
	f.add(t, later, "sms", "pending", cityIstanbul, time.Time{})
	f.add(t, later, "sms", "pending", cityAnkara, time.Time{})
	f.add(t, later, "email", "pending", cityIstanbul, time.Time{})
	f.add(t, due, "sms", "pending", cityIstanbul, time.Time{})

	o := read(t, f, analytics.Filter{Range: "7d"})
	check(t, "queue", o.KPIs.Queued, analytics.Queue{Total: 4, Pending: 2, InFlight: 1, Retry: 1, SMS: 3, Email: 1, Scheduled: 3})

	only := read(t, f, analytics.Filter{Range: "7d", CampaignID: later})
	check(t, "scheduled campaign queue", only.KPIs.Queued, analytics.Queue{Scheduled: 3})
}
