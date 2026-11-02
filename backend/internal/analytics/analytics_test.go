package analytics_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"reflect"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/analytics"
	"smart-campaign-composer/backend/internal/geo"
	"smart-campaign-composer/backend/internal/testutil"
)

var (
	istanbul, _ = time.LoadLocation("Europe/Istanbul")
	now         = time.Date(2026, 10, 27, 15, 0, 0, 0, istanbul) // 12:00 UTC
)

const (
	cityIstanbul = int64(745044)
	cityAnkara   = int64(323786)
	cityFar      = int64(999000111) // a GeoNames id the catalog does not have
)

type fixture struct {
	db   *testutil.DB
	a, b int64 // campaign A is sending, B is completed
	n    int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{db: testutil.NewDB(t)}
	ctx := context.Background()
	var chat string
	if err := f.db.Pool.QueryRow(ctx, `INSERT INTO chats DEFAULT VALUES RETURNING id::text`).Scan(&chat); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id     *int64
		status string
		ord    int
	}{{&f.a, "sending", 1}, {&f.b, "completed", 2}} {
		if err := f.db.Pool.QueryRow(ctx, `INSERT INTO campaigns (chat_id, ordinal, name, status) VALUES ($1, $2, 'K', $3) RETURNING id`,
			chat, c.ord, c.status).Scan(c.id); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// add inserts one delivery to a new person in city (0: no city). at is when
// it was sent (status sent) or ended (failed, unknown); queued rows ignore it.
func (f *fixture) add(t *testing.T, campaign int64, channel, status string, city int64, at time.Time) {
	t.Helper()
	ctx := context.Background()
	f.n++
	var cityID *int64
	if city != 0 {
		cityID = &city
	}
	var user int64
	if err := f.db.Pool.QueryRow(ctx, `
		INSERT INTO users (first_name, last_name, email, country_code, city_geoname_id, birth_date)
		VALUES ('Ayşe', 'Yılmaz', $1, 'TR', $2, '1990-01-01') RETURNING id`, randHex()+"@example.com", cityID).Scan(&user); err != nil {
		t.Fatal(err)
	}
	var sentAt, finishedAt *time.Time
	switch status {
	case "sent":
		sentAt, finishedAt = &at, &at
	case "failed", "unknown":
		finishedAt = &at
	}
	if _, err := f.db.Pool.Exec(ctx, `
		INSERT INTO deliveries (campaign_id, user_id, channel, body, content_hash, status, city_geoname_id, sent_at, finished_at)
		VALUES ($1, $2, $3, 'x', $4, $5, $6, $7, $8)`, campaign, user, channel, randHex(), status, cityID, sentAt, finishedAt); err != nil {
		t.Fatal(err)
	}
}

func randHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func local(day, clock string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", day+" "+clock, istanbul)
	if err != nil {
		panic(err)
	}
	return t
}

// seeded builds the shared data set (times are Istanbul time):
//
//	A sms   sent     İstanbul  10-27 10:00
//	A email sent     İstanbul  10-27 00:30  (still 10-26 in UTC)
//	A sms   sent     Ankara    10-25 12:00
//	A sms   sent     no city   10-27 11:00
//	A sms   sent     İstanbul  10-10 12:00  (outside 7 days)
//	A sms   failed   İstanbul  10-27 09:00
//	A sms   unknown  Ankara    10-26 09:00
//	A sms pending, A email in_flight, A sms retry
//	B email sent     far city  10-24 08:00
//	B email sent     Ankara    10-26 23:59
func seeded(t *testing.T) *fixture {
	f := newFixture(t)
	f.add(t, f.a, "sms", "sent", cityIstanbul, local("2026-10-27", "10:00"))
	f.add(t, f.a, "email", "sent", cityIstanbul, local("2026-10-27", "00:30"))
	f.add(t, f.a, "sms", "sent", cityAnkara, local("2026-10-25", "12:00"))
	f.add(t, f.a, "sms", "sent", 0, local("2026-10-27", "11:00"))
	f.add(t, f.a, "sms", "sent", cityIstanbul, local("2026-10-10", "12:00"))
	f.add(t, f.a, "sms", "failed", cityIstanbul, local("2026-10-27", "09:00"))
	f.add(t, f.a, "sms", "unknown", cityAnkara, local("2026-10-26", "09:00"))
	f.add(t, f.a, "sms", "pending", cityIstanbul, time.Time{})
	f.add(t, f.a, "email", "in_flight", cityIstanbul, time.Time{})
	f.add(t, f.a, "sms", "retry", cityIstanbul, time.Time{})
	f.add(t, f.b, "email", "sent", cityFar, local("2026-10-24", "08:00"))
	f.add(t, f.b, "email", "sent", cityAnkara, local("2026-10-26", "23:59"))
	return f
}

func read(t *testing.T, f *fixture, filter analytics.Filter) analytics.Overview {
	t.Helper()
	if filter.Cities == 0 {
		filter.Cities = 10
	}
	o, err := analytics.Read(context.Background(), f.db.Pool, geo.Default(), istanbul, now, filter)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func check(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s =\n  %+v\nwant\n  %+v", what, got, want)
	}
}

func TestRead_LastSevenDays(t *testing.T) {
	o := read(t, seeded(t), analytics.Filter{Range: "7d"})

	check(t, "window", o.Window, analytics.Window{Range: "7d", From: "2026-10-21", To: "2026-10-27", TZ: "Europe/Istanbul", Bucket: "day"})
	check(t, "kpis", o.KPIs, analytics.KPIs{
		Sent:    analytics.Counts{Total: 6, SMS: 3, Email: 3},
		Queued:  analytics.Queue{Total: 3, Pending: 1, InFlight: 1, Retry: 1, SMS: 2, Email: 1},
		Failed:  analytics.Counts{Total: 1, SMS: 1},
		Unknown: analytics.Counts{Total: 1, SMS: 1},
	})
	check(t, "series", o.Series, []analytics.Point{
		{At: "2026-10-21"}, {At: "2026-10-22"}, {At: "2026-10-23"},
		{At: "2026-10-24", Email: 1},
		{At: "2026-10-25", SMS: 1},
		{At: "2026-10-26", Email: 1},
		{At: "2026-10-27", SMS: 2, Email: 1}, // 00:30 Istanbul is 10-27 here, though 10-26 in UTC
	})
	check(t, "cities", o.Cities, analytics.Cities{
		Top: []analytics.City{
			{GeonameID: cityAnkara, Name: "Ankara", Country: "TR", Admin1: "TR-06", SMS: 1, Email: 1, Total: 2},
			{GeonameID: cityIstanbul, Name: "İstanbul", Country: "TR", Admin1: "TR-34", SMS: 1, Email: 1, Total: 2},
			{GeonameID: cityFar, Email: 1, Total: 1},
		},
		Unknown: analytics.Counts{Total: 1, SMS: 1},
	})
}

// Today is counted by hour.
func TestRead_TodayByHour(t *testing.T) {
	o := read(t, seeded(t), analytics.Filter{Range: "today"})

	check(t, "window", o.Window, analytics.Window{Range: "today", From: "2026-10-27", To: "2026-10-27", TZ: "Europe/Istanbul", Bucket: "hour"})
	if len(o.Series) != 24 || o.Series[0].At != "2026-10-27T00:00" || o.Series[23].At != "2026-10-27T23:00" {
		t.Fatalf("series = %+v; want the 24 hours of today", o.Series)
	}
	check(t, "00:00", o.Series[0], analytics.Point{At: "2026-10-27T00:00", Email: 1})
	check(t, "10:00", o.Series[10], analytics.Point{At: "2026-10-27T10:00", SMS: 1})
	check(t, "11:00", o.Series[11], analytics.Point{At: "2026-10-27T11:00", SMS: 1})
	check(t, "sent", o.KPIs.Sent, analytics.Counts{Total: 3, SMS: 2, Email: 1})
	check(t, "failed", o.KPIs.Failed, analytics.Counts{Total: 1, SMS: 1})
	check(t, "unknown", o.KPIs.Unknown, analytics.Counts{})
}

// One past day is counted by hour, over that day's local hours only.
func TestRead_OneDayByHour(t *testing.T) {
	o := read(t, seeded(t), analytics.Filter{Range: "day", Day: "2026-10-26"})

	check(t, "window", o.Window, analytics.Window{Range: "day", From: "2026-10-26", To: "2026-10-26", TZ: "Europe/Istanbul", Bucket: "hour"})
	if len(o.Series) != 24 || o.Series[0].At != "2026-10-26T00:00" || o.Series[23].At != "2026-10-26T23:00" {
		t.Fatalf("series = %+v; want the 24 hours of 2026-10-26", o.Series)
	}
	check(t, "00:00", o.Series[0], analytics.Point{At: "2026-10-26T00:00"}) // 10-27 00:30 is the next day
	check(t, "23:00", o.Series[23], analytics.Point{At: "2026-10-26T23:00", Email: 1})
	check(t, "sent", o.KPIs.Sent, analytics.Counts{Total: 1, Email: 1})
	check(t, "failed", o.KPIs.Failed, analytics.Counts{})
	check(t, "unknown", o.KPIs.Unknown, analytics.Counts{Total: 1, SMS: 1})
	if len(o.Cities.Top) != 1 || o.Cities.Top[0].Name != "Ankara" {
		t.Fatalf("cities = %+v; want Ankara only", o.Cities.Top)
	}
}

// All time starts at the first send.
func TestRead_AllFromFirstSend(t *testing.T) {
	o := read(t, seeded(t), analytics.Filter{Range: "all"})
	if o.Window.From != "2026-10-10" || o.Window.To != "2026-10-27" || len(o.Series) != 18 {
		t.Fatalf("window %+v with %d points; want 2026-10-10 … 2026-10-27, 18 days", o.Window, len(o.Series))
	}
	check(t, "sent", o.KPIs.Sent, analytics.Counts{Total: 7, SMS: 4, Email: 3})
}

func TestRead_ChannelCampaignAndCityLimit(t *testing.T) {
	f := seeded(t)

	sms := read(t, f, analytics.Filter{Range: "7d", Channel: "sms"})
	check(t, "sms sent", sms.KPIs.Sent, analytics.Counts{Total: 3, SMS: 3})
	check(t, "sms queue", sms.KPIs.Queued, analytics.Queue{Total: 2, Pending: 1, Retry: 1, SMS: 2})
	check(t, "sms cities", len(sms.Cities.Top), 2)

	b := read(t, f, analytics.Filter{Range: "7d", CampaignID: f.b})
	check(t, "campaign B sent", b.KPIs.Sent, analytics.Counts{Total: 2, Email: 2})
	check(t, "campaign B queue", b.KPIs.Queued, analytics.Queue{})
	check(t, "campaign B failed", b.KPIs.Failed, analytics.Counts{})

	two := read(t, f, analytics.Filter{Range: "7d", Cities: 2})
	if len(two.Cities.Top) != 2 || two.Cities.Top[0].Name != "Ankara" || two.Cities.Top[1].Name != "İstanbul" {
		t.Fatalf("top 2 = %+v", two.Cities.Top)
	}
	check(t, "other", two.Cities.Other, analytics.Other{Cities: 1, Total: 1})
	check(t, "unknown city", two.Cities.Unknown, analytics.Counts{Total: 1, SMS: 1})
}

// Nothing sent yet: every number is zero, the series still spans the range.
func TestRead_Empty(t *testing.T) {
	o := read(t, newFixture(t), analytics.Filter{Range: "7d"})
	if len(o.Series) != 7 || o.KPIs != (analytics.KPIs{}) || len(o.Cities.Top) != 0 || o.Cities.Top == nil {
		t.Fatalf("overview = %+v; want zeros over 7 days and an empty (not null) ranking", o)
	}
}

func TestParseFilter(t *testing.T) {
	f, err := analytics.ParseFilter(url.Values{"range": {"30d"}, "channel": {"email"}, "campaign_id": {"12"}, "cities": {"5"}})
	if err != nil || f != (analytics.Filter{Range: "30d", Channel: "email", CampaignID: 12, Cities: 5}) {
		t.Fatalf("ParseFilter = %+v, %v", f, err)
	}
	if f, err := analytics.ParseFilter(url.Values{}); err != nil || f != (analytics.Filter{Range: "7d", Cities: 10}) {
		t.Fatalf("defaults = %+v, %v; want 7d and 10 cities", f, err)
	}
	if f, err := analytics.ParseFilter(url.Values{"range": {"day"}, "day": {"2026-10-26"}}); err != nil || f != (analytics.Filter{Range: "day", Day: "2026-10-26", Cities: 10}) {
		t.Fatalf("one day = %+v, %v", f, err)
	}
	for _, bad := range []url.Values{
		{"range": {"yesterday"}}, {"channel": {"fax"}}, {"campaign_id": {"x"}}, {"cities": {"0"}}, {"cities": {"51"}},
		{"range": {"day"}}, {"range": {"day"}, "day": {"26.10.2026"}}, {"range": {"day"}, "day": {"2026-02-30"}},
		{"range": {"7d"}, "day": {"2026-10-26"}},
	} {
		if _, err := analytics.ParseFilter(bad); err == nil {
			t.Errorf("ParseFilter(%v) accepted", bad)
		}
	}
}
