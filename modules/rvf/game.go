package rvf

import "strings"

// SegmentKind is what a wheel segment does.
type SegmentKind int

const (
	SegMoney SegmentKind = iota
	SegBankrupt
	SegLoseTurn
	SegJoker
)

// Segment is one wheel wedge.
type Segment struct {
	Kind   SegmentKind `json:"kind"`
	Amount int         `json:"amount"`
}

// wheel is the 1990 RTL4 rad: 24 segments, the documented guilder
// distribution plus bankroet, verliesbeurt and the joker (which saves
// the holder's turn once). English games spin the same wheel, only the
// currency label differs.
var wheel = []Segment{
	{SegMoney, 50}, {SegMoney, 150},
	{SegMoney, 200}, {SegMoney, 200}, {SegMoney, 200},
	{SegMoney, 300}, {SegMoney, 350},
	{SegMoney, 400}, {SegMoney, 400}, {SegMoney, 400},
	{SegMoney, 450}, {SegMoney, 450},
	{SegMoney, 550},
	{SegMoney, 600}, {SegMoney, 600}, {SegMoney, 600},
	{SegMoney, 700}, {SegMoney, 750},
	{SegMoney, 800}, {SegMoney, 800},
	{SegMoney, 1000},
	{SegBankrupt, 0}, {SegLoseTurn, 0}, {SegJoker, 0},
}

// GameState is what the current player owes the game.
type GameState int

const (
	StateAction GameState = iota // draai, koop, los op, pas
	StateLetter                  // a consonant, after a money spin
)

// OutcomeKind classifies what a letter call or vowel buy did.
type OutcomeKind int

const (
	OutHit     OutcomeKind = iota // letter present, Count times
	OutMiss                       // absent (or duplicate): turn passed unless joker
	OutInvalid                    // not a valid letter for this action; no penalty
	OutBroke                      // vowel buy without the money; no penalty
)

// Outcome is the result of a letter call or vowel buy.
type Outcome struct {
	Kind       OutcomeKind
	Count      int  // occurrences on a hit
	Amount     int  // money won on a consonant hit
	Dup        bool // the miss was an already-called letter
	JokerSaved bool // a miss that the joker absorbed
}

// SpinFX is what a spin did beyond the segment itself.
type SpinFX struct {
	JokerSaved bool
	Lost       int // round money wiped by a bankrupt
}

// Player is one contestant.
type Player struct {
	Nick  string `json:"nick"`
	Money int    `json:"money"` // round money
	Joker bool   `json:"joker"`
}

// Game is one puzzle being played in one channel (or query). All state
// is exported for JSON persistence; mutation happens on the dispatcher.
type Game struct {
	Lang     string          `json:"lang"` // "nl" or "en"
	Category string          `json:"category"`
	Puzzle   string          `json:"puzzle"` // uppercase
	Guessed  map[string]bool `json:"guessed"`
	Players  []*Player       `json:"players"`
	Turn     int             `json:"turn"`
	State    GameState       `json:"state"`
	Pending  int             `json:"pending"`  // spun amount awaiting a consonant
	Timeouts int             `json:"timeouts"` // consecutive, for the abort guard
	Done     bool            `json:"done"`
}

const vowels = "AEIOU"

// NewGame starts a puzzle for the given players (one nick = solo).
func NewGame(lang, category, puzzle string, nicks []string) *Game {
	players := make([]*Player, 0, len(nicks))
	for _, n := range nicks {
		players = append(players, &Player{Nick: n})
	}
	return &Game{
		Lang: lang, Category: category, Puzzle: strings.ToUpper(puzzle),
		Guessed: make(map[string]bool), Players: players,
	}
}

// Current is the player whose turn it is.
func (g *Game) Current() *Player { return g.Players[g.Turn] }

// IsCurrent reports whether nick is the current player (IRC nicks are
// case-insensitive).
func (g *Game) IsCurrent(nick string) bool {
	return strings.EqualFold(g.Current().Nick, nick)
}

// IsPlayer reports whether nick plays in this game.
func (g *Game) IsPlayer(nick string) bool {
	for _, p := range g.Players {
		if strings.EqualFold(p.Nick, nick) {
			return true
		}
	}
	return false
}

// advance passes the turn to the next player.
func (g *Game) advance() {
	g.Turn = (g.Turn + 1) % len(g.Players)
	g.State = StateAction
	g.Pending = 0
}

// loseTurn passes the turn unless the current player holds the joker,
// which is then consumed. Reports whether the joker saved it.
func (g *Game) loseTurn() bool {
	if g.Current().Joker {
		g.Current().Joker = false
		g.State = StateAction
		g.Pending = 0
		return true
	}
	g.advance()
	return false
}

// Spin turns the wheel; idx is the module's die roll into the wheel
// slice. Money puts the game in StateLetter; specials resolve directly.
func (g *Game) Spin(idx int) (Segment, SpinFX) {
	seg := wheel[idx%len(wheel)]
	switch seg.Kind {
	case SegMoney:
		g.State = StateLetter
		g.Pending = seg.Amount
	case SegBankrupt:
		lost := g.Current().Money
		g.Current().Money = 0
		return seg, SpinFX{JokerSaved: g.loseTurn(), Lost: lost}
	case SegLoseTurn:
		return seg, SpinFX{JokerSaved: g.loseTurn()}
	case SegJoker:
		g.Current().Joker = true
		g.State = StateAction
	}
	return seg, SpinFX{}
}

// count returns how often letter (single uppercase char) occurs.
func (g *Game) count(letter string) int {
	return strings.Count(g.Puzzle, letter)
}

// CallLetter judges a consonant call after a money spin.
func (g *Game) CallLetter(letter string) Outcome {
	l := strings.ToUpper(strings.TrimSpace(letter))
	if len(l) != 1 || l[0] < 'A' || l[0] > 'Z' || strings.ContainsAny(l, vowels) {
		return Outcome{Kind: OutInvalid}
	}
	if g.Guessed[l] {
		// the show's rule: calling an already-used letter costs the turn
		return Outcome{Kind: OutMiss, Dup: true, JokerSaved: g.loseTurn()}
	}
	g.Guessed[l] = true
	n := g.count(l)
	if n == 0 {
		return Outcome{Kind: OutMiss, JokerSaved: g.loseTurn()}
	}
	won := g.Pending * n
	g.Current().Money += won
	g.State = StateAction
	g.Pending = 0
	return Outcome{Kind: OutHit, Count: n, Amount: won}
}

// BuyVowel buys a vowel for cost from the current player's round money.
// An absent vowel costs the money AND the turn (show rules).
func (g *Game) BuyVowel(letter string, cost int) Outcome {
	l := strings.ToUpper(strings.TrimSpace(letter))
	if len(l) != 1 || !strings.ContainsAny(l, vowels) || g.Guessed[l] {
		return Outcome{Kind: OutInvalid}
	}
	if g.Current().Money < cost {
		return Outcome{Kind: OutBroke}
	}
	g.Current().Money -= cost
	g.Guessed[l] = true
	n := g.count(l)
	if n == 0 {
		return Outcome{Kind: OutMiss, JokerSaved: g.loseTurn()}
	}
	return Outcome{Kind: OutHit, Count: n}
}

// normalize reduces a solution attempt to bare uppercase words so
// punctuation, case and spacing cannot fail an honest answer.
func normalize(s string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.ToUpper(s) {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
			space = false
		default:
			if !space {
				b.WriteByte(' ')
				space = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// Solve judges a full solution. Correct ends the game; wrong passes
// the turn (no joker rescue: the show had none for solutions either).
func (g *Game) Solve(attempt string) bool {
	if normalize(attempt) == normalize(g.Puzzle) {
		g.Done = true
		return true
	}
	g.advance()
	return false
}

// Pass gives up the turn voluntarily.
func (g *Game) Pass() { g.advance() }

// Board renders the puzzle with unguessed letters masked. Punctuation
// and digits are always visible, like the show's board.
func (g *Game) Board() string {
	var b strings.Builder
	for _, r := range g.Puzzle {
		if r >= 'A' && r <= 'Z' && !g.Guessed[string(r)] {
			b.WriteRune('░')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
