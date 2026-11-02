package delivery_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/delivery"
)

// Two stores stand for two workers: the window lives in the database, so
// the calls of both count against one limit.
func TestGrant_WindowSharedByWorkers(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	a, b := delivery.NewStore(w.db.Pool, delivery.DefaultPolicy), delivery.NewStore(w.db.Pool, delivery.DefaultPolicy)
	r := delivery.Rate{Calls: 3, Window: 500 * time.Millisecond}
	for _, s := range []*delivery.Store{a, b} {
		if err := s.SetLimit(ctx, "sms", r); err != nil {
			t.Fatal(err)
		}
	}
	for i, s := range []*delivery.Store{a, b, a} {
		if g, err := s.Grant(ctx, "sms"); err != nil || !g.Granted() {
			t.Fatalf("call %d: %+v, err %v; want it granted", i+1, g, err)
		}
	}
	g, err := b.Grant(ctx, "sms")
	if err != nil || g.Granted() || g.Paused || g.Wait < 400*time.Millisecond || g.Wait > 600*time.Millisecond {
		t.Fatalf("fourth call within the window: %+v, err %v; want a wait of about 550ms (window + margin)", g, err)
	}
	time.Sleep(g.Wait + 10*time.Millisecond)
	if g, err := a.Grant(ctx, "sms"); err != nil || !g.Granted() {
		t.Fatalf("after the window: %+v, err %v; want it granted", g, err)
	}

	// Another channel has its own window.
	if err := a.SetLimit(ctx, "email", r); err != nil {
		t.Fatal(err)
	}
	if g, err := b.Grant(ctx, "email"); err != nil || !g.Granted() {
		t.Fatalf("email: %+v, err %v; want it granted", g, err)
	}
}

// The limiter's own promise, by the database clock and under contention:
// however many workers ask at once, no more than Calls grants fall in any
// window + margin. (That the calls themselves start in time is the
// worker's part; see the worker tests.)
func TestGrant_ConcurrentWorkers_GrantsKeepWindow(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	r := delivery.Rate{Calls: 5, Window: 100 * time.Millisecond}
	var (
		mu    sync.Mutex
		at    []time.Time
		wg    sync.WaitGroup
		until = time.Now().Add(700 * time.Millisecond)
	)
	for range 4 { // workers
		s := delivery.NewStore(w.db.Pool, delivery.DefaultPolicy)
		if err := s.SetLimit(ctx, "sms", r); err != nil {
			t.Fatal(err)
		}
		for range 4 { // concurrent attempts per worker
			wg.Add(1)
			go func() {
				defer wg.Done()
				for time.Now().Before(until) {
					g, err := s.Grant(ctx, "sms")
					if err != nil {
						t.Error(err)
						return
					}
					if g.Granted() {
						mu.Lock()
						at = append(at, g.At)
						mu.Unlock()
						continue
					}
					time.Sleep(min(g.Wait, 5*time.Millisecond))
				}
			}()
		}
	}
	wg.Wait()
	slices.SortFunc(at, func(a, b time.Time) int { return a.Compare(b) })
	if len(at) < 15 {
		t.Fatalf("only %d grants in 700ms; want about 5 per 150ms", len(at))
	}
	assertWindow(t, at, r.Calls, r.Window+50*time.Millisecond)
}

// A 429 noted by one worker pauses the channel for all of them; a pause
// only ever extends.
func TestGrant_PauseSharedAndOnlyExtends(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	a, b := delivery.NewStore(w.db.Pool, delivery.DefaultPolicy), delivery.NewStore(w.db.Pool, delivery.DefaultPolicy)
	r := delivery.Rate{Calls: 100, Window: time.Second}
	for _, ch := range []string{"sms", "email"} {
		if err := a.SetLimit(ctx, ch, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Pause(ctx, "sms", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	g, err := b.Grant(ctx, "sms")
	if err != nil || g.Granted() || !g.Paused || g.Wait < 1800*time.Millisecond || g.Wait > 2*time.Second {
		t.Fatalf("during the pause: %+v, err %v; want paused for about 2s", g, err)
	}
	if err := a.Pause(ctx, "sms", 500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if g, _ := b.Grant(ctx, "sms"); !g.Paused || g.Wait < 1500*time.Millisecond {
		t.Fatalf("a shorter pause shortened it: %+v", g)
	}
	if g, err := b.Grant(ctx, "email"); err != nil || !g.Granted() {
		t.Fatalf("email: %+v, err %v; want it granted", g, err)
	}
}

// The limit lives in the database, one pace per channel: while another
// worker runs (a recent heartbeat), a worker configured differently is
// refused rather than allowed to keep its own pace.
func TestSetLimit_RefusedWhileAnotherWorkerRuns(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	r := delivery.Rate{Calls: 100, Window: time.Second}
	if err := w.store.SetLimit(ctx, "sms", r); err != nil {
		t.Fatal(err)
	}
	if err := w.store.SetLimit(ctx, "sms", r); err != nil {
		t.Fatalf("the same limit again: %v", err)
	}
	if err := w.store.Beat(ctx, "worker-a"); err != nil {
		t.Fatal(err)
	}
	if err := w.store.SetLimit(ctx, "sms", delivery.Rate{Calls: 50, Window: time.Second}); err == nil {
		t.Fatal("a different limit was accepted while another worker runs with the stored one")
	}
	if n := w.count(t, `SELECT calls FROM channel_limits WHERE channel = 'sms'`); n != 100 {
		t.Fatalf("limit = %d calls; the refused one must not be stored", n)
	}
}

// With no worker running, a changed setting replaces the stored limit, so
// a worker restarted with a new rate starts instead of failing for good.
// The old limit's grants go with it: the new limit counts from zero.
func TestSetLimit_ChangedWhileNoWorkerRuns(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if err := w.store.SetLimit(ctx, "sms", delivery.Rate{Calls: 100, Window: time.Second}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if g, err := w.store.Grant(ctx, "sms"); err != nil || !g.Granted() {
			t.Fatalf("grant = %+v, err %v", g, err)
		}
	}
	w.exec(t, `INSERT INTO worker_heartbeats (name, beat_at) VALUES ('stopped', now() - interval '1 minute')`)
	if err := w.store.SetLimit(ctx, "sms", delivery.Rate{Calls: 2, Window: time.Second}); err != nil {
		t.Fatalf("a new limit with no worker running: %v", err)
	}
	if n := w.count(t, `SELECT calls FROM channel_limits WHERE channel = 'sms' AND next_slot = 0`); n != 2 {
		t.Fatalf("stored limit = %d calls; want the new 2, its ring from the start", n)
	}
	if n := w.count(t, `SELECT count(*) FROM channel_grants WHERE channel = 'sms'`); n != 0 {
		t.Fatalf("%d grants of the old limit left", n)
	}
	for i := range 3 {
		g, err := w.store.Grant(ctx, "sms")
		if err != nil {
			t.Fatal(err)
		}
		if g.Granted() != (i < 2) {
			t.Fatalf("grant %d = %+v; the new limit allows 2 calls per second", i+1, g)
		}
	}
}
