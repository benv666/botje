package cmd

import (
	"slices"
	"testing"

	"go-botje/internal/bus"
)

func ev(msg string) *bus.Event {
	e := &bus.Event{Name: "IRC_PRIVMSG", Channel: "#testing", Msg: msg}
	e.Sender.Nick = "BenV"
	return e
}

func TestRegisteredCommandFires(t *testing.T) {
	r := New()
	var got *Data
	r.Register("karma", "karma", func(d *Data) bool {
		got = d
		return true
	})

	r.Handle(ev("!karma  beer  "))
	if got == nil {
		t.Fatal("handler not called")
	}
	if got.Command != "karma" || got.Data != "beer" {
		t.Errorf("Data = %+v, want command karma, data beer (trimmed)", got)
	}
	if got.Event == nil || got.Event.Sender.Nick != "BenV" {
		t.Errorf("event not passed through: %+v", got.Event)
	}
}

func TestNoDataCommand(t *testing.T) {
	r := New()
	var got *Data
	r.Register("m", "pizza", func(d *Data) bool { got = d; return true })
	r.Handle(ev("!pizza"))
	if got == nil || got.Data != "" {
		t.Fatalf("Data = %+v, want empty data", got)
	}
}

func TestMultiModuleSameWordAllFire(t *testing.T) {
	r := New()
	var order []string
	r.Register("a", "stats", func(*Data) bool { order = append(order, "a"); return true })
	r.Register("b", "stats", func(*Data) bool { order = append(order, "b"); return true })
	r.Handle(ev("!stats"))
	if !slices.Equal(order, []string{"a", "b"}) {
		t.Fatalf("order = %v, want both modules in registration order", order)
	}
}

func TestNonCommandsIgnored(t *testing.T) {
	r := New()
	called := false
	r.Register("m", "karma", func(*Data) bool { called = true; return true })
	r.RegisterDefault("m", 1, false, func(*Data) bool { called = true; return true })
	for _, msg := range []string{"hello there", "! spaced", "!!bang", "", "karma beer", "?karma"} {
		if r.Handle(ev(msg)) {
			t.Errorf("Handle(%q) = true, want ignored", msg)
		}
	}
	if called {
		t.Fatal("handler called for non-command text")
	}
}

func TestDefaultCommandsPriorityAndStop(t *testing.T) {
	r := New()
	var order []string
	r.RegisterDefault("low", 1, false, func(*Data) bool { order = append(order, "low"); return true })
	r.RegisterDefault("high", 9, false, func(*Data) bool { order = append(order, "high"); return false })
	r.RegisterDefault("mid", 5, false, func(d *Data) bool {
		order = append(order, "mid:"+d.Data)
		return true
	})
	suggested := false
	r.Reply = func(*bus.Event, string) { suggested = true }

	r.Handle(ev("!unknowncmd trailing data"))
	// high (9) returns false, mid (5) returns true and stops, low never runs
	want := []string{"high", "mid:unknowncmd trailing data"}
	if !slices.Equal(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	if suggested {
		t.Fatal("suggestion sent although a default handled the command")
	}
}

func TestContinueDefaultsAllRun(t *testing.T) {
	r := New()
	var order []string
	r.RegisterDefault("stop", 5, false, func(*Data) bool { order = append(order, "stop"); return false })
	r.RegisterDefault("c1", 9, true, func(*Data) bool { order = append(order, "c1"); return true })
	r.RegisterDefault("c2", 1, true, func(*Data) bool { order = append(order, "c2"); return true })

	r.Handle(ev("!unknown"))
	// continue=0 first (unhandled), then ALL continue=1 by priority
	want := []string{"stop", "c1", "c2"}
	if !slices.Equal(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

func TestSuggestion(t *testing.T) {
	r := New()
	r.Rand = func(int) int { return 0 } // no annoyance suffix
	r.Register("m", "karma", func(*Data) bool { return true })
	var replyEv *bus.Event
	var reply string
	r.Reply = func(e *bus.Event, msg string) { replyEv, reply = e, msg }

	r.Handle(ev("!krama beer"))
	want := "BenV: Maybe you meant: {W}karma{/}?"
	if reply != want {
		t.Errorf("reply = %q, want %q", reply, want)
	}
	if replyEv == nil || replyEv.Channel != "#testing" {
		t.Errorf("reply event = %+v", replyEv)
	}
}

func TestSuggestionMultipleSortedByDistance(t *testing.T) {
	r := New()
	r.Rand = func(int) int { return 0 }
	for _, w := range []string{"seen", "sean", "scene"} {
		r.Register("m", w, func(*Data) bool { return true })
	}
	var reply string
	r.Reply = func(_ *bus.Event, msg string) { reply = msg }

	r.Handle(ev("!sen"))
	// dist: seen 1, sean 1, scene 2; short word (len<=3) allows dist<=1
	want := "BenV: Maybe you meant any of: {W}sean{/}, {W}seen{/}?"
	if reply != want {
		t.Errorf("reply = %q, want %q", reply, want)
	}
}

func TestSuggestionAnnoyanceSuffix(t *testing.T) {
	r := New()
	r.Rand = func(n int) int { return 1 } // triggers suffix, picks annoy[1]
	r.Register("m", "karma", func(*Data) bool { return true })
	var reply string
	r.Reply = func(_ *bus.Event, msg string) { reply = msg }

	r.Handle(ev("!krama"))
	want := "BenV: Maybe you meant: {W}karma{/}? ;)"
	if reply != want {
		t.Errorf("reply = %q, want %q", reply, want)
	}
}

func TestShortWordDistanceOne(t *testing.T) {
	r := New()
	r.Rand = func(int) int { return 0 }
	r.Register("m", "ego", func(*Data) bool { return true })
	var reply string
	r.Reply = func(_ *bus.Event, msg string) { reply = msg }

	r.Handle(ev("!eg"))
	if reply == "" {
		t.Fatal("no suggestion for !eg -> ego (dist 1)")
	}
	reply = ""
	r.Handle(ev("!gg")) // dist 2 from ego, short word budget is 1: no suggestion
	if reply != "" {
		t.Fatalf("suggestion %q for dist-2 short word, want none", reply)
	}
}

func TestPanickingCommandHandlerRemoved(t *testing.T) {
	r := New()
	calls := 0
	r.Register("crasher", "x", func(*Data) bool { panic("boom") })
	r.Register("healthy", "x", func(*Data) bool { calls++; return true })

	r.Handle(ev("!x")) // must not panic
	if calls != 1 {
		t.Fatal("healthy handler did not run after crasher panicked")
	}
	r.Handle(ev("!x"))
	if calls != 2 {
		t.Fatal("second dispatch broken")
	}
	// crasher was removed: only healthy ran twice, no more panics
}

func TestPanickingDefaultRemoved(t *testing.T) {
	r := New()
	calls := 0
	r.RegisterDefault("crasher", 9, false, func(*Data) bool { panic("boom") })
	r.RegisterDefault("healthy", 1, false, func(*Data) bool { calls++; return true })

	r.Handle(ev("!nocmd"))
	r.Handle(ev("!nocmd"))
	if calls != 2 {
		t.Fatalf("healthy default ran %d times, want 2", calls)
	}
}

func TestUnregisterModule(t *testing.T) {
	r := New()
	called := false
	r.Register("m", "karma", func(*Data) bool { called = true; return true })
	r.RegisterDefault("m", 1, false, func(*Data) bool { called = true; return true })
	r.UnregisterModule("m")
	r.Handle(ev("!karma"))
	if called {
		t.Fatal("handlers survived UnregisterModule")
	}
	if len(r.Commands()) != 0 {
		t.Fatalf("Commands = %v after UnregisterModule", r.Commands())
	}
}

func TestLevenshtein(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"kitten", "sitting", 3},
		{"karma", "krama", 2},
		{"", "abc", 3},
		{"same", "same", 0},
		{"héhé", "hehe", 2},
	} {
		if got := levenshtein(tc.a, tc.b); got != tc.want {
			t.Errorf("levenshtein(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
