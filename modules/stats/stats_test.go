package stats

import (
	"strings"
	"testing"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/conf"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/irc/pager"
	"go-botje/internal/metrics"
	"go-botje/internal/module"
	"go-botje/internal/sched"
	"go-botje/internal/storage"
)

type fixture struct {
	m     *Module
	b     *bus.Bus
	cmds  *cmd.Registry
	sch   *sched.Sched
	saver *storage.Saver
	reg   *metrics.Registry
	clk   time.Time
	sent  []string
}

func newFixture(t *testing.T, store storage.Store) *fixture {
	t.Helper()
	f := &fixture{clk: time.Date(2026, 7, 13, 15, 0, 0, 0, time.UTC), reg: metrics.New()}
	f.b = bus.New()
	for _, ev := range []string{"IRC_PRIVMSG", "IRC_JOIN", "IRC_KICK"} {
		f.b.RegisterEvent(ev)
	}
	f.cmds = cmd.New()
	f.sch = sched.New(func() time.Time { return f.clk })
	f.saver = storage.NewSaver(store,
		func(fn func()) { fn() },
		func(err error) { t.Errorf("saver: %v", err) })
	f.m = New()
	f.m.Now = func() time.Time { return f.clk }
	send := func(ch, msg string) { f.sent = append(f.sent, ch+"|"+msg) }
	pg := pager.New(f.sch, send)
	pg.MaxLines = func() int { return 4 }
	f.cmds.Reply = func(ev *bus.Event, msg string) { send(ev.Channel, msg) }
	err := f.m.Load(&module.Context{
		Bus: f.b, Cmd: f.cmds, Conf: conf.New(), Store: store, Saver: f.saver,
		Sched: f.sch, Pager: pg, Metrics: f.reg,
		Privmsg: send,
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
	if !strings.HasPrefix(channel, "#") {
		ev.Query = true
	}
	f.b.Submit(ev)
	f.cmds.Handle(ev)
}

func (f *fixture) join(nick, channel string) {
	ev := &bus.Event{Name: "IRC_JOIN", Server: "junerules", Channel: channel,
		Extra: map[string]any{}}
	ev.Sender.Nick = nick
	f.b.Submit(ev)
}

func (f *fixture) kick(kicker, target, channel string) {
	ev := &bus.Event{Name: "IRC_KICK", Server: "junerules",
		Extra: map[string]any{"channel": channel, "target": target, "reason": "out"}}
	ev.Sender.Nick = kicker
	f.b.Submit(ev)
}

func (f *fixture) take() []string {
	s := f.sent
	f.sent = nil
	return s
}

func (f *fixture) tally(nick string) *Tally {
	return f.m.tallies["junerules #testing "+strings.ToLower(nick)]
}

func TestCountsLinesWordsChars(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "drie woorden hier")
	f.msg("BenV", "#testing", "nog een zin")
	ta := f.tally("BenV")
	if ta == nil || ta.Lines != 2 || ta.Words != 6 || ta.Chars != len("drie woorden hier")+len("nog een zin") {
		t.Fatalf("tally = %+v", ta)
	}
	if ta.Nick != "BenV" {
		t.Fatalf("display nick = %q", ta.Nick)
	}
}

func TestOwnLinesAndQueriesIgnored(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: "#testing",
		Msg: "ik tel niet mee", SenderMe: true, Extra: map[string]any{}}
	ev.Sender.Nick = "Meretrix"
	f.b.Submit(ev)
	f.msg("BenV", "BenV", "query telt niet")
	if len(f.m.tallies) != 0 {
		t.Fatalf("tallies = %v, want none", f.m.tallies)
	}
}

func TestLinksQuestionsShoutsActions(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "kijk https://example.com/x en http://foo.nl ook www.bar.nl")
	f.msg("BenV", "#testing", "werkt dit wel?")
	f.msg("BenV", "#testing", "DIT IS SCHREEUWEN JA")
	f.msg("BenV", "#testing", "\x01ACTION doet iets stoms\x01")
	ta := f.tally("BenV")
	if ta.Links != 3 || ta.Questions != 1 || ta.Shouts != 1 || ta.Actions != 1 {
		t.Fatalf("tally = %+v", ta)
	}
}

func TestSentiment(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("Vrolijk", "#testing", "haha wat leuk :)")
	f.msg("Somber", "#testing", "jammer, wat een kutdag :(")
	if ta := f.tally("Vrolijk"); ta.Happy != 3 || ta.Sad != 0 {
		t.Fatalf("vrolijk = %+v", ta)
	}
	// "kutdag" is not the bare word "kut": word match only, plus the :(
	if ta := f.tally("Somber"); ta.Sad != 2 || ta.Happy != 0 {
		t.Fatalf("somber = %+v", ta)
	}
}

func TestHoursAndJoinsAndKicks(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.clk = time.Date(2026, 7, 13, 3, 12, 0, 0, time.UTC) // night owl hour
	f.msg("Uil", "#testing", "diepe nacht gedachten")
	f.join("Uil", "#testing")
	f.kick("Uil", "Slachtoffer", "#testing")
	uil := f.tally("Uil")
	if uil.Hours[3] != 1 || uil.Joins != 1 || uil.KicksGiven != 1 {
		t.Fatalf("uil = %+v", uil)
	}
	if got := f.tally("Slachtoffer"); got == nil || got.KicksGot != 1 {
		t.Fatalf("slachtoffer = %+v", got)
	}
}

func TestPersistAndReload(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	f.msg("BenV", "#testing", "bewaar deze regel")
	if err := f.saver.FlushSync(); err != nil {
		t.Fatal(err)
	}
	f2 := newFixture(t, store)
	f2.msg("BenV", "#testing", "en nog een")
	if ta := f2.tally("BenV"); ta.Lines != 2 {
		t.Fatalf("lines after reload = %+v", ta)
	}
}

func TestStatsCommandNumbers(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "een regel met https://x.nl toch?")
	f.msg("Lotjuh", "#testing", "!stats benv")
	got := f.take()
	if len(got) == 0 || !strings.Contains(got[0], "BenV") || !strings.Contains(got[0], "1 regels") {
		t.Fatalf("stats output = %q", got)
	}
}

func TestStatsCommandTitles(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	for range 5 {
		f.msg("Kletskous", "#testing", "praat praat praat")
	}
	f.msg("Spammer", "#testing", "https://a.nl https://b.nl https://c.nl")
	f.msg("Vrolijk", "#testing", "haha leuk :) super")
	f.msg("BenV", "#testing", "!stats")
	got := strings.Join(f.take(), "\n")
	for _, want := range []string{"Kletskous", "Spammer", "Vrolijk"} {
		if !strings.Contains(got, want) {
			t.Fatalf("titles output missing %s:\n%s", want, got)
		}
	}
}

func TestMetricsPushed(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "meetbare regel")
	f.clk = f.clk.Add(2 * time.Minute)
	f.sch.RunDue()
	var sb strings.Builder
	f.reg.WriteText(&sb)
	if !strings.Contains(sb.String(), `botje_user_lines_total{channel="#testing",nick="benv",server="junerules"} 1`) {
		t.Fatalf("metrics missing user lines:\n%s", sb.String())
	}
}
