package remind

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
	"go-botje/internal/sched"
	"go-botje/internal/storage"
)

type fixture struct {
	m    *Module
	b    *bus.Bus
	cmds *cmd.Registry
	sch  *sched.Sched
	clk  time.Time
	sent []string
}

// Monday 2026-07-06 10:00 local
var t0 = time.Date(2026, 7, 6, 10, 0, 0, 0, time.Local)

func newFixtureAt(t *testing.T, store storage.Store, clk time.Time) *fixture {
	t.Helper()
	f := &fixture{clk: clk}
	f.b = bus.New()
	f.b.RegisterEvent("IRC_PRIVMSG")
	f.cmds = cmd.New()
	f.sch = sched.New(func() time.Time { return f.clk })
	f.m = New()
	f.m.Now = func() time.Time { return f.clk }
	err := f.m.Load(&module.Context{
		Bus: f.b, Cmd: f.cmds, Store: store, Sched: f.sch,
		Privmsg: func(ch, msg string) {
			for l := range strings.SplitSeq(msg, "\n") {
				f.sent = append(f.sent, ch+"|"+l)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func newFixture(t *testing.T, store storage.Store) *fixture {
	return newFixtureAt(t, store, t0)
}

func (f *fixture) say(nick, msg string) {
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: "#testing",
		Msg: msg}
	ev.Sender.Nick = nick
	f.b.Submit(ev)
	f.cmds.Handle(ev)
}

func (f *fixture) advance(d time.Duration) {
	f.clk = f.clk.Add(d)
	f.sch.RunDue()
}

func (f *fixture) take() []string {
	s := f.sent
	f.sent = nil
	return s
}

func (f *fixture) last(t *testing.T) string {
	t.Helper()
	if len(f.sent) == 0 {
		t.Fatal("nothing sent")
	}
	return f.sent[len(f.sent)-1]
}

// --- remember / recall / forget

func TestRememberRecall(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.say("BenV", "!remember wifi hetgrotegeheim123")
	want := "#testing|Sure BenV, \x1b[31mwifi\x1b[0m: added \x1b[32mhetgrotegeheim123\x1b[0m."
	if got := f.last(t); got != want {
		t.Fatalf("remember = %q\nwant %q", got, want)
	}
	f.take()
	f.say("BenV", "!recall wifi")
	want = "#testing|\x1b[31mwifi\x1b[0m: \x1b[32mhetgrotegeheim123\x1b[0m [1/1]"
	if got := f.last(t); got != want {
		t.Fatalf("recall = %q\nwant %q", got, want)
	}
}

func TestRememberStackOfThree(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	for i := 1; i <= 4; i++ {
		f.say("BenV", fmt.Sprintf("!remember ding waarde%d", i))
	}
	// the fourth push drops the oldest
	if got := f.last(t); !strings.Contains(got, "added \x1b[32mwaarde4\x1b[0m. Removed: \x1b[33mwaarde1") {
		t.Fatalf("remember 4th = %q", got)
	}
	f.take()
	f.say("BenV", "!recall ding 3")
	if got := f.last(t); !strings.Contains(got, "\x1b[32mwaarde2\x1b[0m [3/3]") {
		t.Fatalf("recall idx = %q", got)
	}
	f.take()
	f.say("BenV", "!recall ding 4")
	if got := f.last(t); !strings.Contains(got, "FUCK YOU FUCK YOU") {
		t.Fatalf("out of range = %q", got)
	}
	f.take()
	f.say("BenV", "!recall ding 0")
	if got := f.last(t); !strings.Contains(got, "TRALALA") {
		t.Fatalf("idx 0 = %q", got)
	}
}

func TestRecallUnknownShowsKeys(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.say("BenV", "!remember Wifi geheim")
	f.say("BenV", "!remember deur 1234")
	f.take()
	f.say("BenV", "!recall bestaatniet")
	got := f.last(t)
	if !strings.Contains(got, "No such thing, choose from: ") ||
		!strings.Contains(got, "deur\x1b[0m(1)") || !strings.Contains(got, "Wifi\x1b[0m(1)") {
		t.Fatalf("keys = %q", got)
	}
	f.take()
	f.say("Other", "!recall iets")
	if got := f.last(t); got != "#testing|No AND you had nothing stored to begin with." {
		t.Fatalf("empty keys = %q", got)
	}
}

func TestRememberWithoutValueRecalls(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.say("BenV", "!remember pin 0000")
	f.take()
	f.say("BenV", "!remember pin")
	if got := f.last(t); !strings.Contains(got, "\x1b[32m0000\x1b[0m [1/1]") {
		t.Fatalf("bare remember = %q", got)
	}
}

func TestForget(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.say("BenV", "!remember Ding waarde")
	f.take()
	f.say("BenV", "!forget ding")
	if got := f.last(t); got != "#testing|\x1b[31mDing\x1b[0m: \x1b[33mForgot what it was...\x1b[0m" {
		t.Fatalf("forget = %q", got)
	}
	f.take()
	f.say("BenV", "!recall ding")
	if got := f.last(t); !strings.Contains(got, "No AND you had nothing stored") {
		t.Fatalf("recall after forget = %q", got)
	}
}

func TestRememberPerNickAndChannel(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.say("BenV", "!remember prive dingetje")
	f.take()
	f.say("Other", "!recall prive")
	if got := f.last(t); !strings.Contains(got, "nothing stored") {
		t.Fatalf("other nick sees data: %q", got)
	}
}

func TestRemembersPersist(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	f.say("BenV", "!remember blijvend inhoud")
	f2 := newFixture(t, store)
	f2.say("BenV", "!recall blijvend")
	if got := f2.last(t); !strings.Contains(got, "\x1b[32minhoud\x1b[0m [1/1]") {
		t.Fatalf("persisted recall = %q", got)
	}
}

// --- cron reminders

func TestRemindSetAndFire(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.say("BenV", "!remind 30 12 * * * lunch halen")
	if got := f.last(t); got != "#testing|Reminder set, ID: \x1b[32m1\x1b[0m." {
		t.Fatalf("set = %q", got)
	}
	f.take()
	f.advance(2*time.Hour + 29*time.Minute)
	if got := f.take(); len(got) != 0 {
		t.Fatalf("fired early: %q", got)
	}
	f.advance(time.Minute) // 12:30
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|BenV: \x1b[31mlunch halen\x1b[0m." {
		t.Fatalf("fire = %q", got)
	}
	// reschedules for tomorrow
	f.advance(24 * time.Hour)
	got = f.take()
	if len(got) != 1 || !strings.Contains(got[0], "lunch halen") {
		t.Fatalf("refire = %q", got)
	}
}

func TestRemindDayNames(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	// t0 is monday 10:00; "mo" at 9:00 means next monday
	f.say("BenV", "!remind 0 9 * * mo maandagochtend")
	f.take()
	f.advance(7*24*time.Hour - time.Hour) // next monday 09:00
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "maandagochtend") {
		t.Fatalf("weekday fire = %q", got)
	}
}

func TestRemindMonthNames(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.say("BenV", "!remind 0 0 1 jan * nieuwjaar")
	if got := f.last(t); !strings.Contains(got, "Reminder set") {
		t.Fatalf("month name set = %q", got)
	}
}

func TestRemindBadCron(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.say("BenV", "!remind 99 99 * * * kapot")
	if got := f.last(t); !strings.HasPrefix(got, "#testing|BOooo: ") {
		t.Fatalf("bad cron = %q", got)
	}
}

func TestRemindHelp(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.say("BenV", "!remind")
	got := f.take()
	if len(got) != 3 || !strings.Contains(got[0], "REMIND ME") {
		t.Fatalf("help = %q", got)
	}
	f.say("BenV", "!remind help")
	if got := f.take(); len(got) != 3 {
		t.Fatalf("help kw = %q", got)
	}
}

func TestRemindShowAndClear(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.say("BenV", "!remind 0 12 * * * lunch")
	f.say("BenV", "!remind 0 18 * * fr borrel")
	f.take()
	f.say("BenV", "!remind show")
	got := f.take()
	if len(got) != 2 ||
		!strings.Contains(got[0], "\x1b[31m1\x1b[0m: \x1b[32mm:0, h:12, d:*, m:*, dow:*\x1b[0m ==> \x1b[33mlunch\x1b[0m") {
		t.Fatalf("show = %q", got)
	}
	f.say("BenV", "!remind clear 1")
	got = f.take()
	if len(got) != 1 || got[0] != "#testing|Deleted reminder \x1b[31m1\x1b[0m." {
		t.Fatalf("clear = %q", got)
	}
	f.say("BenV", "!remind clear 99")
	if got := f.take(); len(got) != 1 || got[0] != "#testing|No matching reminds found." {
		t.Fatalf("clear miss = %q", got)
	}
	f.advance(3 * time.Hour) // 13:00, lunch would have fired
	for _, l := range f.take() {
		if strings.Contains(l, "lunch") {
			t.Fatalf("cleared reminder fired: %q", l)
		}
	}
}

func TestRemindRestore(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	f.say("BenV", "!remind 30 12 * * * lunch halen")
	f.take()
	f.m.Unload()

	f2 := newFixtureAt(t, store, t0.Add(time.Hour)) // 11:00
	f2.advance(90 * time.Minute)                    // 12:30
	got := f2.take()
	if len(got) != 1 || !strings.Contains(got[0], "lunch halen") {
		t.Fatalf("restored fire = %q", got)
	}
	// ids continue after the restored max
	f2.say("BenV", "!remind 0 20 * * * avond")
	if got := f2.last(t); !strings.Contains(got, "ID: \x1b[32m2\x1b[0m") {
		t.Fatalf("next id = %q", got)
	}
}
