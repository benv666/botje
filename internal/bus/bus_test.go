package bus

import (
	"slices"
	"sync"
	"testing"
	"time"
)

func newTestBus(t *testing.T) *Bus {
	t.Helper()
	b := New()
	b.RegisterEvent("PRIVMSG")
	b.RegisterEvent("JOIN")
	return b
}

func TestHookReceivesEvent(t *testing.T) {
	b := newTestBus(t)
	var got *Event
	if err := b.RegisterHook("karma", "PRIVMSG", func(ev *Event) (Handled, any) {
		got = ev
		return None, nil
	}); err != nil {
		t.Fatal(err)
	}

	ev := &Event{Name: "PRIVMSG", Channel: "#testing", Msg: "hello"}
	ev.Sender.Nick = "BenV"
	b.Submit(ev)

	if got == nil {
		t.Fatal("hook not called")
	}
	if got.Channel != "#testing" || got.Msg != "hello" || got.Sender.Nick != "BenV" {
		t.Fatalf("hook got %+v", got)
	}
}

func TestRegisterHookUndeclaredEvent(t *testing.T) {
	b := newTestBus(t)
	if err := b.RegisterHook("karma", "NOSUCH", func(*Event) (Handled, any) { return None, nil }); err == nil {
		t.Fatal("RegisterHook on undeclared event did not error")
	}
}

func TestOneHookPerModuleEventReplaces(t *testing.T) {
	b := newTestBus(t)
	calls := []string{}
	b.RegisterHook("karma", "PRIVMSG", func(*Event) (Handled, any) {
		calls = append(calls, "old")
		return None, nil
	})
	b.RegisterHook("karma", "PRIVMSG", func(*Event) (Handled, any) {
		calls = append(calls, "new")
		return None, nil
	})
	b.Submit(&Event{Name: "PRIVMSG"})
	if len(calls) != 1 || calls[0] != "new" {
		t.Fatalf("calls = %v, want just [new]: one hook per (module,event)", calls)
	}
}

func TestAllModulesFireInRegistrationOrder(t *testing.T) {
	b := newTestBus(t)
	var order []string
	for _, m := range []string{"karma", "markov", "lastseen"} {
		b.RegisterHook(m, "PRIVMSG", func(*Event) (Handled, any) {
			order = append(order, m)
			return None, nil
		})
	}
	b.Submit(&Event{Name: "PRIVMSG"})
	if len(order) != 3 || order[0] != "karma" || order[1] != "markov" || order[2] != "lastseen" {
		t.Fatalf("order = %v, want registration order", order)
	}
}

func TestReturnValuesCollected(t *testing.T) {
	b := newTestBus(t)
	b.RegisterHook("a", "PRIVMSG", func(*Event) (Handled, any) { return Replied, 42 })
	b.RegisterHook("b", "PRIVMSG", func(*Event) (Handled, any) { return None, nil })
	b.RegisterHook("c", "PRIVMSG", func(*Event) (Handled, any) { return Replied, "spec" })

	got := b.Submit(&Event{Name: "PRIVMSG"})
	if len(got) != 2 || got[0] != 42 || got[1] != "spec" {
		t.Fatalf("collected = %v, want [42 spec]", got)
	}
}

func TestStopPropagation(t *testing.T) {
	b := newTestBus(t)
	called := []string{}
	b.RegisterHook("first", "PRIVMSG", func(*Event) (Handled, any) {
		called = append(called, "first")
		return Stop, nil
	})
	b.RegisterHook("second", "PRIVMSG", func(*Event) (Handled, any) {
		called = append(called, "second")
		return None, nil
	})
	b.Submit(&Event{Name: "PRIVMSG"})
	if len(called) != 1 || called[0] != "first" {
		t.Fatalf("called = %v, want [first]: Stop must halt propagation", called)
	}
}

func TestPanicIsolatesAndUnloadsModule(t *testing.T) {
	b := newTestBus(t)
	var panicked struct {
		module string
		event  string
	}
	b.OnPanic = func(module, event string, v any) {
		panicked.module, panicked.event = module, event
	}
	survived := false
	b.RegisterHook("crasher", "PRIVMSG", func(*Event) (Handled, any) { panic("boom") })
	b.RegisterHook("healthy", "PRIVMSG", func(*Event) (Handled, any) {
		survived = true
		return None, nil
	})

	b.Submit(&Event{Name: "PRIVMSG"}) // must not panic
	if !survived {
		t.Fatal("healthy module's hook did not run after crasher panicked")
	}
	if panicked.module != "crasher" || panicked.event != "PRIVMSG" {
		t.Fatalf("OnPanic got %+v", panicked)
	}

	// crashing module is force-unloaded: its hooks are gone
	if got := b.Modules(); slices.Contains(got, "crasher") {
		t.Fatalf("crasher still registered: %v", got)
	}
	panicked.module = ""
	b.Submit(&Event{Name: "PRIVMSG"})
	if panicked.module != "" {
		t.Fatal("crasher hook ran again after force-unload")
	}
}

func TestCallchainRefusesReentry(t *testing.T) {
	b := newTestBus(t)
	calls := 0
	b.RegisterHook("looper", "PRIVMSG", func(ev *Event) (Handled, any) {
		calls++
		b.Submit(&Event{Name: "PRIVMSG"}) // would recurse forever without the guard
		return None, nil
	})
	b.Submit(&Event{Name: "PRIVMSG"})
	if calls != 1 {
		t.Fatalf("looper ran %d times, want 1 (callchain must refuse re-entry)", calls)
	}
}

func TestCallchainAllowsOtherModulesOnReentry(t *testing.T) {
	b := newTestBus(t)
	aCalls, bCalls := 0, 0
	b.RegisterHook("a", "PRIVMSG", func(ev *Event) (Handled, any) {
		aCalls++
		if aCalls == 1 {
			b.Submit(&Event{Name: "PRIVMSG"})
		}
		return None, nil
	})
	b.RegisterHook("b", "PRIVMSG", func(*Event) (Handled, any) {
		bCalls++
		return None, nil
	})
	b.Submit(&Event{Name: "PRIVMSG"})
	// a runs once (re-entry refused), b runs twice (outer + a's inner submit)
	if aCalls != 1 || bCalls != 2 {
		t.Fatalf("aCalls = %d bCalls = %d, want 1 and 2", aCalls, bCalls)
	}
}

func TestUnregisterModule(t *testing.T) {
	b := newTestBus(t)
	called := false
	b.RegisterHook("karma", "PRIVMSG", func(*Event) (Handled, any) {
		called = true
		return None, nil
	})
	b.UnregisterModule("karma")
	b.Submit(&Event{Name: "PRIVMSG"})
	if called {
		t.Fatal("hook ran after UnregisterModule")
	}
	if slices.Contains(b.Modules(), "karma") {
		t.Fatal("module still listed after UnregisterModule")
	}
}

func TestCallStats(t *testing.T) {
	b := newTestBus(t)
	b.RegisterHook("karma", "PRIVMSG", func(*Event) (Handled, any) { return None, nil })
	for range 3 {
		b.Submit(&Event{Name: "PRIVMSG"})
	}
	st := b.Stats()
	cs, ok := st[HookID{Module: "karma", Event: "PRIVMSG"}]
	if !ok {
		t.Fatalf("no stats for karma/PRIVMSG: %v", st)
	}
	if cs.Count != 3 {
		t.Fatalf("Count = %d, want 3", cs.Count)
	}
	if cs.Min < 0 || cs.Min > cs.Max || cs.Total < cs.Max {
		t.Fatalf("stats not coherent: %+v", cs)
	}
}

func TestRunDispatchesPublishedEvents(t *testing.T) {
	b := newTestBus(t)
	var mu sync.Mutex
	got := 0
	done := make(chan struct{})
	b.RegisterHook("counter", "PRIVMSG", func(*Event) (Handled, any) {
		mu.Lock()
		got++
		if got == 100 {
			close(done)
		}
		mu.Unlock()
		return None, nil
	})

	go b.Run(t.Context())

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			for range 10 {
				b.Publish(&Event{Name: "PRIVMSG"})
			}
		})
	}
	wg.Wait()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("dispatched %d of 100 published events", got)
	}
}
