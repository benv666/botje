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

	// topic is the help topic the bot keeps on its dedicated game
	// channels; winBanner is the nick line woven into the victory art.
	topic     string
	winBanner string

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
	spinFirsts               []string // letter called before any spin
	wrongAction              []string // anything but a letter while one is owed
	tooMany                  string
	sillyNick                string
	broke                    string // vowel cost
	solveWin                 string // nick, amount, solution
	solveWrong               string
	hiscoreEntry             string // position
	wrongLabel               string // prefix of the wrong-guesses list
	timeout                  string // nick
	dropped                  string // nick
	notYourTurn              string // current nick
	aborted                  string // solution
	stopped                  string // solution
	alreadyGame              string
	notHere                  string // game channels list
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

		topic: "Rad van Fortuin! {y}!start nick1,nick2{/} om te spelen | " +
			"draai / koop <klinker> / los op <zin> / pas | {y}!top10{/} voor de eregalerij",
		winBanner: "{G}%s WINT HET RAD!{/}",

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
		spinFirsts: []string{
			"Nee, je moet eerst {y}draai{/}en!",
			"Zo werkt het niet: eerst het rad {y}draai{/}en, dan letters roepen.",
			"Rustig aan. Eerst {y}draai{/}en.",
			"Zonder draai geen letter. {y}draai{/}!",
		},
		wrongAction: []string{
			"Nee nee, eerst een {y}medeklinker{/} noemen!",
			"Het rad wacht op een {y}medeklinker{/}...",
			"Leuk geprobeerd. Medeklinker. Nu.",
			"Je bent me een {y}medeklinker{/} schuldig.",
		},
		tooMany:      "Zoveel stoelen heeft de studio niet (max {y}8{/} spelers).",
		sillyNick:    "En hoe gaan we die dan roepen? Normale nicks graag.",
		broke:        "Daar heb je het geld niet voor (een klinker kost %s).",
		solveWin:     "{G}JUIST!{/} {B}{b}%s{/} wint {g}%s{/}! De oplossing: {y}%s{/}",
		solveWrong:   "{r}Helaas, dat is het niet.{/}",
		hiscoreEntry: " {c}Plek %d in de toptien!{/}",
		wrongLabel:   "fout:",
		timeout:      "{B}{b}%s{/} zit te slapen, de beurt gaat voorbij.",
		dropped:      "{B}{b}%s{/} doet niet meer mee (blijft maar slapen).",
		notYourTurn:  "Rustig, {B}{b}%s{/} is aan de beurt.",
		aborted:      "Niemand doet meer mee, spel gestopt. De oplossing was: {y}%s{/}",
		stopped:      "Spel gestopt. De oplossing was: {y}%s{/}",
		alreadyGame:  "Er loopt hier al een spel (probeer {y}!stop{/}).",
		notHere:      "Hier spelen we niet: kom naar {y}%s{/}, of speel solo in een query ({y}!rvf{/}).",
		noPuzzles:    "Geen puzzels beschikbaar.",
		playersOnly:  "Alleen spelers kunnen het spel stoppen.",
		top10Title:   "{B}{b}Rad van Fortuin toptien{/}",
		top10Empty:   "Nog geen winnaars.",
		queryHelp:    "Zeg {y}draai{/}, {y}koop <klinker>{/}, {y}los op <zin>{/} of {y}pas{/}.",

		spinRe:  regexp.MustCompile(`^(?i)\s*draai(en)?\s*[!.]*$`),
		buyRe:   regexp.MustCompile(`^(?i)\s*koop\s+([a-zA-Z])\s*[!?.]*$`),
		solveRe: regexp.MustCompile(`^(?i)\s*los\s+op[:\s]\s*(.+?)\s*$`),
		passRe:  regexp.MustCompile(`^(?i)\s*pas\s*[!.]*$`),
	},
	"en": {
		money: func(n int) string { return fmt.Sprintf("$%d", n) },

		topic: "Wheel of Fortune! {y}!start nick1,nick2{/} to play | " +
			"spin / buy <vowel> / solve <phrase> / pass | {y}!top10{/} for the hall of fame",
		winBanner: "{G}%s WINS THE WHEEL!{/}",

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
		spinFirsts: []string{
			"No, you have to {y}spin{/} first!",
			"That is not how this works: {y}spin{/} the wheel, then call letters.",
			"Easy there. {y}spin{/} first.",
			"No spin, no letter. {y}spin{/}!",
		},
		wrongAction: []string{
			"No no, call a {y}consonant{/} first!",
			"The wheel is waiting for a {y}consonant{/}...",
			"Nice try. Consonant. Now.",
			"You owe me a {y}consonant{/}.",
		},
		tooMany:      "The studio only has {y}8{/} chairs.",
		sillyNick:    "And how would we call that out? Normal nicks please.",
		broke:        "You cannot afford that (a vowel costs %s).",
		solveWin:     "{G}CORRECT!{/} {B}{b}%s{/} wins {g}%s{/}! The answer: {y}%s{/}",
		solveWrong:   "{r}Sorry, that is not it.{/}",
		hiscoreEntry: " {c}Number %d on the leaderboard!{/}",
		wrongLabel:   "wrong:",
		timeout:      "{B}{b}%s{/} fell asleep, moving on.",
		dropped:      "{B}{b}%s{/} is out (keeps sleeping).",
		notYourTurn:  "Easy, it is {B}{b}%s{/}'s turn.",
		aborted:      "Nobody is playing, game over. The answer was: {y}%s{/}",
		stopped:      "Game stopped. The answer was: {y}%s{/}",
		alreadyGame:  "There is already a game here (try {y}!stop{/}).",
		notHere:      "Not here: come to {y}%s{/}, or play solo in a query ({y}!rvf{/}).",
		noPuzzles:    "No puzzles available.",
		playersOnly:  "Only players can stop the game.",
		top10Title:   "{B}{b}Wheel of Fortune top ten{/}",
		top10Empty:   "No winners yet.",
		queryHelp:    "Say {y}spin{/}, {y}buy <vowel>{/}, {y}solve <phrase>{/} or {y}pass{/}.",

		spinRe:  regexp.MustCompile(`^(?i)\s*spin\s*[!.]*$`),
		buyRe:   regexp.MustCompile(`^(?i)\s*buy\s+([a-zA-Z])\s*[!?.]*$`),
		solveRe: regexp.MustCompile(`^(?i)\s*solve[:\s]\s*(.+?)\s*$`),
		passRe:  regexp.MustCompile(`^(?i)\s*pass\s*[!.]*$`),
	},
}
