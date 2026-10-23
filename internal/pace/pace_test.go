package pace

import (
	"context"
	"testing"
	"time"
)

func TestBurstThenSpaced(t *testing.T) {
	l := New(50*time.Millisecond, nil)
	start := time.Now()
	for i := 0; i < 5; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// 3 come straight away, the other 2 wait about 50 ms each.
	if d := time.Since(start); d < 80*time.Millisecond || d > 400*time.Millisecond {
		t.Fatalf("5 calls took %v", d)
	}
	if got := l.Status().CallsLastHour; got != 5 {
		t.Fatalf("calls = %d", got)
	}
}

func TestPauseHoldsCallsAndBacksOff(t *testing.T) {
	l := New(time.Millisecond, nil)
	if d := l.PauseFor(0, "x"); d != time.Minute {
		t.Fatalf("first backoff %v", d)
	}
	if d := l.PauseFor(0, "x"); d != 2*time.Minute {
		t.Fatalf("second backoff %v", d)
	}
	if l.Status().State != "paused" {
		t.Fatal("not paused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx); err == nil {
		t.Fatal("call went through during a pause")
	}
	l.Resume()
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.Succeeded()
	if d := l.PauseFor(0, "x"); d != time.Minute {
		t.Fatalf("backoff not reset: %v", d)
	}
}

func TestExplicitPause(t *testing.T) {
	l := New(time.Millisecond, nil)
	l.PauseFor(80*time.Millisecond, "Evernote asked")
	start := time.Now()
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 70*time.Millisecond {
		t.Fatalf("pause too short: %v", d)
	}
	if l.Status().State != "slowing" {
		t.Fatalf("state %q after a pause", l.Status().State)
	}
}
