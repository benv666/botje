package flood

import (
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// drain pulls sendable lines, advancing the clock by the reported waits,
// with a safety cap. Returns lines in send order.
func drain(t *testing.T, clk *fakeClock, q *Queue) []string {
	t.Helper()
	var out []string
	for range 1000 {
		line, wait, ok := q.Next()
		switch {
		case ok:
			out = append(out, line)
		case wait > 0:
			clk.advance(wait)
		default:
			return out
		}
	}
	t.Fatal("drain did not terminate")
	return nil
}

func TestEmptyQueue(t *testing.T) {
	q := New(newFakeClock().now)
	if line, wait, ok := q.Next(); ok || wait != 0 || line != "" {
		t.Fatalf("Next on empty = %q %v %v", line, wait, ok)
	}
}

func TestFirstSendImmediateSecondWaits(t *testing.T) {
	clk := newFakeClock()
	q := New(clk.now)
	q.Push("PRIVMSG #a :one")
	q.Push("PRIVMSG #a :two")

	line, _, ok := q.Next()
	if !ok || line != "PRIVMSG #a :one" {
		t.Fatalf("first Next = %q %v", line, ok)
	}
	line, wait, ok := q.Next()
	if ok {
		t.Fatalf("second Next sent %q immediately, want rate limit", line)
	}
	if wait != time.Second {
		t.Fatalf("second Next wait = %v, want 1s", wait)
	}
	clk.advance(wait)
	if line, _, ok = q.Next(); !ok || line != "PRIVMSG #a :two" {
		t.Fatalf("after wait Next = %q %v", line, ok)
	}
}

func TestRoundRobinAcrossChannels(t *testing.T) {
	clk := newFakeClock()
	q := New(clk.now)
	q.Push("PRIVMSG #a :a1")
	q.Push("PRIVMSG #a :a2")
	q.Push("PRIVMSG #a :a3")
	q.Push("PRIVMSG #b :b1")
	q.Push("PRIVMSG #c :c1")

	got := drain(t, clk, q)
	want := []string{
		"PRIVMSG #a :a1", "PRIVMSG #b :b1", "PRIVMSG #c :c1",
		"PRIVMSG #a :a2", "PRIVMSG #a :a3",
	}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("drain = %q, want %q (busy channel must not starve others)", got, want)
		}
	}
}

func TestHighPriorityBypassesChannels(t *testing.T) {
	clk := newFakeClock()
	q := New(clk.now)
	q.Push("PRIVMSG #a :normal")
	q.PushHigh("PONG :srv")
	q.PushHigh("PONG :srv2")

	got := drain(t, clk, q)
	want := []string{"PONG :srv", "PONG :srv2", "PRIVMSG #a :normal"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("drain = %q, want high prio first: %q", got, want)
		}
	}
}

func TestNonPrivmsgSharesSentinelBucket(t *testing.T) {
	clk := newFakeClock()
	q := New(clk.now)
	q.Push("JOIN #a")
	q.Push("JOIN #b")
	q.Push("PRIVMSG #a :hi")

	got := drain(t, clk, q)
	// both JOINs share one bucket: RR gives JOIN, PRIVMSG, JOIN
	want := []string{"JOIN #a", "PRIVMSG #a :hi", "JOIN #b"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("drain = %q, want %q", got, want)
		}
	}
}

func TestLongLineCountsMultiple(t *testing.T) {
	clk := newFakeClock()
	q := New(clk.now)
	long := "PRIVMSG #a :" + string(make([]byte, 188)) // 200 bytes: 3 extra events
	q.Push(long)
	q.Push("PRIVMSG #a :after")

	if _, _, ok := q.Next(); !ok {
		t.Fatal("long line itself did not send")
	}
	// window now holds 4 events at t0: wait is 4s but capped at 3
	_, wait, ok := q.Next()
	if ok || wait != 3*time.Second {
		t.Fatalf("wait after long line = %v %v, want capped 3s", wait, ok)
	}
	clk.advance(wait)
	_, wait, ok = q.Next()
	if ok || wait != time.Second {
		t.Fatalf("remaining wait = %v %v, want 1s", wait, ok)
	}
	clk.advance(wait)
	if line, _, ok := q.Next(); !ok || line != "PRIVMSG #a :after" {
		t.Fatalf("after full wait = %q %v", line, ok)
	}
}

func TestWaitCappedAtThreeSeconds(t *testing.T) {
	clk := newFakeClock()
	q := New(clk.now)
	huge := "PRIVMSG #a :" + string(make([]byte, 788)) // 800 bytes: 10 extras
	q.Push(huge)
	q.Push("PRIVMSG #a :next")
	q.Next()
	if _, wait, ok := q.Next(); ok || wait != 3*time.Second {
		t.Fatalf("wait = %v %v, want 3s cap however long the backlog", wait, ok)
	}
}

func TestPacingIsOnePerSecond(t *testing.T) {
	clk := newFakeClock()
	q := New(clk.now)
	start := clk.now()
	for i := range 5 {
		q.Push("PRIVMSG #a :" + string(rune('a'+i)))
	}
	got := drain(t, clk, q)
	if len(got) != 5 {
		t.Fatalf("drained %d of 5", len(got))
	}
	if elapsed := clk.now().Sub(start); elapsed != 4*time.Second {
		t.Fatalf("5 sends took %v, want 4s (1 msg/s after the first)", elapsed)
	}
}

func TestLen(t *testing.T) {
	q := New(newFakeClock().now)
	q.Push("PRIVMSG #a :x")
	q.Push("JOIN #b")
	q.PushHigh("PONG :s")
	if got := q.Len(); got != 3 {
		t.Fatalf("Len = %d, want 3", got)
	}
	q.Next()
	if got := q.Len(); got != 2 {
		t.Fatalf("Len after send = %d, want 2", got)
	}
}

func TestDepths(t *testing.T) {
	clk := newFakeClock()
	q := New(clk.now)
	q.Push("PRIVMSG #a :x")
	q.Push("PRIVMSG #a :y")
	q.Push("PRIVMSG #b :z")
	q.Push("MODE #a +b") // generic bucket
	q.PushHigh("PONG :s")
	d := q.Depths()
	if d["#a"] != 2 || d["#b"] != 1 || d[genericBucket] != 1 {
		t.Fatalf("Depths = %v", d)
	}
	q.Next() // high line goes first, buckets untouched
	if total := sumDepths(q.Depths()); total != 4 {
		t.Fatalf("Depths after high send sum to %d, want 4", total)
	}
	clk.advance(2 * time.Second)
	q.Next() // one round-robin bucket pick
	if total := sumDepths(q.Depths()); total != 3 {
		t.Fatalf("Depths after bucket send sum to %d, want 3", total)
	}
}

func sumDepths(d map[string]int) int {
	n := 0
	for _, v := range d {
		n += v
	}
	return n
}
