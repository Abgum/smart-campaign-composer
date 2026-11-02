// Package delivery takes approved deliveries to the provider: it claims due
// deliveries, makes provider attempts, and decides and records what happens
// after each.
//
// The rule behind "aynı kişiye aynı mesaj iki kez gitmez": a delivery ends
// failed, freeing the message for a later campaign, only when every attempt
// definitely missed the person. Once an attempt may have been accepted
// (ambiguous), the delivery can only end sent or unknown, and unknown keeps
// the message's dedupe slot: not sending is preferred to sending twice.
package delivery

import (
	"math/rand/v2"
	"time"

	"smart-campaign-composer/backend/internal/provider"
)

// Delivery statuses. failed and skipped_no_consent are the only ends that
// free the message for a later campaign.
const (
	StatusPending          = "pending"
	StatusInFlight         = "in_flight"
	StatusRetry            = "retry"
	StatusSent             = "sent"
	StatusFailed           = "failed"
	StatusUnknown          = "unknown"
	StatusSkippedNoConsent = "skipped_no_consent"
)

// Attempts counts a delivery's provider attempts.
type Attempts struct {
	Made      int // attempts made
	Ambiguous int // of which the provider may have accepted
}

// Decision is where a delivery goes after an attempt.
type Decision struct {
	Status   string    // StatusSent, StatusRetry, StatusFailed or StatusUnknown
	Attempts Attempts  // including the attempt just made
	RetryAt  time.Time // when Status is StatusRetry
}

// Policy bounds retries: at most MaxAttempts attempts; after the n-th, a
// full-jitter wait between BaseDelay and min(MaxDelay, BaseDelay·2^(n-1)),
// never shorter than the provider's Retry-After.
type Policy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Rand        func() float64 // in [0, 1); nil means math/rand/v2
}

// DefaultPolicy allows 5 attempts with waits between 1s and 60s.
var DefaultPolicy = Policy{MaxAttempts: 5, BaseDelay: time.Second, MaxDelay: time.Minute}

// Decide applies one attempt's result to the attempts made before it.
func (p Policy) Decide(a Attempts, r provider.Result, now time.Time) Decision {
	a.Made++
	if r.Outcome == provider.Ambiguous {
		a.Ambiguous++
	}
	d := Decision{Attempts: a}
	switch {
	case r.Outcome == provider.Sent:
		d.Status = StatusSent
	case r.Outcome == provider.NotSentPermanent || a.Made >= p.MaxAttempts:
		// Ambiguity is sticky: one attempt that may have been accepted
		// rules out failed.
		d.Status = StatusFailed
		if a.Ambiguous > 0 {
			d.Status = StatusUnknown
		}
	default:
		d.Status = StatusRetry
		d.RetryAt = now.Add(max(p.backoff(a.Made), r.RetryAfter))
	}
	return d
}

// Unreachable decides for a delivery whose recipient can no longer be
// messaged on its channel (consent or contact withdrawn), with no attempt
// made. It ends skipped_no_consent, which frees the message, unless an
// earlier attempt may already have delivered it: ambiguity stays sticky,
// so it ends unknown and the message stays held.
func Unreachable(a Attempts) Decision {
	d := Decision{Status: StatusSkippedNoConsent, Attempts: a}
	if a.Ambiguous > 0 {
		d.Status = StatusUnknown
	}
	return d
}

// backoff is the wait after the n-th attempt.
func (p Policy) backoff(n int) time.Duration {
	ceil := p.BaseDelay
	for i := 1; i < n && ceil < p.MaxDelay; i++ {
		ceil *= 2
	}
	ceil = min(ceil, p.MaxDelay)
	if ceil <= p.BaseDelay {
		return ceil
	}
	draw := rand.Float64
	if p.Rand != nil {
		draw = p.Rand
	}
	return p.BaseDelay + time.Duration(draw()*float64(ceil-p.BaseDelay))
}
