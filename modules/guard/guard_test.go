package guard

import (
	"strings"
	"testing"
	"time"

	"go-botje/internal/admin"
	"go-botje/internal/bus"
	"go-botje/internal/conf"
	"go-botje/internal/module"
	"go-botje/internal/sched"
	"go-botje/internal/storage"
)

type fixture struct {
	m   *Module
	b   *bus.Bus
	cf  *conf.Conf
	sch *sched.Sched
	st  storage.Store
	clk time.Time
}

func newFixture(t *testing.T, st storage.Store) *fixture {
	t.Helper()
	f := &fixture{st: st, clk: time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)}
	f.b = bus.New()
	for _, ev := range []string{"IRC_PRIVMSG", "IRC_JOIN", "QUIT", "COMMAND"} {
		f.b.RegisterEvent(ev)
	}
	f.cf = conf.New()
	f.sch = sched.New(func() time.Time { return f.clk })
	f.m = New()
	f.m.Now = func() time.Time { return f.clk }
	if err := f.m.Load(&module.Context{
		Bus: f.b, Conf: f.cf, Store: st, Sched: f.sch,
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) event(name, nick, user, host, channel string) {
	ev := &bus.Event{Name: name, Server: "junerules", Channel: channel,
		Msg: "hoi"}
	ev.Sender.Nick, ev.Sender.User, ev.Sender.Host = nick, user, host
	f.b.Submit(ev)
}

func (f *fixture) guardCmd(t *testing.T, args string) string {
	t.Helper()
	for _, payload := range f.b.Submit(&bus.Event{Name: "COMMAND"}) {
		if spec, ok := payload.(admin.Spec); ok && spec.Name == "guard" {
			line := "guard"
			if args != "" {
				line += " " + args
			}
			if spec.Match.FindStringIndex(line) == nil {
				t.Fatalf("guard spec does not match %q", line)
			}
			return spec.Run("benv", line)
		}
	}
	t.Fatal("no guard admin spec")
	return ""
}

func (f *fixture) stored(t *testing.T) map[string]map[string]int64 {
	t.Helper()
	var out map[string]map[string]int64
	if _, err := f.st.Get("guard", "residents", &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// speaking and joining during normal (guard off) operation builds the
// residents table; the bot itself and userless senders do not count.
func TestResidentsCollectedWhileIdle(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.event("IRC_PRIVMSG", "BenV", "benv", "host.example", "#testing")
	f.event("IRC_JOIN", "Lotjuh", "lot", "other.example", "#testing")
	f.event("IRC_PRIVMSG", "server.example", "", "", "#testing") // no user@host
	me := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: "#t",
		SenderMe: true}
	me.Sender.Nick, me.Sender.User, me.Sender.Host = "Meretrix", "Botje", "bot.example"
	f.b.Submit(me)

	out := f.guardCmd(t, "status")
	if !strings.Contains(out, "off") || !strings.Contains(out, "2") {
		t.Fatalf("status = %q, want off with 2 residents", out)
	}
}

// the table persists (flush timer) and a fresh module reloads it.
func TestResidentsPersistAndReload(t *testing.T) {
	st := storage.NewMemory()
	f := newFixture(t, st)
	f.event("IRC_PRIVMSG", "BenV", "benv", "host.example", "#testing")
	f.clk = f.clk.Add(6 * time.Minute)
	f.sch.RunDue() // flush timer
	if got := f.stored(t); got["junerules"]["benv@host.example"] == 0 {
		t.Fatalf("stored residents = %v", got)
	}

	f2 := newFixture(t, st)
	if out := f2.guardCmd(t, "status"); !strings.Contains(out, "1") {
		t.Fatalf("reloaded status = %q, want 1 resident", out)
	}
}

// masks unseen for longer than guard_resident_days age out at load.
func TestResidentsAgeOut(t *testing.T) {
	st := storage.NewMemory()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix() // >90 days before fixture clock
	st.Put("guard", "residents", map[string]map[string]int64{
		"junerules": {"ancient@host": old, "benv@host.example": time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).Unix()},
	})
	f := newFixture(t, st)
	if out := f.guardCmd(t, "status"); !strings.Contains(out, "1") {
		t.Fatalf("status after aging = %q, want 1 resident", out)
	}
}

// guard on|off flips the persisted conf setting and is reported; while
// ON, new masks are NOT added (a spam wave must not become resident)
// and non-resident joins are counted as strangers.
func TestToggleFreezesAndCounts(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.event("IRC_PRIVMSG", "BenV", "benv", "host.example", "#testing")

	if out := f.guardCmd(t, "on"); !strings.Contains(strings.ToLower(out), "on") {
		t.Fatalf("guard on = %q", out)
	}
	if !f.cf.Bool("guard_enabled") {
		t.Fatal("guard_enabled not set")
	}

	// resident joins: no stranger count
	f.event("IRC_JOIN", "BenV", "benv", "host.example", "#testing")
	// stranger joins and speaks: counted, NOT recorded
	f.event("IRC_JOIN", "spammer", "x", "evil.example", "#testing")
	f.event("IRC_PRIVMSG", "spammer", "x", "evil.example", "#testing")

	out := f.guardCmd(t, "status")
	if !strings.Contains(out, "1 resident") || !strings.Contains(out, "1 stranger") {
		t.Fatalf("status = %q, want 1 resident and 1 stranger", out)
	}

	if out := f.guardCmd(t, "off"); !strings.Contains(strings.ToLower(out), "off") {
		t.Fatalf("guard off = %q", out)
	}
	// off again: spammer's next line makes it resident (normal times)
	f.event("IRC_PRIVMSG", "spammer", "x", "evil.example", "#testing")
	if out := f.guardCmd(t, "status"); !strings.Contains(out, "2 resident") {
		t.Fatalf("status = %q, want 2 residents after off", out)
	}
}

// repeat sightings within the hour do not advance the stored timestamp
// (write throttling); a later sighting does.
func TestLastSeenThrottle(t *testing.T) {
	st := storage.NewMemory()
	f := newFixture(t, st)
	f.event("IRC_PRIVMSG", "BenV", "benv", "host.example", "#testing")
	first := f.clk.Unix()

	f.clk = f.clk.Add(6 * time.Minute)
	f.sch.RunDue() // flush
	f.event("IRC_PRIVMSG", "BenV", "benv", "host.example", "#testing")
	f.clk = f.clk.Add(6 * time.Minute)
	f.sch.RunDue()
	if got := f.stored(t)["junerules"]["benv@host.example"]; got != first {
		t.Fatalf("lastSeen advanced within the hour: %d != %d", got, first)
	}

	f.clk = f.clk.Add(2 * time.Hour)
	f.event("IRC_PRIVMSG", "BenV", "benv", "host.example", "#testing")
	f.clk = f.clk.Add(6 * time.Minute)
	f.sch.RunDue()
	if got := f.stored(t)["junerules"]["benv@host.example"]; got == first {
		t.Fatal("lastSeen never advances")
	}
}

// shutdown (the QUIT event) flushes pending changes.
func TestQuitFlushes(t *testing.T) {
	st := storage.NewMemory()
	f := newFixture(t, st)
	f.event("IRC_PRIVMSG", "BenV", "benv", "host.example", "#testing")
	f.b.Submit(&bus.Event{Name: "QUIT"})
	if got := f.stored(t); got["junerules"]["benv@host.example"] == 0 {
		t.Fatalf("QUIT did not flush: %v", got)
	}
}
