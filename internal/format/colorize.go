// Package format is the output formatting layer: {x} color tags, IRC
// line wrapping and truncation, sparklines, and the telnet table
// formatter. Behavior is golden-tested against the real Perl code
// (testdata/golden-gen.pl); deliberate divergences are documented on the
// functions and covered in format_test.go.
package format

import (
	"regexp"
	"strconv"
	"strings"
)

// tag -> ANSI escape, the Perl Log.pm %ctt table.
var tagANSI = map[string]string{
	"{/}": "\x1b[0m",
	"{r}": "\x1b[31m", "{R}": "\x1b[31;1m",
	"{g}": "\x1b[32m", "{G}": "\x1b[32;1m",
	"{y}": "\x1b[33m", "{Y}": "\x1b[33;1m",
	"{b}": "\x1b[34m",
	"{m}": "\x1b[35m", "{M}": "\x1b[35;1m",
	"{c}": "\x1b[36m", "{C}": "\x1b[36;1m",
	"{w}": "\x1b[37m", "{W}": "\x1b[37;1m",
	"{B}": "\x1b[1m", // bold, not blue
	"{_}": "\x1b[4m",
}

var tagRe = regexp.MustCompile(`\{[0-9A-Za-z_/]\}`)

// ToANSI replaces {x} color tags with ANSI escapes (the Perl
// Log::subColorTags). Unknown single-char tags are stripped. Divergences
// from Perl: {n} strips the tag and keeps the following word unstyled
// (only the Perl console log path colored it, no IRC module uses it),
// and the debug-only tags {i} {u} {I} are stripped instead of being
// replaced with literal placeholder words.
func ToANSI(s string) string {
	return tagRe.ReplaceAllStringFunc(s, func(tag string) string {
		return tagANSI[tag] // unknown, {n}, {i}, {u}, {I}: strip
	})
}

// ToIRC converts {x} color tags to mIRC control codes: the full Perl
// output pipeline, colorize followed by IRC translateColors. All
// IRC-bound module output goes through this.
func ToIRC(s string) string {
	return ansiToMIRC(ToANSI(s))
}

// mIRC color/formatting control codes plus stray ANSI escapes.
var controlRe = regexp.MustCompile(`\x03(?:[0-9]{1,2}(?:,[0-9]{1,2})?)?|[\x02\x0f\x16\x1d\x1f]|\x1b\[[0-9;]*m`)

// Strip removes all formatting: {x} tags, mIRC control codes, and ANSI
// escapes. For plain-text sinks like log files, where both bot output
// (tagged) and user input (raw mIRC codes) must come out readable.
func Strip(s string) string {
	return controlRe.ReplaceAllString(tagRe.ReplaceAllString(s, ""), "")
}

// ANSI SGR code -> mIRC code, the Perl IRC.pm %colorCodes table. Bold
// is a state, not a code: it selects the bright variant of following
// colors (mIRC \x02 is never emitted, same as Perl).
var mircNormal = map[int]string{
	0:  "\x0f",   // reset
	31: "\x0305", // brown
	32: "\x0303", // green
	33: "\x0307", // orange
	34: "\x0302", // blue
	35: "\x0306", // purple
	36: "\x0310", // cyan
	37: "\x0315", // white
	4:  "\x1f",   // underline
}

var mircBold = map[int]string{
	0:  "\x0f",
	31: "\x0304", // red
	32: "\x0309",
	33: "\x0308",
	34: "\x0312",
	35: "\x0313",
	36: "\x0311",
	37: "\x0300",
	4:  "\x1f",
}

var (
	ansiRe  = regexp.MustCompile(`\x1b\[([0-9;]+)m`)
	commaRe = regexp.MustCompile(`(\x03[0-9]{2},)`)
)

// ansiToMIRC is the Perl translateColors: ANSI SGR escapes to mIRC
// codes with the bold state machine, then the comma fixup that keeps a
// literal ",<digit>" after a color code from being eaten as a background
// color (by inserting an explicit 01 background).
func ansiToMIRC(s string) string {
	bold := false
	out := ansiRe.ReplaceAllStringFunc(s, func(esc string) string {
		var repl strings.Builder
		var active []int
		for part := range strings.SplitSeq(esc[2:len(esc)-1], ";") {
			code, err := strconv.Atoi(part)
			if err != nil {
				continue
			}
			if code == 1 {
				bold = true
				continue
			}
			if code == 0 {
				bold = false
			}
			if _, ok := mircNormal[code]; ok {
				active = append(active, code)
			}
		}
		table := mircNormal
		if bold {
			table = mircBold
		}
		for _, code := range active {
			repl.WriteString(table[code])
		}
		return repl.String()
	})
	return commaRe.ReplaceAllString(out, "${1}01,")
}
