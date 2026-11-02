// Package clock abstracts time so time-dependent code can be tested with a
// fake clock.
package clock

import "time"

// Clock is the time source used by rate limiting, retries and leases.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// Real is the wall clock.
type Real struct{}

func (Real) Now() time.Time                         { return time.Now() }
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }
