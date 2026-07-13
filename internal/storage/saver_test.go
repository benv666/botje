package storage

import (
	"errors"
	"testing"
	"time"
)

// saverFixture runs a Saver against a Memory store with a manual
// dispatcher: delivered completions run when the test says so. deliver
// is a channel like the real work queue, so the goroutine handoff is
// race-free.
type saverFixture struct {
	store   Store
	puts    int // PutMany calls that reached the backend
	queue   chan func()
	errs    []error
	failing bool // next PutMany fails
	blocked chan struct{}
	saver   *Saver
}

type fixtureStore struct {
	Store
	f *saverFixture
}

func (s fixtureStore) PutMany(ns string, values map[string]any) error {
	if s.f.blocked != nil {
		<-s.f.blocked
	}
	if s.f.failing {
		s.f.failing = false
		return errors.New("backend down")
	}
	s.f.puts++
	return s.Store.PutMany(ns, values)
}

func newSaverFixture() *saverFixture {
	f := &saverFixture{store: NewMemory(), queue: make(chan func(), 32)}
	f.saver = NewSaver(
		fixtureStore{f.store, f},
		func(fn func()) { f.queue <- fn },
		func(err error) { f.errs = append(f.errs, err) },
	)
	return f
}

// drain waits for the async write to finish and runs the delivered
// completion callbacks on the "dispatcher". The small timeout covers
// the gap between the write finishing and its completion being
// delivered.
func (f *saverFixture) drain(t *testing.T) {
	t.Helper()
	f.saver.wait()
	for {
		select {
		case fn := <-f.queue:
			fn()
		case <-time.After(200 * time.Millisecond):
			return
		}
	}
}

func TestSaverFlushWritesDirty(t *testing.T) {
	f := newSaverFixture()
	f.saver.Mark("markov", "w:beer", func() any { return 3 })
	f.saver.Mark("markov", "w:kaas", func() any { return 1 })
	f.saver.Mark("stats", "benv", func() any { return "lurker" })
	f.saver.Flush()
	f.drain(t)

	var n int
	if ok, _ := f.store.Get("markov", "w:beer", &n); !ok || n != 3 {
		t.Fatalf("markov/w:beer = %v %d", ok, n)
	}
	var s string
	if ok, _ := f.store.Get("stats", "benv", &s); !ok || s != "lurker" {
		t.Fatalf("stats/benv = %v %q", ok, s)
	}
	if f.puts != 2 {
		t.Fatalf("PutMany calls = %d, want 2 (one per namespace)", f.puts)
	}
	// nothing dirty anymore: another flush must not touch the backend
	f.saver.Flush()
	f.drain(t)
	if f.puts != 2 {
		t.Fatalf("clean flush wrote to the backend (%d calls)", f.puts)
	}
}

func TestSaverSerializesAtFlushTime(t *testing.T) {
	f := newSaverFixture()
	v := 1
	f.saver.Mark("ns", "k", func() any { return v })
	f.saver.Mark("ns", "k", func() any { return v }) // coalesces
	v = 2
	f.saver.Flush()
	f.drain(t)
	var got int
	f.store.Get("ns", "k", &got)
	if got != 2 {
		t.Fatalf("stored %d, want the flush-time value 2", got)
	}
	if f.puts != 1 {
		t.Fatalf("PutMany calls = %d, want 1", f.puts)
	}
}

func TestSaverSkipsWhileInflight(t *testing.T) {
	f := newSaverFixture()
	f.blocked = make(chan struct{})
	f.saver.Mark("ns", "a", func() any { return 1 })
	f.saver.Flush() // hangs in the backend
	f.saver.Mark("ns", "b", func() any { return 2 })
	f.saver.Flush() // must not start a second write
	close(f.blocked) // stays closed: later gate checks pass right through
	f.drain(t)
	if f.puts != 1 {
		t.Fatalf("PutMany calls = %d, want 1 (second flush ran while inflight)", f.puts)
	}
	f.saver.Flush() // now b goes out
	f.drain(t)
	var got int
	if ok, _ := f.store.Get("ns", "b", &got); !ok || got != 2 {
		t.Fatalf("ns/b = %v %d after the catch-up flush", ok, got)
	}
}

func TestSaverRequeuesOnFailure(t *testing.T) {
	f := newSaverFixture()
	f.failing = true
	f.saver.Mark("ns", "k", func() any { return 7 })
	f.saver.Flush()
	f.drain(t)
	if len(f.errs) != 1 {
		t.Fatalf("errors reported = %v, want the backend failure", f.errs)
	}
	if ok, _ := f.store.Get("ns", "k", new(int)); ok {
		t.Fatal("value written despite failure")
	}
	f.saver.Flush() // retry from the requeued bytes
	f.drain(t)
	var got int
	if ok, _ := f.store.Get("ns", "k", &got); !ok || got != 7 {
		t.Fatalf("ns/k = %v %d after retry", ok, got)
	}
}

func TestSaverFlushSync(t *testing.T) {
	f := newSaverFixture()
	f.saver.Mark("ns", "k", func() any { return 9 })
	if err := f.saver.FlushSync(); err != nil {
		t.Fatal(err)
	}
	var got int
	if ok, _ := f.store.Get("ns", "k", &got); !ok || got != 9 {
		t.Fatalf("ns/k = %v %d after FlushSync", ok, got)
	}
	if len(f.queue) != 0 {
		t.Fatalf("FlushSync went async (%d queued callbacks)", len(f.queue))
	}
}
