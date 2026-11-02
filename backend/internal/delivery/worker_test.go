package delivery_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"smart-campaign-composer/backend/internal/clock"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/delivery"
	"smart-campaign-composer/backend/internal/metrics"
	"smart-campaign-composer/backend/internal/mockprovider"
	"smart-campaign-composer/backend/internal/provider"
	"smart-campaign-composer/backend/internal/testutil"
)

var (
	// fastPolicy and fastConfig keep worker tests on a real clock quick.
	fastPolicy = delivery.Policy{MaxAttempts: 5, BaseDelay: 10 * time.Millisecond, MaxDelay: 40 * time.Millisecond}
	fastConfig = delivery.Config{Lease: 2 * time.Second, CallBudget: time.Second, Batch: 100, Concurrency: 16,
		Idle: 20 * time.Millisecond, ReapEvery: 50 * time.Millisecond}
)

// rates gives both channels the same limit.
func rates(calls int, window time.Duration) map[string]delivery.Rate {
	r := delivery.Rate{Calls: calls, Window: window}
	return map[string]delivery.Rate{"sms": r, "email": r}
}

func (w *world) runWorker(t *testing.T, p delivery.Provider, channels map[string]delivery.Rate) (stop func()) {
	t.Helper()
	return w.runWorkerWith(t, p, channels, fastConfig)
}

// runWorkerWith runs a worker, with its own store as a separate process
// would have, until the returned stop is called or the test ends.
func (w *world) runWorkerWith(t *testing.T, p delivery.Provider, channels map[string]delivery.Rate, cfg delivery.Config) (stop func()) {
	t.Helper()
	return w.runPrepared(t, &delivery.Worker{Store: delivery.NewStore(w.db.Pool, w.policy), Provider: p, Channels: channels,
		Clock: w.clk, Config: cfg})
}

// runPrepared runs wk until the returned stop is called or the test ends.
func (w *world) runPrepared(t *testing.T, wk *delivery.Worker) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := wk.Run(ctx); err != nil {
			t.Errorf("worker: %v", err)
		}
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("the worker did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func (w *world) eventually(t *testing.T, timeout time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// settled waits until no delivery is waiting or in flight.
func (w *world) settled(t *testing.T, timeout time.Duration) {
	t.Helper()
	w.eventually(t, timeout, "deliveries to settle", func() bool {
		return w.count(t, `SELECT count(*) FROM deliveries WHERE status IN ('pending', 'retry', 'in_flight')`) == 0
	})
}

func (w *world) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := w.db.Pool.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (w *world) campaignStatus(t *testing.T, id int64) string {
	t.Helper()
	var s string
	if err := w.db.Pool.QueryRow(context.Background(), `SELECT status FROM campaigns WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (w *world) nextAttempt(t *testing.T, id string) time.Time {
	t.Helper()
	var at time.Time
	if err := w.db.Pool.QueryRow(context.Background(), `SELECT next_attempt_at FROM deliveries WHERE id = $1`, id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// claim leases what is due on a channel now; it must find something.
func (w *world) claim(t *testing.T, channel string) []delivery.Claimed {
	t.Helper()
	claimed, err := w.store.Claim(context.Background(), channel, 100, w.clk.Now(), time.Minute)
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim %s: %d deliveries, err %v", channel, len(claimed), err)
	}
	return claimed
}

func (w *world) deliveryIDs(t *testing.T, channel string) map[string]bool {
	t.Helper()
	rows, err := w.db.Pool.Query(context.Background(), `SELECT id::text FROM deliveries WHERE channel = $1`, channel)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[id] = true
	}
	return ids
}

// firstOf rate limits the first request carrying any of keys.
type firstOf struct {
	keys  map[string]bool
	fired atomic.Bool
}

func (f *firstOf) Fault(key string, _ int) mockprovider.Fault {
	if f.keys[key] && f.fired.CompareAndSwap(false, true) {
		return mockprovider.Reject429
	}
	return mockprovider.Pass
}

type call struct {
	channel, key string
	start, end   time.Time
	status       int
	outcome      provider.Outcome
	err          error
}

// String is how a failure message shows a call.
func (c call) String() string {
	return fmt.Sprintf("%s %d %v at %s", c.outcome, c.status, c.err, c.start.Format("15:04:05.000"))
}

// recording notes every call: when Provider.Send started and returned, for
// which delivery, and how it ended.
type recording struct {
	delivery.Provider
	mu    sync.Mutex
	calls []call
}

func (r *recording) Send(ctx context.Context, m provider.Message) provider.Result {
	start := time.Now()
	res := r.Provider.Send(ctx, m)
	r.mu.Lock()
	r.calls = append(r.calls, call{channel: m.Channel, key: m.IdempotencyKey, start: start, end: time.Now(),
		status: res.Status, outcome: res.Outcome, err: res.Err})
	r.mu.Unlock()
	return res
}

// on returns the calls on a channel by start.
func (r *recording) on(channel string) []call {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []call
	for _, c := range r.calls {
		if c.channel == channel {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b call) int { return a.start.Compare(b.start) })
	return out
}

func (r *recording) starts(channel string) []time.Time {
	var out []time.Time
	for _, c := range r.on(channel) {
		out = append(out, c.start)
	}
	return out
}

func (r *recording) first429() (call, bool) {
	for _, c := range r.on("sms") {
		if c.status == http.StatusTooManyRequests {
			return c, true
		}
	}
	return call{}, false
}

// assertWindow checks that at most limit of the sorted call starts fall in
// any [t, t+window).
func assertWindow(t *testing.T, starts []time.Time, limit int, window time.Duration) {
	t.Helper()
	for i := 0; i+limit < len(starts); i++ {
		if d := starts[i+limit].Sub(starts[i]); d < window {
			t.Fatalf("calls %d and %d started %v apart: %d calls within %v", i, i+limit, d, limit+1, window)
		}
	}
}

// assertCalls checks the SMS calls made for n deliveries: each delivery was
// called, the provider accepted each message once, and every call beyond
// one per delivery was a retry after an attempt that brought no acceptance
// back. Under load the provider may answer after the client's timeout: the
// attempt is ambiguous and its retry, under the same key, gets the first
// acceptance back. That is the design, so such retries are allowed; a
// delivery called again after an acceptance, or a call the database did not
// count as an attempt, is not.
func (w *world) assertCalls(t *testing.T, rec *recording, mp *mockprovider.Provider, n int) {
	t.Helper()
	calls := rec.on("sms")
	byKey := map[string][]call{}
	for _, c := range calls {
		byKey[c.key] = append(byKey[c.key], c)
	}
	retriedAfter := map[string]int{}
	for key, cs := range byKey {
		for _, c := range cs[:len(cs)-1] {
			if c.outcome == provider.Sent {
				t.Fatalf("delivery %s was called again after the provider accepted it: %v", key, cs)
			}
			retriedAfter[fmt.Sprintf("%s %d %v", c.outcome, c.status, c.err)]++
		}
	}
	attempts := w.count(t, `SELECT coalesce(sum(attempts), 0) FROM deliveries`)
	if st := mp.Stats(); len(byKey) != n || st.Accepted != n || attempts != len(calls) {
		t.Fatalf("%d deliveries called, %d accepted, %d calls for %d attempts; want %d, %d and one call per attempt (retries after: %v)",
			len(byKey), st.Accepted, len(calls), attempts, n, n, retriedAfter)
	}
	if sent := w.count(t, `SELECT count(*) FROM deliveries WHERE status = 'sent'`); sent != n {
		t.Fatalf("%d of %d deliveries reached sent; provider acceptance alone does not prove the database settled them", sent, n)
	}
	if len(calls) > n {
		t.Logf("%d retries, after: %v", len(calls)-n, retriedAfter)
	}
}

// slowProvider answers after delay.
type slowProvider struct {
	delivery.Provider
	delay time.Duration
}

func (p slowProvider) Send(ctx context.Context, m provider.Message) provider.Result {
	time.Sleep(p.delay)
	return p.Provider.Send(ctx, m)
}

// No call was made, so nothing counts as an attempt: a delivery stopped
// before its call goes back to the queue as it was.
func TestAttempt_StoppedBeforeCall_GivesDeliveryBack(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.person(t, "Ayşe")
	w.approve(t, &content.SMS{Text: "Yarın son gün!"}, nil)
	mp, sender := w.provider(t, nil)

	// Shutting down before the consent check.
	c := w.claim(t, "sms")[0]
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := sender.Attempt(cancelled, c); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if s := w.state(t, c.ID); s != (state{"pending", 0, 0}) {
		t.Fatalf("after a stop before the check: %+v, want pending as before", s)
	}

	// The channel is paused (429) when the delivery's turn comes.
	resume := w.clk.Now().Add(3 * time.Second)
	sender.Gate = func(context.Context, string, time.Time) (time.Time, error) {
		return time.Time{}, &delivery.PausedError{Until: resume}
	}
	c = w.claim(t, "sms")[0]
	var paused *delivery.PausedError
	if _, err := sender.Attempt(ctx, c); !errors.As(err, &paused) {
		t.Fatalf("err = %v, want *PausedError", err)
	}
	if s := w.state(t, c.ID); s != (state{"pending", 0, 0}) {
		t.Fatalf("after a pause: %+v, want pending as before", s)
	}
	if at := w.nextAttempt(t, c.ID); !at.Equal(resume) {
		t.Fatalf("due again at %v, want the end of the pause %v", at, resume)
	}
	if st := mp.Stats(); st.Requests != 0 {
		t.Fatalf("the provider got %d requests, want none", st.Requests)
	}
}

func TestSettleCampaigns(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	x := w.person(t, "Ayşe")
	w.person(t, "Mehmet")
	done := w.approve(t, &content.SMS{Text: "Bir"}, nil)
	waiting := w.approve(t, &content.SMS{Text: "İki"}, nil)
	started := w.approve(t, &content.SMS{Text: "Üç"}, nil)
	empty := w.approve(t, &content.SMS{Text: "Bir"}, nil) // everyone already has this message
	w.exec(t, `UPDATE deliveries SET status = 'sent' WHERE campaign_id = $1`, done.CampaignID)
	w.exec(t, `UPDATE deliveries SET status = 'retry', attempts = 1 WHERE campaign_id = $1 AND user_id = $2`, started.CampaignID, x.id)

	if err := w.store.SettleCampaigns(ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]string{done.CampaignID: "completed", waiting.CampaignID: "approved",
		started.CampaignID: "sending", empty.CampaignID: "completed"} {
		if got := w.campaignStatus(t, id); got != want {
			t.Errorf("campaign %d: %s, want %s", id, got, want)
		}
	}

	w.exec(t, `UPDATE deliveries SET status = 'unknown' WHERE campaign_id = $1`, started.CampaignID)
	if err := w.store.SettleCampaigns(ctx); err != nil {
		t.Fatal(err)
	}
	if got := w.campaignStatus(t, started.CampaignID); got != "completed" {
		t.Fatalf("after its last delivery ended: %s, want completed", got)
	}
}

// What was approved is what goes out: the worker sends the content rendered
// at approval, even if the person's details change afterwards.
func TestWorker_SendsStoredRenderedContent(t *testing.T) {
	w := newWorldWith(t, clock.Real{}, fastPolicy)
	x := w.person(t, "Ayşe")
	res := w.approve(t, &content.SMS{Text: "Merhaba {{first_name}}, siparişin hazır!"}, nil)
	w.exec(t, `UPDATE users SET first_name = 'Zeynep' WHERE id = $1`, x.id)

	mp, client := w.mockClient(t, nil)
	w.runWorker(t, client, rates(100, time.Second))
	w.settled(t, 10*time.Second)
	if approved, renamed := mp.AcceptedFor(x.phone, "sms", "", "Merhaba Ayşe, siparişin hazır!"),
		mp.AcceptedFor(x.phone, "sms", "", "Merhaba Zeynep, siparişin hazır!"); approved != 1 || renamed != 0 {
		t.Fatalf("accepted: approved text %d, re-rendered text %d; want 1 and 0", approved, renamed)
	}
	w.eventually(t, 2*time.Second, "the campaign to complete", func() bool {
		return w.campaignStatus(t, res.CampaignID) == "completed"
	})
}

// A worker stopped while its claimed deliveries wait for the channel's
// limit gives them back untouched: no call was made, so nothing counts as
// an attempt and nothing becomes ambiguous.
func TestWorker_ShutdownBeforeCall_GivesLeasesBack(t *testing.T) {
	w := newWorldWith(t, clock.Real{}, fastPolicy)
	ctx := context.Background()
	for i := range 5 {
		w.person(t, fmt.Sprintf("Kişi%d", i))
	}
	w.approve(t, &content.SMS{Text: "Yarın son gün!"}, nil)
	mp, client := w.mockClient(t, nil)

	// The channel's one call for the next 5s is already taken.
	slow := map[string]delivery.Rate{"sms": {Calls: 1, Window: 5 * time.Second}}
	if err := w.store.SetLimit(ctx, "sms", slow["sms"]); err != nil {
		t.Fatal(err)
	}
	if g, err := w.store.Grant(ctx, "sms"); err != nil || !g.Granted() {
		t.Fatalf("taking the call: %+v, err %v", g, err)
	}
	stop := w.runWorkerWith(t, client, slow, delivery.Config{Lease: 10 * time.Second, CallBudget: time.Second,
		Batch: 100, Concurrency: 16, Idle: 20 * time.Millisecond, ReapEvery: 50 * time.Millisecond})
	w.eventually(t, 5*time.Second, "the deliveries to be claimed", func() bool {
		return w.count(t, `SELECT count(*) FROM deliveries WHERE status = 'in_flight'`) == 5
	})
	time.Sleep(200 * time.Millisecond) // well into the wait for the limit

	stop()
	if n := w.count(t, `SELECT count(*) FROM deliveries
		WHERE status = 'pending' AND attempts = 0 AND ambiguous_attempts = 0 AND locked_until IS NULL`); n != 5 {
		t.Fatalf("%d of 5 deliveries given back untouched", n)
	}
	if st := mp.Stats(); st.Requests != 0 {
		t.Fatalf("the provider got %d requests, want none", st.Requests)
	}
}

// Slow calls must not let claimed deliveries' leases run out while they
// wait for a slot: a delivery is claimed only when it can start, so none is
// reclaimed, none counts an ambiguous attempt, and each is called once.
func TestWorker_SlowCalls_LeasesDoNotExpire(t *testing.T) {
	w := newWorldWith(t, clock.Real{}, fastPolicy)
	for i := range 10 {
		w.person(t, fmt.Sprintf("Kişi%d", i))
	}
	w.approve(t, &content.SMS{Text: "Kargon yola çıktı!"}, nil)
	mp, client := w.mockClient(t, nil)
	cfg := delivery.Config{Lease: time.Second, CallBudget: 600 * time.Millisecond, Batch: 10, Concurrency: 2,
		Idle: 20 * time.Millisecond, ReapEvery: 50 * time.Millisecond}
	w.runWorkerWith(t, slowProvider{client, 400 * time.Millisecond}, rates(100, time.Second), cfg)
	w.settled(t, 15*time.Second)

	if n := w.count(t, `SELECT count(*) FROM deliveries WHERE status = 'sent' AND attempts = 1 AND ambiguous_attempts = 0`); n != 10 {
		t.Fatalf("%d of 10 deliveries sent on a first, unambiguous attempt; leases ran out while waiting", n)
	}
	if st := mp.Stats(); st.Requests != 10 {
		t.Fatalf("the provider got %d requests, want one per delivery", st.Requests)
	}
}

// holdLock locks a table in a transaction of its own, making every query
// that needs the table wait, until the returned release is called.
func (w *world) holdLock(t *testing.T, table string) (release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := w.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Commit(ctx) }) }
	t.Cleanup(release)
	return release
}

// grantInstants returns a channel's recorded grants, by the database clock.
func (w *world) grantInstants(t *testing.T, channel string) []time.Time {
	t.Helper()
	rows, err := w.db.Pool.Query(context.Background(), `SELECT granted_at FROM channel_grants WHERE channel = $1 ORDER BY granted_at`, channel)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var at []time.Time
	for rows.Next() {
		var g time.Time
		if err := rows.Scan(&g); err != nil {
			t.Fatal(err)
		}
		at = append(at, g)
	}
	return at
}

type started struct {
	at        time.Time     // when Provider.Send was called
	leaseLeft time.Duration // what the delivery's lease had left then
}

// startCheck notes, for every call, when Provider.Send started and how much
// lease its delivery had left.
type startCheck struct {
	delivery.Provider
	w     *world
	mu    sync.Mutex
	calls []started
}

func (s *startCheck) Send(ctx context.Context, m provider.Message) provider.Result {
	at := time.Now()
	var until time.Time
	if err := s.w.db.Pool.QueryRow(ctx, `SELECT locked_until FROM deliveries WHERE id = $1`, m.IdempotencyKey).Scan(&until); err == nil {
		s.mu.Lock()
		s.calls = append(s.calls, started{at, until.Sub(at)})
		s.mu.Unlock()
	}
	return s.Provider.Send(ctx, m)
}

func (s *startCheck) list() []started {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// The check before the call is slow (the consent query waits ~800ms on a
// lock) and the grant then comes at once, with no wait. The lease is nearly
// spent by now: the call must not start. The delivery is given back
// untouched and sent later, once, with a fresh lease.
func TestWorker_SlowCheck_ZeroWaitGrant_RespectsLease(t *testing.T) {
	w := newWorldWith(t, clock.Real{}, fastPolicy)
	w.person(t, "Ayşe")
	w.approve(t, &content.SMS{Text: "Siparişin yolda!"}, nil)
	mp, client := w.mockClient(t, nil)
	sc := &startCheck{Provider: client, w: w}
	release := w.holdLock(t, "users")
	cfg := delivery.Config{Lease: time.Second, CallBudget: 600 * time.Millisecond, Batch: 10, Concurrency: 2,
		Idle: 20 * time.Millisecond, ReapEvery: 50 * time.Millisecond}
	w.runWorkerWith(t, sc, rates(100, time.Second), cfg)
	w.eventually(t, 5*time.Second, "the delivery to be claimed", func() bool {
		return w.count(t, `SELECT count(*) FROM deliveries WHERE status = 'in_flight'`) == 1
	})
	time.Sleep(800 * time.Millisecond)
	release()
	w.settled(t, 10*time.Second)

	calls := sc.list()
	for _, c := range calls {
		if c.leaseLeft < cfg.CallBudget {
			t.Fatalf("a call started with %v of lease left; it needs at least %v", c.leaseLeft, cfg.CallBudget)
		}
	}
	if n := w.count(t, `SELECT count(*) FROM deliveries WHERE status = 'sent' AND attempts = 1 AND ambiguous_attempts = 0`); n != 1 || len(calls) != 1 {
		t.Fatalf("%d calls, %d clean sends; want the delivery sent once, on its first counted attempt", len(calls), n)
	}
	if st := mp.Stats(); st.Requests != 1 {
		t.Fatalf("the provider got %d requests, want 1", st.Requests)
	}
}

// The grant query is slow (it waits ~300ms on a lock) and its answer comes
// back long after the database granted the call. Starting the call then
// could crowd a later window, so that grant is not used: the worker asks
// again, and every call starts within the 50ms margin of a grant.
func TestWorker_SlowGrant_StaleGrantNotUsed(t *testing.T) {
	w := newWorldWith(t, clock.Real{}, fastPolicy)
	w.person(t, "Ayşe")
	w.approve(t, &content.SMS{Text: "Siparişin yolda!"}, nil)
	mp, client := w.mockClient(t, nil)
	sc := &startCheck{Provider: client, w: w}
	release := w.holdLock(t, "channel_grants")
	cfg := delivery.Config{Lease: 10 * time.Second, CallBudget: time.Second, Batch: 10, Concurrency: 2,
		Idle: 20 * time.Millisecond, ReapEvery: 50 * time.Millisecond}
	w.runWorkerWith(t, sc, rates(100, time.Second), cfg)
	w.eventually(t, 5*time.Second, "the delivery to be claimed", func() bool {
		return w.count(t, `SELECT count(*) FROM deliveries WHERE status = 'in_flight'`) == 1
	})
	time.Sleep(300 * time.Millisecond)
	release()
	w.settled(t, 10*time.Second)

	grants, calls := w.grantInstants(t, "sms"), sc.list()
	if len(calls) != 1 {
		t.Fatalf("%d calls, want 1", len(calls))
	}
	for _, c := range calls {
		var since time.Duration = -1
		for _, g := range grants {
			if d := c.at.Sub(g); d >= 0 && (since < 0 || d < since) {
				since = d
			}
		}
		if since < 0 || since > 50*time.Millisecond {
			t.Fatalf("a call started %v after the latest grant before it (grants %v); the margin is 50ms", since, grants)
		}
	}
	if len(grants) != 2 {
		t.Fatalf("%d grants; want the late one left unused and a second one asked for", len(grants))
	}
	if st := mp.Stats(); st.Requests != 1 {
		t.Fatalf("the provider got %d requests, want 1", st.Requests)
	}
}

// The worker records what it does: attempts by outcome, how long each call
// took and waited for its grant, and a heartbeat under its name.
func TestWorker_RecordsMetricsAndHeartbeat(t *testing.T) {
	w := newWorldWith(t, clock.Real{}, fastPolicy)
	for i := range 3 {
		w.person(t, fmt.Sprintf("Kişi%d", i))
	}
	w.approve(t, &content.SMS{Text: "Metrikler"}, nil)
	var refused string
	for id := range w.deliveryIDs(t, "sms") {
		refused = id
		break
	}
	_, client := w.mockClient(t, mockprovider.Script{refused: {mockprovider.Reject400}})
	reg := prometheus.NewRegistry()
	w.runPrepared(t, &delivery.Worker{Store: delivery.NewStore(w.db.Pool, w.policy), Provider: client,
		Channels: rates(100, time.Second), Clock: w.clk, Config: fastConfig, Name: "worker-1", Metrics: metrics.New(reg)})
	w.settled(t, 10*time.Second)

	sms := map[string]string{"channel": "sms"}
	outcome := func(o string) map[string]string { return map[string]string{"channel": "sms", "outcome": o} }
	for _, c := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"scc_provider_attempts_total", outcome("sent"), 2},
		{"scc_provider_attempts_total", outcome("not_sent_permanent"), 1},
		{"scc_provider_latency_seconds", sms, 3},
		{"scc_ratelimit_wait_seconds", sms, 3},
	} {
		if got := testutil.MetricValue(t, reg, c.name, c.labels); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.labels, got, c.want)
		}
	}
	w.eventually(t, 2*time.Second, "a heartbeat", func() bool {
		return w.count(t, `SELECT count(*) FROM worker_heartbeats WHERE name = 'worker-1' AND beat_at > now() - interval '10 seconds'`) == 1
	})
}

// A 429 pauses its channel for the provider's Retry-After (1s here): the
// next SMS call waits for it, while email goes on.
func TestWorker_429PausesChannel(t *testing.T) {
	w := newWorldWith(t, clock.Real{}, fastPolicy)
	for i := range 3 {
		w.person(t, fmt.Sprintf("Kişi%d", i))
	}
	w.approve(t, &content.SMS{Text: "SMS duyurusu"}, &content.Email{Subject: "Duyuru", Body: "Email duyurusu"})
	_, client := w.mockClient(t, &firstOf{keys: w.deliveryIDs(t, "sms")})
	rec := &recording{Provider: client}
	w.runWorker(t, rec, rates(1, 100*time.Millisecond))
	w.settled(t, 10*time.Second)

	sms, email := rec.on("sms"), rec.on("email")
	if len(sms) < 2 || sms[0].status != http.StatusTooManyRequests {
		t.Fatalf("sms calls = %+v, want a 429 first", sms)
	}
	if gap := sms[1].start.Sub(sms[0].end); gap < 900*time.Millisecond {
		t.Fatalf("the next SMS call came %v after the 429; want the channel paused for about 1s", gap)
	}
	if len(email) != 3 || !email[2].start.Before(sms[1].start) {
		t.Fatalf("email calls = %+v; they must not wait for the SMS pause", email)
	}
	if n := w.count(t, `SELECT count(*) FROM deliveries WHERE status = 'sent'`); n != 6 {
		t.Fatalf("%d of 6 deliveries sent", n)
	}
}

// The limit holds on a real clock through the whole worker, measured where
// it matters: in any [t, t+1s) at most 100 Provider.Send calls start,
// retries included.
func TestRealClock_WindowLE100(t *testing.T) {
	w := newWorldWith(t, clock.Real{}, fastPolicy)
	for i := range 250 {
		w.person(t, fmt.Sprintf("Kişi%d", i))
	}
	w.approve(t, &content.SMS{Text: "Merhaba {{first_name}}, hafta sonu indirimi!"}, nil)
	mp, client := w.mockClient(t, nil)
	rec := &recording{Provider: client}
	start := time.Now()
	w.runWorker(t, rec, rates(100, time.Second))
	w.settled(t, 20*time.Second)
	elapsed := time.Since(start)

	w.assertCalls(t, rec, mp, 250)
	assertWindow(t, rec.starts("sms"), 100, time.Second)
	if elapsed < 2*time.Second {
		t.Fatalf("250 calls took %v; at 100/s they need at least 2s", elapsed)
	}
}

// Two workers share one channel limit: together they start at most 100
// calls in any second, not 100 each.
func TestWorkers_TwoShareChannelLimit(t *testing.T) {
	w := newWorldWith(t, clock.Real{}, fastPolicy)
	for i := range 250 {
		w.person(t, fmt.Sprintf("Kişi%d", i))
	}
	w.approve(t, &content.SMS{Text: "Merhaba {{first_name}}, iki worker!"}, nil)
	mp, client := w.mockClient(t, nil)
	rec := &recording{Provider: client}
	w.runWorker(t, rec, rates(100, time.Second))
	w.runWorker(t, rec, rates(100, time.Second))
	w.settled(t, 20*time.Second)

	w.assertCalls(t, rec, mp, 250)
	assertWindow(t, rec.starts("sms"), 100, time.Second)
}

// A worker restarted right after a burst does not start a fresh burst: the
// last second's calls still count.
func TestWorker_RestartKeepsChannelLimit(t *testing.T) {
	w := newWorldWith(t, clock.Real{}, fastPolicy)
	for i := range 200 {
		w.person(t, fmt.Sprintf("Kişi%d", i))
	}
	w.approve(t, &content.SMS{Text: "Merhaba {{first_name}}, yeniden başlatma!"}, nil)
	_, client := w.mockClient(t, nil)
	rec := &recording{Provider: client}
	stop := w.runWorker(t, rec, rates(100, time.Second))
	w.eventually(t, 5*time.Second, "the first 100 calls", func() bool { return len(rec.starts("sms")) >= 100 })
	stop()
	w.runWorker(t, rec, rates(100, time.Second))
	w.settled(t, 20*time.Second)

	assertWindow(t, rec.starts("sms"), 100, time.Second)
}

// A 429 pauses the channel for every worker, including one started during
// the pause.
func TestPause_SharedAcrossWorkersAndRestarts(t *testing.T) {
	w := newWorldWith(t, clock.Real{}, fastPolicy)
	for i := range 6 {
		w.person(t, fmt.Sprintf("Kişi%d", i))
	}
	w.approve(t, &content.SMS{Text: "SMS duyurusu"}, nil)
	_, client := w.mockClient(t, &firstOf{keys: w.deliveryIDs(t, "sms")})
	rec := &recording{Provider: client}
	slow := rates(1, 100*time.Millisecond)
	stopA := w.runWorker(t, rec, slow)
	w.runWorker(t, rec, slow)
	w.eventually(t, 5*time.Second, "the 429", func() bool { _, ok := rec.first429(); return ok })
	stopA()
	w.runWorker(t, rec, slow) // started during the pause
	w.settled(t, 15*time.Second)

	the429, _ := rec.first429()
	for _, c := range rec.on("sms") {
		if c.start.After(the429.start) && c.start.Before(the429.end.Add(900*time.Millisecond)) {
			t.Fatalf("an SMS call started %v after the 429 answer; the 1s pause holds for every worker",
				c.start.Sub(the429.end))
		}
	}
	if n := w.count(t, `SELECT count(*) FROM deliveries WHERE status = 'sent'`); n != 6 {
		t.Fatalf("%d of 6 deliveries sent", n)
	}
}

// A worker asked to stop while it is still starting (setting its limits,
// first heartbeat) stops: that is the stop asked for, as it is once the
// worker runs, not a failure. A worker stopped right after another one's
// 429 (TestPause_SharedAcrossWorkersAndRestarts) used to report "context
// canceled". A real startup failure still is one.
func TestWorker_StopWhileStarting_IsAStop(t *testing.T) {
	w := newWorld(t)
	starting := func(r delivery.Rate) *delivery.Worker {
		return &delivery.Worker{Store: delivery.NewStore(w.db.Pool, w.policy), Channels: map[string]delivery.Rate{"sms": r},
			Clock: w.clk, Config: fastConfig, Name: "starting"}
	}
	limit := delivery.Rate{Calls: 100, Window: time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := starting(limit).Run(ctx); err != nil {
		t.Fatalf("stopped before it started: %v; want a clean stop", err)
	}

	// Stopped while its heartbeat (or its limit) waits on the database.
	release := w.holdLock(t, "worker_heartbeats")
	ctx, cancel = context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- starting(limit).Run(ctx) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stopped while starting: %v; want a clean stop", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not stop")
	}
	release()

	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.store.SetLimit(ctx, "sms", limit); err != nil {
		t.Fatal(err)
	}
	if err := w.store.Beat(ctx, "running"); err != nil {
		t.Fatal(err)
	}
	if err := starting(delivery.Rate{Calls: 50, Window: time.Second}).Run(ctx); err == nil {
		t.Fatal("a worker with another limit than a running worker's started")
	}
}

// A stopped worker no longer counts as running: restarted with a changed
// rate (a new SMS_RATE_PER_SECOND), the worker starts at once with it
// instead of being refused until the old heartbeat ages.
func TestWorker_RestartWithChangedLimitStarts(t *testing.T) {
	w := newWorldWith(t, clock.Real{}, fastPolicy)
	_, client := w.mockClient(t, nil)
	stop := w.runWorker(t, client, rates(100, time.Second))
	w.eventually(t, 5*time.Second, "a heartbeat", func() bool {
		return w.count(t, `SELECT count(*) FROM worker_heartbeats`) == 1
	})
	stop()
	if n := w.count(t, `SELECT count(*) FROM worker_heartbeats`); n != 0 {
		t.Fatalf("%d heartbeats left by the stopped worker", n)
	}
	w.runWorker(t, client, rates(50, time.Second))
	w.eventually(t, 5*time.Second, "the new limit", func() bool {
		return w.count(t, `SELECT calls FROM channel_limits WHERE channel = 'sms'`) == 50
	})
}

// restartable serves a mock provider that can be restarted from its log,
// like a provider process restart.
type restartable struct {
	path   string
	faults mockprovider.Faults
	mu     sync.RWMutex
	cur    *mockprovider.Provider
}

func (r *restartable) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.RLock()
	p := r.cur
	r.mu.RUnlock()
	p.ServeHTTP(w, req)
}

func (r *restartable) restart(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != nil {
		_ = r.cur.Close()
	}
	p, err := mockprovider.Open(r.path, r.faults)
	if err != nil {
		t.Fatal(err)
	}
	r.cur = p
}

func (r *restartable) provider() *mockprovider.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cur
}

// Chaos: a flaky provider (refusals, rate limits, 503s that did or did not
// accept, lost answers), a worker that died holding leases, a provider
// restart, consent withdrawn and the same content approved again mid-run,
// and the worker itself replaced. However it plays out, every delivery
// ends, no message is accepted twice, every sent delivery was accepted, and
// none that ended failed or skipped ever was.
func TestChaos_AllTerminal_AtMostOneAcceptancePerMessage(t *testing.T) {
	w := newWorldWith(t, clock.Real{}, fastPolicy)
	ctx := context.Background()
	var people []person
	for i := range 40 {
		people = append(people, w.person(t, fmt.Sprintf("Kişi%d", i)))
	}
	smsA := &content.SMS{Text: "Merhaba {{first_name}}, A kampanyası başladı!"}
	w.approve(t, smsA, &content.Email{Subject: "A kampanyası", Body: "Merhaba {{first_name}}, A kampanyası başladı."})
	w.approve(t, &content.SMS{Text: "{{first_name}}, son gün yarın!"}, nil)

	faults, err := mockprovider.NewRandom(42, map[mockprovider.Fault]float64{
		mockprovider.Reject429: 0.03, mockprovider.Reject400: 0.05, mockprovider.Fail503: 0.05,
		mockprovider.AcceptThen503: 0.05, mockprovider.AcceptThenDrop: 0.05,
	})
	if err != nil {
		t.Fatal(err)
	}
	front := &restartable{path: filepath.Join(t.TempDir(), "provider.log"), faults: faults}
	front.restart(t)
	t.Cleanup(func() { _ = front.provider().Close() })
	srv := httptest.NewServer(front)
	t.Cleanup(srv.Close)
	client := provider.NewClient(srv.URL, provider.Options{Timeout: 150 * time.Millisecond})

	// A worker died holding leases on some deliveries.
	if _, err := w.store.Claim(ctx, "sms", 10, w.clk.Now(), 300*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	stop := w.runWorker(t, client, rates(500, time.Second))
	time.Sleep(700 * time.Millisecond)
	front.restart(t)
	for i := 0; i < len(people); i += 7 {
		w.setSMSConsent(t, people[i], false)
	}
	w.approve(t, smsA, nil) // only people whose first A message definitely failed get it now
	stop()
	w.runWorker(t, client, rates(500, time.Second))
	w.settled(t, 30*time.Second)

	st := front.provider().Stats()
	for _, m := range st.ByMessage {
		if m.Accepted > 1 {
			t.Errorf("%s %s %.8s… accepted %d times", m.To, m.Channel, m.ContentHash, m.Accepted)
		}
	}
	rows, err := w.db.Pool.Query(ctx, `SELECT id::text, status FROM deliveries`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ended := map[string]int{}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			t.Fatal(err)
		}
		ended[status]++
		switch status {
		case "sent":
			if st.ByKey[id] != 1 {
				t.Errorf("delivery %s is sent but the provider never accepted it", id)
			}
		case "failed", "skipped_no_consent":
			if st.ByKey[id] != 0 {
				t.Errorf("delivery %s ended %s but the provider accepted it", id, status)
			}
		case "unknown":
		default:
			t.Errorf("delivery %s did not end: %s", id, status)
		}
	}
	t.Logf("deliveries by status: %v; provider: %d requests since the restart, %d acceptances", ended, st.Requests, st.Accepted)
	w.eventually(t, 2*time.Second, "every campaign to complete", func() bool {
		return w.count(t, `SELECT count(*) FROM campaigns WHERE status <> 'completed'`) == 0
	})
}
