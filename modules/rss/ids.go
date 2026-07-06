package rss

// short item ids: base-32 (A-Z plus 0 2 4 6 8 9), wrapping at 32768 so
// they stay at most three characters. Two things get skipped: rss
// command words, and (the !LI6 fix, absent in the Perl) ids still
// attached to live history items.

// rssCommands are words that can never be item ids.
var rssCommands = map[string]bool{
	"add": true, "del": true, "help": true, "refresh": true,
	"last": true, "list": true,
}

var idAlphabet = []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZ024689")

// idAlloc hands out short ids. inUse reports whether an id is still
// referenced (nil means nothing is).
type idAlloc struct {
	max   int
	inUse func(id string) bool
}

func newIDAlloc(counterStart int, inUse func(string) bool) *idAlloc {
	return &idAlloc{max: counterStart, inUse: inUse}
}

// counter exposes the position for persistence.
func (a *idAlloc) counter() int { return a.max }

// next returns the next free short id.
func (a *idAlloc) next() string {
	for {
		n := a.max
		a.max++
		if a.max >= 32768 {
			a.max = 1 // wrap around so the ids stay short
		}
		id := encodeID(n)
		if rssCommands[id] || (a.inUse != nil && a.inUse(id)) {
			continue
		}
		return id
	}
}

func encodeID(n int) string {
	var buf []byte
	for {
		buf = append(buf, idAlphabet[n&31])
		n >>= 5
		if n == 0 {
			break
		}
	}
	// reverse
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}
