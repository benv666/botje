package lastseen

import (
	"strings"
	"testing"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
	"go-botje/internal/storage"
)

type fixture struct {
	m    *Module
	b    *bus.Bus
	cmds *cmd.Registry
	now  time.Time
	sent []string
}

func newFixture(t *testing.T, store storage.Store) *fixture {
	t.Helper()
	f := &fixture{now: time.Date(2026, 7, 3, 12, 0, 0, 0, time.Local)}
	f.b = bus.New()
	f.b.RegisterEvent("IRC_PRIVMSG")
	f.cmds = cmd.New()
	f.m = New()
	f.m.Now = func() time.Time { return f.now }
	err := f.m.Load(&module.Context{
		Bus: f.b, Cmd: f.cmds, Store: store,
		Privmsg: func(ch, msg string) { f.sent = append(f.sent, ch+"|"+msg) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) msg(nick, channel, text string) {
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: channel,
		Msg: text, Extra: map[string]any{}}
	ev.Sender.Nick = nick
	f.b.Submit(ev)
	f.cmds.Handle(ev)
}

func TestLastSameChannel(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "brb koffie")
	f.now = f.now.Add(10 * time.Minute)
	f.msg("Someone", "#testing", "!last benv")

	want := "#testing|benv was last seen 10 minutes ago on this channel: 12:00:00 <benv> brb koffie"
	if len(f.sent) != 1 || f.sent[0] != want {
		t.Fatalf("sent = %q\nwant %q", f.sent, want)
	}
}

func TestLastNeverSeenInThisChannel(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#other", "elders bezig")
	f.now = f.now.Add(2 * time.Hour)
	f.msg("Someone", "#testing", "!last benv")

	want := "#testing|benv was last seen in #other 2 hours ago. Never seen in this channel!"
	if len(f.sent) != 1 || f.sent[0] != want {
		t.Fatalf("sent = %q\nwant %q", f.sent, want)
	}
}

func TestLastElsewhereButKnownHere(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "hier was ik")
	f.now = f.now.Add(30 * time.Minute)
	f.msg("BenV", "#other", "nu hier")
	f.now = f.now.Add(30 * time.Minute)
	f.msg("Someone", "#testing", "!last benv")

	got := f.sent[0]
	if !strings.HasPrefix(got, "#testing|benv was last seen in #other 30 minutes ago. Last here about an hour ago: ") {
		t.Fatalf("sent = %q", got)
	}
	if !strings.HasSuffix(got, "<benv> hier was ik") {
		t.Fatalf("sent = %q, want this channel's last message", got)
	}
}

func TestUnknownNick(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("Someone", "#testing", "!last ghost")
	want := "#testing|ghost destroyed (never seen that nick anywhere)."
	if len(f.sent) != 1 || f.sent[0] != want {
		t.Fatalf("sent = %q, want %q", f.sent, want)
	}
}

func TestBareLastShowsHelp(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("Someone", "#testing", "!last")
	want := "#testing|!last <nick> Shows last known activity for <nick> on this channel"
	if len(f.sent) != 1 || f.sent[0] != want {
		t.Fatalf("sent = %q, want %q", f.sent, want)
	}
}

func TestCommandsNotRecorded(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "!karma iets")
	f.msg("Someone", "#testing", "!last benv")
	if !strings.Contains(f.sent[len(f.sent)-1], "destroyed") {
		t.Fatalf("sent = %q, command attempts must not count as activity", f.sent)
	}
}

func TestUnloadPersists(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	f.msg("BenV", "#testing", "onthoud dit")
	if err := f.m.Unload(); err != nil {
		t.Fatal(err)
	}
	f2 := newFixture(t, store)
	f2.now = f.now.Add(5 * time.Minute)
	f2.msg("Someone", "#testing", "!last benv")
	if !strings.Contains(f2.sent[0], "onthoud dit") {
		t.Fatalf("sent = %q, want persisted activity", f2.sent)
	}
}

func TestTimeAgo(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{90 * time.Second, "a minute ago"},
		{10 * time.Minute, "10 minutes ago"},
		{70 * time.Minute, "about an hour ago"},
		{5 * time.Hour, "5 hours ago"},
		{30 * time.Hour, "yesterday"},
		{3 * 24 * time.Hour, "3 days ago"},
		{10 * 24 * time.Hour, "last week"},
		{40 * 24 * time.Hour, "last month"},
		{100 * 24 * time.Hour, "3 months ago"},
		{800 * 24 * time.Hour, "2 years ago"},
	} {
		if got := timeAgo(tc.d); got != tc.want {
			t.Errorf("timeAgo(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
