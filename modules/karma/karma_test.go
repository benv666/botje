package karma

import (
	"strings"
	"testing"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/irc/pager"
	"go-botje/internal/module"
	"go-botje/internal/sched"
	"go-botje/internal/storage"
)

type fixture struct {
	m     *Module
	b     *bus.Bus
	cmds  *cmd.Registry
	store storage.Store
	sent  []string // direct privmsgs: "channel|msg"
	paged []string // pager lines: "channel|line"
}

func newFixture(t *testing.T, store storage.Store) *fixture {
	t.Helper()
	f := &fixture{store: store}
	f.b = bus.New()
	f.b.RegisterEvent("IRC_PRIVMSG")
	f.b.RegisterEvent("IRC_KICK")
	f.cmds = cmd.New()
	sch := sched.New(time.Now)
	pg := pager.New(sch, func(ch, line string) { f.paged = append(f.paged, ch+"|"+line) })
	f.m = New()
	err := f.m.Load(&module.Context{
		Bus: f.b, Cmd: f.cmds, Pager: pg, Store: store, Sched: sch,
		Privmsg: func(ch, msg string) { f.sent = append(f.sent, ch+"|"+msg) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) privmsg(channel, msg string) {
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: channel, Msg: msg,
		Extra: map[string]any{}}
	ev.Sender.Nick = "BenV"
	f.b.Submit(ev)
	f.cmds.Handle(ev)
}

func (f *fixture) lastSent(t *testing.T) string {
	t.Helper()
	if len(f.sent) == 0 {
		t.Fatal("nothing sent")
	}
	return f.sent[len(f.sent)-1]
}

func TestIncrement(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.privmsg("#testing", "!beer++")
	want := "#testing|Karma for {m}beer{/} in this channel is now {m}1{/} (global karma is {m}1{/})."
	if got := f.lastSent(t); got != want {
		t.Errorf("reply = %q, want %q", got, want)
	}
}

func TestDecrementAndQuery(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.privmsg("#testing", "!beer--")
	f.privmsg("#testing", "!beer--")
	f.privmsg("#testing", "!beer?")
	want := "#testing|Karma for {m}beer{/} in this channel is currently {m}-2{/} (global karma is {m}-2{/})."
	if got := f.lastSent(t); got != want {
		t.Errorf("reply = %q, want %q", got, want)
	}
}

func TestChannelVsGlobal(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.privmsg("#a", "!beer++")
	f.privmsg("#b", "!beer++")
	want := "#b|Karma for {m}beer{/} in this channel is now {m}1{/} (global karma is {m}2{/})."
	if got := f.lastSent(t); got != want {
		t.Errorf("reply = %q, want %q", got, want)
	}
}

func TestItemLowercasedAndSpaces(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.privmsg("#testing", "!BenV's Cooking++")
	f.privmsg("#testing", "!benv's cooking?")
	if got := f.lastSent(t); !strings.Contains(got, "currently {m}1{/}") {
		t.Errorf("reply = %q, want karma 1 for lowercased multiword item", got)
	}
}

func TestSmileySuffix(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.privmsg("#testing", "!beer++ :)")
	if got := f.lastSent(t); !strings.Contains(got, "{m}beer{/}") || !strings.Contains(got, "now {m}1{/}") {
		t.Errorf("reply = %q, want beer at 1 despite smiley", got)
	}
}

func TestQuestionWithSmiley(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.privmsg("#testing", "!beer? ;)")
	if got := f.lastSent(t); !strings.Contains(got, "currently {m}0{/}") {
		t.Errorf("reply = %q", got)
	}
}

func TestRegisteredCommandSkipped(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.cmds.Register("core", "more", func(*cmd.Data) bool { return true })
	f.privmsg("#testing", "!more++")
	if len(f.sent) != 0 {
		t.Errorf("karma replied to a registered command word: %q", f.sent)
	}
}

func TestNonKarmaIgnored(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	for _, msg := range []string{"hello", "!singleplus+", "beer++", "!x + +"} {
		f.privmsg("#testing", msg)
	}
	if len(f.sent) != 0 {
		t.Errorf("replied to non-karma text: %q", f.sent)
	}
}

func TestOwnMessagesIgnored(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: "#testing",
		Msg: "!beer++", SenderMe: true, Extra: map[string]any{}}
	f.b.Submit(ev)
	if len(f.sent) != 0 {
		t.Errorf("replied to own message: %q", f.sent)
	}
}

func TestReasonsAndWhyKarma(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.privmsg("#testing", "!beer++ # goede pils")
	f.privmsg("#testing", "!beer++ # goede pils")
	f.privmsg("#testing", "!beer++ # lekker koud")
	f.privmsg("#testing", "!wku beer")

	want := []string{
		"#testing|The following reasons were found for the karma increase of {m}beer{/}:",
		"#testing|goede pils: {G}+2{/}",
		"#testing|lekker koud: {G}+1{/}",
	}
	if len(f.paged) != 3 {
		t.Fatalf("paged = %q, want 3 lines", f.paged)
	}
	for i := range want {
		if f.paged[i] != want[i] {
			t.Errorf("paged[%d] = %q, want %q", i, f.paged[i], want[i])
		}
	}
}

func TestWhyKarmaNoReasons(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.privmsg("#testing", "!wkd beer")
	want := "#testing|No reasons were found for the karma decrease of {m}beer{/}."
	if len(f.paged) != 1 || f.paged[0] != want {
		t.Errorf("paged = %q, want [%q]", f.paged, want)
	}
}

func TestShortReasonNotRecorded(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.privmsg("#testing", "!beer++ # ok")
	f.privmsg("#testing", "!wku beer")
	if len(f.paged) != 1 || !strings.Contains(f.paged[0], "No reasons") {
		t.Errorf("paged = %q, want no reasons (too short to record)", f.paged)
	}
}

func TestKickCostsKarma(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	ev := &bus.Event{Name: "IRC_KICK", Server: "junerules", TargetMe: true,
		Extra: map[string]any{"channel": "#testing", "target": "Meretrix"}}
	ev.Sender.Nick = "BenV"
	f.b.Submit(ev)
	if len(f.sent) != 0 {
		t.Errorf("kick handler replied: %q", f.sent)
	}
	f.privmsg("#testing", "!benv?")
	if got := f.lastSent(t); !strings.Contains(got, "currently {m}-1{/}") {
		t.Errorf("kicker karma = %q, want -1", got)
	}
	f.privmsg("#testing", "!wkd benv")
	found := false
	for _, l := range f.paged {
		if strings.Contains(l, "For kicking defenseless bots: {R}-1{/}") {
			found = true
		}
	}
	if !found {
		t.Errorf("kick reason missing: %q", f.paged)
	}
}

func TestKickOfOthersIgnored(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	ev := &bus.Event{Name: "IRC_KICK", Server: "junerules", TargetMe: false,
		Extra: map[string]any{"channel": "#testing", "target": "Someone"}}
	ev.Sender.Nick = "BenV"
	f.b.Submit(ev)
	f.privmsg("#testing", "!benv?")
	if got := f.lastSent(t); !strings.Contains(got, "currently {m}0{/}") {
		t.Errorf("karma = %q, want untouched 0", got)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	f.privmsg("#testing", "!beer++ # goede pils")

	f2 := newFixture(t, store)
	f2.privmsg("#testing", "!beer?")
	if got := f2.lastSent(t); !strings.Contains(got, "currently {m}1{/} (global karma is {m}1{/})") {
		t.Errorf("fresh module karma = %q, want persisted 1", got)
	}
}
