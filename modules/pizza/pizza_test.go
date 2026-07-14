package pizza

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
	m     *Module
	b     *bus.Bus
	cmds  *cmd.Registry
	sch   *sched.Sched
	clk   time.Time
	sent  []string
	randi int
}

func newFixture(t *testing.T, store storage.Store) *fixture {
	return newFixtureAt(t, store, now)
}

// newFixtureAt loads the module with the clock already at clk, for
// restore-after-restart tests.
func newFixtureAt(t *testing.T, store storage.Store, clk time.Time) *fixture {
	t.Helper()
	f := &fixture{clk: clk}
	f.b = bus.New()
	f.b.RegisterEvent("IRC_PRIVMSG")
	f.cmds = cmd.New()
	f.sch = sched.New(func() time.Time { return f.clk })
	f.m = New()
	f.m.Now = func() time.Time { return f.clk }
	f.m.Rand = func() float64 { f.randi++; return float64(f.randi%17) / 17 }
	err := f.m.Load(&module.Context{
		Bus: f.b, Cmd: f.cmds, Store: store, Sched: f.sch,
		Privmsg: func(ch, msg string) { f.sent = append(f.sent, ch+"|"+msg) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) pizza(nick, data string) {
	msg := "!pizza"
	if data != "" {
		msg += " " + data
	}
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

func (f *fixture) lastReply(t *testing.T) string {
	t.Helper()
	if len(f.sent) == 0 {
		t.Fatal("nothing sent")
	}
	return f.sent[len(f.sent)-1]
}

// setTimer sets one and returns its id, plucked from the confirmation.
func (f *fixture) setTimer(t *testing.T, nick, spec string) string {
	t.Helper()
	f.pizza(nick, spec)
	reply := f.lastReply(t)
	if !strings.Contains(reply, "Alarm set for") {
		t.Fatalf("set %q: %q", spec, reply)
	}
	g := strings.SplitN(strings.SplitN(reply, "{r}", 2)[1], "{/}", 2)
	f.take()
	return g[0]
}

func TestSetAndFire(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	id := f.setTimer(t, "BenV", "+5m")

	f.advance(4 * time.Minute)
	if len(f.take()) != 0 {
		t.Fatal("fired early")
	}
	f.advance(time.Minute)
	want := fmt.Sprintf("#testing|BenV: QUICK! Pizza %s is burning!", id)
	got := f.take()
	if len(got) != 1 || got[0] != want {
		t.Fatalf("fire = %q, want %q", got, want)
	}
	f.pizza("BenV", "")
	if !strings.Contains(f.lastReply(t), "You have no timers set.") {
		t.Fatalf("after fire = %q, timer must be gone", f.lastReply(t))
	}
}

func TestCustomMessage(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.setTimer(t, "BenV", "+1m pizza uit de oven")
	f.advance(time.Minute)
	if got := f.lastReply(t); got != "#testing|BenV: pizza uit de oven" {
		t.Fatalf("fire = %q", got)
	}
}

func TestListTimers(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	id1 := f.setTimer(t, "BenV", "+10m eerste")
	id2 := f.setTimer(t, "BenV", "+2h tweede")
	f.pizza("BenV", "")
	got := f.lastReply(t)
	if !strings.HasPrefix(got, "#testing|BenV: Time remaining: ") {
		t.Fatalf("list = %q", got)
	}
	// sorted by remaining time: id1 (10m) before id2 (2.0h)
	if !strings.Contains(got, id1+": at 12:10:00 (in 10m) eerste") ||
		!strings.Contains(got, id2+": at 14:00:00 (in 2.0h) tweede") {
		t.Fatalf("list = %q", got)
	}
	if strings.Index(got, id1+":") > strings.Index(got, id2+":") {
		t.Fatalf("list not sorted by remaining time: %q", got)
	}
}

func TestWhen(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	id := f.setTimer(t, "BenV", "+1d morgen")
	f.pizza("BenV", "when "+id)
	want := fmt.Sprintf("#testing|BenV: {M}%s: Saturday 04-07-2026 at 12:00:00{/}", id)
	if got := f.lastReply(t); got != want {
		t.Fatalf("when = %q, want %q", got, want)
	}
	f.pizza("BenV", "when spook-1")
	if !strings.Contains(f.lastReply(t), "You have no timer set with id spook-1") {
		t.Fatalf("when unknown = %q", f.lastReply(t))
	}
}

func TestClearOne(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	id := f.setTimer(t, "BenV", "+5m")
	f.pizza("BenV", "clear "+id)
	if got := f.lastReply(t); got != "#testing|BenV: Removed timer: "+id {
		t.Fatalf("clear = %q", got)
	}
	f.take()
	f.advance(10 * time.Minute)
	if got := f.take(); len(got) != 0 {
		t.Fatalf("cleared timer fired: %q", got)
	}
	f.pizza("BenV", "clear nogeen-2")
	if !strings.Contains(f.lastReply(t), "No nogeen-2 for you..") {
		t.Fatalf("clear unknown = %q", f.lastReply(t))
	}
}

func TestClearAllNeedsConfirmation(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	id := f.setTimer(t, "BenV", "+5m")
	f.pizza("BenV", "clear")
	if !strings.Contains(f.lastReply(t), "Say again?") {
		t.Fatalf("first clear = %q", f.lastReply(t))
	}
	f.pizza("BenV", "clear")
	if !strings.Contains(f.lastReply(t), "Removed timers: "+id) {
		t.Fatalf("second clear = %q", f.lastReply(t))
	}
	// confirmation expires after 60s
	f.setTimer(t, "BenV", "+5m")
	f.pizza("BenV", "clear")
	f.clk = f.clk.Add(61 * time.Second)
	f.pizza("BenV", "clear")
	if !strings.Contains(f.lastReply(t), "Say again?") {
		t.Fatalf("stale confirmation honored: %q", f.lastReply(t))
	}
}

func TestClearAllEmpty(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.pizza("BenV", "clear")
	f.pizza("BenV", "clear")
	if !strings.Contains(f.lastReply(t), "you had no timers, idiot") {
		t.Fatalf("clear-all empty = %q", f.lastReply(t))
	}
}

func TestStopwatch(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.pizza("BenV", "start")
	if !strings.Contains(f.lastReply(t), "benv: Time stored, use stop to get time elapsed.") {
		t.Fatalf("start = %q", f.lastReply(t))
	}
	f.pizza("BenV", "start")
	if !strings.Contains(f.lastReply(t), "Already started one, use stop to stop.") {
		t.Fatalf("double start = %q", f.lastReply(t))
	}
	f.clk = f.clk.Add(65 * time.Second)
	f.pizza("BenV", "stop")
	if got := f.lastReply(t); got != "#testing|benv: Time elapsed: 1 minutes, 5 seconds." {
		t.Fatalf("stop = %q", got)
	}
	f.pizza("BenV", "stop")
	if !strings.Contains(f.lastReply(t), "Use start to start a timer first.") {
		t.Fatalf("second stop = %q", f.lastReply(t))
	}
}

func TestRepeatReschedules(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	id := f.setTimer(t, "BenV", "+1h r{1h} herhaal")
	f.advance(time.Hour)
	got := f.take()
	if len(got) != 2 || got[0] != "#testing|BenV: herhaal" ||
		!strings.Contains(got[1], "Alarm set for") {
		t.Fatalf("repeat fire = %q", got)
	}
	f.advance(time.Hour)
	got = f.take()
	if len(got) != 2 || got[0] != "#testing|BenV: herhaal" {
		t.Fatalf("second repeat fire = %q", got)
	}
	_ = id
}

func TestRepeatTooShortDenied(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	id := f.setTimer(t, "BenV", "+1h r{10m} te vaak")
	f.advance(time.Hour)
	got := f.take()
	if len(got) != 2 || got[0] != "#testing|BenV: te vaak" ||
		!strings.Contains(got[1], fmt.Sprintf("Rescheduling of %s denied, period too short, FU.", id)) {
		t.Fatalf("denied fire = %q", got)
	}
	f.advance(time.Hour)
	if got := f.take(); len(got) != 0 {
		t.Fatalf("denied repeat fired again: %q", got)
	}
}

func TestPastRefused(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.pizza("BenV", "1-1-2020 12:00 verleden")
	if !strings.Contains(f.lastReply(t), "This bot travels forward in time.") {
		t.Fatalf("past = %q", f.lastReply(t))
	}
}

func TestHelp(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.pizza("BenV", "help")
	if len(f.sent) != 4 || !strings.Contains(f.sent[0], "Sets timer.") {
		t.Fatalf("help = %q", f.sent)
	}
}

func TestRestoreAfterReload(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	id := f.setTimer(t, "BenV", "+2h straks")
	if err := f.m.Unload(); err != nil {
		t.Fatal(err)
	}

	// restart half an hour later: the timer must fire at its original
	// due time
	f2 := newFixtureAt(t, store, f.clk.Add(30*time.Minute))
	f2.advance(90 * time.Minute)
	got := f2.take()
	found := false
	for _, l := range got {
		if l == "#testing|BenV: straks" {
			found = true
		}
	}
	if !found {
		t.Fatalf("restored timer did not fire: %q (id %s)", got, id)
	}
}

func TestRestoreNearPastFiresAfter20s(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	f.setTimer(t, "BenV", "+5m bijna")
	f.m.Unload()

	// restart well past the due time: fires ~20s after boot instead
	f2 := newFixtureAt(t, store, f.clk.Add(10*time.Minute))
	f2.advance(21 * time.Second)
	got := f2.take()
	if len(got) != 1 || got[0] != "#testing|BenV: bijna" {
		t.Fatalf("near-past restore = %q, want fire ~20s after boot", got)
	}
}

// A running stopwatch must survive reload as a stopwatch, not become a
// broken timer (the Perl restored it as one and pinged "nick: " after
// boot).
func TestStopwatchSurvivesReload(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	f.pizza("BenV", "start")
	f.m.Unload()

	f2 := newFixtureAt(t, store, f.clk.Add(90*time.Second))
	f2.advance(30 * time.Second)
	if got := f2.take(); len(got) != 0 {
		t.Fatalf("stopwatch became a timer on restore: %q", got)
	}
	f2.pizza("BenV", "stop")
	if got := f2.lastReply(t); got != "#testing|benv: Time elapsed: 2 minutes, 0 seconds." {
		t.Fatalf("stop after reload = %q", got)
	}
}
