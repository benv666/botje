package storage

import (
	"encoding/json"
	"fmt"
	"sync"
)

// Saver batches writes for modules with hot data (markov learns on
// every channel line): mark (ns, name) dirty with a value func, and a
// periodic Flush serializes every dirty entry and writes one PutMany
// per namespace in a goroutine, off the dispatcher. This replaces the
// hand-rolled save cadences (the markov every-51st-line whole-blob
// rewrite blocked the dispatcher for seconds).
//
// Threading: Mark, Flush, and FlushSync run on the dispatcher, so the
// value funcs can read module data without locks; serialization also
// happens there, only the database round trip leaves it. Completions
// re-enter through deliver. Failed writes are reported and requeued as
// their already-serialized bytes for the next flush.
type Saver struct {
	store   Store
	deliver func(fn func()) // re-enter the dispatcher
	report  func(err error)

	dirty    map[savedKey]func() any
	inflight bool
	wg       sync.WaitGroup // tracks the async write, for FlushSync/tests
}

type savedKey struct{ ns, name string }

// NewSaver returns a Saver writing to store. deliver hands completion
// callbacks back to the dispatcher; report receives write errors (both
// must be non-nil).
func NewSaver(store Store, deliver func(fn func()), report func(err error)) *Saver {
	return &Saver{
		store:   store,
		deliver: deliver,
		report:  report,
		dirty:   make(map[savedKey]func() any),
	}
}

// Mark flags (ns, name) dirty. value is called at flush time and its
// result marshaled; marking the same key again replaces the func.
func (s *Saver) Mark(ns, name string, value func() any) {
	s.dirty[savedKey{ns, name}] = value
}

// Dirty reports the number of dirty entries (metrics food).
func (s *Saver) Dirty() int { return len(s.dirty) }

// Flush serializes the dirty set and writes it asynchronously. While a
// write is in flight further flushes are no-ops; newly marked entries
// go out on the next one.
func (s *Saver) Flush() {
	if s.inflight || len(s.dirty) == 0 {
		return
	}
	batches, raws := s.serialize()
	s.inflight = true
	s.wg.Add(1)
	go func() {
		failed := s.write(batches)
		s.wg.Done()
		s.deliver(func() {
			s.inflight = false
			// requeue what did not land, unless freshly re-marked
			for k, raw := range raws {
				if _, isFailed := failed[k.ns]; !isFailed {
					continue
				}
				if _, remarked := s.dirty[k]; !remarked {
					r := raw
					s.dirty[k] = func() any { return r }
				}
			}
		})
	}()
}

// FlushSync serializes and writes the dirty set on the calling
// goroutine: the shutdown path. It first waits out any in-flight
// async write.
func (s *Saver) FlushSync() error {
	s.wg.Wait()
	if len(s.dirty) == 0 {
		return nil
	}
	batches, _ := s.serialize()
	s.inflight = false // any completion callback still queued is stale
	if failed := s.write(batches); len(failed) > 0 {
		return fmt.Errorf("storage: final flush: %d namespace(s) failed", len(failed))
	}
	return nil
}

// serialize marshals every dirty entry and clears the dirty set.
// Values that refuse to marshal are reported and dropped (retrying
// cannot fix them).
func (s *Saver) serialize() (map[string]map[string]any, map[savedKey]json.RawMessage) {
	batches := make(map[string]map[string]any)
	raws := make(map[savedKey]json.RawMessage)
	for k, value := range s.dirty {
		raw, err := json.Marshal(value())
		if err != nil {
			s.report(fmt.Errorf("storage: marshal %s/%s: %w", k.ns, k.name, err))
			continue
		}
		if batches[k.ns] == nil {
			batches[k.ns] = make(map[string]any)
		}
		batches[k.ns][k.name] = json.RawMessage(raw)
		raws[k] = raw
	}
	s.dirty = make(map[savedKey]func() any)
	return batches, raws
}

// write pushes the batches to the store and returns the namespaces
// that failed. Runs off the dispatcher; report must be goroutine-safe
// (slog is).
func (s *Saver) write(batches map[string]map[string]any) map[string]bool {
	failed := make(map[string]bool)
	for ns, values := range batches {
		if err := s.store.PutMany(ns, values); err != nil {
			s.report(fmt.Errorf("storage: flush %s: %w", ns, err))
			failed[ns] = true
		}
	}
	return failed
}

// wait blocks until no async write is in flight; test helper.
func (s *Saver) wait() { s.wg.Wait() }
