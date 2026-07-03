package format

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The telnet table formatter (Perl Functions::tfcprint, "stolen from
// tt01"). A format is pipe-separated "<width><printf-fmt>" fields, e.g.
// "10%s|20%s"; colors is a pipe-separated list of {x} tag letters per
// column. Validation errors are returned as printable strings, Perl
// style. Widths and substring math count runes (Perl chars).

var (
	fieldWidthRe = regexp.MustCompile(`^(\d+)`)
	fieldRe      = regexp.MustCompile(`^(\d+)(%.*)$`)
	tableTagRe   = regexp.MustCompile(`\{[^}]{1,2}\}`)
	tableWsRe    = regexp.MustCompile(`\s+`)
)

// validateFormat strips whitespace and applies the Perl sanity checks
// (which only ever look at the first field). Returns the fields and an
// error string; exactly one of them is non-zero.
func validateFormat(format string) ([]string, string) {
	format = tableWsRe.ReplaceAllString(format, "")
	if format == "" {
		return nil, ""
	}
	fields := strings.Split(format, "|")
	m := fieldWidthRe.FindString(fields[0])
	if m == "" {
		return nil, "Failed to parse format " + format + "!\n"
	}
	if w, _ := strconv.Atoi(m); w <= 4 {
		return nil, format + " has length < 5. Minimum is 5 (2 padding, 3 text)!\n"
	}
	return fields, ""
}

// TableSep renders the +-----+----+ separator line for format.
func TableSep(format string) string {
	fields, errStr := validateFormat(format)
	if fields == nil {
		return errStr
	}
	var out strings.Builder
	lastW := 0
	for _, f := range fields {
		if m := fieldWidthRe.FindString(f); m != "" {
			lastW, _ = strconv.Atoi(m)
		}
		out.WriteString("+")
		out.WriteString(strings.Repeat("-", lastW))
	}
	out.WriteString("+\n")
	return out.String()
}

// TableRow renders args as one table row (more lines when a value does
// not fit its column; continuations get a "+>" marker). Color tags
// inside a value are preserved and count as zero width; a {/} inside a
// value resets to the column color, not to the terminal default.
func TableRow(format, colors string, args []string) string {
	fields, errStr := validateFormat(format)
	if fields == nil {
		return errStr
	}
	colorList := splitColors(colors)
	if len(fields) != len(args) || len(fields) != len(colorList) {
		return fmt.Sprintf("[%s]: #Formats != #args != #colors ! --formats: %d colors: %d args: %d\n",
			tableWsRe.ReplaceAllString(format, ""), len(fields), len(colorList), len(args))
	}

	// split values into alternating text/tag pieces, tags zero-width
	pieces := make([][]string, len(args))
	for i, a := range args {
		pieces[i] = splitKeepTags(a)
	}

	var out strings.Builder
	again := true
	for n := 0; again; n++ {
		again = false
		out.WriteString("| {/}")
		for i, f := range fields {
			fm := fieldRe.FindStringSubmatch(f)
			if fm == nil {
				continue
			}
			l, _ := strconv.Atoi(fm[1])
			pf := fm[2]
			if n > 0 && l > 2 {
				l -= 2
				out.WriteString("+>")
			}
			if l > 2 {
				l -= 2
			}
			tmp, lastColor := collectPieces(&pieces[i], l, colorList[i])
			fmt.Fprintf(&out, "{%s}"+pf+"{/} | ", colorList[i], tmp)
			if len(pieces[i]) > 0 {
				again = true
			}
			if again && lastColor != "" {
				pieces[i] = append([]string{"", lastColor}, pieces[i]...)
			}
		}
		out.WriteString("\n")
	}
	return out.String()
}

// collectPieces consumes as much of the value as fits in width runes,
// carrying color tags for free, and pads with spaces. Returns the cell
// content and the color tag the next line must restart with, if any.
func collectPieces(arg *[]string, width int, fieldColor string) (string, string) {
	var tmp strings.Builder
	lastColor := ""
	realLen := 0
	for len(*arg) > 0 && realLen < width {
		text := []rune((*arg)[0])
		if realLen+len(text) <= width {
			tmp.WriteString((*arg)[0])
			realLen += len(text)
			*arg = (*arg)[1:]
			if len(*arg) > 0 {
				tag := (*arg)[0]
				tmp.WriteString(tag)
				*arg = (*arg)[1:]
				if tag == "{/}" {
					// reset to column color
					tmp.WriteString("{")
					tmp.WriteString(fieldColor)
					tmp.WriteString("}")
					lastColor = ""
				} else {
					lastColor = tag
				}
			}
		} else {
			take := width - realLen
			tmp.WriteString(string(text[:take]))
			(*arg)[0] = string(text[take:])
			realLen = width
		}
	}
	if pad := width - realLen; pad > 0 {
		tmp.WriteString(strings.Repeat(" ", pad))
	}
	return tmp.String(), lastColor
}

// splitColors splits "G|/" into per-column color letters, trimming
// whitespace around the pipes (Perl split /\s*\|\s*/).
func splitColors(colors string) []string {
	parts := strings.Split(colors, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// splitKeepTags splits a value into alternating text and {xx} tag
// pieces, keeping the tags as their own elements and dropping trailing
// empties (Perl split with a capturing pattern).
func splitKeepTags(s string) []string {
	var parts []string
	last := 0
	for _, loc := range tableTagRe.FindAllStringIndex(s, -1) {
		parts = append(parts, s[last:loc[0]], s[loc[0]:loc[1]])
		last = loc[1]
	}
	parts = append(parts, s[last:])
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}
