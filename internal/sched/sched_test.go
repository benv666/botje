package sched

import (
	"testing"
	"time"
)

// fakeClock is a manually advanced clock for deterministic tests.
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestAfterDoesNotFireBeforeDue(t *testing.T) {
	clk := newFakeClock()
	s := New(clk.now)
	fired := false
	s.After(5*time.Second, func() { fired = true })

	clk.advance(4 * time.Second)
	if n := s.RunDue(); n != 0 {
		t.Fatalf("RunDue fired %d timers before due time", n)
	}
	if fired {
		t.Fatal("callback ran before due time")
	}
}

func TestAfterFiresWhenDue(t *testing.T) {
	clk := newFakeClock()
	s := New(clk.now)
	fired := false
	s.After(5*time.Second, func() { fired = true })

	clk.advance(5 * time.Second)
	if n := s.RunDue(); n != 1 {
		t.Fatalf("RunDue = %d, want 1", n)
	}
	if !fired {
		t.Fatal("callback did not run at due time")
	}
	// one-shot: does not fire again
	clk.advance(time.Hour)
	if n := s.RunDue(); n != 0 {
		t.Fatalf("timer fired again, RunDue = %d", n)
	}
}

func TestFiresInTimeOrder(t *testing.T) {
	clk := newFakeClock()
	s := New(clk.now)
	var order []int
	s.After(3*time.Second, func() { order = append(order, 3) })
	s.After(1*time.Second, func() { order = append(order, 1) })
	s.After(2*time.Second, func() { order = append(order, 2) })

	clk.advance(10 * time.Second)
	if n := s.RunDue(); n != 3 {
		t.Fatalf("RunDue = %d, want 3", n)
	}
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("fire order = %v, want [1 2 3]", order)
	}
}

func TestSameTimeFiresFIFO(t *testing.T) {
	clk := newFakeClock()
	s := New(clk.now)
	var order []int
	for i := 1; i <= 5; i++ {
		i := i
		s.After(time.Second, func() { order = append(order, i) })
	}
	clk.advance(time.Second)
	s.RunDue()
	for i, v := range order {
		if v != i+1 {
			t.Fatalf("same-time fire order = %v, want FIFO", order)
		}
	}
	if len(order) != 5 {
		t.Fatalf("fired %d of 5 same-time timers", len(order))
	}
}

func TestUnschedule(t *testing.T) {
	clk := newFakeClock()
	s := New(clk.now)
	fired := false
	tag := s.After(time.Second, func() { fired = true })

	if !s.Unschedule(tag) {
		t.Fatal("Unschedule of pending timer returned false")
	}
	if s.Unschedule(tag) {
		t.Fatal("second Unschedule of same tag returned true")
	}
	clk.advance(time.Minute)
	s.RunDue()
	if fired {
		t.Fatal("unscheduled timer fired")
	}
}

func TestScheduleAbsolute(t *testing.T) {
	clk := newFakeClock()
	s := New(clk.now)
	fired := false
	s.Schedule(clk.now().Add(30*time.Second), func() { fired = true })

	clk.advance(29 * time.Second)
	s.RunDue()
	if fired {
		t.Fatal("fired early")
	}
	clk.advance(time.Second)
	s.RunDue()
	if !fired {
		t.Fatal("did not fire at absolute time")
	}
}

func TestNextIn(t *testing.T) {
	clk := newFakeClock()
	s := New(clk.now)

	if _, ok := s.NextIn(); ok {
		t.Fatal("NextIn on empty scheduler returned ok")
	}
	s.After(10*time.Second, func() {})
	d, ok := s.NextIn()
	if !ok || d != 10*time.Second {
		t.Fatalf("NextIn = %v,%v, want 10s,true", d, ok)
	}
	// overdue timers clamp to 0, never negative
	clk.advance(time.Minute)
	d, ok = s.NextIn()
	if !ok || d != 0 {
		t.Fatalf("NextIn overdue = %v,%v, want 0,true", d, ok)
	}
}

func TestPanicIsolation(t *testing.T) {
	clk := newFakeClock()
	s := New(clk.now)
	fired := false
	s.After(time.Second, func() { panic("boom") })
	s.After(time.Second, func() { fired = true })

	clk.advance(time.Second)
	n := s.RunDue() // must not panic
	if n != 2 {
		t.Fatalf("RunDue = %d, want 2 (panicking timer still counts as fired)", n)
	}
	if !fired {
		t.Fatal("timer after panicking timer did not fire")
	}
}

func TestCallbackSchedulingDueTimerWaitsForNextPass(t *testing.T) {
	clk := newFakeClock()
	s := New(clk.now)
	rescheduled := false
	s.After(0, func() {
		s.After(0, func() { rescheduled = true })
	})

	if n := s.RunDue(); n != 1 {
		t.Fatalf("first RunDue = %d, want 1 (new due timer must wait)", n)
	}
	if rescheduled {
		t.Fatal("timer scheduled during RunDue fired in same pass")
	}
	if n := s.RunDue(); n != 1 {
		t.Fatalf("second RunDue = %d, want 1", n)
	}
	if !rescheduled {
		t.Fatal("timer scheduled during RunDue never fired")
	}
}

func TestLen(t *testing.T) {
	clk := newFakeClock()
	s := New(clk.now)
	if s.Len() != 0 {
		t.Fatal("new scheduler not empty")
	}
	tag := s.After(time.Second, func() {})
	s.After(2*time.Second, func() {})
	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2", s.Len())
	}
	s.Unschedule(tag)
	if s.Len() != 1 {
		t.Fatalf("Len after Unschedule = %d, want 1", s.Len())
	}
}
