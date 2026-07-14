package rvf

import (
	"fmt"
	"regexp"
)

// texts is the per-language table: message templates and the input
// grammar. Both languages spin the same RTL4 wheel; the currency label
// and the words differ. The joker is the US "free spin" in english.
type texts struct {
	money func(int) string

	turn                     string // nick, board money summary appended by caller
	turnActions              string // vowel cost
	spinMoney                string // amount, nick
	spinBankrupt             string // lost amount
	spinLoseTurn             string
	spinJoker                string // nick
	jokerSaved               string
	hit                      string // count, letter, amount won
	hitVowel                 string // count, letter
	miss                     string // letter
	dup                      string // letter
	needConsonant, needVowel string
	spinFirst                string
	broke                    string // vowel cost
	solveWin                 string // nick, amount, solution
	solveWrong               string
	hiscoreEntry             string // position
	timeout                  string // nick
	aborted                  string // solution
	stopped                  string // solution
	alreadyGame              string
	noPuzzles                string
	playersOnly              string
	top10Title               string
	top10Empty               string
	queryHelp                string

	spinRe, buyRe, solveRe, passRe *regexp.Regexp
}

var letterRe = regexp.MustCompile(`^\s*([a-zA-Z])\s*[!?.]*$`)

var langs = map[string]*texts{
	"nl": {
		money: func(n int) string { return fmt.Sprintf("fl. %d", n) },

		turn:          "{B}{b}%s{/} is aan de beurt",
		turnActions:   "{y}draai{/}, {y}koop <klinker>{/} (%s), {y}los op <zin>{/} of {y}pas{/}",
		spinMoney:     "Het rad valt op {g}%s{/}. Noem een medeklinker, {B}{b}%s{/}!",
		spinBankrupt:  "{R}BANKROET!{/} Daar gaat %s.",
		spinLoseTurn:  "{y}Verliesbeurt!{/}",
		spinJoker:     "{c}Joker!{/} Die mag je houden, {B}{b}%s{/}.",
		jokerSaved:    "{c}De joker redt je beurt!{/}",
		hit:           "{g}%dx %s!{/} Dat is %s erbij.",
		hitVowel:      "{g}%dx %s!{/}",
		miss:          "{r}Geen %s.{/}",
		dup:           "{r}De %s was al geweest.{/}",
		needConsonant: "Een {y}medeklinker{/} graag.",
		needVowel:     "Een {y}klinker{/} graag (a e i o u).",
		spinFirst:     "Eerst {y}draai{/}en.",
		broke:         "Daar heb je het geld niet voor (een klinker kost %s).",
		solveWin:      "{G}JUIST!{/} {B}{b}%s{/} wint {g}%s{/}! De oplossing: {y}%s{/}",
		solveWrong:    "{r}Helaas, dat is het niet.{/}",
		hiscoreEntry:  " {c}Plek %d in de toptien!{/}",
		timeout:       "{B}{b}%s{/} zit te slapen, de beurt gaat voorbij.",
		aborted:       "Niemand doet meer mee, spel gestopt. De oplossing was: {y}%s{/}",
		stopped:       "Spel gestopt. De oplossing was: {y}%s{/}",
		alreadyGame:   "Er loopt hier al een spel (probeer {y}!stop{/}).",
		noPuzzles:     "Geen puzzels beschikbaar.",
		playersOnly:   "Alleen spelers kunnen het spel stoppen.",
		top10Title:    "{B}{b}Rad van Fortuin toptien{/}",
		top10Empty:    "Nog geen winnaars.",
		queryHelp:     "Zeg {y}draai{/}, {y}koop <klinker>{/}, {y}los op <zin>{/} of {y}pas{/}.",

		spinRe:  regexp.MustCompile(`^(?i)\s*draai(en)?\s*[!.]*$`),
		buyRe:   regexp.MustCompile(`^(?i)\s*koop\s+([a-zA-Z])\s*[!?.]*$`),
		solveRe: regexp.MustCompile(`^(?i)\s*los\s+op[:\s]\s*(.+?)\s*$`),
		passRe:  regexp.MustCompile(`^(?i)\s*pas\s*[!.]*$`),
	},
	"en": {
		money: func(n int) string { return fmt.Sprintf("$%d", n) },

		turn:          "{B}{b}%s{/} is up",
		turnActions:   "{y}spin{/}, {y}buy <vowel>{/} (%s), {y}solve <phrase>{/} or {y}pass{/}",
		spinMoney:     "The wheel lands on {g}%s{/}. Call a consonant, {B}{b}%s{/}!",
		spinBankrupt:  "{R}BANKRUPT!{/} There goes %s.",
		spinLoseTurn:  "{y}Lose a turn!{/}",
		spinJoker:     "{c}Free spin!{/} Keep it, {B}{b}%s{/}.",
		jokerSaved:    "{c}Your free spin saves you!{/}",
		hit:           "{g}%dx %s!{/} That adds %s.",
		hitVowel:      "{g}%dx %s!{/}",
		miss:          "{r}No %s.{/}",
		dup:           "{r}%s was already called.{/}",
		needConsonant: "A {y}consonant{/}, please.",
		needVowel:     "A {y}vowel{/}, please (a e i o u).",
		spinFirst:     "You have to {y}spin{/} first.",
		broke:         "You cannot afford that (a vowel costs %s).",
		solveWin:      "{G}CORRECT!{/} {B}{b}%s{/} wins {g}%s{/}! The answer: {y}%s{/}",
		solveWrong:    "{r}Sorry, that is not it.{/}",
		hiscoreEntry:  " {c}Number %d on the leaderboard!{/}",
		timeout:       "{B}{b}%s{/} fell asleep, moving on.",
		aborted:       "Nobody is playing, game over. The answer was: {y}%s{/}",
		stopped:       "Game stopped. The answer was: {y}%s{/}",
		alreadyGame:   "There is already a game here (try {y}!stop{/}).",
		noPuzzles:     "No puzzles available.",
		playersOnly:   "Only players can stop the game.",
		top10Title:    "{B}{b}Wheel of Fortune top ten{/}",
		top10Empty:    "No winners yet.",
		queryHelp:     "Say {y}spin{/}, {y}buy <vowel>{/}, {y}solve <phrase>{/} or {y}pass{/}.",

		spinRe:  regexp.MustCompile(`^(?i)\s*spin\s*[!.]*$`),
		buyRe:   regexp.MustCompile(`^(?i)\s*buy\s+([a-zA-Z])\s*[!?.]*$`),
		solveRe: regexp.MustCompile(`^(?i)\s*solve[:\s]\s*(.+?)\s*$`),
		passRe:  regexp.MustCompile(`^(?i)\s*pass\s*[!.]*$`),
	},
}
