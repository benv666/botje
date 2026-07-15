package rvf

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go-botje/internal/admin"
	"go-botje/internal/bus"
	"go-botje/internal/conf"
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
	cf    *conf.Conf
	sch   *sched.Sched
	clk   time.Time
	saver *storage.Saver
	sent  []string
	raw   []string // SendRaw lines (topic assertions)
	rolls []int    // scripted Rand results, consumed in order
}

func newFixture(t *testing.T, store storage.Store) *fixture {
	t.Helper()
	f := &fixture{clk: time.Date(2026, 7, 14, 20, 0, 0, 0, time.Local)}
	f.b = bus.New()
	f.b.RegisterEvent("IRC_PRIVMSG")
	f.b.RegisterEvent("IRC_JOIN")
	f.b.RegisterEvent("IRC_TOPIC")
	f.b.RegisterEvent("COMMAND")
	f.cmds = cmd.New()
	f.cf = conf.New()
	f.sch = sched.New(func() time.Time { return f.clk })
	f.m = New()
	f.m.Now = func() time.Time { return f.clk }
	f.m.Rand = func(n int) int {
		if len(f.rolls) == 0 {
			t.Fatalf("unscripted Rand(%d) call", n)
		}
		r := f.rolls[0]
		f.rolls = f.rolls[1:]
		return r % n
	}
	pg := pager.New(f.sch, func(ch, msg string) { f.sent = append(f.sent, ch+"|"+msg) })
	pg.MaxLines = func() int { return 12 }
	f.saver = storage.NewSaver(store,
		func(fn func()) { fn() },
		func(err error) { t.Errorf("saver: %v", err) })
	err := f.m.Load(&module.Context{
		Bus: f.b, Cmd: f.cmds, Conf: f.cf, Store: store, Sched: f.sch,
		Saver: f.saver, Pager: pg,
		Privmsg: func(ch, msg string) { f.sent = append(f.sent, ch+"|"+msg) },
		SendRaw: func(line string) { f.raw = append(f.raw, line) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.saver.FlushSync() })
	return f
}

func (f *fixture) msg(nick, channel, text string) {
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: channel,
		Msg: text}
	ev.Sender.Nick = nick
	f.b.Submit(ev)
	f.cmds.Handle(ev)
}

func (f *fixture) query(nick, text string) {
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: nick,
		Msg: text, Query: true}
	ev.Sender.Nick = nick
	f.b.Submit(ev)
	f.cmds.Handle(ev)
}

func (f *fixture) take() []string {
	s := f.sent
	f.sent = nil
	return s
}

func (f *fixture) all() string { return strings.Join(f.take(), "\n") }

// startGame scripts the puzzle roll to 0: the first corpus entry, which
// for nl is "Wie het laatst lacht lacht het best" (Spreekwoord).
func (f *fixture) startGame(channel, players string) {
	f.rolls = append(f.rolls, 0)
	f.msg("BenV", channel, strings.TrimSpace("!start "+players))
}

func TestStartAnnouncesBoardAndTurn(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.startGame("#radvanfortuin", "BenV,Lotjuh")
	out := f.all()
	if !strings.Contains(out, "[Spreekwoord]") {
		t.Fatalf("no category: %q", out)
	}
	if strings.ContainsAny(out[strings.Index(out, "]"):], "WHLT") {
		t.Fatalf("board leaks letters: %q", out)
	}
	if !strings.Contains(out, "░") || !strings.Contains(out, "{B}{b}BenV{/} is aan de beurt") {
		t.Fatalf("board/turn missing: %q", out)
	}
}

func TestFullChannelGame(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.startGame("#radvanfortuin", "BenV,Lotjuh")
	f.take()

	// BenV spins fl. 1000 (wheel index 20) and calls the T: 7x = 7000
	f.rolls = []int{20}
	f.msg("BenV", "#radvanfortuin", "draai")
	if out := f.all(); !strings.Contains(out, "fl. 1000") || !strings.Contains(out, "medeklinker") {
		t.Fatalf("spin reply: %q", out)
	}
	f.msg("BenV", "#radvanfortuin", "t")
	out := f.all()
	if !strings.Contains(out, "7x T") || !strings.Contains(out, "fl. 7000") {
		t.Fatalf("hit reply: %q", out)
	}
	// still BenV's turn after a hit
	if !strings.Contains(out, "{B}{b}BenV{/} is aan de beurt") {
		t.Fatalf("turn moved after a hit: %q", out)
	}

	// buys the E (4x), then solves
	f.msg("BenV", "#radvanfortuin", "koop e")
	if out := f.all(); !strings.Contains(out, "4x E") {
		t.Fatalf("vowel reply: %q", out)
	}
	f.rolls = []int{0} // art variant
	f.msg("BenV", "#radvanfortuin", "los op: wie het laatst lacht, lacht het best")
	out = f.all()
	if !strings.Contains(out, "JUIST") || !strings.Contains(out, "fl. 6750") {
		t.Fatalf("win reply: %q", out)
	}
	if !strings.Contains(out, "toptien") && !strings.Contains(out, "Plek 1") {
		t.Fatalf("no hiscore mention: %q", out)
	}

	// the game is gone: new input is ignored
	f.msg("BenV", "#radvanfortuin", "draai")
	if out := f.all(); out != "" {
		t.Fatalf("dead game still replies: %q", out)
	}

	// and the leaderboard has the entry
	f.msg("Lotjuh", "#radvanfortuin", "!top10")
	if out := f.all(); !strings.Contains(out, "BenV") || !strings.Contains(out, "fl. 6750") {
		t.Fatalf("top10: %q", out)
	}
}

func TestMissPassesTurnToNextPlayer(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.startGame("#radvanfortuin", "BenV,Lotjuh")
	f.take()
	f.rolls = []int{20}
	f.msg("BenV", "#radvanfortuin", "draai")
	f.take()
	f.msg("BenV", "#radvanfortuin", "z")
	out := f.all()
	if !strings.Contains(out, "Geen Z") || !strings.Contains(out, "{B}{b}Lotjuh{/} is aan de beurt") {
		t.Fatalf("miss reply: %q", out)
	}
	// now BenV's game moves get the out-of-turn nudge
	f.msg("BenV", "#radvanfortuin", "draai")
	if out := f.all(); !strings.Contains(out, "Rustig") || !strings.Contains(out, "Lotjuh") {
		t.Fatalf("not-your-turn nudge: %q", out)
	}
	f.rolls = []int{20}
	f.msg("Lotjuh", "#radvanfortuin", "draai")
	if out := f.all(); !strings.Contains(out, "Lotjuh") {
		t.Fatalf("current player ignored: %q", out)
	}
}

func TestEnglishChannel(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.rolls = []int{0}
	f.msg("BenV", "#wheeloffortune", "!start BenV")
	out := f.all()
	if !strings.Contains(out, "[Phrase]") || !strings.Contains(out, "{B}{b}BenV{/} is up") {
		t.Fatalf("english start: %q", out)
	}
	f.rolls = []int{20}
	f.msg("BenV", "#wheeloffortune", "spin")
	if out := f.all(); !strings.Contains(out, "$1000") || !strings.Contains(out, "consonant") {
		t.Fatalf("english spin: %q", out)
	}
	// The early bird catches the worm: 3x R
	f.msg("BenV", "#wheeloffortune", "r")
	if out := f.all(); !strings.Contains(out, "3x R") || !strings.Contains(out, "$3000") {
		t.Fatalf("english hit: %q", out)
	}
	f.rolls = []int{2} // art variant
	f.msg("BenV", "#wheeloffortune", "solve the early bird catches the worm")
	if out := f.all(); !strings.Contains(out, "CORRECT") {
		t.Fatalf("english solve: %q", out)
	}
}

func TestQuerySoloGameAndStopPropagation(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	// a hook registered AFTER rvf (like markov) must not see live-game
	// input; register a canary
	var leaked []string
	f.b.RegisterHook("canary", "IRC_PRIVMSG", func(ev *bus.Event) (bus.Handled, any) {
		leaked = append(leaked, ev.Msg)
		return bus.None, nil
	})

	f.rolls = []int{0}
	f.query("BenV", "!start")
	if out := f.all(); !strings.Contains(out, "[Spreekwoord]") {
		t.Fatalf("query start: %q", out)
	}
	f.rolls = []int{20}
	f.query("BenV", "draai")
	if out := f.all(); !strings.Contains(out, "fl. 1000") {
		t.Fatalf("query spin: %q", out)
	}
	// mid-game noise gets usage, and markov-alikes never see it
	f.query("BenV", "talk to me")
	if out := f.all(); !strings.Contains(out, "medeklinker") && !strings.Contains(out, "draai") {
		t.Fatalf("query noise reply: %q", out)
	}
	for _, l := range leaked {
		if l != "!start" {
			t.Fatalf("game input leaked past rvf to later hooks: %q", l)
		}
	}
}

// an AFK player is dropped after missing their own turns; the game
// continues for whoever is left and only ends when nobody is.
func TestAfkPlayerDroppedGameContinues(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.startGame("#radvanfortuin", "BenV,Lotjuh")
	f.take()

	// BenV misses turn 1 -> pass; Lotjuh misses turn 1 -> pass
	f.clk = f.clk.Add(91 * time.Second)
	f.sch.RunDue()
	out := f.all()
	if !strings.Contains(out, "{B}{b}BenV{/} zit te slapen") || !strings.Contains(out, "Lotjuh") {
		t.Fatalf("timeout 1: %q", out)
	}
	f.clk = f.clk.Add(91 * time.Second)
	f.sch.RunDue()
	f.take()
	// BenV misses his second turn: dropped, Lotjuh plays on
	f.clk = f.clk.Add(91 * time.Second)
	f.sch.RunDue()
	out = f.all()
	if !strings.Contains(out, "doet niet meer mee") || !strings.Contains(out, "{B}{b}Lotjuh{/} is aan de beurt") {
		t.Fatalf("drop: %q", out)
	}
	g := f.m.games[gameKey("junerules", "#radvanfortuin")]
	if g == nil || len(g.Players) != 1 || !g.IsCurrent("Lotjuh") {
		t.Fatalf("game after drop: %+v", g)
	}
	// the survivor can still play and win
	f.rolls = []int{20}
	f.msg("Lotjuh", "#radvanfortuin", "draai")
	f.msg("Lotjuh", "#radvanfortuin", "t")
	f.take()
	f.rolls = []int{0}
	f.msg("Lotjuh", "#radvanfortuin", "los op: wie het laatst lacht lacht het best")
	if out := f.all(); !strings.Contains(out, "JUIST") {
		t.Fatalf("survivor cannot win: %q", out)
	}
}

// when the last player is dropped too, the game ends with the reveal.
func TestAllPlayersDroppedAborts(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.startGame("#radvanfortuin", "BenV")
	f.take()
	f.clk = f.clk.Add(91 * time.Second)
	f.sch.RunDue()
	f.take() // miss 1
	f.clk = f.clk.Add(91 * time.Second)
	f.sch.RunDue()
	out := f.all()
	if !strings.Contains(out, "spel gestopt") || !strings.Contains(out, "WIE HET LAATST LACHT") {
		t.Fatalf("abort: %q", out)
	}
	// no further timers fire
	f.clk = f.clk.Add(300 * time.Second)
	f.sch.RunDue()
	if out := f.all(); out != "" {
		t.Fatalf("dead game timer fired: %q", out)
	}
}

// a move resets that player's missed-turn counter.
func TestMoveResetsMissedTurns(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.startGame("#radvanfortuin", "BenV,Lotjuh")
	f.take()
	// BenV misses once, Lotjuh misses once, then BenV plays: his
	// counter resets, so his next miss is again only his "first"
	for i := 0; i < 2; i++ {
		f.clk = f.clk.Add(91 * time.Second)
		f.sch.RunDue()
	}
	f.take()
	f.msg("BenV", "#radvanfortuin", "pas")
	f.take()
	// Lotjuh misses a second time: dropped; BenV (reset) plays on
	f.clk = f.clk.Add(91 * time.Second)
	f.sch.RunDue()
	out := f.all()
	if !strings.Contains(out, "doet niet meer mee") {
		t.Fatalf("second miss should drop Lotjuh: %q", out)
	}
	g := f.m.games[gameKey("junerules", "#radvanfortuin")]
	if g == nil || len(g.Players) != 1 || !g.IsCurrent("BenV") {
		t.Fatalf("game after Lotjuh drop: %+v", g)
	}
	// BenV misses again: only miss #1 after the reset, still in
	f.clk = f.clk.Add(91 * time.Second)
	f.sch.RunDue()
	f.take()
	if g := f.m.games[gameKey("junerules", "#radvanfortuin")]; g == nil {
		t.Fatal("BenV dropped despite the counter reset")
	}
}

func TestStopCommand(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.startGame("#radvanfortuin", "BenV,Lotjuh")
	f.take()
	// a non-player cannot stop it
	f.msg("Verty", "#radvanfortuin", "!stop")
	if out := f.all(); !strings.Contains(out, "Alleen spelers") {
		t.Fatalf("non-player stop: %q", out)
	}
	f.msg("Lotjuh", "#radvanfortuin", "!stop")
	out := f.all()
	if !strings.Contains(out, "Spel gestopt") || !strings.Contains(out, "WIE HET LAATST LACHT") {
		t.Fatalf("stop: %q", out)
	}
}

func TestStartRefusedWhileGameRuns(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.startGame("#radvanfortuin", "BenV")
	f.take()
	f.msg("Lotjuh", "#radvanfortuin", "!start Lotjuh")
	if out := f.all(); !strings.Contains(out, "al een spel") {
		t.Fatalf("second start: %q", out)
	}
}

func TestGameSurvivesRestart(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	f.startGame("#radvanfortuin", "BenV,Lotjuh")
	f.rolls = []int{20}
	f.msg("BenV", "#radvanfortuin", "draai")
	f.msg("BenV", "#radvanfortuin", "t")
	f.take()

	// "restart": flush the dirty state, then a fresh module on the store
	if err := f.saver.FlushSync(); err != nil {
		t.Fatal(err)
	}
	f2 := newFixture(t, store)
	g := f2.m.games[gameKey("junerules", "#radvanfortuin")]
	if g == nil {
		t.Fatal("game not restored")
	}
	if g.Current().Nick != "BenV" || g.Current().Money != 7000 {
		t.Fatalf("restored state: %+v", g.Current())
	}
	// play on through the restored module
	f2.rolls = []int{0} // art variant
	f2.msg("BenV", "#radvanfortuin", "los op: wie het laatst lacht lacht het best")
	if out := f2.all(); !strings.Contains(out, "JUIST") {
		t.Fatalf("restored game not playable: %q", out)
	}
	// and its turn timer was re-armed
	f2.startGame("#radvanfortuin", "BenV")
	f2.take()
	f2.clk = f2.clk.Add(91 * time.Second)
	f2.sch.RunDue()
	if out := f2.all(); !strings.Contains(out, "zit te slapen") {
		t.Fatalf("restored timer: %q", out)
	}
}

func TestTelnetAddDelList(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	var spec admin.Spec
	for _, payload := range f.b.Submit(&bus.Event{Name: "COMMAND"}) {
		if s, ok := payload.(admin.Spec); ok && s.Name == "rvf" {
			spec = s
		}
	}
	if spec.Run == nil || !spec.Su {
		t.Fatalf("no su rvf admin spec: %+v", spec)
	}
	if out := spec.Run("", "rvf add nl Gezegde: Zo geel als boter"); !strings.Contains(out, "Added") {
		t.Fatalf("add: %q", out)
	}
	if out := spec.Run("", "rvf add nl Gezegde: Ongeldig teken é"); !strings.Contains(out, "Error") {
		t.Fatalf("diacritics accepted: %q", out)
	}
	if out := spec.Run("", "rvf list"); !strings.Contains(out, "1 extra") ||
		!strings.Contains(out, "Zo geel als boter") {
		t.Fatalf("list: %q", out)
	}
	// the added puzzle is at the end of the pool: script the roll there
	pool := f.m.pool("nl")
	f.rolls = []int{len(pool) - 1}
	f.msg("BenV", "#radvanfortuin", "!start")
	if out := f.all(); !strings.Contains(out, "[Gezegde]") {
		t.Fatalf("extra puzzle not in pool: %q", out)
	}
	if out := spec.Run("", "rvf del nl zo geel als boter"); !strings.Contains(out, "Deleted") {
		t.Fatalf("del: %q", out)
	}
}

// two winners sort by score, not by arrival.
func TestHiscoreOrdering(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	// game 1: Lotjuh wins small (fl. 50 spin, 7x T = 350)
	f.rolls = []int{0}
	f.msg("Lotjuh", "#radvanfortuin", "!start Lotjuh")
	f.rolls = []int{0} // wheel index 0 = fl. 50
	f.msg("Lotjuh", "#radvanfortuin", "draai")
	f.msg("Lotjuh", "#radvanfortuin", "t")
	f.rolls = []int{0} // art variant
	f.msg("Lotjuh", "#radvanfortuin", "los op: wie het laatst lacht lacht het best")
	f.take()

	// game 2: BenV wins big (fl. 1000 spin, 2x T = 2000). Roll 0 now
	// lands on the SECOND corpus entry: game 1's puzzle is recent.
	f.rolls = []int{0}
	f.msg("BenV", "#radvanfortuin", "!start BenV")
	f.rolls = []int{20}
	f.msg("BenV", "#radvanfortuin", "draai")
	f.msg("BenV", "#radvanfortuin", "t")
	f.rolls = []int{3} // art variant
	f.msg("BenV", "#radvanfortuin", "los op: de appel valt niet ver van de boom")
	out := f.all()
	if !strings.Contains(out, "Plek 1") {
		t.Fatalf("the bigger win should enter at position 1: %q", out)
	}

	f.msg("Verty", "#radvanfortuin", "!top10")
	out = f.all()
	benv, lotjuh := strings.Index(out, "BenV"), strings.Index(out, "Lotjuh")
	if benv < 0 || lotjuh < 0 || benv > lotjuh {
		t.Fatalf("top10 not sorted by score: %q", out)
	}
}

// channel games only run in the dedicated game channels; queries are
// always fine.
func TestStartRefusedOutsideGameChannels(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	for _, start := range []string{"!start", "!rvf"} {
		f.msg("BenV", "#testing", start)
		out := f.all()
		if !strings.Contains(out, "#radvanfortuin") {
			t.Fatalf("%s in #testing should point at the game channels: %q", start, out)
		}
		if len(f.m.games) != 0 {
			t.Fatalf("%s created a game outside the game channels", start)
		}
	}
}

// !rvf starts a game too (Bram-in-query ergonomics), and query games
// are always solo: only the query owner can ever speak there, so a
// player list would deadlock on the first turn pass.
func TestRvfAliasAndQuerySolo(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.rolls = []int{0}
	f.query("BenV", "!rvf")
	if out := f.all(); !strings.Contains(out, "[Spreekwoord]") {
		t.Fatalf("!rvf in query: %q", out)
	}
	g := f.m.games[gameKey("junerules", "BenV")]
	if g == nil || len(g.Players) != 1 {
		t.Fatalf("query game players = %+v", g)
	}
	f.msg("BenV", "BenV", "!stop") // cleanup via the query channel key
	f.take()

	f.rolls = []int{0}
	f.query("BenV", "!start Bram,Piet")
	f.take()
	g = f.m.games[gameKey("junerules", "BenV")]
	if g == nil || len(g.Players) != 1 || !g.IsCurrent("BenV") {
		t.Fatalf("query start with a player list should force solo: %+v", g)
	}
}

// query timeouts stay quiet (no "zit te slapen" spam at yourself); the
// final abort still reveals the solution.
func TestQueryTimeoutSilent(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.rolls = []int{0}
	f.query("BenV", "!start")
	f.take()

	f.clk = f.clk.Add(91 * time.Second)
	f.sch.RunDue()
	if out := f.all(); out != "" {
		t.Fatalf("query timeout spoke up: %q", out)
	}
	// second miss drops the only player: reveal and clean up
	f.clk = f.clk.Add(91 * time.Second)
	f.sch.RunDue()
	out := f.all()
	if !strings.Contains(out, "WIE HET LAATST LACHT") {
		t.Fatalf("query abort should reveal the solution: %q", out)
	}
	if _, ok := f.m.games[gameKey("junerules", "BenV")]; ok {
		t.Fatal("expired query game not cleaned up")
	}
}

// !commands in a query mid-game belong to the command registry: the
// game's query-noise fallback must not butt in with a help line first
// (live wart: "!stop" got "Zeg draai, ..." before "Spel gestopt").
func TestQueryCommandsSkipTheHelpLine(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.rolls = []int{0}
	f.query("BenV", "!rvf")
	f.take()
	f.query("BenV", "!stop")
	out := f.take()
	if len(out) != 1 || !strings.Contains(out[0], "Spel gestopt") {
		t.Fatalf("query !stop = %q, want only the stop", out)
	}
}

func TestSpinFirstHint(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.startGame("#radvanfortuin", "BenV")
	f.take()
	f.rolls = []int{1} // fun-line variant
	f.msg("BenV", "#radvanfortuin", "t")
	if out := f.all(); !strings.Contains(out, "draai") {
		t.Fatalf("letter before spin: %q", out)
	}
}

// games persisted before the Query flag existed are healed at load:
// a nick-keyed game is a query game (BenV got "zit te slapen" in his
// query from a game that predated the flag).
func TestRestoredNickKeyGameIsQuery(t *testing.T) {
	store := storage.NewMemory()
	games := map[string]*Game{
		"junerules benv":           NewGame("nl", "Gezegde", "RUST ROEST", []string{"BenV"}),
		"junerules #radvanfortuin": NewGame("nl", "Gezegde", "RUST ROEST", []string{"BenV"}),
	}
	if err := store.Put("rvf", "games", games); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, store)
	if g := f.m.games["junerules benv"]; g == nil || !g.Query {
		t.Fatalf("nick-keyed game not healed to query: %+v", g)
	}
	if g := f.m.games["junerules #radvanfortuin"]; g == nil || g.Query {
		t.Fatalf("channel game wrongly marked query: %+v", g)
	}
}

// a PLAYER using game verbs out of turn gets a nudge (Bram shouting
// DRAAI at a game stuck on BenV's turn, 2026-07-14); spectators and
// plain chatter stay ignored.
func TestNotYourTurnNudge(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.startGame("#radvanfortuin", "BenV,Lotjuh")
	f.take()
	f.msg("Lotjuh", "#radvanfortuin", "DRAAI")
	out := f.all()
	if !strings.Contains(out, "{B}{b}BenV{/}") || !strings.Contains(out, "beurt") {
		t.Fatalf("player nudge: %q", out)
	}
	// plain chatter from the same player: silence
	f.msg("Lotjuh", "#radvanfortuin", "kutspel!")
	if out := f.all(); out != "" {
		t.Fatalf("chatter answered: %q", out)
	}
	// a spectator using game verbs: silence too
	f.msg("Verty", "#radvanfortuin", "draai")
	if out := f.all(); out != "" {
		t.Fatalf("spectator answered: %q", out)
	}
}

// the board (with the wrong guesses so far) is visible on EVERY turn
// change, not only on hits: the 15:32 game had six misses in a row
// with an invisible board and Bram begging "laat het woord dan zien".
func TestBoardAlwaysVisible(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.startGame("#radvanfortuin", "BenV,Lotjuh")
	f.take()

	// a miss shows the board and the wrong letter
	f.rolls = []int{20}
	f.msg("BenV", "#radvanfortuin", "draai")
	f.take()
	f.msg("BenV", "#radvanfortuin", "z")
	out := f.all()
	if !strings.Contains(out, "░") || !strings.Contains(out, "fout: Z") {
		t.Fatalf("miss reply lacks board/wrong letters: %q", out)
	}
	// a wrong solve shows the board
	f.msg("Lotjuh", "#radvanfortuin", "los op: iets heel anders")
	if out := f.all(); !strings.Contains(out, "░") {
		t.Fatalf("wrong solve lacks the board: %q", out)
	}
	// a pass shows the board
	f.msg("BenV", "#radvanfortuin", "pas")
	if out := f.all(); !strings.Contains(out, "░") {
		t.Fatalf("pass lacks the board: %q", out)
	}
	// a timeout shows the board
	f.clk = f.clk.Add(91 * time.Second)
	f.sch.RunDue()
	if out := f.all(); !strings.Contains(out, "░") {
		t.Fatalf("timeout lacks the board: %q", out)
	}
	// wrong letters accumulate sorted, dups do not repeat
	f.rolls = []int{20}
	f.msg("BenV", "#radvanfortuin", "draai")
	f.take()
	f.msg("BenV", "#radvanfortuin", "q")
	out = f.all()
	if !strings.Contains(out, "fout: Q Z") {
		t.Fatalf("wrong letters not accumulated sorted: %q", out)
	}
	// a HIT letter is on the board, not in the wrong list
	f.rolls = []int{20}
	f.msg("Lotjuh", "#radvanfortuin", "draai")
	f.take()
	f.msg("Lotjuh", "#radvanfortuin", "t")
	out = f.all()
	if !strings.Contains(out, "fout: Q Z") || strings.Contains(out, "fout: Q T Z") {
		t.Fatalf("hit letter leaked into the wrong list: %q", out)
	}
}

// bankrupt and the other specials show the board too.
func TestBoardOnSpecials(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.startGame("#radvanfortuin", "BenV,Lotjuh")
	f.take()
	f.rolls = []int{segIndex(t, SegBankrupt, 0)}
	f.msg("BenV", "#radvanfortuin", "draai")
	out := f.all()
	if !strings.Contains(out, "BANKROET") || !strings.Contains(out, "░") {
		t.Fatalf("bankrupt lacks the board: %q", out)
	}
}

// "!start BenV, Ventiel draai" (Ventiel live 2026-07-15): the eager
// trailing verb became a phantom player the game then stalled on for
// two timeout rounds. Game words are not nicks.
func TestStartRefusesGameVerbsAsPlayers(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("Ventiel", "#radvanfortuin", "!start BenV, Ventiel draai")
	out := f.all()
	if strings.Contains(out, "aan de beurt") {
		t.Fatalf("game started with a verb as player: %q", out)
	}
	if len(f.m.games) != 0 {
		t.Fatal("refused start left a game behind")
	}
	// the english verbs are blocked in english channels too
	f.msg("Bram", "#wheeloffortune", "!start Bram, spin")
	if out := f.all(); strings.Contains(out, "is up") || len(f.m.games) != 0 {
		t.Fatalf("english verb accepted as player: %q", out)
	}
	// and a clean start still works
	f.startGame("#radvanfortuin", "BenV,Ventiel")
	if out := f.all(); !strings.Contains(out, "aan de beurt") {
		t.Fatalf("clean start blocked: %q", out)
	}
}

// A fl. 0 win is not a hiscore ("Plek 4 in de toptien!" for zero
// guilders, BenV live 2026-07-15).
func TestZeroScoreWinSkipsHiscores(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.startGame("#radvanfortuin", "BenV")
	f.take()
	f.rolls = []int{0} // art variant
	f.msg("BenV", "#radvanfortuin", "los op: wie het laatst lacht lacht het best")
	out := f.all()
	if !strings.Contains(out, "JUIST") {
		t.Fatalf("solve failed: %q", out)
	}
	if strings.Contains(out, "Plek") {
		t.Fatalf("zero win entered the toptien: %q", out)
	}
	f.msg("BenV", "#radvanfortuin", "!top10")
	if out := f.all(); !strings.Contains(out, "Nog geen winnaars") {
		t.Fatalf("top10 not empty after a zero win: %q", out)
	}
}

// The toptien shows what they won with ("die top10 moet ook de spreuk
// erbij waar ze mee wonnen", BenV live 2026-07-15). Entries from before
// this field render without one.
func TestTop10ShowsWinningPuzzle(t *testing.T) {
	store := storage.NewMemory()
	if err := store.Put("rvf", "hiscores", []map[string]any{
		{"nick": "Oudje", "channel": "#radvanfortuin", "lang": "nl", "score": 100, "when": 1},
	}); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, store)
	f.startGame("#radvanfortuin", "BenV")
	f.take()
	f.rolls = []int{20}
	f.msg("BenV", "#radvanfortuin", "draai")
	f.msg("BenV", "#radvanfortuin", "t")
	f.rolls = []int{0} // art variant
	f.msg("BenV", "#radvanfortuin", "los op: wie het laatst lacht lacht het best")
	f.take()
	f.msg("BenV", "#radvanfortuin", "!top10")
	out := f.all()
	if !strings.Contains(out, "WIE HET LAATST LACHT LACHT HET BEST") {
		t.Fatalf("top10 missing the winning puzzle: %q", out)
	}
	if !strings.Contains(out, "Oudje") {
		t.Fatalf("legacy entry without puzzle lost: %q", out)
	}
}

// The same puzzle two games in a row (HET IS KOEK EN EI on 2026-07-14
// AND the next morning, "bad random seed or verdomd toeval"): it was
// toeval, but fresh games now skip recently played puzzles.
func TestRecentPuzzlesNotRepeated(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	f.startGame("#radvanfortuin", "BenV")
	f.take()
	first := f.m.games[gameKey("junerules", "#radvanfortuin")].Puzzle
	f.msg("BenV", "#radvanfortuin", "!stop")
	f.take()

	f.startGame("#radvanfortuin", "BenV") // roll 0 again
	second := f.m.games[gameKey("junerules", "#radvanfortuin")].Puzzle
	if second == first {
		t.Fatalf("same puzzle twice in a row: %q", first)
	}
	f.msg("BenV", "#radvanfortuin", "!stop")
	f.take()

	// the recent list survives a restart
	if err := f.saver.FlushSync(); err != nil {
		t.Fatal(err)
	}
	f2 := newFixture(t, store)
	f2.startGame("#radvanfortuin", "BenV")
	third := f2.m.games[gameKey("junerules", "#radvanfortuin")].Puzzle
	if third == first || third == second {
		t.Fatalf("recent puzzle repeated after restart: %q %q %q", first, second, third)
	}
}

// When every puzzle is recent (tiny extra-only pools, or a corpus
// shrink) the filter falls back to the full pool instead of refusing.
func TestRecentPuzzlesExhaustedFallsBack(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	for _, p := range f.m.pool("nl") {
		f.m.recent["nl"] = append(f.m.recent["nl"], p.Text)
	}
	f.startGame("#radvanfortuin", "BenV")
	if out := f.all(); !strings.Contains(out, "aan de beurt") {
		t.Fatalf("exhausted recent list blocked the start: %q", out)
	}
}

// The recent list stays capped: oldest entries fall off.
func TestRecentPuzzlesCapped(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	for i := range recentMax {
		f.m.recent["nl"] = append(f.m.recent["nl"], fmt.Sprintf("fake %d", i))
	}
	f.startGame("#radvanfortuin", "BenV")
	f.take()
	got := f.m.recent["nl"]
	if len(got) != recentMax {
		t.Fatalf("recent list not capped: %d entries", len(got))
	}
	if got[0] != "fake 1" || !strings.Contains(got[recentMax-1], "Wie het laatst") {
		t.Fatalf("recent list not FIFO: first %q last %q", got[0], got[recentMax-1])
	}
}
