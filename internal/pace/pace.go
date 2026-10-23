// Package pace spaces out calls to Evernote and pauses everything when
// Evernote says the app is going too fast.
package pace

import (
	"context"
	"sync"
	"time"
)

// Status is what the window shows in its footer and pause banner.
type Status struct {
	State         string    `json:"state"` // "steady", "slowing" or "paused"
	PausedUntil   time.Time `json:"pausedUntil,omitzero"`
	Reason        string    `json:"reason,omitempty"`
	CallsLastHour int       `json:"callsLastHour"`
	IntervalMs    int64     `json:"intervalMs"`
	Pauses        int       `json:"pauses"` // pauses since the app started
}

// Limiter lets one call through every Interval, with a small burst, and
// holds every call while paused.
type Limiter struct {
	mu          sync.Mutex
	interval    time.Duration
	burst       int
	tokens      float64
	last        time.Time
	pausedUntil time.Time
	reason      string
	backoff     time.Duration
	calls       []time.Time
	pauses      int
	slowUntil   time.Time
	wake        chan struct{}
	onChange    func(Status)
	now         func() time.Time
}

// New makes a limiter. onChange (may be nil) is called whenever the status
// changes, for example to save the pause and update the window.
func New(interval time.Duration, onChange func(Status)) *Limiter {
	return &Limiter{interval: interval, burst: 3, tokens: 3, onChange: onChange,
		wake: make(chan struct{}), now: time.Now}
}

// SetInterval changes the spacing between calls.
func (l *Limiter) SetInterval(d time.Duration) {
	l.mu.Lock()
	l.interval = max(d, 100*time.Millisecond)
	l.mu.Unlock()
	l.changed()
}

// Restore re-applies a pause saved before the app was closed.
func (l *Limiter) Restore(until time.Time, reason string) {
	if until.After(l.now()) {
		l.mu.Lock()
		l.pausedUntil, l.reason = until, reason
		l.mu.Unlock()
		l.changed()
	}
}

// Wait blocks until a call may be made.
func (l *Limiter) Wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		now := l.now()
		var wait time.Duration
		if now.Before(l.pausedUntil) {
			wait = l.pausedUntil.Sub(now)
		} else {
			if !l.pausedUntil.IsZero() {
				l.pausedUntil, l.reason = time.Time{}, ""
				defer l.changed()
			}
			if !l.last.IsZero() && l.interval > 0 {
				l.tokens += float64(now.Sub(l.last)) / float64(l.interval)
			}
			l.last = now
			if l.tokens > float64(l.burst) {
				l.tokens = float64(l.burst)
			}
			if l.tokens >= 1 {
				l.tokens--
				l.calls = append(l.calls, now)
				l.trim(now)
				l.mu.Unlock()
				return nil
			}
			wait = time.Duration((1 - l.tokens) * float64(l.interval))
		}
		wake := l.wake
		l.mu.Unlock()
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		case <-wake:
			t.Stop()
		}
	}
}

// PauseFor holds every call for d. When d is 0 (Evernote didn't say how
// long), it backs off 1, 2, 4… minutes, up to an hour.
func (l *Limiter) PauseFor(d time.Duration, reason string) time.Duration {
	l.mu.Lock()
	if d <= 0 {
		if l.backoff == 0 {
			l.backoff = time.Minute
		} else {
			l.backoff = min(l.backoff*2, time.Hour)
		}
		d = l.backoff
	}
	until := l.now().Add(d)
	if until.After(l.pausedUntil) {
		l.pausedUntil = until
	}
	l.reason = reason
	l.pauses++
	l.slowUntil = until.Add(10 * time.Minute)
	l.mu.Unlock()
	l.changed()
	return d
}

// Resume ends a pause early (the "Try now" button).
func (l *Limiter) Resume() {
	l.mu.Lock()
	l.pausedUntil, l.reason = time.Time{}, ""
	close(l.wake)
	l.wake = make(chan struct{})
	l.mu.Unlock()
	l.changed()
}

// Succeeded tells the limiter a call went through, which resets the backoff.
func (l *Limiter) Succeeded() {
	l.mu.Lock()
	l.backoff = 0
	l.mu.Unlock()
}

// Status reports the current state.
func (l *Limiter) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.trim(now)
	s := Status{State: "steady", CallsLastHour: len(l.calls), IntervalMs: l.interval.Milliseconds(), Pauses: l.pauses}
	switch {
	case now.Before(l.pausedUntil):
		s.State, s.PausedUntil, s.Reason = "paused", l.pausedUntil, l.reason
	case now.Before(l.slowUntil):
		s.State = "slowing"
	}
	return s
}

func (l *Limiter) trim(now time.Time) {
	cut := now.Add(-time.Hour)
	i := 0
	for i < len(l.calls) && l.calls[i].Before(cut) {
		i++
	}
	l.calls = l.calls[i:]
}

func (l *Limiter) changed() {
	if l.onChange != nil {
		l.onChange(l.Status())
	}
}
