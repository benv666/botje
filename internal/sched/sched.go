// Package sched is a one-shot timer heap for the dispatcher loop, the Go
// counterpart of the Perl Select.pm scheduler. Not goroutine-safe by design:
// the dispatcher goroutine owns it, same single-threaded property as the
// Perl select loop.
package sched

import (
	"container/heap"
	"log/slog"
	"time"
)

// Tag identifies a scheduled timer, for Unschedule.
type Tag uint64

type timer struct {
	at  time.Time
	seq Tag // creation order, doubles as the tag and the FIFO tiebreak
	fn  func()
}

type timerHeap []*timer

func (h timerHeap) Len() int { return len(h) }
func (h timerHeap) Less(i, j int) bool {
	if h[i].at.Equal(h[j].at) {
		return h[i].seq < h[j].seq
	}
	return h[i].at.Before(h[j].at)
}
func (h timerHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *timerHeap) Push(x any)   { *h = append(*h, x.(*timer)) }
func (h *timerHeap) Pop() any {
	old := *h
	n := len(old)
	t := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return t
}

// Sched holds pending one-shot timers ordered by due time.
type Sched struct {
	now     func() time.Time
	heap    timerHeap
	pending map[Tag]*timer
	seq     Tag
}

// New returns a scheduler that reads time from now (inject a fake in tests).
func New(now func() time.Time) *Sched {
	return &Sched{now: now, pending: make(map[Tag]*timer)}
}

// After schedules fn to run d from now. Returns a tag for Unschedule.
func (s *Sched) After(d time.Duration, fn func()) Tag {
	return s.Schedule(s.now().Add(d), fn)
}

// Schedule schedules fn to run at the absolute time at.
func (s *Sched) Schedule(at time.Time, fn func()) Tag {
	s.seq++
	t := &timer{at: at, seq: s.seq, fn: fn}
	heap.Push(&s.heap, t)
	s.pending[t.seq] = t
	return t.seq
}

// Unschedule cancels a pending timer. Returns false if it already fired
// or was already cancelled.
func (s *Sched) Unschedule(tag Tag) bool {
	t, ok := s.pending[tag]
	if !ok {
		return false
	}
	delete(s.pending, tag)
	t.fn = nil // cancelled; popped and skipped whenever it surfaces
	return true
}

// NextIn returns the time until the earliest pending timer (clamped to 0
// if overdue), or false if no timers are pending. This is the select
// timeout in the dispatcher loop.
func (s *Sched) NextIn() (time.Duration, bool) {
	s.dropCancelled()
	if len(s.heap) == 0 {
		return 0, false
	}
	return max(s.heap[0].at.Sub(s.now()), 0), true
}

// RunDue fires every timer due at the time of the call, in due-time order
// (FIFO for equal times), each panic-isolated. Timers scheduled by a firing
// callback wait for the next pass. Returns the number fired.
func (s *Sched) RunDue() int {
	now := s.now()
	var due []*timer
	for len(s.heap) > 0 && !s.heap[0].at.After(now) {
		t := heap.Pop(&s.heap).(*timer)
		if t.fn == nil {
			continue // cancelled
		}
		delete(s.pending, t.seq)
		due = append(due, t)
	}
	for _, t := range due {
		s.fire(t)
	}
	return len(due)
}

func (s *Sched) fire(t *timer) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("sched: timer callback panicked", "tag", t.seq, "panic", r)
		}
	}()
	t.fn()
}

// Len reports the number of pending timers.
func (s *Sched) Len() int { return len(s.pending) }

// dropCancelled pops cancelled timers off the top so NextIn never reports
// a deadline for a timer that will not fire.
func (s *Sched) dropCancelled() {
	for len(s.heap) > 0 && s.heap[0].fn == nil {
		heap.Pop(&s.heap)
	}
}
