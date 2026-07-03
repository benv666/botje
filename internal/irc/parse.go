// Package irc is the IRC protocol layer: line framing, parsing,
// numerics, and (to come) the connection state machine and flood
// control. Stdlib only, by design; behavior mirrors the Perl IRC.pm.
package irc

import (
	"regexp"
	"strings"
)

// Line is one parsed IRC line, pre-splitParams: the Perl %command hash.
type Line struct {
	Prefix string // raw prefix without the leading ':', empty if absent
	Cmd    string // command as received: "PRIVMSG" or "332"
	Name   string // numerics resolved via the numerics table, else Cmd
	Code   string // the numeric code when Cmd is one, else empty
	Params string // unparsed params, split lazily with SplitParams
}

var (
	//                <prefix>          <cmd>     <params>
	lineRe   = regexp.MustCompile(`^(?::(\S+)\s+)?(\S+)(?:\s+(.+))?$`)
	cmdRe    = regexp.MustCompile(`^(?:\d\d\d|[a-zA-Z]+)$`)
	prefixRe = regexp.MustCompile(`^(.+?)(?:!(.+?))?(?:@(.+?))?$`)
	middleRe = regexp.MustCompile(`^\s*([^\s:]+)\s+`)
)

// ParseLine parses one full IRC line (no CRLF). Reports false for lines
// the Perl parser drops: empty, no command, or a command that is neither
// three digits nor letters.
func ParseLine(line string) (Line, bool) {
	m := lineRe.FindStringSubmatch(line)
	if m == nil {
		return Line{}, false
	}
	l := Line{Prefix: m[1], Cmd: m[2], Params: m[3]}
	if !cmdRe.MatchString(l.Cmd) {
		return Line{}, false
	}
	l.Name = l.Cmd
	if l.Cmd[0] >= '0' && l.Cmd[0] <= '9' {
		l.Code = l.Cmd
		if name, ok := numerics[l.Cmd]; ok {
			l.Name = name
		}
	}
	return l, true
}

// ParsePrefix splits "nick!user@host"; missing parts come back empty.
// A server prefix lands entirely in nick, like the Perl.
func ParsePrefix(prefix string) (nick, user, host string) {
	m := prefixRe.FindStringSubmatch(prefix)
	if m == nil {
		return "", "", ""
	}
	return m[1], m[2], m[3]
}

// SplitParams splits an IRC params string into middles and trailing
// (the Perl splitParams, quirks included: a middle containing ':'
// starts the trailing).
func SplitParams(params string) []string {
	var out []string
	for {
		m := middleRe.FindStringSubmatch(params)
		if m == nil {
			break
		}
		out = append(out, m[1])
		params = params[len(m[0]):]
	}
	if len(params) > 0 {
		out = append(out, strings.TrimPrefix(params, ":"))
	}
	return out
}

// LineBuffer frames a raw byte stream into complete IRC lines: split on
// \r?\n, empty lines dropped, partial lines (and partial UTF-8 runes,
// since bytes buffer until the newline) kept for the next Feed. Invalid
// UTF-8 bytes inside a complete line are dropped.
type LineBuffer struct {
	buf []byte
}

// Feed appends data and returns every complete line now available.
func (b *LineBuffer) Feed(data []byte) []string {
	b.buf = append(b.buf, data...)
	var lines []string
	for {
		nl := strings.IndexByte(string(b.buf), '\n')
		if nl < 0 {
			return lines
		}
		line := b.buf[:nl]
		b.buf = b.buf[nl+1:]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if len(line) == 0 {
			continue
		}
		lines = append(lines, strings.ToValidUTF8(string(line), ""))
	}
}
