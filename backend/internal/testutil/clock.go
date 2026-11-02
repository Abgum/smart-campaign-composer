package testutil

import (
	"sync"
	"testing"
	"time"
)

// FakeClock is a manually advanced clock.Clock.
type FakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []fakeTimer
}

type fakeTimer struct {
	at time.Time
	ch chan time.Time
}

// NewFakeClock returns a clock frozen at t.
func NewFakeClock(t time.Time) *FakeClock { return &FakeClock{now: t} }

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *FakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.timers = append(c.timers, fakeTimer{at: c.now.Add(d), ch: ch})
	return ch
}

// Advance moves time forward and fires every timer that became due.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	pending := c.timers[:0]
	for _, t := range c.timers {
		if !t.at.After(c.now) {
			t.ch <- c.now
			continue
		}
		pending = append(pending, t)
	}
	c.timers = pending
}

// Waiters returns the number of timers not yet fired.
func (c *FakeClock) Waiters() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// WaitForWaiters blocks (in real time, up to 2s) until at least n timers are
// pending, so a test can advance the clock only after goroutines are parked.
func (c *FakeClock) WaitForWaiters(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for c.Waiters() < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d clock waiters (have %d)", n, c.Waiters())
		}
		time.Sleep(time.Millisecond)
	}
}
