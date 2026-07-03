package format

import (
	"regexp"

	"github.com/rivo/uniseg"
)

// maxLine is the outbound IRC line budget used by SplitMessage: the
// 512-byte RFC limit minus slack for the server-added prefix, same 448
// the Perl bot settled on.
const maxLine = 448

var wsRe = regexp.MustCompile(`\s+`)

// WrapText splits msg into lines of at most maxBytes UTF-8 bytes,
// breaking on whitespace; a single word longer than maxBytes is split on
// rune boundaries. Perl parity with one fix: the Perl version corrupts
// multibyte words that get joined with a following word ('use bytes'
// concat); here the bytes stay intact.
func WrapText(msg string, maxBytes int) []string {
	words := wsRe.Split(msg, -1)
	if len(words) == 1 && words[0] == "" {
		return nil
	}
	var parts []string
	for len(words) > 0 {
		line := words[0]
		words = words[1:]
		if len(line) > maxBytes {
			// single big word, such as a bargraph
			parts = append(parts, splitBigWord(line, maxBytes)...)
			continue
		}
		for len(words) > 0 && len(line)+1+len(words[0]) < maxBytes {
			line += " " + words[0]
			words = words[1:]
		}
		parts = append(parts, line)
	}
	return parts
}

// splitBigWord chunks a word on rune boundaries: flush when the next
// rune would push the chunk past maxBytes (Perl wrapText inner loop).
func splitBigWord(word string, maxBytes int) []string {
	var parts []string
	part := ""
	runes := []rune(word)
	for i, r := range runes {
		part += string(r)
		if i+1 < len(runes) && len(part)+len(string(runes[i+1])) > maxBytes {
			parts = append(parts, part)
			part = ""
		}
	}
	if len(part) > 0 {
		parts = append(parts, part)
	}
	return parts
}

// SplitMessage prefixes msg for the wire ("PRIVMSG #chan :" + text),
// wrapping into multiple lines when prefix+msg reaches 448 bytes (Perl
// splitMessageData).
func SplitMessage(prefix, msg string) []string {
	if len(prefix)+len(msg) < maxLine {
		return []string{prefix + msg}
	}
	lines := WrapText(msg, maxLine-len(prefix))
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = prefix + l
	}
	return out
}

// TruncateIRC cuts s to at most maxBytes without splitting an extended
// grapheme cluster (the Perl Unicode::Truncate::truncate_egc at 510
// bytes in writeServer).
func TruncateIRC(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	kept := 0
	state := -1
	rest := s
	for len(rest) > 0 {
		cluster, tail, _, next := uniseg.FirstGraphemeClusterInString(rest, state)
		if kept+len(cluster) > maxBytes {
			break
		}
		kept += len(cluster)
		rest, state = tail, next
	}
	return s[:kept]
}
