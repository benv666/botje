package markov

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/conf"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
	"go-botje/internal/sched"
	"go-botje/internal/storage"
)

type fixture struct {
	m    *Module
	b    *bus.Bus
	cmds *cmd.Registry
	cf   *conf.Conf
	sch  *sched.Sched
	clk  time.Time
	sent []string
	rand []float64
	ri   int
}

func newFixture(t *testing.T, store storage.Store) *fixture {
	t.Helper()
	f := &fixture{clk: time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)}
	f.b = bus.New()
	f.b.RegisterEvent("IRC_PRIVMSG")
	f.cmds = cmd.New()
	f.cf = conf.New()
	// order 1 keeps the chains single-choice so tests are deterministic
	f.cf.LoadStored(map[string]string{"markov_order": "1"})
	f.sch = sched.New(func() time.Time { return f.clk })
	f.m = New()
	f.m.Now = func() time.Time { return f.clk }
	f.m.Rand = func() float64 {
		if f.ri >= len(f.rand) {
			return 0.9999 // never random-fail, pick last bucket
		}
		v := f.rand[f.ri]
		f.ri++
		return v
	}
	err := f.m.Load(&module.Context{
		Bus: f.b, Cmd: f.cmds, Conf: f.cf, Store: store, Sched: f.sch,
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
	if channel == nick {
		ev.Query = true
	}
	f.b.Submit(ev)
	f.cmds.Handle(ev)
}

func (f *fixture) take() []string {
	s := f.sent
	f.sent = nil
	return s
}

func TestLearnsAndTalksDeterministically(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "aap noot mies.")

	// walking from "aap": each level has exactly one choice, rand 0.9999
	// avoids the random-fail, so the whole sentence comes back
	f.msg("BenV", "#testing", "!talk aap")
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|Aap noot mies." {
		t.Fatalf("talk = %q, want deterministic aap noot mies.", got)
	}
}

func TestPunctuationSplitAndNormalization(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "hoi, wereld!")
	// learned tokens: hoi , wereld !  -> regenerating joins without
	// space before punctuation and capitalizes
	f.msg("BenV", "#testing", "!talk hoi")
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|Hoi, wereld!" {
		t.Fatalf("talk = %q", got)
	}
}

func TestSentenceGetsClosingDot(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "zin zonder einde")
	f.msg("BenV", "#testing", "!talk zin")
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|Zin zonder einde." {
		t.Fatalf("talk = %q, want trailing dot added at learn time", got)
	}
}

func TestBadEndNoDotRunsAway(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "dit eindigt op de")
	// "de" is a bad end: no dot appended, so this tiny chain never
	// reaches an EoL and the runaway guard closes with "...."
	f.msg("BenV", "#testing", "!talk dit")
	got := f.take()
	if len(got) != 1 || !strings.HasSuffix(got[0], "....") {
		t.Fatalf("talk = %q, want runaway cap", got)
	}
}

func TestEmptyDictionary(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "!talk")
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|*BLLEUEURRURUHGHG*." {
		t.Fatalf("empty dict talk = %q", got)
	}
}

func TestNickCapitalized(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("Someone", "#testing", "benv maakt bots.")
	f.msg("Someone", "#testing", "!talk benv")
	got := f.take()
	if len(got) != 1 || !strings.HasPrefix(got[0], "#testing|Benv maakt") {
		t.Fatalf("talk = %q, want nick capitalized", got)
	}
}

func TestUnknownSeedPicksRandom(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "enige zin hier.")
	f.rand = []float64{0} // random chain word: first key
	f.ri = 0
	f.msg("BenV", "#testing", "!talk onbekendwoord")
	got := f.take()
	if len(got) != 1 || strings.Contains(got[0], "onbekendwoord") {
		t.Fatalf("talk = %q, unknown seed must not appear", got)
	}
}

// The variety roll: a low rand drops the chain order and picks a
// random dictionary word instead of the deterministic next.
func TestVarietyRollDropsOrder(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "aap noot mies.")
	f.rand = []float64{0.001, 0.0} // roll fails order 1, random picks keys[0]
	f.ri = 0
	f.msg("BenV", "#testing", "!talk aap")
	got := f.take()
	want := "#testing|Aap aap noot mies."
	if len(got) != 1 || got[0] != want {
		t.Fatalf("talk = %q, want %q (random word injected)", got, want)
	}
}

func TestBotsIgnored(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("hoer", "#testing", "botpraat hier.")
	f.msg("somebot", "#testing", "meer botpraat.")
	f.msg("X", "#testing", "undernet service.")
	f.msg("BenV", "#testing", "!talk botpraat")
	got := f.take()
	if len(got) != 1 || strings.Contains(got[0], "botpraat") {
		t.Fatalf("talk = %q, bot lines must not be learned", got)
	}
}

func TestRegisteredCommandsNotLearned(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.cmds.Register("other", "login", func(*cmd.Data) bool { return true })
	f.msg("BenV", "#testing", "!login geheim wachtwoord")
	f.msg("BenV", "#testing", "!talk geheim")
	got := f.take()
	if len(got) != 1 || strings.Contains(got[0], "wachtwoord") {
		t.Fatalf("talk = %q, command lines must not be learned", got)
	}
}

func TestUnknownCommandTriggersTalk(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "aap noot mies.")
	f.take()
	f.msg("BenV", "#testing", "!jemoeder")
	got := f.take()
	if len(got) != 1 || !strings.HasPrefix(got[0], "#testing|") {
		t.Fatalf("unknown command talk = %q", got)
	}
	if strings.Contains(got[0], "Maybe you meant") {
		t.Fatalf("suggestion shown instead of talk: %q", got)
	}
}

func TestQueryTalk(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("Someone", "#testing", "aap noot mies.")
	f.take()
	f.msg("BenV", "BenV", "talk aap")
	got := f.take()
	if len(got) != 1 || got[0] != "BenV|Aap noot mies." {
		t.Fatalf("query talk = %q", got)
	}
}

func TestPersistedEveryFiftyLines(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	// the perl post-decrement saves on the 51st line
	for i := range 51 {
		f.msg("BenV", "#testing", fmt.Sprintf("zin nummer %d hier.", i))
	}
	f2 := newFixture(t, store)
	f2.msg("BenV", "#testing", "!talk zin")
	got := f2.take()
	if len(got) != 1 || !strings.HasPrefix(got[0], "#testing|Zin nummer") {
		t.Fatalf("restored talk = %q", got)
	}
}

func TestUnloadSaves(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	f.msg("BenV", "#testing", "bewaar dit.")
	f.m.Unload()
	f2 := newFixture(t, store)
	f2.msg("BenV", "#testing", "!talk bewaar")
	if got := f2.take(); len(got) != 1 || got[0] != "#testing|Bewaar dit." {
		t.Fatalf("restored talk = %q", got)
	}
}

func TestIdleTalker(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.cf.Set("markov_idle_talk", "true")
	f.cf.Set("markov_idle_talk_timeout", "10") // minutes
	f.cf.Set("markov_idle_talk_channels", "#testing #idle")

	f.msg("BenV", "#testing", "stilte voor de storm.")
	f.take()
	f.clk = f.clk.Add(10 * time.Minute)
	f.sch.RunDue()
	got := f.take()
	if len(got) != 1 || !strings.HasPrefix(got[0], "#testing|") {
		t.Fatalf("idle talk = %q", got)
	}
	// and it reschedules itself
	f.clk = f.clk.Add(10 * time.Minute)
	f.sch.RunDue()
	if got := f.take(); len(got) != 1 {
		t.Fatalf("second idle talk = %q", got)
	}
}

func TestIdleTalkerActivityResets(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.cf.Set("markov_idle_talk", "true")
	f.cf.Set("markov_idle_talk_timeout", "10")
	f.cf.Set("markov_idle_talk_channels", "#testing")

	f.msg("BenV", "#testing", "eerste bericht hier.")
	f.clk = f.clk.Add(9 * time.Minute)
	f.sch.RunDue()
	f.msg("BenV", "#testing", "nog steeds actief.")
	f.take()
	f.clk = f.clk.Add(9 * time.Minute)
	f.sch.RunDue()
	if got := f.take(); len(got) != 0 {
		t.Fatalf("idle talk fired despite activity: %q", got)
	}
}

func TestIdleTalkerOffByDefault(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "gewoon een zin.")
	f.take()
	f.clk = f.clk.Add(24 * time.Hour)
	f.sch.RunDue()
	if got := f.take(); len(got) != 0 {
		t.Fatalf("idle talker on by default: %q", got)
	}
}

func TestRunawayGenerationCapped(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	// a -> a -> a ... never reaches EoL naturally
	f.msg("BenV", "#testing", strings.Repeat("la ", 80)+"la.")
	f.msg("BenV", "#testing", "!talk la")
	got := f.take()
	if len(got) != 1 {
		t.Fatalf("talk = %q", got)
	}
	words := strings.Fields(got[0])
	if len(words) > 60 {
		t.Fatalf("runaway generation: %d words", len(words))
	}
}
