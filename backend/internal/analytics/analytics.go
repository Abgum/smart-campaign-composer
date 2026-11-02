// Package analytics answers the overview's questions about sending over a
// period: the headline counts, a series by day (or by hour for one day), and
// where messages went, by the recipient's city.
package analytics

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"smart-campaign-composer/backend/internal/geo"
)

// ErrBadFilter is a filter the API cannot read.
var ErrBadFilter = errors.New("analytics: bad filter")

// Filter selects the slice every number is computed over.
type Filter struct {
	Range      string // today | day | 7d | 30d | 90d | all
	Day        string // 2026-10-27, the local day when Range is "day"
	Channel    string // "" (both) | sms | email
	CampaignID int64  // 0: every campaign
	Cities     int    // how many cities to rank
}

// Counts are deliveries per channel.
type Counts struct {
	Total int `json:"total"`
	SMS   int `json:"sms"`
	Email int `json:"email"`
}

// Queue is what waits to be sent now, whatever the range.
type Queue struct {
	Total    int `json:"total"`
	Pending  int `json:"pending"`
	InFlight int `json:"in_flight"`
	Retry    int `json:"retry"`
	SMS      int `json:"sms"`
	Email    int `json:"email"`
	// Scheduled are pending deliveries of campaigns whose scheduled time has
	// not come; they are in none of the counts above.
	Scheduled int `json:"scheduled"`
}

// KPIs are the headline counts.
type KPIs struct {
	Sent    Counts `json:"sent"`    // sent in the range
	Queued  Queue  `json:"queued"`  // now
	Failed  Counts `json:"failed"`  // failed in the range
	Unknown Counts `json:"unknown"` // ended unknown in the range
}

// Point is one bucket of sent deliveries.
type Point struct {
	At    string `json:"at"` // 2026-10-27, or 2026-10-27T14:00 by hour
	SMS   int    `json:"sms"`
	Email int    `json:"email"`
}

// City is the sent deliveries to people in one city.
type City struct {
	GeonameID int64  `json:"geoname_id"`
	Name      string `json:"name"` // empty when the catalog does not know the id
	Country   string `json:"country,omitempty"`
	Admin1    string `json:"admin1,omitempty"`
	SMS       int    `json:"sms"`
	Email     int    `json:"email"`
	Total     int    `json:"total"`
}

// Other folds the cities below the ranking.
type Other struct {
	Cities int `json:"cities"`
	Total  int `json:"total"`
}

// Cities ranks where sent deliveries went. Unknown counts recipients
// without a known city; it is never ranked.
type Cities struct {
	Top     []City `json:"top"`
	Other   Other  `json:"other"`
	Unknown Counts `json:"unknown"`
}

// Window is the period the numbers cover, in local days.
type Window struct {
	Range  string `json:"range"`
	From   string `json:"from"` // first day
	To     string `json:"to"`   // last day
	TZ     string `json:"tz"`
	Bucket string `json:"bucket"` // day | hour
}

// Overview is everything the overview's charts show for one filter.
type Overview struct {
	Window Window  `json:"window"`
	KPIs   KPIs    `json:"kpis"`
	Series []Point `json:"series"`
	Cities Cities  `json:"cities"`
}

// rangeDays are the ranges and how many local days each spans; "day" is
// the one day the filter names, "all" runs from the first send, at most
// maxAllDays back.
var rangeDays = map[string]int{"today": 1, "day": 1, "7d": 7, "30d": 30, "90d": 90, "all": 0}

const (
	maxAllDays = 366
	maxCities  = 50
	dayFormat  = "2006-01-02"
	hourFormat = "2006-01-02T15:00"
)

// ParseFilter reads ?range&day&channel&campaign_id&cities; it defaults to the
// last 7 days, both channels, every campaign and 10 cities. A day goes with
// range=day only.
func ParseFilter(q url.Values) (Filter, error) {
	f := Filter{Range: "7d", Cities: 10}
	if v := q.Get("range"); v != "" {
		if _, ok := rangeDays[v]; !ok {
			return Filter{}, fmt.Errorf("%w: range %q (today, day, 7d, 30d, 90d, all)", ErrBadFilter, v)
		}
		f.Range = v
	}
	switch v := q.Get("day"); {
	case f.Range == "day":
		if _, err := time.Parse(dayFormat, v); err != nil {
			return Filter{}, fmt.Errorf("%w: day %q (YYYY-MM-DD)", ErrBadFilter, v)
		}
		f.Day = v
	case v != "":
		return Filter{}, fmt.Errorf("%w: day goes with range=day", ErrBadFilter)
	}
	switch v := q.Get("channel"); v {
	case "", "sms", "email":
		f.Channel = v
	default:
		return Filter{}, fmt.Errorf("%w: channel %q (sms, email)", ErrBadFilter, v)
	}
	if v := q.Get("campaign_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			return Filter{}, fmt.Errorf("%w: campaign_id %q", ErrBadFilter, v)
		}
		f.CampaignID = id
	}
	if v := q.Get("cities"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxCities {
			return Filter{}, fmt.Errorf("%w: cities %q (1-%d)", ErrBadFilter, v, maxCities)
		}
		f.Cities = n
	}
	return f, nil
}

// Read computes the overview for f as of now, with days in loc.
//
// Sent counts by sent_at, failed and unknown by finished_at, both within the
// range's local days; the queue is what waits now. The series and the
// cities count sent deliveries only, by the recipient's city recorded at
// approval.
func Read(ctx context.Context, db *pgxpool.Pool, cat *geo.Catalog, loc *time.Location, now time.Time, f Filter) (Overview, error) {
	if f.Cities <= 0 {
		f.Cities = 10
	}
	today := midnight(now.In(loc))
	last, end := today, today.AddDate(0, 0, 1) // the window's last day, and the end of it
	start, bucket := today.AddDate(0, 0, 1-rangeDays[f.Range]), "day"
	from := start // the lower bound of the counts
	switch f.Range {
	case "today":
		bucket = "hour"
	case "day":
		day, err := time.ParseInLocation(dayFormat, f.Day, loc)
		if err != nil {
			return Overview{}, fmt.Errorf("%w: day %q", ErrBadFilter, f.Day)
		}
		start, from, last, end, bucket = day, day, day, day.AddDate(0, 0, 1), "hour"
	case "all":
		first, err := firstSend(ctx, db, f, end)
		if err != nil {
			return Overview{}, err
		}
		start = today
		if first != nil {
			start = midnight(first.In(loc))
		}
		if oldest := today.AddDate(0, 0, 1-maxAllDays); start.Before(oldest) {
			start = oldest
		}
		from = time.Unix(0, 0)
	}
	o := Overview{Window: Window{Range: f.Range, From: start.Format(dayFormat), To: last.Format(dayFormat), TZ: loc.String(), Bucket: bucket}}
	var err error
	if o.KPIs, err = readKPIs(ctx, db, f, from, end, now); err != nil {
		return Overview{}, err
	}
	if o.Series, err = readSeries(ctx, db, f, loc, start, end, bucket); err != nil {
		return Overview{}, err
	}
	if o.Cities, err = readCities(ctx, db, cat, f, from, end); err != nil {
		return Overview{}, err
	}
	return o, nil
}

// slice is the WHERE part shared by every query: $3 the channel (” for
// both), $4 the campaign (0 for all).
const slice = `($3::text = '' OR channel = $3::text) AND ($4::bigint = 0 OR campaign_id = $4::bigint)`

// firstSend is the earliest send before end, if any.
func firstSend(ctx context.Context, db *pgxpool.Pool, f Filter, end time.Time) (*time.Time, error) {
	var first *time.Time
	err := db.QueryRow(ctx, `
		SELECT min(sent_at) FROM deliveries
		WHERE status = 'sent' AND sent_at >= $1 AND sent_at < $2 AND `+slice,
		time.Unix(0, 0), end, f.Channel, f.CampaignID).Scan(&first)
	return first, err
}

// readKPIs counts the range's outcomes and the queue now. A pending delivery
// of a campaign scheduled after now is not queued yet: it counts as
// scheduled.
func readKPIs(ctx context.Context, db *pgxpool.Pool, f Filter, from, to, now time.Time) (KPIs, error) {
	rows, err := db.Query(ctx, `
		SELECT channel,
		       count(*) FILTER (WHERE status = 'sent' AND sent_at >= $1 AND sent_at < $2),
		       count(*) FILTER (WHERE status = 'failed' AND finished_at >= $1 AND finished_at < $2),
		       count(*) FILTER (WHERE status = 'unknown' AND finished_at >= $1 AND finished_at < $2),
		       count(*) FILTER (WHERE status = 'pending' AND NOT later),
		       count(*) FILTER (WHERE status = 'in_flight'),
		       count(*) FILTER (WHERE status = 'retry'),
		       count(*) FILTER (WHERE status = 'pending' AND later)
		FROM (SELECT d.*, coalesce(c.scheduled_at > $5, false) AS later
		      FROM deliveries d JOIN campaigns c ON c.id = d.campaign_id) deliveries
		WHERE (status IN ('pending', 'in_flight', 'retry')
		       OR (status = 'sent' AND sent_at >= $1 AND sent_at < $2)
		       OR (status IN ('failed', 'unknown') AND finished_at >= $1 AND finished_at < $2))
		  AND `+slice+`
		GROUP BY channel`, from, to, f.Channel, f.CampaignID, now)
	if err != nil {
		return KPIs{}, err
	}
	var k KPIs
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var (
			channel                                                      string
			sent, failed, unknown, pending, inFlight, retried, scheduled int
		)
		if err := row.Scan(&channel, &sent, &failed, &unknown, &pending, &inFlight, &retried, &scheduled); err != nil {
			return struct{}{}, err
		}
		k.Sent.add(channel, sent)
		k.Failed.add(channel, failed)
		k.Unknown.add(channel, unknown)
		k.Queued.Scheduled += scheduled
		k.Queued.Pending += pending
		k.Queued.InFlight += inFlight
		k.Queued.Retry += retried
		waiting := pending + inFlight + retried
		k.Queued.Total += waiting
		if channel == "sms" {
			k.Queued.SMS += waiting
		} else {
			k.Queued.Email += waiting
		}
		return struct{}{}, nil
	})
	return k, err
}

func (c *Counts) add(channel string, n int) {
	c.Total += n
	if channel == "sms" {
		c.SMS += n
	} else {
		c.Email += n
	}
}

func readSeries(ctx context.Context, db *pgxpool.Pool, f Filter, loc *time.Location, start, end time.Time, bucket string) ([]Point, error) {
	pgFormat, goFormat, step := `YYYY-MM-DD`, dayFormat, func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }
	if bucket == "hour" {
		pgFormat, goFormat, step = `YYYY-MM-DD"T"HH24:00`, hourFormat, func(t time.Time) time.Time { return t.Add(time.Hour) }
	}
	rows, err := db.Query(ctx, `
		SELECT to_char(date_trunc($5, sent_at AT TIME ZONE $6), $7), channel, count(*)
		FROM deliveries
		WHERE status = 'sent' AND sent_at >= $1 AND sent_at < $2 AND `+slice+`
		GROUP BY 1, 2`, start, end, f.Channel, f.CampaignID, bucket, loc.String(), pgFormat)
	if err != nil {
		return nil, err
	}
	at := map[string]*Point{}
	series := []Point{}
	for t := start; t.Before(end); t = step(t) {
		series = append(series, Point{At: t.In(loc).Format(goFormat)})
	}
	for i := range series {
		at[series[i].At] = &series[i]
	}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var (
			key, channel string
			n            int
		)
		if err := row.Scan(&key, &channel, &n); err != nil {
			return struct{}{}, err
		}
		if p, ok := at[key]; ok {
			if channel == "sms" {
				p.SMS += n
			} else {
				p.Email += n
			}
		}
		return struct{}{}, nil
	})
	return series, err
}

func readCities(ctx context.Context, db *pgxpool.Pool, cat *geo.Catalog, f Filter, from, to time.Time) (Cities, error) {
	rows, err := db.Query(ctx, `
		SELECT city_geoname_id, channel, count(*)
		FROM deliveries
		WHERE status = 'sent' AND sent_at >= $1 AND sent_at < $2 AND `+slice+`
		GROUP BY 1, 2`, from, to, f.Channel, f.CampaignID)
	if err != nil {
		return Cities{}, err
	}
	out := Cities{Top: []City{}}
	byID := map[int64]*City{}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var (
			id      *int64
			channel string
			n       int
		)
		if err := row.Scan(&id, &channel, &n); err != nil {
			return struct{}{}, err
		}
		if id == nil {
			out.Unknown.add(channel, n)
			return struct{}{}, nil
		}
		c := byID[*id]
		if c == nil {
			c = &City{GeonameID: *id}
			if known, ok := cat.City(*id); ok {
				c.Name, c.Country, c.Admin1 = known.Name, known.Country, known.Admin1
			}
			byID[*id] = c
		}
		if channel == "sms" {
			c.SMS += n
		} else {
			c.Email += n
		}
		c.Total += n
		return struct{}{}, nil
	})
	if err != nil {
		return Cities{}, err
	}
	all := make([]City, 0, len(byID))
	for _, c := range byID {
		all = append(all, *c)
	}
	// Most sent first; ties by name (named cities before unnamed ids).
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		switch {
		case a.Total != b.Total:
			return a.Total > b.Total
		case (a.Name == "") != (b.Name == ""):
			return a.Name != ""
		case a.Name != b.Name:
			return a.Name < b.Name
		}
		return a.GeonameID < b.GeonameID
	})
	if len(all) > f.Cities {
		for _, c := range all[f.Cities:] {
			out.Other.Cities++
			out.Other.Total += c.Total
		}
		all = all[:f.Cities]
	}
	out.Top = append(out.Top, all...)
	return out, nil
}

func midnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
