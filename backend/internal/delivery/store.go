package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"smart-campaign-composer/backend/internal/provider"
	"smart-campaign-composer/backend/internal/segment"
)

// ErrLeaseLost means the caller no longer holds the delivery: the reaper
// reclaimed it after the lease ran out, already counting the attempt as
// ambiguous. The attempt's result is not recorded.
var ErrLeaseLost = errors.New("delivery lease lost")

// errLeaseExpired is what a reclaimed attempt records as its error.
var errLeaseExpired = errors.New("lease expired mid-attempt; the provider may have accepted the message")

// reapBatch bounds one reaper pass; the next pass takes the rest.
const reapBatch = 500

// Lease is one claim of a delivery by a worker. Attempts, the attempts made
// before the claim, fences it: a reclaim counts an attempt, so a stale
// lease no longer matches.
type Lease struct {
	ID       string
	Attempts int
}

// Claimed is a delivery leased for one attempt, with its stored rendered
// content.
type Claimed struct {
	Lease
	LockedUntil time.Time // when the lease runs out
	UserID      int64
	Channel     string
	Subject     string
	Body        string
}

// Rate is a send limit shared by all workers: at most Calls calls start on
// a channel in any Window.
type Rate struct {
	Calls  int
	Window time.Duration
}

// startMargin widens every channel's window a little. A call starts a few
// milliseconds after its grant (commit, return, building the request); the
// margin keeps the limit true for the calls' actual starts, not just for
// the grants.
const startMargin = 50 * time.Millisecond

// liveFor is how recent a heartbeat must be for its worker to count as
// running, as on the dashboard.
const liveFor = 30 * time.Second

// SetLimit registers a channel's send limit. The limit lives in the
// database, shared by every worker and kept across restarts. One channel has
// one pace: a worker set to a different limit is refused while another
// worker runs (a heartbeat within liveFor). With none running, the new limit
// replaces the stored one, so a changed rate takes effect when the workers
// start again; the old limit's grants go with it, as none of them can still
// be in a window.
func (s *Store) SetLimit(ctx context.Context, channel string, r Rate) error {
	if r.Calls <= 0 || r.Window <= 0 {
		return fmt.Errorf("delivery: invalid limit for %s: %d calls per %v", channel, r.Calls, r.Window)
	}
	windowMS := (r.Window + startMargin).Milliseconds()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var (
		calls int
		ms    int64
	)
	// The upsert locks the channel's row until commit: limits are set one
	// worker at a time.
	err = tx.QueryRow(ctx, `
		INSERT INTO channel_limits (channel, calls, window_ms) VALUES ($1, $2, $3)
		ON CONFLICT (channel) DO UPDATE SET channel = EXCLUDED.channel
		RETURNING calls, window_ms`, channel, r.Calls, windowMS).Scan(&calls, &ms)
	if err != nil {
		return err
	}
	if calls == r.Calls && ms == windowMS {
		return tx.Commit(ctx)
	}
	var (
		running string
		ago     float64
	)
	err = tx.QueryRow(ctx, `
		SELECT name, extract(epoch FROM now() - beat_at) FROM worker_heartbeats
		WHERE beat_at > now() - make_interval(secs => $1) ORDER BY beat_at DESC LIMIT 1`, liveFor.Seconds()).Scan(&running, &ago)
	switch {
	case err == nil:
		return fmt.Errorf("delivery: %s is limited to %d calls per %dms in the database and worker %s (heartbeat %.0fs ago) runs with it; "+
			"this worker has %d per %dms. All workers share one limit: stop the others to change it",
			channel, calls, ms, running, ago, r.Calls, windowMS)
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE channel_limits SET calls = $2, window_ms = $3, next_slot = 0 WHERE channel = $1`,
		channel, r.Calls, windowMS); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM channel_grants WHERE channel = $1`, channel); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	slog.Info("channel limit changed", "channel", channel, "calls", r.Calls, "window_ms", windowMS, "was_calls", calls, "was_window_ms", ms)
	return nil
}

// Leave removes a stopped worker's heartbeat: it no longer runs, so a
// restart with a changed limit need not wait for the heartbeat to age.
func (s *Store) Leave(ctx context.Context, name string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM worker_heartbeats WHERE name = $1`, name)
	return err
}

// Grant is the database's answer to a request to start one call.
type Grant struct {
	At     time.Time     // when the call was granted, by the database clock; zero if it was not
	Wait   time.Duration // if not granted: how long until asking again
	Paused bool          // the wait is a provider pause (429), not the limit
}

// Granted reports whether the call may start.
func (g Grant) Granted() bool { return !g.At.IsZero() }

// Grant asks to start one call on a channel now, by the database clock.
func (s *Store) Grant(ctx context.Context, channel string) (Grant, error) {
	var (
		g  Grant
		at *time.Time
		ms float64
	)
	if err := s.pool.QueryRow(ctx, `SELECT granted_at, wait_ms, paused FROM grant_call($1)`, channel).Scan(&at, &ms, &g.Paused); err != nil {
		return Grant{}, err
	}
	if at != nil {
		g.At = *at
	}
	g.Wait = time.Duration(ms * float64(time.Millisecond))
	return g, nil
}

// Pause stops calls on a channel for d from now, for every worker. A pause
// only ever extends.
func (s *Store) Pause(ctx context.Context, channel string, d time.Duration) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE channel_limits
		SET paused_until = greatest(coalesce(paused_until, '-infinity'), clock_timestamp() + make_interval(secs => $2))
		WHERE channel = $1`, channel, d.Seconds())
	return err
}

// Store persists delivery state.
type Store struct {
	pool   *pgxpool.Pool
	policy Policy
}

// NewStore returns a store deciding with policy p.
func NewStore(pool *pgxpool.Pool, p Policy) *Store { return &Store{pool: pool, policy: p} }

// Claim leases up to limit due deliveries on a channel (pending, or retry
// whose time has come), oldest first, until now+lease. SKIP LOCKED lets
// workers claim side by side without waiting for each other.
func (s *Store) Claim(ctx context.Context, channel string, limit int, now time.Time, lease time.Duration) ([]Claimed, error) {
	rows, err := s.pool.Query(ctx, `
		WITH due AS (
			SELECT id FROM deliveries
			WHERE channel = $1 AND status IN ('pending', 'retry') AND next_attempt_at <= $2
			ORDER BY next_attempt_at, id LIMIT $4
			FOR UPDATE SKIP LOCKED)
		UPDATE deliveries d SET status = 'in_flight', locked_until = $3, updated_at = now()
		FROM due WHERE d.id = due.id
		RETURNING d.id::text, d.attempts, d.locked_until, d.user_id, d.channel, coalesce(d.subject, ''), d.body`,
		channel, now, now.Add(lease), limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Claimed, error) {
		var c Claimed
		err := row.Scan(&c.ID, &c.Attempts, &c.LockedUntil, &c.UserID, &c.Channel, &c.Subject, &c.Body)
		return c, err
	})
}

// Release gives leased deliveries back to the queue, due at at, without
// counting an attempt: their provider call was never made. Leases already
// lost are left alone.
func (s *Store) Release(ctx context.Context, at time.Time, leases ...Lease) error {
	if len(leases) == 0 {
		return nil
	}
	ids := make([]string, len(leases))
	attempts := make([]int32, len(leases))
	for i, l := range leases {
		ids[i], attempts[i] = l.ID, int32(l.Attempts)
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE deliveries d SET status = CASE WHEN d.attempts = 0 THEN 'pending' ELSE 'retry' END,
		       locked_until = NULL, next_attempt_at = $1, updated_at = now()
		FROM unnest($2::text[], $3::int[]) AS l(id, attempts)
		WHERE d.id = l.id::uuid AND d.status = 'in_flight' AND d.attempts = l.attempts`, at, ids, attempts)
	return err
}

// Beat records that a worker is alive (database time).
func (s *Store) Beat(ctx context.Context, name string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO worker_heartbeats (name, beat_at) VALUES ($1, now())
		ON CONFLICT (name) DO UPDATE SET beat_at = now()`, name)
	return err
}

// SettleCampaigns moves campaigns along as their deliveries do: approved or
// sending becomes completed once no delivery is waiting or in flight (also
// when there was none to send), and approved becomes sending once one has
// been taken up.
func (s *Store) SettleCampaigns(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE campaigns c SET status = 'completed', updated_at = now()
		WHERE c.status IN ('approved', 'sending') AND NOT EXISTS (
			SELECT 1 FROM deliveries d WHERE d.campaign_id = c.id AND d.status IN ('pending', 'retry', 'in_flight'))`); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE campaigns c SET status = 'sending', updated_at = now()
		WHERE c.status = 'approved' AND EXISTS (
			SELECT 1 FROM deliveries d WHERE d.campaign_id = c.id AND d.status <> 'pending')`)
	return err
}

// contact returns where to send on a channel and whether the person can
// still be messaged there: approval's reachability rule, read again now.
func (s *Store) contact(ctx context.Context, userID int64, channel string) (string, bool, error) {
	q := `SELECT coalesce(u.phone, ''), ` + segment.SMSReachable + ` FROM users u WHERE u.id = $1`
	if channel == "email" {
		q = `SELECT coalesce(u.email, ''), ` + segment.EmailReachable + ` FROM users u WHERE u.id = $1`
	}
	var (
		to string
		ok bool
	)
	if err := s.pool.QueryRow(ctx, q, userID).Scan(&to, &ok); err != nil {
		return "", false, err
	}
	return to, ok, nil
}

// Record applies an attempt's result to a leased delivery and releases the
// lease. It returns ErrLeaseLost, changing nothing, if the lease was lost.
func (s *Store) Record(ctx context.Context, l Lease, r provider.Result, now time.Time) (Decision, error) {
	return s.settle(ctx, l, now, func(a Attempts) (Decision, outcome) {
		d := s.policy.Decide(a, r, now)
		return d, resultOutcome(d, r)
	})
}

// skip ends a leased delivery whose recipient can no longer be messaged on
// its channel, without an attempt (see Unreachable).
func (s *Store) skip(ctx context.Context, l Lease, now time.Time) (Decision, error) {
	return s.settle(ctx, l, now, func(a Attempts) (Decision, outcome) {
		return Unreachable(a), outcome{lastError: "consent or contact withdrawn before sending"}
	})
}

// settle decides for a leased delivery and writes the decision in one
// transaction, releasing the lease; ErrLeaseLost if the lease is gone.
func (s *Store) settle(ctx context.Context, l Lease, now time.Time, decide func(Attempts) (Decision, outcome)) (Decision, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Decision{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var a Attempts
	err = tx.QueryRow(ctx, `
		SELECT attempts, ambiguous_attempts FROM deliveries
		WHERE id = $1 AND status = 'in_flight' AND attempts = $2 FOR UPDATE`, l.ID, l.Attempts).Scan(&a.Made, &a.Ambiguous)
	if errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, ErrLeaseLost
	}
	if err != nil {
		return Decision{}, err
	}
	d, o := decide(a)
	if err := apply(ctx, tx, l.ID, d, o, now); err != nil {
		return Decision{}, err
	}
	return d, tx.Commit(ctx)
}

// ReapExpired reclaims in_flight deliveries whose lease ended before now:
// their worker died or stalled mid-attempt. The attempt may have reached the
// provider, so it counts as ambiguous: the delivery is retried under the
// same idempotency key, or ends unknown when no attempts are left. It
// returns how many deliveries it reclaimed.
func (s *Store) ReapExpired(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	type expired struct {
		id string
		a  Attempts
	}
	rows, err := tx.Query(ctx, `
		SELECT id::text, attempts, ambiguous_attempts FROM deliveries
		WHERE status = 'in_flight' AND locked_until < $1
		ORDER BY locked_until LIMIT $2 FOR UPDATE SKIP LOCKED`, now, reapBatch)
	if err != nil {
		return 0, err
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (expired, error) {
		var e expired
		err := row.Scan(&e.id, &e.a.Made, &e.a.Ambiguous)
		return e, err
	})
	if err != nil {
		return 0, err
	}
	lost := provider.Result{Outcome: provider.Ambiguous, Err: errLeaseExpired}
	for _, e := range list {
		d := s.policy.Decide(e.a, lost, now)
		if err := apply(ctx, tx, e.id, d, resultOutcome(d, lost), now); err != nil {
			return 0, err
		}
	}
	return len(list), tx.Commit(ctx)
}

// outcome is what a decision writes besides the status and the counts.
type outcome struct {
	messageID string // the provider's id, when sent
	lastError string // why the delivery is not sent (yet)
}

func resultOutcome(d Decision, r provider.Result) outcome {
	if d.Status == StatusSent {
		return outcome{messageID: r.MessageID}
	}
	return outcome{lastError: describe(r)}
}

// apply writes a decision and releases the lease.
func apply(ctx context.Context, tx pgx.Tx, id string, d Decision, o outcome, now time.Time) error {
	var retryAt, sentAt, finishedAt *time.Time
	switch d.Status {
	case StatusSent:
		sentAt, finishedAt = &now, &now
	case StatusRetry:
		retryAt = &d.RetryAt
	case StatusFailed, StatusUnknown, StatusSkippedNoConsent:
		finishedAt = &now
	}
	_, err := tx.Exec(ctx, `
		UPDATE deliveries SET status = $2, attempts = $3, ambiguous_attempts = $4,
		       next_attempt_at = coalesce($5, next_attempt_at), locked_until = NULL, last_error = nullif($6, ''),
		       provider_message_id = coalesce(nullif($7, ''), provider_message_id), sent_at = $8, finished_at = $9,
		       updated_at = now()
		WHERE id = $1`, id, d.Status, d.Attempts.Made, d.Attempts.Ambiguous, retryAt, o.lastError, o.messageID, sentAt, finishedAt)
	return err
}

// describe is the last_error text of an attempt.
func describe(r provider.Result) string {
	s := string(r.Outcome)
	if r.Status != 0 {
		s += fmt.Sprintf(": HTTP %d", r.Status)
	}
	if r.Err != nil {
		s += ": " + r.Err.Error()
	}
	return s
}
