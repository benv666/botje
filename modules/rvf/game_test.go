package rvf

import (
	"strings"
	"testing"
)

func testGame(nicks ...string) *Game {
	return NewGame("nl", "Gezegde", "WIE HET LAATST LACHT, LACHT HET BEST", nicks)
}

// segIndex finds a wheel segment for scripted spins.
func segIndex(t *testing.T, kind SegmentKind, amount int) int {
	t.Helper()
	for i, s := range wheel {
		if s.Kind == kind && (kind != SegMoney || s.Amount == amount) {
			return i
		}
	}
	t.Fatalf("no wheel segment kind=%d amount=%d", kind, amount)
	return -1
}

func TestWheelIsTheRTL4Wheel(t *testing.T) {
	if len(wheel) != 24 {
		t.Fatalf("wheel has %d segments, want 24", len(wheel))
	}
	// the documented 1990 RTL4 money distribution
	counts := map[int]int{}
	special := map[SegmentKind]int{}
	for _, s := range wheel {
		if s.Kind == SegMoney {
			counts[s.Amount]++
		} else {
			special[s.Kind]++
		}
	}
	want := map[int]int{50: 1, 150: 1, 200: 3, 300: 1, 350: 1, 400: 3,
		450: 2, 550: 1, 600: 3, 700: 1, 750: 1, 800: 2, 1000: 1}
	for amt, n := range want {
		if counts[amt] != n {
			t.Errorf("amount %d appears %dx, want %dx", amt, counts[amt], n)
		}
	}
	if special[SegBankrupt] != 1 || special[SegLoseTurn] != 1 || special[SegJoker] != 1 {
		t.Errorf("specials = %v, want one each", special)
	}
}

func TestSpinMoneyThenConsonantHit(t *testing.T) {
	g := testGame("BenV", "Lotjuh")
	seg, _ := g.Spin(segIndex(t, SegMoney, 400))
	if seg.Amount != 400 || g.State != StateLetter {
		t.Fatalf("spin = %+v state %d", seg, g.State)
	}
	res := g.CallLetter("t")
	// WIE HET LAATST LACHT, LACHT HET BEST: t appears 7 times
	if res.Kind != OutHit || res.Count != 7 {
		t.Fatalf("res = %+v, want 7x T", res)
	}
	if g.Current().Nick != "BenV" || g.Current().Money != 2800 {
		t.Fatalf("player = %+v, hit must keep the turn and pay", g.Current())
	}
	if g.State != StateAction {
		t.Fatal("after a hit the player picks the next action")
	}
}

func TestConsonantMissPassesTurn(t *testing.T) {
	g := testGame("BenV", "Lotjuh")
	g.Spin(segIndex(t, SegMoney, 400))
	res := g.CallLetter("z")
	if res.Kind != OutMiss {
		t.Fatalf("res = %+v", res)
	}
	if g.Current().Nick != "Lotjuh" || g.State != StateAction {
		t.Fatalf("turn did not pass: %s state %d", g.Current().Nick, g.State)
	}
}

func TestVowelWhenConsonantExpectedIsInvalid(t *testing.T) {
	g := testGame("BenV")
	g.Spin(segIndex(t, SegMoney, 400))
	if res := g.CallLetter("e"); res.Kind != OutInvalid {
		t.Fatalf("vowel accepted as consonant: %+v", res)
	}
	if g.Current().Nick != "BenV" || g.State != StateLetter {
		t.Fatal("an invalid call must not cost the turn")
	}
}

func TestDuplicateLetterLosesTurn(t *testing.T) {
	g := testGame("BenV", "Lotjuh")
	g.Spin(segIndex(t, SegMoney, 400))
	g.CallLetter("t")
	g.Spin(segIndex(t, SegMoney, 200))
	if res := g.CallLetter("t"); res.Kind != OutMiss {
		t.Fatalf("duplicate letter = %+v, want the show's lose-a-turn", res)
	}
	if g.Current().Nick != "Lotjuh" {
		t.Fatal("turn did not pass on duplicate")
	}
}

func TestBuyVowel(t *testing.T) {
	g := testGame("BenV", "Lotjuh")
	g.Spin(segIndex(t, SegMoney, 400))
	g.CallLetter("t") // 7x400 = 2800
	if res := g.BuyVowel("e", 250); res.Kind != OutHit || res.Count != 4 {
		t.Fatalf("buy e = %+v, want 4x E", res)
	}
	if g.Current().Money != 2550 {
		t.Fatalf("money = %d, want 2800-250", g.Current().Money)
	}
	// absent vowel: pay AND lose the turn (show rules)
	if res := g.BuyVowel("o", 250); res.Kind != OutMiss {
		t.Fatalf("buy o = %+v", res)
	}
	if g.Players[0].Money != 2300 || g.Current().Nick != "Lotjuh" {
		t.Fatalf("miss must cost money and turn: %d %s", g.Players[0].Money, g.Current().Nick)
	}
}

func TestBuyVowelNeedsMoney(t *testing.T) {
	g := testGame("BenV")
	if res := g.BuyVowel("e", 250); res.Kind != OutBroke {
		t.Fatalf("broke buy = %+v", res)
	}
	if g.Current().Nick != "BenV" {
		t.Fatal("refusal must not cost the turn")
	}
}

func TestBankruptWipesRoundMoney(t *testing.T) {
	g := testGame("BenV", "Lotjuh")
	g.Spin(segIndex(t, SegMoney, 400))
	g.CallLetter("t")
	seg, _ := g.Spin(segIndex(t, SegBankrupt, 0))
	if seg.Kind != SegBankrupt {
		t.Fatalf("seg = %+v", seg)
	}
	if g.Players[0].Money != 0 || g.Current().Nick != "Lotjuh" {
		t.Fatalf("bankrupt: money %d current %s", g.Players[0].Money, g.Current().Nick)
	}
}

func TestJokerSavesTurnOnce(t *testing.T) {
	g := testGame("BenV", "Lotjuh")
	seg, _ := g.Spin(segIndex(t, SegJoker, 0))
	if seg.Kind != SegJoker || !g.Current().Joker || g.Current().Nick != "BenV" {
		t.Fatalf("joker spin: %+v joker=%v", seg, g.Players[0].Joker)
	}
	// lose turn: the joker burns instead
	g.Spin(segIndex(t, SegLoseTurn, 0))
	if g.Current().Nick != "BenV" || g.Current().Joker {
		t.Fatalf("joker did not save: current %s joker %v", g.Current().Nick, g.Current().Joker)
	}
	// second lose turn: no joker left, turn passes
	g.Spin(segIndex(t, SegLoseTurn, 0))
	if g.Current().Nick != "Lotjuh" {
		t.Fatal("turn should pass without a joker")
	}
}

func TestJokerSavesMoneyNotBankruptcy(t *testing.T) {
	// bankroet with a joker: round money still gone, but the turn survives
	g := testGame("BenV", "Lotjuh")
	g.Spin(segIndex(t, SegJoker, 0))
	g.Spin(segIndex(t, SegMoney, 400))
	g.CallLetter("t")
	g.Spin(segIndex(t, SegBankrupt, 0))
	if g.Players[0].Money != 0 {
		t.Fatal("bankrupt must wipe the money, joker or not")
	}
	if g.Current().Nick != "BenV" || g.Current().Joker {
		t.Fatal("joker should save the turn and be consumed")
	}
}

func TestSolve(t *testing.T) {
	g := testGame("BenV", "Lotjuh")
	g.Spin(segIndex(t, SegMoney, 1000))
	g.CallLetter("t")
	// punctuation, case and extra spaces must not matter
	if !g.Solve("wie het laatst  lacht lacht het best") {
		t.Fatal("correct solution rejected")
	}
	if !g.Done {
		t.Fatal("game not done after solve")
	}
}

func TestWrongSolvePassesTurn(t *testing.T) {
	g := testGame("BenV", "Lotjuh")
	if g.Solve("wie het eerst lacht") {
		t.Fatal("wrong solution accepted")
	}
	if g.Done || g.Current().Nick != "Lotjuh" {
		t.Fatalf("done=%v current=%s", g.Done, g.Current().Nick)
	}
}

func TestSolveOnlyFromActionState(t *testing.T) {
	// after a money spin a consonant is owed; solving then is refused
	// silently rather than judged (the input parser should not offer it)
	g := testGame("BenV")
	g.Spin(segIndex(t, SegMoney, 400))
	if g.State != StateLetter {
		t.Fatal("setup")
	}
}

func TestBoardMasksUnguessedLetters(t *testing.T) {
	g := testGame("BenV")
	board := g.Board()
	if strings.ContainsAny(board, "WIEHLATSCB") {
		t.Fatalf("fresh board leaks letters: %q", board)
	}
	if !strings.Contains(board, ",") {
		t.Fatalf("punctuation should be visible: %q", board)
	}
	g.Spin(segIndex(t, SegMoney, 400))
	g.CallLetter("t")
	board = g.Board()
	if !strings.Contains(board, "T") || strings.Contains(board, "W") {
		t.Fatalf("board after T: %q", board)
	}
}

func TestPassTurn(t *testing.T) {
	g := testGame("BenV", "Lotjuh", "Verty")
	g.Pass()
	if g.Current().Nick != "Lotjuh" {
		t.Fatalf("current = %s", g.Current().Nick)
	}
	g.Pass()
	g.Pass()
	if g.Current().Nick != "BenV" {
		t.Fatalf("turn order should wrap: %s", g.Current().Nick)
	}
}

func TestIsPlayerAndCurrentOnly(t *testing.T) {
	g := testGame("BenV", "Lotjuh")
	if !g.IsCurrent("benv") {
		t.Fatal("nick match must be case-insensitive")
	}
	if g.IsCurrent("Lotjuh") {
		t.Fatal("Lotjuh is not the current player")
	}
}
