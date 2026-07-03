package pizza

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// the greek-letter id names, verbatim
var names = []string{
	"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta",
	"iota", "kappa", "lambda", "mu", "nu", "xi", "omicron", "pi", "rho",
	"sigma", "tau", "upsilon", "phi", "chi", "psi", "omega",
}

// idPool hands out unique greek-digit timer ids (the Perl getID).
type idPool struct {
	rand     func() float64
	used     map[string]bool
	fallback int
}

func newIDPool(rand func() float64) *idPool {
	return &idPool{rand: rand, used: make(map[string]bool)}
}

func (p *idPool) get() string {
	for range 10 {
		id := fmt.Sprintf("%s-%d",
			names[int(p.rand()*float64(len(names)-1)+0.5)], int(p.rand()*8))
		if !p.used[id] {
			p.used[id] = true
			return id
		}
	}
	// in the rare event of all used...
	p.fallback++
	return fmt.Sprintf("crowded-%d", p.fallback)
}

func (p *idPool) mark(id string)  { p.used[id] = true }
func (p *idPool) clear(id string) { delete(p.used, id) }

// repeatPart is one "+<n><unit>" of a repeat spec.
type repeatPart struct {
	Unit string `json:"unit"` // y mo w d h m s
	N    int    `json:"n"`
}

// timeInfo is a parsed timer spec (the Perl timeInfo hash).
type timeInfo struct {
	abs     time.Time
	diff    time.Duration
	id      string
	message string
	repeat  []repeatPart
}

var (
	timePartRe = regexp.MustCompile(`^(?i:(monday|tuesday|wednesday|thursday|friday|saturday|sunday|tu|we|th|fr|sa|su|mo|[\d:+\- ]|[ymshdw]))+\s`)
	repeatRe   = regexp.MustCompile(`^\s*r\{(.*?)\}\s*`)
	repeatOKRe = regexp.MustCompile(`^(\d+([ymshdw]|mo)\s*)+$`)
	repeatPRe  = regexp.MustCompile(`(\d+)([ymshdw]|mo)`)
	hmsRe      = regexp.MustCompile(`^(\d+):(\d+)(?::(\d+))?$`)
	dmyRe      = regexp.MustCompile(`^(\d+)-(\d+)-(\d+)$`)
	relRe      = regexp.MustCompile(`^([+\-])?(\d+)([smhdyw]|mo)$`)
	weekdayRe  = regexp.MustCompile(`^(?i:monday|tuesday|wednesday|thursday|friday|saturday|sunday|mo|tu|we|th|fr|sa|su)$`)
)

var weekdays = map[string]int{
	"monday": 1, "tuesday": 2, "wednesday": 3, "thursday": 4,
	"friday": 5, "saturday": 6, "sunday": 7,
	"mo": 1, "tu": 2, "we": 3, "th": 4, "fr": 5, "sa": 6, "su": 7,
}

// parse state for one spec
type parseState struct {
	absTime  *[3]int // h m s
	absDate  *[3]int // d m y
	relative map[string]int
}

// parseTime parses a timer spec ("18:30 r{1d} eten"). On failure the
// second return is the user-facing error, Perl texts kept.
func parseTime(input string, now time.Time, ids *idPool) (*timeInfo, string) {
	// divergence: the Perl regex demanded whitespace after the time
	// part, but command data arrives trimmed, so a bare "+5m" failed
	// and the default burning-pizza message was unreachable; the
	// trailing space here makes both work as evidently intended
	input += " "
	m := timePartRe.FindString(input)
	if m == "" {
		return nil, "Unable to parse date/time part"
	}
	timePart := strings.TrimRight(m, " \t")
	rest := input[len(m):]

	var repeat []repeatPart
	if rm := repeatRe.FindStringSubmatch(rest); rm != nil {
		if !repeatOKRe.MatchString(rm[1]) {
			return nil, "Unable to parse repeat part"
		}
		for _, g := range repeatPRe.FindAllStringSubmatch(rm[1], -1) {
			n, _ := strconv.Atoi(g[1])
			repeat = append(repeat, repeatPart{Unit: g[2], N: n})
		}
		rest = rest[len(rm[0]):]
	}

	id := ids.get()
	message := strings.TrimSpace(rest)
	if message == "" {
		message = fmt.Sprintf("QUICK! Pizza %s is burning!", id)
	}

	st := &parseState{relative: make(map[string]int)}
	for token := range strings.FieldsSeq(timePart) {
		if !parseTimePart(token, st, now) {
			ids.clear(id)
			return nil, "Unable to parse " + token
		}
	}

	// start from now, override with absolutes, then add relatives
	year, month, day := now.Date()
	hour, minute, second := now.Clock()
	if st.absDate != nil {
		day, month, year = st.absDate[0], time.Month(st.absDate[1]), st.absDate[2]
	}
	if st.absTime != nil {
		hour, minute, second = st.absTime[0], st.absTime[1], st.absTime[2]
	}
	at := time.Date(year, month, day, hour, minute, second, 0, now.Location())
	at = addRelative(at, st.relative)

	if at.Before(now) {
		if st.absDate != nil {
			ids.clear(id)
			return nil, "This bot travels forward in time."
		}
		at = at.AddDate(0, 0, 1)
	}
	if at.Before(now) {
		ids.clear(id)
		return nil, "This bot travels forward in time."
	}
	return &timeInfo{
		abs: at, diff: at.Sub(now), id: id, message: message, repeat: repeat,
	}, ""
}

// parseTimePart handles one whitespace-separated token.
func parseTimePart(token string, st *parseState, now time.Time) bool {
	if g := hmsRe.FindStringSubmatch(token); g != nil {
		h, _ := strconv.Atoi(g[1])
		m, _ := strconv.Atoi(g[2])
		s := 0
		if g[3] != "" {
			s, _ = strconv.Atoi(g[3])
		}
		st.absTime = &[3]int{h, m, s}
		return true
	}
	if g := dmyRe.FindStringSubmatch(token); g != nil {
		d, _ := strconv.Atoi(g[1])
		m, _ := strconv.Atoi(g[2])
		y, _ := strconv.Atoi(g[3])
		st.absDate = &[3]int{d, m, y}
		return true
	}
	if g := relRe.FindStringSubmatch(token); g != nil {
		n, _ := strconv.Atoi(g[2])
		if g[1] == "-" {
			n = -n
		}
		st.relative[g[3]] += n
		return true
	}
	if weekdayRe.MatchString(token) {
		dow := weekdays[strings.ToLower(token)]
		base := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		if st.absDate != nil {
			base = time.Date(st.absDate[2], time.Month(st.absDate[1]), st.absDate[0],
				0, 0, 0, 0, now.Location())
		}
		baseDow := int(base.Weekday())
		if baseDow == 0 {
			baseDow = 7 // sunday, perl DateTime counts 1..7
		}
		switch {
		case baseDow == dow:
			// only idiots would mean the current day, unless the base
			// is some absolute date further out
			if d := base.Sub(now); d < 24*time.Hour && d > -24*time.Hour {
				base = base.AddDate(0, 0, 7)
			}
		case baseDow < dow:
			base = base.AddDate(0, 0, dow-baseDow)
		default:
			base = base.AddDate(0, 0, 7-(baseDow-dow))
		}
		st.absDate = &[3]int{base.Day(), int(base.Month()), base.Year()}
		return true
	}
	return false
}

// relOrder fixes the application order of relative offsets (the Perl
// iterated hash keys, so its order was luck; largest-first is sane).
var relOrder = []string{"y", "mo", "w", "d", "h", "m", "s"}

func addRelative(t time.Time, rel map[string]int) time.Time {
	for _, unit := range relOrder {
		n := rel[unit]
		if n == 0 {
			continue
		}
		t = addUnit(t, unit, n)
	}
	return t
}

func addUnit(t time.Time, unit string, n int) time.Time {
	switch unit {
	case "y":
		return t.AddDate(n, 0, 0)
	case "mo":
		return t.AddDate(0, n, 0)
	case "w":
		return t.AddDate(0, 0, 7*n)
	case "d":
		return t.AddDate(0, 0, n)
	case "h":
		return t.Add(time.Duration(n) * time.Hour)
	case "m":
		return t.Add(time.Duration(n) * time.Minute)
	default: // s
		return t.Add(time.Duration(n) * time.Second)
	}
}

// nextRepeat applies the repeat parts to the previous due time.
func nextRepeat(prev time.Time, parts []repeatPart) time.Time {
	for _, p := range parts {
		prev = addUnit(prev, p.Unit, p.N)
	}
	return prev
}
