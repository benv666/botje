package rvf

import "fmt"

// victoryArts are the win banners: 3-4 colorized lines each, one %s
// for the language-specific winner line. No stray single-char {x}
// sequences besides real color tags: colorize would eat them
// (TestVictoryArtVariants keeps this honest).
var victoryArts = []string{
	// fireworks
	"{r}*{/}  {y}.{/}    {c}*{/}    {m}.{/}   {y}*{/}\n" +
		"{y} \\ | /{/}    %s\n" +
		"{y}-- {Y}O{/} {y}--{/}   {c}*{/}    {r}.{/}\n" +
		"{y} / | \\{/}   {m}*{/}    {c}.{/}",
	// trophy
	"{y}  .-'''-.{/}    %s\n" +
		"{y} (  {Y}\\o/{/}{y}  ){/}\n" +
		"{y}  '-. .-'{/}\n" +
		"{y}   _|_|_{/}",
	// jackpot
	"{g} $ $ $ $ $ $ $ ${/}\n" +
		"{g}${/}  {Y}J A C K P O T{/}  {g}${/}   %s\n" +
		"{g} $ $ $ $ $ $ $ ${/}",
	// the wheel itself
	"{m}   .-'{Y}*{/}{m}'-.{/}\n" +
		"{m}  /  {Y}WIN{/}{m}  \\{/}    %s\n" +
		"{m}  \\   {Y}*{/}{m}   /{/}\n" +
		"{m}   '-.__.-'{/}",
	// podium
	"{Y} *    *    *    *{/}\n" +
		"{c}  ____________{/}    %s\n" +
		"{c}  \\   {Y}# 1{/}{c}    /{/}\n" +
		"{c}   '--------'{/}",
}

// victory renders a random win banner for nick, display-truncated so a
// novelty nick cannot wreck the art.
func victory(t *texts, roll func(int) int, nick string) string {
	if len(nick) > 20 {
		nick = nick[:20] + "…"
	}
	banner := fmt.Sprintf(t.winBanner, nick)
	return fmt.Sprintf(victoryArts[roll(len(victoryArts))], banner)
}
