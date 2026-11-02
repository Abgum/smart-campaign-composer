package delivery_test

import (
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/delivery"
	"smart-campaign-composer/backend/internal/provider"
)

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

// policy is the default policy with a fixed jitter draw.
func policy(r float64) delivery.Policy {
	p := delivery.DefaultPolicy
	p.Rand = func() float64 { return r }
	return p
}

func outcome(o provider.Outcome) provider.Result { return provider.Result{Outcome: o} }

// play applies attempt results in order, as the worker would.
func play(p delivery.Policy, results ...provider.Result) []delivery.Decision {
	var (
		a   delivery.Attempts
		out []delivery.Decision
	)
	for _, r := range results {
		d := p.Decide(a, r, t0)
		out = append(out, d)
		a = d.Attempts
	}
	return out
}

func TestDecide_Sent(t *testing.T) {
	d := policy(0).Decide(delivery.Attempts{Made: 1, Ambiguous: 1}, outcome(provider.Sent), t0)
	if d.Status != delivery.StatusSent || d.Attempts != (delivery.Attempts{Made: 2, Ambiguous: 1}) {
		t.Fatalf("decision = %+v", d)
	}
}

// Every attempt definitely missed the person: the delivery fails, which
// frees the message for a later campaign.
func TestTerminal_AllDefinite_Failed(t *testing.T) {
	r := outcome(provider.NotSentRetryable)
	ds := play(policy(0.5), r, r, r, r, r)
	for i, d := range ds[:4] {
		if d.Status != delivery.StatusRetry || !d.RetryAt.After(t0) {
			t.Fatalf("attempt %d: %+v, want a later retry", i+1, d)
		}
	}
	if last := ds[4]; last.Status != delivery.StatusFailed || last.Attempts != (delivery.Attempts{Made: 5}) {
		t.Fatalf("after 5 definite misses: %+v, want failed", last)
	}
}

func TestTerminal_PermanentRejection_FailsAtOnce(t *testing.T) {
	d := policy(0).Decide(delivery.Attempts{}, outcome(provider.NotSentPermanent), t0)
	if d.Status != delivery.StatusFailed || d.Attempts != (delivery.Attempts{Made: 1}) {
		t.Fatalf("decision = %+v, want failed", d)
	}
}

// One attempt the provider may have accepted makes the end uncertain for
// good: later definite misses cannot turn the delivery into failed.
func TestTerminal_StickyAmbiguity_Unknown(t *testing.T) {
	retryable := outcome(provider.NotSentRetryable)
	ds := play(policy(0.5), outcome(provider.Ambiguous), retryable, retryable, retryable, retryable)
	for i, d := range ds[:4] {
		if d.Status != delivery.StatusRetry {
			t.Fatalf("attempt %d: %+v, want retry", i+1, d)
		}
	}
	if last := ds[4]; last.Status != delivery.StatusUnknown || last.Attempts != (delivery.Attempts{Made: 5, Ambiguous: 1}) {
		t.Fatalf("timeout then four 429s: %+v, want unknown, not failed", last)
	}
	ds = play(policy(0.5), outcome(provider.Ambiguous), outcome(provider.NotSentPermanent))
	if last := ds[1]; last.Status != delivery.StatusUnknown {
		t.Fatalf("timeout then 422: %+v, want unknown", last)
	}
}

// Attempt n waits between 1s and min(60s, 2^(n-1) s); Retry-After raises
// the wait.
func TestBackoff_FullJitterBounds(t *testing.T) {
	r := outcome(provider.NotSentRetryable)
	for made := 0; made < 4; made++ {
		ceil := min(time.Minute, time.Second<<made)
		lo := policy(0).Decide(delivery.Attempts{Made: made}, r, t0).RetryAt.Sub(t0)
		hi := policy(0.999999).Decide(delivery.Attempts{Made: made}, r, t0).RetryAt.Sub(t0)
		if lo != time.Second || hi < ceil-time.Millisecond || hi > ceil {
			t.Errorf("attempt %d: waits %v..%v, want 1s..%v", made+1, lo, hi, ceil)
		}
	}
	long := policy(0.999999)
	long.MaxAttempts = 20
	if w := long.Decide(delivery.Attempts{Made: 10}, r, t0).RetryAt.Sub(t0); w > time.Minute || w < time.Minute-time.Millisecond {
		t.Errorf("capped wait = %v, want 60s", w)
	}
	limited := provider.Result{Outcome: provider.NotSentRetryable, Status: 429, RetryAfter: 30 * time.Second}
	if w := policy(0).Decide(delivery.Attempts{}, limited, t0).RetryAt.Sub(t0); w != 30*time.Second {
		t.Errorf("Retry-After 30s: wait = %v", w)
	}
}
