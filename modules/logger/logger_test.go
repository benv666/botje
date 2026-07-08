package logger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/conf"
	"go-botje/internal/module"
)

type fixture struct {
	m   *Module
	b   *bus.Bus
	cf  *conf.Conf
	dir string
	clk time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		dir: t.TempDir(),
		clk: time.Date(2026, 7, 8, 21, 15, 42, 0, time.UTC),
	}
	f.b = bus.New()
	for _, ev := range []string{"IRC_PRIVMSG", "IRC_NOTICE", "IRC_JOIN", "IRC_PART",
		"IRC_QUIT", "IRC_KICK", "IRC_MODE", "IRC_TOPIC", "IRC_INVITE", "IRC_SENT"} {
		f.b.RegisterEvent(ev)
	}
	f.cf = conf.New()
	f.m = New()
	f.m.Now = func() time.Time { return f.clk }
	if err := f.m.Load(&module.Context{Bus: f.b, Conf: f.cf}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.m.Unload() })
	if err := f.cf.Set("logger_dir", f.dir); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) event(name string, mut func(*bus.Event)) {
	ev := &bus.Event{Name: name, Server: "junerules", Extra: map[string]any{}}
	ev.Sender.Nick, ev.Sender.User, ev.Sender.Host = "BenV", "benv", "host.example"
	if mut != nil {
		mut(ev)
	}
	f.b.Submit(ev)
}

func (f *fixture) read(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func TestChannelLogLines(t *testing.T) {
	f := newFixture(t)
	f.event("IRC_PRIVMSG", func(ev *bus.Event) { ev.Channel = "#Testing"; ev.Msg = "hello \x0304world\x03" })
	f.event("IRC_SENT", func(ev *bus.Event) {
		ev.Channel = "#testing"
		ev.Msg = "{y}reply{/} here"
		ev.Sender.Nick = "Meretrix"
		ev.SenderMe = true
	})
	f.event("IRC_PRIVMSG", func(ev *bus.Event) { ev.Channel = "#testing"; ev.Msg = "\x01ACTION waves\x01" })
	f.event("IRC_JOIN", func(ev *bus.Event) { ev.Channel = "#testing" })
	f.event("IRC_PART", func(ev *bus.Event) {
		ev.Channel = "#testing"
		ev.Raw.Params = "#testing :cya"
	})
	f.event("IRC_KICK", func(ev *bus.Event) {
		ev.Extra["channel"] = "#testing"
		ev.Extra["target"] = "spammer"
		ev.Extra["reason"] = "flood"
	})
	f.event("IRC_TOPIC", func(ev *bus.Event) {
		ev.Channel = "#testing"
		ev.Extra["topic"] = "new topic"
	})
	f.event("IRC_MODE", func(ev *bus.Event) {
		ev.Channel = "#testing"
		ev.Extra["mode"] = "+o"
		ev.Extra["unparsed"] = []string{"Meretrix"}
	})

	got := f.read(t, "junerules/#testing/2026-07-08.log")
	for _, want := range []string{
		"21:15:42 <BenV> hello world",
		"21:15:42 <Meretrix> reply here",
		"21:15:42 * BenV waves",
		"21:15:42 -!- BenV [benv@host.example] has joined #testing",
		"21:15:42 -!- BenV [benv@host.example] has left #testing [cya]",
		"21:15:42 -!- spammer was kicked from #testing by BenV [flood]",
		"21:15:42 -!- BenV changed the topic of #testing to: new topic",
		"21:15:42 -!- mode/#testing [+o Meretrix] by BenV",
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("log missing %q, got:\n%s", want, got)
		}
	}
}

func TestQueryAndServerPaths(t *testing.T) {
	f := newFixture(t)
	f.event("IRC_PRIVMSG", func(ev *bus.Event) {
		ev.Channel = "BenV" // query: session rewrote target to sender
		ev.Query = true
		ev.Msg = "psst"
	})
	f.event("IRC_QUIT", func(ev *bus.Event) { ev.Extra["msg"] = "Quit: brb" })

	if got := f.read(t, "junerules/queries/benv/2026-07-08.log"); !strings.Contains(got, "<BenV> psst") {
		t.Errorf("query log = %q", got)
	}
	if got := f.read(t, "junerules/server/2026-07-08.log"); !strings.Contains(got, "-!- BenV [benv@host.example] has quit [Quit: brb]") {
		t.Errorf("server log = %q", got)
	}
}

func TestDayRollover(t *testing.T) {
	f := newFixture(t)
	f.event("IRC_PRIVMSG", func(ev *bus.Event) { ev.Channel = "#testing"; ev.Msg = "before midnight" })
	f.clk = f.clk.Add(3 * time.Hour) // 2026-07-09 00:15
	f.event("IRC_PRIVMSG", func(ev *bus.Event) { ev.Channel = "#testing"; ev.Msg = "after midnight" })

	if got := f.read(t, "junerules/#testing/2026-07-08.log"); !strings.Contains(got, "before midnight") {
		t.Errorf("day one log = %q", got)
	}
	if got := f.read(t, "junerules/#testing/2026-07-09.log"); !strings.Contains(got, "after midnight") {
		t.Errorf("day two log = %q", got)
	}
}

func TestDisabledWithoutDir(t *testing.T) {
	f := newFixture(t)
	if err := f.cf.Set("logger_dir", ""); err != nil {
		t.Fatal(err)
	}
	f.event("IRC_PRIVMSG", func(ev *bus.Event) { ev.Channel = "#testing"; ev.Msg = "into the void" })
	entries, _ := os.ReadDir(f.dir)
	if len(entries) != 0 {
		t.Fatalf("disabled logger wrote files: %v", entries)
	}
}

func TestPathSanitized(t *testing.T) {
	f := newFixture(t)
	f.event("IRC_PRIVMSG", func(ev *bus.Event) { ev.Channel = "#../../evil"; ev.Msg = "escape attempt" })
	if _, err := os.Stat(filepath.Join(f.dir, "junerules", "#.._.._evil", "2026-07-08.log")); err != nil {
		t.Fatalf("sanitized path missing: %v", err)
	}
}

// server notices (dotted sender, or the pre-registration "*" target)
// belong in the server log, not in a query file.
func TestServerNoticesGoToServerLog(t *testing.T) {
	f := newFixture(t)
	f.event("IRC_NOTICE", func(ev *bus.Event) {
		ev.Sender.Nick = "irc.example.com"
		ev.Sender.User, ev.Sender.Host = "", ""
		ev.Channel = "*"
		ev.Msg = "*** Looking up your hostname..."
	})
	got := f.read(t, "junerules/server/2026-07-08.log")
	if !strings.Contains(got, "-irc.example.com- *** Looking up your hostname...") {
		t.Errorf("server log = %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "junerules", "queries")); !os.IsNotExist(err) {
		t.Errorf("server notice created a queries dir (err=%v)", err)
	}
}
