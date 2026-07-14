package markov

import (
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
	m     *Module
	b     *bus.Bus
	cmds  *cmd.Registry
	cf    *conf.Conf
	sch   *sched.Sched
	saver *storage.Saver
	clk   time.Time
	sent  []string
	rand  []float64
	ri    int
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
	f.saver = storage.NewSaver(store,
		func(fn func()) { fn() }, // tests flush synchronously only
		func(err error) { t.Errorf("saver: %v", err) })
	err := f.m.Load(&module.Context{
		Bus: f.b, Cmd: f.cmds, Conf: f.cf, Store: store, Sched: f.sch,
		Saver:   f.saver,
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

func TestBadEndTerminatesCleanly(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "dit eindigt op de")
	// "de" is a bad end: no dot is appended, but the END sentinel still
	// terminates the walk. Before the sentinels this chain dead-ended
	// and the runaway guard produced "*BLLEUEURRURUHGHG*...." noise.
	f.msg("BenV", "#testing", "!talk dit eindigt")
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|Dit eindigt op de" {
		t.Fatalf("talk = %q, want the clean bad-end sentence", got)
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
	// two-word seed keeps this on the plain forward walk (a single
	// word would go middle-out); roll fails order 1, the random pick
	// lands on keys[1] = "aap"
	f.rand = []float64{0.001, 0.3}
	f.ri = 0
	f.msg("BenV", "#testing", "!talk aap noot")
	got := f.take()
	want := "#testing|Aap noot aap noot mies."
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

// Learning marks the touched top words dirty in the saver; a flush
// persists them as one row per word (the 2026-07-13 rewrite of the
// whole-dictionary blob whose put froze the dispatcher for ~2s), and a
// fresh module loads them back.
func TestPersistPerWordViaSaver(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	f.msg("BenV", "#testing", "zin nummer een hier.")
	if err := f.saver.FlushSync(); err != nil {
		t.Fatal(err)
	}

	names, err := store.Names("markov")
	if err != nil {
		t.Fatal(err)
	}
	var wordRows int
	for _, n := range names {
		if n == "dictionary_1_default" {
			t.Fatal("whole-dictionary blob written; want per-word rows only")
		}
		if strings.HasPrefix(n, "dictionary_1_default:") {
			wordRows++
		}
	}
	// top-level words: START hier zin nummer een "." (the dot heads
	// the final window now that END follows it; END itself is only
	// ever a child)
	if wordRows != 6 {
		t.Fatalf("word rows = %d (%v), want 6", wordRows, names)
	}

	f2 := newFixture(t, store)
	f2.msg("BenV", "#testing", "!talk zin")
	got := f2.take()
	if len(got) != 1 || !strings.HasPrefix(got[0], "#testing|Zin nummer") {
		t.Fatalf("restored talk = %q", got)
	}
}

// Marks survive a module unload: the core flushes the saver at
// shutdown, after modules are gone.
func TestMarksSurviveUnload(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	f.msg("BenV", "#testing", "bewaar dit.")
	f.m.Unload()
	if err := f.saver.FlushSync(); err != nil {
		t.Fatal(err)
	}
	f2 := newFixture(t, store)
	f2.msg("BenV", "#testing", "!talk bewaar")
	if got := f2.take(); len(got) != 1 || got[0] != "#testing|Bewaar dit." {
		t.Fatalf("restored talk = %q", got)
	}
}

// A pre-rewrite store holds the whole dictionary as one blob; loading
// splits it into per-word rows once and deletes the blob.
func TestLegacyBlobMigration(t *testing.T) {
	store := storage.NewMemory()
	blob := map[string]*Node{
		"aap":  {Count: 1, Children: map[string]*Node{"noot": {Count: 1, Children: map[string]*Node{"mies": {Count: 1}}}}},
		"noot": {Count: 1, Children: map[string]*Node{"mies": {Count: 1, Children: map[string]*Node{".": {Count: 1}}}}},
		"mies": {Count: 1, Children: map[string]*Node{".": {Count: 1}}},
		".":    {Count: 1},
	}
	if err := store.Put("markov", "dictionary_1_default", blob); err != nil {
		t.Fatal(err)
	}

	f := newFixture(t, store)
	if ok, _ := store.Get("markov", "dictionary_1_default", new(map[string]*Node)); ok {
		t.Fatal("legacy blob survived the migration")
	}
	// the blob lives on as a backup so a rolled-back build can be fed
	if ok, _ := store.Get("markov", "dictionary_1_default_blob_backup", new(map[string]*Node)); !ok {
		t.Fatal("no blob backup written")
	}
	var nd Node
	if ok, _ := store.Get("markov", "dictionary_1_default:aap", &nd); !ok || nd.Children["noot"] == nil {
		t.Fatalf("migrated row aap = %v %+v", ok, nd)
	}
	f.msg("BenV", "#testing", "!talk aap")
	if got := f.take(); len(got) != 1 || got[0] != "#testing|Aap noot mies." {
		t.Fatalf("talk from migrated dictionary = %q", got)
	}

	// a second boot must not re-migrate or lose anything
	f2 := newFixture(t, store)
	f2.msg("BenV", "#testing", "!talk aap")
	if got := f2.take(); len(got) != 1 || got[0] != "#testing|Aap noot mies." {
		t.Fatalf("talk on second boot = %q", got)
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

// every talker learns into their own dictionary too: !talklike speaks
// from that dict only.
func TestTalkLike(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("Bram", "#testing", "kaas is heerlijk.")
	f.msg("Willow", "#testing", "thee is beter.")

	f.msg("BenV", "#testing", "!talklike bram kaas")
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|Kaas is heerlijk." {
		t.Fatalf("talklike bram = %q", got)
	}
	// nick lookup is case-insensitive
	f.msg("BenV", "#testing", "!talklike WILLOW thee")
	got = f.take()
	if len(got) != 1 || got[0] != "#testing|Thee is beter." {
		t.Fatalf("talklike WILLOW = %q", got)
	}
	// an unknown nick explains itself
	f.msg("BenV", "#testing", "!talklike niemand")
	got = f.take()
	if len(got) != 1 || !strings.Contains(got[0], "niks geleerd") {
		t.Fatalf("talklike unknown = %q", got)
	}
	// and bare !talklike gives usage
	f.msg("BenV", "#testing", "!talklike")
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0], "Gebruik") {
		t.Fatalf("talklike usage = %q", got)
	}
}

// nick dictionaries persist as their own rows and load back grouped.
func TestNickDictsPersistAcrossLoads(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	f.msg("Bram", "#testing", "kaas is heerlijk.")
	if err := f.saver.FlushSync(); err != nil {
		t.Fatal(err)
	}

	f2 := newFixture(t, store)
	f2.msg("BenV", "#testing", "!talklike bram kaas")
	got := f2.take()
	if len(got) != 1 || got[0] != "#testing|Kaas is heerlijk." {
		t.Fatalf("restored talklike = %q", got)
	}
	// the global dict learned the same line
	f2.msg("BenV", "#testing", "!talk kaas")
	if got := f2.take(); len(got) != 1 || got[0] != "#testing|Kaas is heerlijk." {
		t.Fatalf("restored global talk = %q", got)
	}
}

// LearnLine is the offline entry point for the bvs bootstrap: same
// sanitizer, same windows, counts land in the passed chains.
func TestLearnLineOffline(t *testing.T) {
	chains := make(map[string]*Node)
	touched := LearnLine(chains, 1, "aap noot mies.")
	if len(touched) == 0 {
		t.Fatal("no touched words")
	}
	if chains["aap"] == nil || chains["aap"].Children["noot"] == nil {
		t.Fatalf("chains = %+v", chains)
	}
	if chains["aap"].Count != 1 || chains["aap"].Children["noot"].Count != 1 {
		t.Fatalf("counts off: %+v", chains["aap"])
	}
}

// mIRC control codes are stripped at learn time: no color junk in the
// dictionary, and nobody can forge a sentence sentinel from IRC.
func TestControlCharsStripped(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "\x0304rood\x03 en \x02vet\x02 spul.")
	f.msg("BenV", "#testing", "!talk rood")
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|Rood en vet spul." {
		t.Fatalf("talk = %q, want the cleaned words", got)
	}
}

// unseeded talk opens at a learned sentence start, not at a uniformly
// random dictionary word.
func TestUnseededTalkOpensAtSentenceStart(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "aap noot mies.")
	f.msg("BenV", "#testing", "!talk")
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|Aap noot mies." {
		t.Fatalf("unseeded talk = %q, want the sentence from its start", got)
	}
}

// a single-word seed goes middle-out: backwards from the seed to the
// sentence start, then forward to the end. Seeding a MIDDLE word
// reconstructs the whole sentence.
func TestMiddleOutTalk(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "aap noot mies.")
	f.msg("BenV", "#testing", "!talk noot")
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|Aap noot mies." {
		t.Fatalf("middle-out talk = %q, want the full sentence", got)
	}
}

// a store with forward chains but no reverse dictionary (the live
// global: years of data that predate the reverse dict) derives it at
// load, exactly, and persists the result.
func TestReverseDerivedAtLoad(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	f.msg("BenV", "#testing", "aap noot mies.")
	f.msg("BenV", "#testing", "kort en klein")
	if err := f.saver.FlushSync(); err != nil {
		t.Fatal(err)
	}
	// wipe the reverse rows: this store now looks like the live one
	names, err := store.Names("markov")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if strings.HasPrefix(n, "dictionary_1_reverse_") {
			if err := store.Delete("markov", n); err != nil {
				t.Fatal(err)
			}
		}
	}

	f2 := newFixture(t, store)
	f2.msg("BenV", "#testing", "!talk noot")
	got := f2.take()
	if len(got) != 1 || got[0] != "#testing|Aap noot mies." {
		t.Fatalf("middle-out after derivation = %q", got)
	}
	// and the derived rows were persisted for the next boot
	names, _ = store.Names("markov")
	revRows := 0
	for _, n := range names {
		if strings.HasPrefix(n, "dictionary_1_reverse_") {
			revRows++
		}
	}
	if revRows == 0 {
		t.Fatal("derived reverse dictionary not persisted")
	}
}
