package ego

import (
	"fmt"
	"strings"
	"testing"

	"go-botje/internal/bus"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
	"go-botje/internal/storage"
)

type fixture struct {
	m     *Module
	b     *bus.Bus
	cmds  *cmd.Registry
	store storage.Store
	sent  []string
}

func newFixture(t *testing.T, store storage.Store) *fixture {
	t.Helper()
	f := &fixture{store: store}
	f.b = bus.New()
	f.b.RegisterEvent("IRC_PRIVMSG")
	f.cmds = cmd.New()
	f.m = New()
	err := f.m.Load(&module.Context{
		Bus: f.b, Cmd: f.cmds, Store: store,
		Privmsg: func(ch, msg string) { f.sent = append(f.sent, ch+"|"+msg) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) msg(nick, text string) {
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: "#testing",
		Msg: text}
	ev.Sender.Nick = nick
	f.b.Submit(ev)
	f.cmds.Handle(ev)
}

func TestEgoCountingAndQuery(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "I think my code is fine")     // i, my = 2
	f.msg("BenV", "ik vind mijn code wel prima") // ik, mijn = 2
	f.msg("BenV", "nothing here")                // 0, but sentence counts
	f.msg("Someone", "!ego benv")

	want := "#testing|Channel ego for {m}benv{/}: {r}4{/}x in {r}3{/} messages, ratio:{g}133.3{/}%. " +
		"Global ego: {r}4{/}x in {r}3{/} messages, ratio:{g}133.3{/}%"
	if len(f.sent) != 1 || f.sent[0] != want {
		t.Fatalf("sent = %q\nwant %q", f.sent, want)
	}
}

func TestWordBoundaries(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "mine is item mining") // mine=1; "item"/"mining" don't count
	f.msg("Someone", "!ego benv")
	if !strings.Contains(f.sent[0], "{r}1{/}x in {r}1{/}") {
		t.Fatalf("sent = %q, want 1 hit", f.sent[0])
	}
}

func TestNoData(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("Someone", "!ego ghost")
	want := "#testing|No data found for [{m}ghost{/}]"
	if len(f.sent) != 1 || f.sent[0] != want {
		t.Fatalf("sent = %q, want %q", f.sent, want)
	}
}

func TestEmptyArgSilent(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("Someone", "!ego")
	if len(f.sent) != 0 {
		t.Fatalf("sent = %q, want silence for bare !ego (perl parity)", f.sent)
	}
}

func TestZeroSentencesNoRatio(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	// nick known globally but never spoke in the queried channel: the
	// channel side must not divide by zero
	f.msg("BenV", "I exist")
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: "#other",
		Msg: "!ego benv"}
	ev.Sender.Nick = "Someone"
	f.b.Submit(ev)
	f.cmds.Handle(ev)
	got := f.sent[len(f.sent)-1]
	if !strings.HasPrefix(got, "#other|Channel ego for {m}benv{/}: {r}0{/}x in {r}0{/} messages. ") {
		t.Fatalf("sent = %q, want channel part without ratio", got)
	}
}

func TestAutoReportAt200(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	// 199 hits: quiet. the 200th crosses the boundary and auto-reports.
	for range 199 {
		f.msg("BenV", "i")
	}
	if len(f.sent) != 0 {
		t.Fatalf("premature report: %q", f.sent)
	}
	f.msg("BenV", "i")
	if len(f.sent) != 1 || !strings.Contains(f.sent[0], "Channel ego for {m}benv{/}") {
		t.Fatalf("sent = %q, want auto-report at 200", f.sent)
	}
	if !strings.Contains(f.sent[0], "{r}200{/}x in {r}200{/}") {
		t.Fatalf("report = %q, want 200 hits in 200 messages", f.sent[0])
	}
}

func TestPersistedAfter100Messages(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	for i := range 100 {
		f.msg("BenV", fmt.Sprintf("I said thing %d", i))
	}
	f2 := newFixture(t, store)
	f2.msg("Someone", "!ego benv")
	if !strings.Contains(f2.sent[0], "{r}100{/}x in {r}100{/}") {
		t.Fatalf("sent = %q, want persisted counts", f2.sent[0])
	}
}

func TestUnloadSaves(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	f.msg("BenV", "me me me") // 3 hits, under the save interval
	if err := f.m.Unload(); err != nil {
		t.Fatal(err)
	}
	f2 := newFixture(t, store)
	f2.msg("Someone", "!ego benv")
	if !strings.Contains(f2.sent[0], "{r}3{/}x in {r}1{/}") {
		t.Fatalf("sent = %q, want unload-flushed counts", f2.sent[0])
	}
}
