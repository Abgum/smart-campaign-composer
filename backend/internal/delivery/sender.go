package delivery

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"smart-campaign-composer/backend/internal/clock"
	"smart-campaign-composer/backend/internal/metrics"
	"smart-campaign-composer/backend/internal/provider"
)

// settleTimeout bounds writing down what already happened.
const settleTimeout = 10 * time.Second

// Provider sends one message; *provider.Client is one.
type Provider interface {
	Send(ctx context.Context, m provider.Message) provider.Result
}

// Sender makes provider attempts for claimed deliveries.
type Sender struct {
	Store    *Store
	Provider Provider
	Clock    clock.Clock
	// Gate, if set, is waited on right before each call (the worker's
	// shared limit and 429 pause); deadline is the latest start the lease
	// allows. It returns startBy, the latest time the call may start for its
	// grant to count: a call starting later is not made, and Gate is asked
	// again. If Gate fails, the delivery is given back without a call, due
	// again when the error says (ResumeAt), or now.
	Gate func(ctx context.Context, channel string, deadline time.Time) (startBy time.Time, err error)
	// CallBudget is the lease a call must have left when it starts.
	CallBudget time.Duration
	Metrics    *metrics.Metrics // may be nil
}

// ErrLeaseShort stops an attempt whose call could no longer start with
// CallBudget left on its lease, as something before the call was slow. The
// delivery is given back without a call, due now.
var ErrLeaseShort = errors.New("too little lease left to start the call")

// resumer is a gate error that knows when the delivery can be tried again.
type resumer interface{ ResumeAt() time.Time }

// Attempt makes one attempt for a claimed delivery:
//
//  1. consent check 2: the person's consent and contact are read again (they
//     may have changed since approval); if the person can no longer be
//     messaged on the channel, the delivery is skipped and no call is made;
//  2. the gate is waited on; then, as the last thing before the call, the
//     lease must still leave CallBudget (else the delivery is given back)
//     and the grant must still be fresh (else the gate is asked again);
//  3. the stored rendered content is sent, exactly as approved, with the
//     delivery id as idempotency key, and the outcome is recorded.
//
// ctx bounds steps 1 and 2: if it ends there, or the check fails, the
// delivery is given back as it was, since no call was made. Once the call
// has started it runs to the client's timeout and its outcome is recorded
// even if ctx has ended.
func (s *Sender) Attempt(ctx context.Context, c Claimed) (Decision, error) {
	to, ok, err := s.Store.contact(ctx, c.UserID, c.Channel)
	if err != nil {
		return Decision{}, s.giveBack(ctx, c, s.Clock.Now(), err)
	}
	if !ok {
		sctx, cancel := detached(ctx)
		defer cancel()
		return s.Store.skip(sctx, c.Lease, s.Clock.Now())
	}
	callBy := c.LockedUntil.Add(-s.CallBudget)
	startBy := callBy
	asked := s.Clock.Now()
	var now time.Time
	for {
		if s.Gate != nil {
			sb, err := s.Gate(ctx, c.Channel, callBy)
			if err != nil {
				at := s.Clock.Now()
				var r resumer
				if errors.As(err, &r) {
					at = r.ResumeAt()
				}
				return Decision{}, s.giveBack(ctx, c, at, err)
			}
			startBy = sb
		}
		// The last checks before the call; nothing else stands between them
		// and Provider.Send. A grant that came back at once still finds the
		// lease spent if what came before was slow.
		now = s.Clock.Now()
		if now.After(callBy) {
			return Decision{}, s.giveBack(ctx, c, now, ErrLeaseShort)
		}
		if !now.After(startBy) {
			break
		}
		// The grant came back too late for the call to start within the
		// margin of it; its slot is spent, so ask for another.
		slog.Debug("grant came back too late; asking again", "delivery_id", c.ID)
	}
	r := s.Provider.Send(context.WithoutCancel(ctx), provider.Message{
		IdempotencyKey: c.ID, Channel: c.Channel, To: to, Subject: c.Subject, Body: c.Body,
	})
	done := s.Clock.Now()
	s.Metrics.RateLimitWait(c.Channel, now.Sub(asked))
	s.Metrics.ProviderAttempt(c.Channel, string(r.Outcome), done.Sub(now))
	sctx, cancel := detached(ctx)
	defer cancel()
	return s.Store.Record(sctx, c.Lease, r, done)
}

// giveBack returns a delivery that err stopped before its call to the
// queue, due at at; it returns err, joined with a failed give-back.
func (s *Sender) giveBack(ctx context.Context, c Claimed, at time.Time, err error) error {
	s.Metrics.GivenBack(c.Channel, givenBackReason(err), 1)
	sctx, cancel := detached(ctx)
	defer cancel()
	if rerr := s.Store.Release(sctx, at, c.Lease); rerr != nil {
		return errors.Join(err, rerr)
	}
	return err
}

// givenBackReason names why an attempt stopped before its call.
func givenBackReason(err error) string {
	var (
		p *PausedError
		b *BusyError
	)
	switch {
	case errors.As(err, &p):
		return "paused"
	case errors.As(err, &b):
		return "busy"
	case errors.Is(err, ErrLeaseShort):
		return "lease_short"
	case errors.Is(err, context.Canceled):
		return "stopped"
	}
	return "error"
}

// detached is a context for writing down what already happened: it
// outlives ctx, but not a stuck database.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
}
