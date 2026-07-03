// Package cmd is the IRC ! command dispatcher, the Go counterpart of
// the Perl IRC.pm command system: registerCommand with
// multi-registration (every registered module fires), default commands
// for unmatched !words (priority descending, continue=false handlers
// run first and stop at the first hit, then all continue=true
// handlers), and the Levenshtein did-you-mean suggestion for unknown
// commands. No permissions on ! commands, by design. Dispatcher
// goroutine only.
package cmd

import (
	"log/slog"
	"math/rand/v2"
	"regexp"
	"slices"
	"sort"
	"strings"

	"go-botje/internal/bus"
)

// Data is what a command handler receives (the Perl cmdHash).
type Data struct {
	Command string // the matched command word, empty for default handlers
	Data    string // trimmed text after the command
	Event   *bus.Event
}

// Handler handles a command. The return value only matters for default
// handlers: true means handled, stopping further continue=false ones.
type Handler func(*Data) bool

type registration struct {
	module string
	fn     Handler
}

type defaultCmd struct {
	module   string
	priority int
	cont     bool
	fn       Handler
	dead     bool // panicked, swept after dispatch
}

// the Perl @annoy list, appended to 1 in 6 suggestions
var annoy = []string{"..derpdurpderp..", ";)", "?????", "Orrr..", "(:"}

var cmdRe = regexp.MustCompile(`^!(\w+)(?:\s+(.*?))?$`)

// Registry holds command registrations. Not goroutine-safe: owned by
// the dispatcher.
type Registry struct {
	// Reply, when set, receives the did-you-mean suggestion, {x}-tagged
	// ("nick: Maybe you meant: ..."). Wire it to cmd_privmsg.
	Reply func(ev *bus.Event, msg string)
	// Rand is injectable randomness for the suggestion annoyance
	// suffix; nil means math/rand.
	Rand func(n int) int

	commands map[string][]registration
	defaults []defaultCmd // kept sorted by priority descending
}

// New returns an empty registry with command char '!'.
func New() *Registry {
	return &Registry{commands: make(map[string][]registration)}
}

// Register adds module's handler for word, replacing the module's
// previous handler for that word. Multiple modules may register the
// same word; all fire in registration order.
func (r *Registry) Register(module, word string, h Handler) {
	regs := r.commands[word]
	for i := range regs {
		if regs[i].module == module {
			regs[i].fn = h
			return
		}
	}
	r.commands[word] = append(regs, registration{module: module, fn: h})
}

// RegisterDefault adds a catch-all for unmatched !words.
func (r *Registry) RegisterDefault(module string, priority int, cont bool, h Handler) {
	r.defaults = append(r.defaults, defaultCmd{module: module, priority: priority, cont: cont, fn: h})
	sort.SliceStable(r.defaults, func(i, j int) bool {
		return r.defaults[i].priority > r.defaults[j].priority
	})
}

// UnregisterModule drops all command and default registrations of module.
func (r *Registry) UnregisterModule(module string) {
	for word, regs := range r.commands {
		regs = slices.DeleteFunc(regs, func(reg registration) bool {
			return reg.module == module
		})
		if len(regs) == 0 {
			delete(r.commands, word)
		} else {
			r.commands[word] = regs
		}
	}
	r.defaults = slices.DeleteFunc(r.defaults, func(d defaultCmd) bool {
		return d.module == module
	})
}

// Has reports whether any module registered word (the Perl
// checkCommand; karma uses it to leave !command++ alone).
func (r *Registry) Has(word string) bool {
	_, ok := r.commands[word]
	return ok
}

// Commands lists all registered command words, sorted.
func (r *Registry) Commands() []string {
	out := make([]string, 0, len(r.commands))
	for word := range r.commands {
		out = append(out, word)
	}
	slices.Sort(out)
	return out
}

// Handle dispatches ev.Msg if it is a !command; reports whether it was
// one. Panicking handlers are dropped from the registry.
func (r *Registry) Handle(ev *bus.Event) bool {
	m := cmdRe.FindStringSubmatch(ev.Msg)
	if m == nil {
		return false
	}
	word, rest := m[1], m[2]

	if regs, ok := r.commands[word]; ok {
		d := &Data{Command: word, Data: strings.TrimSpace(rest), Event: ev}
		var dead []string
		for _, reg := range slices.Clone(regs) {
			if !r.call(reg.module, "!"+word, reg.fn, d) {
				dead = append(dead, reg.module)
			}
		}
		for _, module := range dead {
			r.removeCommand(word, module)
		}
		return true
	}

	// unknown command: defaults, then did-you-mean, then continue defaults
	d := &Data{Data: strings.TrimSpace(word + " " + rest), Event: ev}
	if r.runDefaults(d, false) {
		return true
	}
	r.suggest(ev, word)
	r.runDefaults(d, true)
	return true
}

// call runs one handler panic-safely; false means it panicked.
func (r *Registry) call(module, what string, h Handler, d *Data) (ok bool) {
	defer func() {
		if rec := recover(); rec != nil {
			ok = false
			slog.Error("cmd: handler panicked, dropping registration",
				"module", module, "command", what, "panic", rec)
		}
	}()
	h(d)
	return true
}

// runDefaults runs the matching default handlers (already priority
// sorted). For cont=false the first handled=true stops the group.
func (r *Registry) runDefaults(d *Data, cont bool) bool {
	handled := false
	for i := range r.defaults {
		dc := &r.defaults[i]
		if dc.cont != cont || dc.dead {
			continue
		}
		var h bool
		if !r.callDefault(dc, d, &h) {
			dc.dead = true
			continue
		}
		handled = h
		if !cont && handled {
			break
		}
	}
	r.defaults = slices.DeleteFunc(r.defaults, func(dc defaultCmd) bool { return dc.dead })
	return handled
}

func (r *Registry) callDefault(dc *defaultCmd, d *Data, handled *bool) (ok bool) {
	defer func() {
		if rec := recover(); rec != nil {
			ok = false
			slog.Error("cmd: default handler panicked, dropping it",
				"module", dc.module, "panic", rec)
		}
	}()
	*handled = dc.fn(d)
	return true
}

func (r *Registry) removeCommand(word, module string) {
	regs := slices.DeleteFunc(r.commands[word], func(reg registration) bool {
		return reg.module == module
	})
	if len(regs) == 0 {
		delete(r.commands, word)
	} else {
		r.commands[word] = regs
	}
}

// suggest sends the Levenshtein did-you-mean for an unknown command:
// distance <= 2, or <= 1 for words of 3 characters or less, ties sorted
// alphabetically (the Perl left ties in hash order).
func (r *Registry) suggest(ev *bus.Event, word string) {
	if r.Reply == nil {
		return
	}
	maxDist := 2
	if len(word) <= 3 {
		maxDist = 1
	}
	type scored struct {
		dist int
		word string
	}
	var possible []scored
	for _, w := range r.Commands() { // sorted, so ties come out alphabetical
		if d := levenshtein(w, word); d <= maxDist {
			possible = append(possible, scored{d, w})
		}
	}
	if len(possible) == 0 {
		return
	}
	sort.SliceStable(possible, func(i, j int) bool { return possible[i].dist < possible[j].dist })

	words := make([]string, len(possible))
	for i, p := range possible {
		words[i] = p.word
	}
	anyOf := ""
	if len(possible) > 1 {
		anyOf = " any of"
	}
	reply := "Maybe you meant" + anyOf + ": {W}" + strings.Join(words, "{/}, {W}") + "{/}?"
	if r.intn(6) == 1 {
		reply += " " + annoy[r.intn(len(annoy))]
	}
	r.Reply(ev, ev.Sender.Nick+": "+reply)
}

func (r *Registry) intn(n int) int {
	if r.Rand != nil {
		return r.Rand(n)
	}
	return rand.IntN(n)
}

// levenshtein is the classic edit distance over runes.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i, ca := range ra {
		cur[0] = i + 1
		for j, cb := range rb {
			cost := 1
			if ca == cb {
				cost = 0
			}
			cur[j+1] = min(prev[j]+cost, prev[j+1]+1, cur[j]+1)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
