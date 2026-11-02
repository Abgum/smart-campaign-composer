package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"smart-campaign-composer/backend/internal/clock"
	"smart-campaign-composer/backend/internal/metrics"
	"smart-campaign-composer/backend/internal/provider"
)

// Config tunes a Worker.
type Config struct {
	Lease       time.Duration // a claim's hold on a delivery
	CallBudget  time.Duration // lease a call must have left when it starts: client timeout plus time to record
	Batch       int           // most deliveries claimed at once per channel
	Concurrency int           // attempts at once per channel
	Idle        time.Duration // pause before looking again when nothing is due
	ReapEvery   time.Duration // how often expired leases are reclaimed and campaigns settled
}

// DefaultConfig suits a 10s provider timeout.
var DefaultConfig = Config{Lease: 30 * time.Second, CallBudget: 15 * time.Second, Batch: 100, Concurrency: 32,
	Idle: 250 * time.Millisecond, ReapEvery: 5 * time.Second}

func (c Config) validate() error {
	switch {
	case c.CallBudget <= 0 || c.Lease <= c.CallBudget:
		return fmt.Errorf("delivery: the lease (%v) must be longer than the call budget (%v)", c.Lease, c.CallBudget)
	case c.Batch <= 0 || c.Concurrency <= 0 || c.Idle <= 0 || c.ReapEvery <= 0:
		return fmt.Errorf("delivery: invalid worker config %+v", c)
	}
	return nil
}

// minPause is how long a 429 pauses its channel when it names no wait.
const minPause = time.Second

// PausedError stops an attempt before its call while the provider has asked
// the channel to slow down (429); the delivery is due again at Until.
type PausedError struct{ Until time.Time }

func (e *PausedError) Error() string {
	return "channel paused until " + e.Until.UTC().Format(time.RFC3339Nano)
}

// ResumeAt is when the delivery is due again.
func (e *PausedError) ResumeAt() time.Time { return e.Until }

// BusyError stops an attempt whose call could not start while its lease
// still covers the call: the channel's limit is taken until Until, when the
// delivery is due again.
type BusyError struct{ Until time.Time }

func (e *BusyError) Error() string {
	return "channel limit taken until " + e.Until.UTC().Format(time.RFC3339Nano)
}

// ResumeAt is when the delivery is due again.
func (e *BusyError) ResumeAt() time.Time { return e.Until }

// Worker sends due deliveries. Channel limits and 429 pauses live in the
// database, so they hold across all workers and restarts. Per channel the
// worker claims only as many deliveries as it has free slots, so no claimed
// delivery waits for a slot while its lease runs; right before each call it
// asks the database for a grant. It also reclaims expired leases and
// settles finished campaigns.
type Worker struct {
	Store    *Store
	Provider Provider
	Channels map[string]Rate // the channels to send, each with its limit
	Clock    clock.Clock
	Config   Config
	Name     string           // in heartbeats; empty means host:pid
	Metrics  *metrics.Metrics // may be nil

	mu     sync.Mutex
	paused map[string]time.Time // pauses seen, so as not to claim meanwhile
}

// Run sends until ctx ends. Then it gives back what it claimed but has not
// called yet, and returns once the calls already made are recorded. If ctx
// ends while the worker is still starting, that too is a stop, not a
// failure.
func (w *Worker) Run(ctx context.Context) error {
	if err := w.Config.validate(); err != nil {
		return err
	}
	for ch, r := range w.Channels {
		if err := w.Store.SetLimit(ctx, ch, r); err != nil {
			return unlessStopped(ctx, err)
		}
	}
	if w.Name == "" {
		w.Name = defaultName()
	}
	if err := w.Store.Beat(ctx, w.Name); err != nil {
		return unlessStopped(ctx, err)
	}
	w.mu.Lock()
	w.paused = map[string]time.Time{}
	w.mu.Unlock()
	s := &Sender{Store: w.Store, Provider: watched{w.Provider, w}, Clock: w.Clock, Gate: w.gate,
		CallBudget: w.Config.CallBudget, Metrics: w.Metrics}
	var wg sync.WaitGroup
	for ch := range w.Channels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.dispatch(ctx, s, ch)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.housekeeping(ctx)
	}()
	wg.Wait()
	// Stopped, its calls recorded: the worker no longer runs, so a restart
	// with a changed limit need not wait for this heartbeat to age.
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := w.Store.Leave(lctx, w.Name); err != nil {
		slog.Warn("remove worker heartbeat", "worker", w.Name, "err", err)
	}
	return nil
}

// dispatch claims and attempts due deliveries on one channel.
func (w *Worker) dispatch(ctx context.Context, s *Sender, channel string) {
	var inflight sync.WaitGroup
	defer inflight.Wait()
	slots := make(chan struct{}, w.Config.Concurrency)
	for w.sleep(ctx, w.pausedUntil(channel).Sub(w.Clock.Now())) {
		// Claim only what can start now: wait for a free slot, then take
		// every other free one, and claim one delivery per slot.
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		free := 1
	take:
		for free < w.Config.Batch {
			select {
			case slots <- struct{}{}:
				free++
			default:
				break take
			}
		}
		claimed, err := w.Store.Claim(ctx, channel, free, w.Clock.Now(), w.Config.Lease)
		if err != nil && ctx.Err() == nil {
			slog.Error("claim deliveries", "channel", channel, "err", err)
		}
		for range free - len(claimed) {
			<-slots
		}
		if len(claimed) == 0 {
			if !w.sleep(ctx, w.Config.Idle) {
				return
			}
			continue
		}
		for _, c := range claimed {
			inflight.Add(1)
			go func() {
				defer func() {
					<-slots
					inflight.Done()
				}()
				if _, err := s.Attempt(ctx, c); err != nil && !givenBack(err) {
					slog.Warn("delivery attempt", "delivery_id", c.ID, "err", err)
				}
			}()
		}
	}
}

// gate waits until a call on channel may start: the channel is not paused
// and its shared limit grants the call. It returns the latest time the
// call may start for the grant to count (see Sender.Gate). It gives up when
// the channel is paused (*PausedError) or when the grant would come after
// deadline (*BusyError); the delivery is then given back, due when the call
// could start.
func (w *Worker) gate(ctx context.Context, channel string, deadline time.Time) (time.Time, error) {
	for {
		asked := w.Clock.Now()
		g, err := w.Store.Grant(ctx, channel)
		if err != nil {
			return time.Time{}, err
		}
		if g.Granted() {
			// The database granted the call somewhere between asked and now.
			return asked.Add(startMargin), nil
		}
		resume := w.Clock.Now().Add(g.Wait)
		if g.Paused {
			w.notePause(channel, resume)
			return time.Time{}, &PausedError{Until: resume}
		}
		if resume.After(deadline) {
			return time.Time{}, &BusyError{Until: resume}
		}
		if !w.sleep(ctx, g.Wait) {
			return time.Time{}, ctx.Err()
		}
	}
}

// notePause remembers a pause so that the channel claims nothing meanwhile.
func (w *Worker) notePause(channel string, until time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if until.After(w.paused[channel]) {
		w.paused[channel] = until
	}
}

func (w *Worker) pausedUntil(channel string) time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.paused[channel]
}

// housekeeping beats, reclaims expired leases and settles campaigns
// periodically.
func (w *Worker) housekeeping(ctx context.Context) {
	for w.sleep(ctx, w.Config.ReapEvery) {
		if err := w.Store.Beat(ctx, w.Name); err != nil && ctx.Err() == nil {
			slog.Error("worker heartbeat", "err", err)
		}
		n, err := w.Store.ReapExpired(ctx, w.Clock.Now())
		switch {
		case err != nil && ctx.Err() == nil:
			slog.Error("reclaim expired leases", "err", err)
		case n > 0:
			w.Metrics.LeasesReclaimed(n)
			slog.Warn("reclaimed expired leases; their attempts count as ambiguous", "count", n)
		}
		if err := w.Store.SettleCampaigns(ctx); err != nil && ctx.Err() == nil {
			slog.Error("settle campaigns", "err", err)
		}
	}
}

// unlessStopped is err, or nil when err is only ctx ending: the stop that
// was asked for.
func unlessStopped(ctx context.Context, err error) error {
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return nil
	}
	return err
}

// defaultName names a worker by host and process.
func defaultName() string {
	host, err := os.Hostname()
	if err != nil {
		host = "worker"
	}
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}

// sleep waits d, or until ctx ends; it reports whether to go on.
func (w *Worker) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-w.Clock.After(d):
		return ctx.Err() == nil
	}
}

// watched passes calls through; a 429 pauses the channel for every worker.
type watched struct {
	Provider
	w *Worker
}

func (p watched) Send(ctx context.Context, m provider.Message) provider.Result {
	r := p.Provider.Send(ctx, m)
	if r.Status == http.StatusTooManyRequests {
		d := max(r.RetryAfter, minPause)
		p.w.notePause(m.Channel, p.w.Clock.Now().Add(d))
		sctx, cancel := detached(ctx)
		defer cancel()
		if err := p.w.Store.Pause(sctx, m.Channel, d); err != nil {
			slog.Error("pause channel", "channel", m.Channel, "err", err)
		}
		slog.Warn("provider rate limit: channel paused", "channel", m.Channel, "for", d)
	}
	return r
}

// givenBack reports errors that only mean an attempt was given back before
// its call: the worker is stopping, the channel is paused or busy, or the
// lease ran short.
func givenBack(err error) bool {
	var (
		p *PausedError
		b *BusyError
	)
	return errors.Is(err, context.Canceled) || errors.Is(err, ErrLeaseShort) || errors.As(err, &p) || errors.As(err, &b)
}
