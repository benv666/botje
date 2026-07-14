// Package rvf is Rad van Fortuin (and Wheel of Fortune): the bot as
// gamekeeper, modeled on the 1990 RTL4 show. !start p1,p2,... runs a
// turn-based channel game (or a solo game, also in queries); the
// current player says draai/koop <klinker>/los op <zin>/pas (spin/buy/
// solve/pass in english channels, conf rvf_channels_en). The wheel is
// the documented 24-segment RTL4 rad incl. bankroet, verliesbeurt and
// the joker. Turns time out (conf rvf_turn_seconds); too many
// consecutive timeouts abort the game. Winners enter a persistent
// top-10 (nick/channel/score). Puzzles: embedded corpus per language
// plus telnet-added extras (admin: rvf add/del/list).
//
// The module must load BEFORE markov: its IRC_PRIVMSG hook returns
// bus.Stop for live-game input, which keeps markov from learning
// letter spam and from query-talking at a player mid-game.
package rvf

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"go-botje/internal/admin"
	"go-botje/internal/bus"
	"go-botje/internal/format"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
)

// Score is one top-10 entry.
type Score struct {
	Nick    string `json:"nick"`
	Channel string `json:"channel"`
	Lang    string `json:"lang"`
	Score   int    `json:"score"`
	When    int64  `json:"when"`
}

// Module implements the game keeper.
type Module struct {
	ctx *module.Context

	// Now and Rand are injectable for tests. Rand returns [0, n).
	Now  func() time.Time
	Rand func(n int) int

	games    map[string]*Game // key: server + " " + lower(channel)
	gen      map[string]int   // turn-timer generations, stale timers no-op
	hiscores []Score
	extra    map[string][]Puzzle // lang -> telnet-added puzzles
	unloaded bool
}

// New returns the module.
func New() *Module {
	return &Module{
		games: make(map[string]*Game),
		gen:   make(map[string]int),
		extra: make(map[string][]Puzzle),
	}
}

// Name implements module.Module.
func (m *Module) Name() string { return "rvf" }

func (m *Module) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Load implements module.Module.
func (m *Module) Load(ctx *module.Context) error {
	m.ctx = ctx
	ctx.Conf.CreateInt("rvf_vowel_cost", 250)
	ctx.Conf.CreateInt("rvf_turn_seconds", 90)
	ctx.Conf.CreateInt("rvf_max_missed_turns", 2)
	ctx.Conf.CreateString("rvf_channels_nl", "#radvanfortuin")
	ctx.Conf.CreateString("rvf_channels_en", "#wheeloffortune")

	ctx.Cmd.Register(m.Name(), "start", m.cbStart)
	ctx.Cmd.Register(m.Name(), "rvf", m.cbStart) // query ergonomics: !rvf works too
	ctx.Cmd.Register(m.Name(), "stop", m.cbStop)
	ctx.Cmd.Register(m.Name(), "top10", m.cbTop10)
	for event, hook := range map[string]bus.Handler{
		"IRC_PRIVMSG": m.onPrivmsg,
		"IRC_JOIN":    m.onJoin,
		"IRC_TOPIC":   m.onTopic,
		"COMMAND":     m.adminSpec,
	} {
		if err := ctx.Bus.RegisterHook(m.Name(), event, hook); err != nil {
			return err
		}
	}

	ctx.Store.Get(m.Name(), "games", &m.games)
	ctx.Store.Get(m.Name(), "hiscores", &m.hiscores)
	ctx.Store.Get(m.Name(), "puzzles", &m.extra)
	if m.games == nil {
		m.games = make(map[string]*Game)
	}
	if m.extra == nil {
		m.extra = make(map[string][]Puzzle)
	}
	// restored games get a fresh full turn timer; nick-keyed games are
	// query games whatever the stored flag says (games persisted before
	// the flag existed restored as noisy channel games)
	for key, g := range m.games {
		if ch := channelOf(key); ch != "" && !strings.ContainsAny(ch[:1], "#&+!") {
			g.Query = true
		}
		m.armTimer(key)
	}
	return nil
}

// Unload implements module.Module.
func (m *Module) Unload() error {
	m.unloaded = true
	m.ctx.Cmd.UnregisterModule(m.Name())
	m.ctx.Bus.UnregisterModule(m.Name())
	return nil
}

func (m *Module) saveGames() {
	m.ctx.Saver.Mark(m.Name(), "games", func() any { return m.games })
}

// gameKey identifies a game: queries rewrite Channel to the sender
// nick, so query solo games fit the same map.
func gameKey(server, channel string) string {
	return server + " " + strings.ToLower(channel)
}

func splitChans(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' })
}

// langFor picks the language: channels in conf rvf_channels_en play in
// english, everything else (incl. queries) in dutch.
func (m *Module) langFor(channel string) string {
	for _, ch := range splitChans(m.ctx.Conf.String("rvf_channels_en")) {
		if strings.EqualFold(ch, channel) {
			return "en"
		}
	}
	return "nl"
}

// gameChannels lists the dedicated game channels (nl + en lists).
func (m *Module) gameChannels() []string {
	var out []string
	for _, setting := range []string{"rvf_channels_nl", "rvf_channels_en"} {
		out = append(out, splitChans(m.ctx.Conf.String(setting))...)
	}
	return out
}

// ownedChannel reports whether this is one of the dedicated game
// channels: the bot keeps their topic, and channel games only run
// there.
func (m *Module) ownedChannel(channel string) bool {
	for _, ch := range m.gameChannels() {
		if strings.EqualFold(ch, channel) {
			return true
		}
	}
	return false
}

// setTopic asserts the help topic. SendRaw because there is no module
// topic API; the raw path still colorizes the {x} tags.
func (m *Module) setTopic(channel string) {
	m.ctx.SendRaw("TOPIC " + channel + " :" + langs[m.langFor(channel)].topic)
}

// onJoin sets the topic when the bot enters an owned channel (also
// covers a stale topic found at join: the set is unconditional).
func (m *Module) onJoin(ev *bus.Event) (bus.Handled, any) {
	if ev.SenderMe && m.ownedChannel(ev.Channel) {
		m.setTopic(ev.Channel)
	}
	return bus.None, nil
}

// onTopic puts the help topic back when someone changes it. Our own
// change echoes back with SenderMe (no loop), and the comparison is
// against the mIRC-colored form: that is what the wire carries.
func (m *Module) onTopic(ev *bus.Event) (bus.Handled, any) {
	if ev.SenderMe || !m.ownedChannel(ev.Channel) {
		return bus.None, nil
	}
	if ev.Topic != format.ToIRC(langs[m.langFor(ev.Channel)].topic) {
		m.setTopic(ev.Channel)
	}
	return bus.None, nil
}

// pool is the puzzle pool for lang: embedded corpus plus extras.
func (m *Module) pool(lang string) []Puzzle {
	builtin, err := builtinPuzzles(lang)
	if err != nil {
		builtin = nil
	}
	return append(builtin, m.extra[lang]...)
}

func (m *Module) rollN(n int) int {
	if m.Rand != nil {
		return m.Rand(n)
	}
	return rand.IntN(n)
}

// boardLine renders the category, the masked puzzle, and the wrong
// guesses so far ("weet ik veel wat er al geweest is", Bram 2026-07-14).
func boardLine(g *Game) string {
	line := fmt.Sprintf("{c}[%s]{/} {y}%s{/}", g.Category, g.Board())
	if wrong := g.WrongGuesses(); len(wrong) > 0 {
		line += fmt.Sprintf(" {r}%s %s{/}", langs[g.Lang].wrongLabel, strings.Join(wrong, " "))
	}
	return line
}

// status is the always-visible game state: the board plus whose turn
// it is. Every turn change sends this (the 15:32 game log had six
// misses in a row with an invisible board).
func (m *Module) status(g *Game) string {
	return boardLine(g) + "\n" + m.turnLine(g)
}

// turnLine renders whose turn it is, their money, and the actions.
func (m *Module) turnLine(g *Game) string {
	t := langs[g.Lang]
	cost := m.ctx.Conf.Int("rvf_vowel_cost")
	line := fmt.Sprintf(t.turn, g.Current().Nick)
	line += fmt.Sprintf(" ({g}%s{/}): ", t.money(g.Current().Money))
	return line + fmt.Sprintf(t.turnActions, t.money(cost))
}

// cbStart begins a game (also registered as !rvf): !start nick1,nick2
// in a game channel, or solo. Queries are always solo: only the query
// owner can ever speak in that "channel", so a player list would
// deadlock on the first turn pass.
func (m *Module) cbStart(d *cmd.Data) bool {
	ev := d.Event
	key := gameKey(ev.Server, ev.Channel)
	lang := m.langFor(ev.Channel)
	t := langs[lang]
	if !ev.Query && !m.ownedChannel(ev.Channel) {
		m.ctx.Privmsg(ev.Channel, fmt.Sprintf(t.notHere, strings.Join(m.gameChannels(), " / ")))
		return true
	}
	if g, ok := m.games[key]; ok && !g.Done {
		m.ctx.Privmsg(ev.Channel, t.alreadyGame)
		return true
	}
	var nicks []string
	if ev.Query {
		nicks = []string{ev.Sender.Nick}
	} else {
		for _, n := range strings.FieldsFunc(d.Data, func(r rune) bool { return r == ',' || r == ' ' }) {
			if len(n) > 30 {
				m.ctx.Privmsg(ev.Channel, t.sillyNick)
				return true
			}
			if !slices.ContainsFunc(nicks, func(have string) bool { return strings.EqualFold(have, n) }) {
				nicks = append(nicks, n)
			}
		}
	}
	if len(nicks) > 8 {
		m.ctx.Privmsg(ev.Channel, t.tooMany)
		return true
	}
	if len(nicks) == 0 {
		nicks = []string{ev.Sender.Nick}
	}
	pool := m.pool(lang)
	if len(pool) == 0 {
		m.ctx.Privmsg(ev.Channel, t.noPuzzles)
		return true
	}
	p := pool[m.rollN(len(pool))]
	g := NewGame(lang, p.Category, p.Text, nicks)
	g.Query = ev.Query
	m.games[key] = g
	m.saveGames()
	m.armTimer(key)
	m.ctx.Privmsg(ev.Channel, m.status(g))
	return true
}

// cbStop aborts a running game (players only).
func (m *Module) cbStop(d *cmd.Data) bool {
	ev := d.Event
	key := gameKey(ev.Server, ev.Channel)
	g, ok := m.games[key]
	if !ok {
		return true
	}
	t := langs[g.Lang]
	if !g.IsPlayer(ev.Sender.Nick) {
		m.ctx.Privmsg(ev.Channel, t.playersOnly)
		return true
	}
	m.endGame(key)
	m.ctx.Privmsg(ev.Channel, fmt.Sprintf(t.stopped, g.Puzzle))
	return true
}

// cbTop10 shows the leaderboard.
func (m *Module) cbTop10(d *cmd.Data) bool {
	t := langs[m.langFor(d.Event.Channel)]
	if len(m.hiscores) == 0 {
		m.ctx.Privmsg(d.Event.Channel, t.top10Empty)
		return true
	}
	lines := []string{t.top10Title}
	for i, s := range m.hiscores {
		lines = append(lines, fmt.Sprintf("%2d. {B}{b}%s{/} {g}%s{/} (%s)",
			i+1, s.Nick, langs[s.Lang].money(s.Score), s.Channel))
	}
	m.ctx.Pager.EventMsg(d.Event, "top10", strings.Join(lines, "\n"))
	return true
}

// endGame removes a game and orphans its timer.
func (m *Module) endGame(key string) {
	delete(m.games, key)
	m.gen[key]++
	m.saveGames()
}

// recordScore inserts a win into the top-10; returns the 1-based
// position, or 0 when it did not make the list.
func (m *Module) recordScore(s Score) int {
	m.hiscores = append(m.hiscores, s)
	sort.SliceStable(m.hiscores, func(i, j int) bool { return m.hiscores[i].Score > m.hiscores[j].Score })
	if len(m.hiscores) > 10 {
		m.hiscores = m.hiscores[:10]
	}
	m.ctx.Saver.Mark(m.Name(), "hiscores", func() any { return m.hiscores })
	for i := range m.hiscores {
		if m.hiscores[i] == s {
			return i + 1
		}
	}
	return 0
}

// armTimer (re)arms the turn timeout for a game. The generation
// counter makes any previously armed timer a no-op.
func (m *Module) armTimer(key string) {
	m.gen[key]++
	gen := m.gen[key]
	secs := m.ctx.Conf.Int("rvf_turn_seconds")
	m.ctx.Sched.After(time.Duration(secs)*time.Second, func() {
		if m.unloaded || m.gen[key] != gen {
			return
		}
		m.turnTimeout(key)
	})
}

// turnTimeout handles a slept-through turn: pass it, and drop players
// who sleep through too many of their own turns in a row (Bram vs an
// AFK BenV, 2026-07-14: the old global 3-timeouts abort killed the
// game under an active player's nose). The game ends when the last
// player is dropped.
func (m *Module) turnTimeout(key string) {
	g, ok := m.games[key]
	if !ok {
		return
	}
	t := langs[g.Lang]
	channel := channelOf(key)
	cur := g.Current()
	cur.Missed++
	if cur.Missed >= m.ctx.Conf.Int("rvf_max_missed_turns") {
		nick := g.DropCurrent()
		if len(g.Players) == 0 {
			m.endGame(key)
			m.ctx.Privmsg(channel, fmt.Sprintf(t.aborted, g.Puzzle))
			return
		}
		m.saveGames()
		m.armTimer(key)
		m.ctx.Privmsg(channel, fmt.Sprintf(t.dropped, nick)+"\n"+m.status(g))
		return
	}
	slept := cur.Nick
	g.Pass()
	m.saveGames()
	m.armTimer(key)
	// a solo query player does not need to hear they are asleep; the
	// reveal above still happens when they never come back
	if !g.Query {
		m.ctx.Privmsg(channel, fmt.Sprintf(t.timeout, slept)+"\n"+m.status(g))
	}
}

// channelOf recovers the channel part of a game key (lowercased, which
// the ircd treats the same as the original).
func channelOf(key string) string {
	_, channel, _ := strings.Cut(key, " ")
	return channel
}

// onPrivmsg is the game input hook. Handled input returns bus.Stop so
// markov (loaded after rvf) neither learns letter spam nor query-talks
// at a player mid-game.
func (m *Module) onPrivmsg(ev *bus.Event) (bus.Handled, any) {
	if ev.SenderMe || ev.Msg == "" {
		return bus.None, nil
	}
	key := gameKey(ev.Server, ev.Channel)
	g, ok := m.games[key]
	if !ok {
		return bus.None, nil
	}
	t := langs[g.Lang]
	reply := func(msg string) { m.ctx.Privmsg(ev.Channel, msg) }
	if !g.IsCurrent(ev.Sender.Nick) {
		// a fellow PLAYER reaching for the wheel out of turn gets a
		// nudge; spectators and plain chatter flow on untouched
		if g.IsPlayer(ev.Sender.Nick) && g.GameMove(ev.Msg) {
			reply(fmt.Sprintf(t.notYourTurn, g.Current().Nick))
			return bus.Stop, nil
		}
		return bus.None, nil
	}

	handled := m.handleMove(key, g, t, ev.Msg, reply)
	if !handled && ev.Query && !strings.HasPrefix(ev.Msg, "!") {
		// mid-game query noise gets the usage line instead of markov;
		// !commands are the registry's business (!stop must not get a
		// help line stacked on top)
		reply(t.queryHelp)
		handled = true
	}
	if handled {
		return bus.Stop, nil
	}
	return bus.None, nil
}

// handleMove parses and applies one game move; false = not game input.
func (m *Module) handleMove(key string, g *Game, t *texts, msg string, reply func(string)) bool {
	cost := m.ctx.Conf.Int("rvf_vowel_cost")
	mover := g.Current()

	if g.State == StateLetter {
		lm := letterRe.FindStringSubmatch(msg)
		if lm == nil {
			// a consonant is owed; game-shaped attempts at anything
			// else get ribbed, plain chatter flows on
			if t.spinRe.MatchString(msg) || t.buyRe.MatchString(msg) ||
				t.solveRe.MatchString(msg) || t.passRe.MatchString(msg) {
				reply(m.pick(t.wrongAction))
				return true
			}
			return false
		}
		letter := strings.ToUpper(lm[1])
		res := g.CallLetter(letter)
		switch res.Kind {
		case OutInvalid:
			reply(t.needConsonant)
		case OutHit:
			reply(fmt.Sprintf(t.hit, res.Count, letter, t.money(res.Amount)) + "\n" + m.status(g))
		case OutMiss:
			out := fmt.Sprintf(t.miss, letter)
			if res.Dup {
				out = fmt.Sprintf(t.dup, letter)
			}
			if res.JokerSaved {
				out += " " + t.jokerSaved
			}
			reply(out + "\n" + m.status(g))
		}
		m.moveDone(key, g, mover)
		return true
	}

	switch {
	case t.spinRe.MatchString(msg):
		seg, fx := g.Spin(m.rollN(len(wheel)))
		switch seg.Kind {
		case SegMoney:
			reply(fmt.Sprintf(t.spinMoney, t.money(seg.Amount), g.Current().Nick))
		case SegBankrupt:
			out := fmt.Sprintf(t.spinBankrupt, t.money(fx.Lost))
			if fx.JokerSaved {
				out += " " + t.jokerSaved
			}
			reply(out + "\n" + m.status(g))
		case SegLoseTurn:
			out := t.spinLoseTurn
			if fx.JokerSaved {
				out += " " + t.jokerSaved
			}
			reply(out + "\n" + m.status(g))
		case SegJoker:
			reply(fmt.Sprintf(t.spinJoker, g.Current().Nick) + "\n" + m.status(g))
		}
		m.moveDone(key, g, mover)
		return true

	case t.buyRe.MatchString(msg):
		letter := strings.ToUpper(t.buyRe.FindStringSubmatch(msg)[1])
		res := g.BuyVowel(letter, cost)
		switch res.Kind {
		case OutInvalid:
			reply(t.needVowel)
		case OutBroke:
			reply(fmt.Sprintf(t.broke, t.money(cost)))
		case OutHit:
			reply(fmt.Sprintf(t.hitVowel, res.Count, letter) + "\n" + m.status(g))
		case OutMiss:
			out := fmt.Sprintf(t.miss, letter)
			if res.JokerSaved {
				out += " " + t.jokerSaved
			}
			reply(out + "\n" + m.status(g))
		}
		m.moveDone(key, g, mover)
		return true

	case t.solveRe.MatchString(msg):
		attempt := t.solveRe.FindStringSubmatch(msg)[1]
		winner := g.Current()
		if g.Solve(attempt) {
			out := victory(t, m.rollN, winner.Nick) + "\n" +
				fmt.Sprintf(t.solveWin, winner.Nick, t.money(winner.Money), g.Puzzle)
			channel := channelOf(key)
			if pos := m.recordScore(Score{
				Nick: winner.Nick, Channel: channel, Lang: g.Lang,
				Score: winner.Money, When: m.now().Unix(),
			}); pos > 0 {
				out += fmt.Sprintf(t.hiscoreEntry, pos)
			}
			m.endGame(key)
			reply(out)
			return true
		}
		reply(t.solveWrong + "\n" + m.status(g))
		m.moveDone(key, g, mover)
		return true

	case t.passRe.MatchString(msg):
		g.Pass()
		reply(m.status(g))
		m.moveDone(key, g, mover)
		return true

	case letterRe.MatchString(msg):
		reply(m.pick(t.spinFirsts))
		return true
	}
	return false
}

// pick draws a random variant from a fun-line list.
func (m *Module) pick(list []string) string {
	return list[m.rollN(len(list))]
}

// moveDone persists and resets the turn clock after any valid move;
// the mover proved awake, so their missed-turn counter clears.
func (m *Module) moveDone(key string, g *Game, mover *Player) {
	mover.Missed = 0
	m.saveGames()
	m.armTimer(key)
}

var rvfAdminRe = regexp.MustCompile(`^rvf(\s+.*)?$`)

// adminSpec is the telnet puzzle management.
func (m *Module) adminSpec(*bus.Event) (bus.Handled, any) {
	return bus.None, admin.Spec{
		Name:  "rvf",
		Match: rvfAdminRe,
		Help:  "Rad van Fortuin puzzles: rvf add <nl|en> <Category>: <puzzle> | rvf del <nl|en> <puzzle> | rvf list",
		Args:  []string{"<add|del|list>", "..."},
		Su:    true,
		Run:   m.adminRun,
	}
}

var validPuzzle = regexp.MustCompile(`^[A-Z ',]+$`)

func (m *Module) adminRun(_, line string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "rvf"))
	verb, rest, _ := strings.Cut(rest, " ")
	switch verb {
	case "add":
		lang, spec, _ := strings.Cut(strings.TrimSpace(rest), " ")
		if langs[lang] == nil {
			return "{r}Error:{/} language must be nl or en"
		}
		puzzles, err := parseCorpus(spec)
		if err != nil || len(puzzles) != 1 {
			return "{r}Error:{/} expected <Category>: <puzzle>"
		}
		p := puzzles[0]
		if !validPuzzle.MatchString(strings.ToUpper(p.Text)) {
			return "{r}Error:{/} puzzle must be plain letters (A-Z, spaces, comma, apostrophe)"
		}
		m.extra[lang] = append(m.extra[lang], p)
		m.ctx.Saver.Mark(m.Name(), "puzzles", func() any { return m.extra })
		return fmt.Sprintf("{g}Added %s puzzle{/} [%s] %s", lang, p.Category, p.Text)
	case "del":
		lang, text, _ := strings.Cut(strings.TrimSpace(rest), " ")
		if langs[lang] == nil {
			return "{r}Error:{/} language must be nl or en"
		}
		text = strings.TrimSpace(text)
		before := len(m.extra[lang])
		m.extra[lang] = slices.DeleteFunc(m.extra[lang], func(p Puzzle) bool {
			return strings.EqualFold(p.Text, text)
		})
		if len(m.extra[lang]) == before {
			return fmt.Sprintf("{r}Error:{/} no extra %s puzzle %q (builtins cannot be deleted)", lang, text)
		}
		m.ctx.Saver.Mark(m.Name(), "puzzles", func() any { return m.extra })
		return fmt.Sprintf("{g}Deleted.{/} %d extra %s puzzles left", len(m.extra[lang]), lang)
	case "list":
		var b strings.Builder
		for _, lang := range []string{"nl", "en"} {
			builtin, _ := builtinPuzzles(lang)
			fmt.Fprintf(&b, "{y}%s{/}: %d builtin + %d extra\n", lang, len(builtin), len(m.extra[lang]))
			for _, p := range m.extra[lang] {
				fmt.Fprintf(&b, "  [%s] %s\n", p.Category, p.Text)
			}
		}
		fmt.Fprintf(&b, "%d games running", len(m.games))
		return b.String()
	default:
		return "usage: rvf add <nl|en> <Category>: <puzzle> | rvf del <nl|en> <puzzle> | rvf list"
	}
}
