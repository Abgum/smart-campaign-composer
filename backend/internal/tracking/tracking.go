// Package tracking reads what the dashboard shows: per campaign and channel,
// who was left out at approval and how the deliveries stand; per channel,
// the queue, the limit and any provider pause; and which workers are alive.
package tracking

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound means there is no such campaign.
var ErrNotFound = errors.New("campaign not found")

const (
	// recent bounds the campaigns the dashboard lists; totals cover all.
	recent = 50
	// workerStale is how old a worker's heartbeat may be for it to count as
	// alive.
	workerStale = 30 * time.Second
)

var channels = []string{"sms", "email"}

// Counts are one channel's numbers, for a campaign or for all campaigns.
type Counts struct {
	ExcludedConsent   int `json:"excluded_consent"`   // left out at approval: no consent or contact
	ExcludedDuplicate int `json:"excluded_duplicate"` // left out at approval: had or will get the same message
	Pending           int `json:"pending"`
	InFlight          int `json:"in_flight"`
	Retry             int `json:"retry"`
	Sent              int `json:"sent"`
	Failed            int `json:"failed"`
	Unknown           int `json:"unknown"` // the provider may have accepted it
	SkippedNoConsent  int `json:"skipped_no_consent"`
}

// ByStatus returns the delivery counts by status, every status included.
func (c Counts) ByStatus() map[string]int {
	return map[string]int{"pending": c.Pending, "in_flight": c.InFlight, "retry": c.Retry, "sent": c.Sent,
		"failed": c.Failed, "unknown": c.Unknown, "skipped_no_consent": c.SkippedNoConsent}
}

// add counts n deliveries in status.
func (c *Counts) add(status string, n int) {
	switch status {
	case "pending":
		c.Pending += n
	case "in_flight":
		c.InFlight += n
	case "retry":
		c.Retry += n
	case "sent":
		c.Sent += n
	case "failed":
		c.Failed += n
	case "unknown":
		c.Unknown += n
	case "skipped_no_consent":
		c.SkippedNoConsent += n
	}
}

// Campaign is a campaign's progress.
type Campaign struct {
	ID          int64             `json:"-"`
	PublicID    string            `json:"id"`
	ChatID      string            `json:"chat_id"`
	Ordinal     int               `json:"ordinal"`
	Name        string            `json:"name"`
	Status      string            `json:"status"`
	UpdatedAt   time.Time         `json:"updated_at"`
	ApprovedAt  *time.Time        `json:"approved_at"`
	ScheduledAt *time.Time        `json:"scheduled_at"` // when sending starts, in UTC; nil: at approval
	LastSentAt  *time.Time        `json:"last_sent_at"`
	Version     *int              `json:"version"`      // the approved version, else the current one
	LLMProvider *string           `json:"llm_provider"` // the model that wrote that version
	Channels    map[string]Counts `json:"channels"`
	// Variants splits an A/B campaign's numbers by arm, then channel; nil
	// for a campaign without an A/B test.
	Variants map[string]map[string]Counts `json:"variants,omitempty"`
}

// ListFilter narrows the campaign list.
type ListFilter struct {
	ID     int64  // one campaign; 0 means any
	Status string // "" | draft | active (approved, sending) | completed
	Query  string // part of the name
	Limit  int    // 0 means 100
}

// ErrBadFilter is a list filter the API cannot read.
var ErrBadFilter = errors.New("tracking: bad filter")

// listStatuses map a list filter's status to campaign statuses.
var listStatuses = map[string]string{
	"draft":     `c.status = 'draft'`,
	"active":    `c.status IN ('approved', 'sending')`,
	"completed": `c.status = 'completed'`,
}

// ListCampaigns lists campaigns across chats, newest first: drafts by their
// last change, the others by their approval.
func ListCampaigns(ctx context.Context, db *pgxpool.Pool, f ListFilter) ([]Campaign, error) {
	where, args := []string{"true"}, []any{}
	if f.ID != 0 {
		args = append(args, f.ID)
		where = append(where, fmt.Sprintf(`c.id = $%d`, len(args)))
	}
	if f.Status != "" {
		cond, ok := listStatuses[f.Status]
		if !ok {
			return nil, fmt.Errorf("%w: status %q (draft, active, completed)", ErrBadFilter, f.Status)
		}
		where = append(where, cond)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		args = append(args, "%"+likeEscaper.Replace(q)+"%")
		where = append(where, fmt.Sprintf(`c.name ILIKE $%d`, len(args)))
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args = append(args, limit)
	return readCampaigns(ctx, db, strings.Join(where, " AND ")+
		fmt.Sprintf(` ORDER BY coalesce(c.approved_at, c.updated_at) DESC, c.id DESC LIMIT $%d`, len(args)), args...)
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// Channel is a channel's sending state.
type Channel struct {
	Due         int        `json:"due"`             // deliveries due now: the queue
	Calls       int        `json:"calls,omitempty"` // the shared limit: calls per window
	WindowMS    int        `json:"window_ms,omitempty"`
	PausedUntil *time.Time `json:"paused_until"` // a provider 429 pause, if one is on
}

// Worker is a worker's last heartbeat.
type Worker struct {
	Name   string    `json:"name"`
	BeatAt time.Time `json:"beat_at"`
	Alive  bool      `json:"alive"`
}

// Totals are the numbers over all campaigns, per channel.
type Totals struct {
	Counts   map[string]Counts  `json:"totals"`
	Channels map[string]Channel `json:"channels"`
}

// Dashboard is everything the dashboard shows at one moment.
type Dashboard struct {
	At        time.Time  `json:"at"`
	Campaigns []Campaign `json:"campaigns"`
	Totals
	Workers []Worker `json:"workers"`
	Version string   `json:"version"` // changes whenever a delivery or campaign does
}

// Read returns the dashboard: the most recent approved campaigns, the
// totals, the channels and the workers. now decides which deliveries are
// due.
func Read(ctx context.Context, db *pgxpool.Pool, now time.Time) (Dashboard, error) {
	d := Dashboard{At: now}
	var err error
	if d.Campaigns, err = readCampaigns(ctx, db,
		`c.status IN ('approved', 'sending', 'completed') ORDER BY c.id DESC LIMIT $1`, recent); err != nil {
		return Dashboard{}, err
	}
	if d.Totals, err = ReadTotals(ctx, db, now); err != nil {
		return Dashboard{}, err
	}
	if d.Workers, err = readWorkers(ctx, db); err != nil {
		return Dashboard{}, err
	}
	d.Version = version(d)
	return d, nil
}

// version fingerprints the campaigns and totals: any delivery or campaign
// change gives a new token.
func version(d Dashboard) string {
	h := sha256.New()
	_ = json.NewEncoder(h).Encode(struct {
		C []Campaign
		T map[string]Counts
	}{d.Campaigns, d.Counts})
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ReadCampaign returns one campaign's progress (a draft has no counts).
func ReadCampaign(ctx context.Context, db *pgxpool.Pool, id int64) (Campaign, error) {
	list, err := readCampaigns(ctx, db, `c.id = $1`, id)
	if err != nil {
		return Campaign{}, err
	}
	if len(list) == 0 {
		return Campaign{}, ErrNotFound
	}
	return list[0], nil
}

// readCampaigns reads the campaigns matching where, with their approval
// snapshots and delivery counts.
func readCampaigns(ctx context.Context, db *pgxpool.Pool, where string, args ...any) ([]Campaign, error) {
	rows, err := db.Query(ctx, `
		SELECT c.id, c.public_id::text, c.chat_id::text, c.ordinal, c.name, c.status, c.approval, c.approval_variants,
		       c.updated_at, c.approved_at, c.scheduled_at, v.version, v.llm_provider
		FROM campaigns c LEFT JOIN campaign_versions v ON v.id = coalesce(c.approved_version_id, c.current_version_id)
		WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Campaign, error) {
		var (
			c               Campaign
			approval, byArm []byte
		)
		if err := row.Scan(&c.ID, &c.PublicID, &c.ChatID, &c.Ordinal, &c.Name, &c.Status, &approval, &byArm, &c.UpdatedAt,
			&c.ApprovedAt, &c.ScheduledAt, &c.Version, &c.LLMProvider); err != nil {
			return c, err
		}
		if byArm != nil {
			var snapshot map[string]map[string]Counts // only the exclusions are set at approval
			if err := json.Unmarshal(byArm, &snapshot); err != nil {
				return c, err
			}
			c.Variants = map[string]map[string]Counts{}
			for arm, byChannel := range snapshot {
				c.Variants[arm] = map[string]Counts{}
				for ch, s := range byChannel {
					c.Variants[arm][ch] = Counts{ExcludedConsent: s.ExcludedConsent, ExcludedDuplicate: s.ExcludedDuplicate}
				}
			}
		}
		if c.ScheduledAt != nil {
			at := c.ScheduledAt.UTC()
			c.ScheduledAt = &at
		}
		c.Channels = map[string]Counts{}
		if approval != nil {
			var snapshot map[string]Counts // only the exclusions are set at approval
			if err := json.Unmarshal(approval, &snapshot); err != nil {
				return c, err
			}
			for ch, s := range snapshot {
				c.Channels[ch] = Counts{ExcludedConsent: s.ExcludedConsent, ExcludedDuplicate: s.ExcludedDuplicate}
			}
		}
		return c, nil
	})
	if err != nil || len(list) == 0 {
		return []Campaign{}, err
	}
	at := make(map[int64]int, len(list))
	ids := make([]int64, len(list))
	for i, c := range list {
		at[c.ID], ids[i] = i, c.ID
	}
	rows, err = db.Query(ctx, `
		SELECT campaign_id, channel, coalesce(ab_variant, ''), status, count(*), max(sent_at)
		FROM deliveries WHERE campaign_id = ANY($1) GROUP BY 1, 2, 3, 4`, ids)
	if err != nil {
		return nil, err
	}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var (
			id                   int64
			channel, arm, status string
			n                    int
			lastSent             *time.Time
		)
		if err := row.Scan(&id, &channel, &arm, &status, &n, &lastSent); err != nil {
			return struct{}{}, err
		}
		c := &list[at[id]]
		if lastSent != nil && (c.LastSentAt == nil || lastSent.After(*c.LastSentAt)) {
			c.LastSentAt = lastSent
		}
		counts := c.Channels[channel]
		counts.add(status, n)
		c.Channels[channel] = counts
		if arm != "" {
			if c.Variants == nil {
				c.Variants = map[string]map[string]Counts{}
			}
			if c.Variants[arm] == nil {
				c.Variants[arm] = map[string]Counts{}
			}
			byArm := c.Variants[arm][channel]
			byArm.add(status, n)
			c.Variants[arm][channel] = byArm
		}
		return struct{}{}, nil
	})
	return list, err
}

// ReadTotals returns the numbers over all campaigns and each channel's
// state; now decides which deliveries are due.
func ReadTotals(ctx context.Context, db *pgxpool.Pool, now time.Time) (Totals, error) {
	t := Totals{Counts: map[string]Counts{}, Channels: map[string]Channel{}}
	for _, ch := range channels {
		t.Counts[ch] = Counts{}
	}
	rows, err := db.Query(ctx, `SELECT channel, status, count(*) FROM deliveries GROUP BY 1, 2`)
	if err != nil {
		return Totals{}, err
	}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var (
			channel, status string
			n               int
		)
		if err := row.Scan(&channel, &status, &n); err != nil {
			return struct{}{}, err
		}
		counts := t.Counts[channel]
		counts.add(status, n)
		t.Counts[channel] = counts
		return struct{}{}, nil
	})
	if err != nil {
		return Totals{}, err
	}

	rows, err = db.Query(ctx, `
		SELECT a.key, coalesce(sum((a.value->>'excluded_consent')::int), 0), coalesce(sum((a.value->>'excluded_duplicate')::int), 0)
		FROM campaigns c, jsonb_each(c.approval) AS a WHERE c.approval IS NOT NULL GROUP BY a.key`)
	if err != nil {
		return Totals{}, err
	}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var (
			channel            string
			consent, duplicate int
		)
		if err := row.Scan(&channel, &consent, &duplicate); err != nil {
			return struct{}{}, err
		}
		counts := t.Counts[channel]
		counts.ExcludedConsent, counts.ExcludedDuplicate = consent, duplicate
		t.Counts[channel] = counts
		return struct{}{}, nil
	})
	if err != nil {
		return Totals{}, err
	}

	rows, err = db.Query(ctx, `
		SELECT c.ch,
		       (SELECT count(*) FROM deliveries d
		        WHERE d.channel = c.ch AND d.status IN ('pending', 'retry') AND d.next_attempt_at <= $1),
		       coalesce(l.calls, 0), coalesce(l.window_ms, 0),
		       CASE WHEN l.paused_until > clock_timestamp() THEN l.paused_until END
		FROM unnest($2::text[]) AS c(ch) LEFT JOIN channel_limits l ON l.channel = c.ch`, now, channels)
	if err != nil {
		return Totals{}, err
	}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var (
			channel string
			s       Channel
		)
		if err := row.Scan(&channel, &s.Due, &s.Calls, &s.WindowMS, &s.PausedUntil); err != nil {
			return struct{}{}, err
		}
		t.Channels[channel] = s
		return struct{}{}, nil
	})
	if err != nil {
		return Totals{}, err
	}
	return t, nil
}

func readWorkers(ctx context.Context, db *pgxpool.Pool) ([]Worker, error) {
	rows, err := db.Query(ctx, `
		SELECT name, beat_at, beat_at > now() - make_interval(secs => $1) FROM worker_heartbeats ORDER BY name`,
		workerStale.Seconds())
	if err != nil {
		return nil, err
	}
	workers, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Worker, error) {
		var w Worker
		err := row.Scan(&w.Name, &w.BeatAt, &w.Alive)
		return w, err
	})
	if workers == nil {
		workers = []Worker{}
	}
	return workers, err
}
