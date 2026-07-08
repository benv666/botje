// Package example is a working skeleton module: it exercises every part
// of the module API (module.Context) with comments explaining each one.
// Writing a new module? Copy this file, rename things, delete what you
// do not need. The Perl-era equivalent was IRC_Test.pm.
//
// It is deliberately NOT in the autoload list (cmd/botje/main.go
// modules()); its test keeps it compiling and loadable so it cannot
// rot. Add example.New() to that list if you want to play with it live.
package example

import (
	"fmt"
	"regexp"
	"time"

	"go-botje/internal/admin"
	"go-botje/internal/bus"
	"go-botje/internal/fetch"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
)

// Module holds the state. One instance lives for the process lifetime;
// Load/Unload may be called repeatedly (telnet `module` command), so
// Load must (re)initialize everything.
//
// CONCURRENCY: every hook, command, timer, and fetch callback runs on
// the single dispatcher goroutine. No locks needed anywhere, same as
// the Perl select loop. Blocking work must go through ctx.Fetch (or
// your own goroutine that re-enters via the work channel) - never
// sleep or do network IO directly in a handler.
type Module struct {
	ctx *module.Context
}

func New() *Module { return &Module{} }

// Name is the module's registry name: storage namespace, conf prefix
// convention (example_*), and the name used by the bus for hooks.
func (m *Module) Name() string { return "example" }

// stored is this module's storage value. One JSON-serializable value
// per key in the module's namespace; keep it a named struct so the
// shape is documented and migratable.
type stored struct {
	Counter int `json:"counter"`
}

func (m *Module) Load(ctx *module.Context) error {
	m.ctx = ctx

	// CONF: typed settings with defaults, changeable at runtime via the
	// telnet `conf` command. Values set via telnet persist in storage
	// (ns core, key conf) and win over the default on the next boot.
	// Convention: prefix with the module name.
	ctx.Conf.CreateString("example_greeting", "Hoi")

	// COMMANDS: `!example ...` in a channel (or query) lands in
	// cbExample. One module may register many words; several modules
	// may register the SAME word (all get called).
	ctx.Cmd.Register(m.Name(), "example", m.cbExample)

	// DEFAULT HANDLER: called for any !word no module claims, highest
	// priority first. continue=false handlers run before the Levenshtein
	// did-you-mean and the first one to return true (=handled) stops the
	// chain; continue=true handlers run after it, unconditionally.
	// (markov registers priority 1 continue=false and always handles, so
	// while it is loaded the suggester never fires - like live hoer.)
	ctx.Cmd.RegisterDefault(m.Name(), 50, true, m.cbDefault)

	// BUS HOOK: raw events. One hook per (module, event); the bus
	// refuses re-entry of the same pair, so a hook can safely Privmsg
	// (which emits IRC_SENT) without recursing. Full event list:
	// internal/core/core.go ircEvents.
	if err := ctx.Bus.RegisterHook(m.Name(), "IRC_JOIN", m.onJoin); err != nil {
		return err
	}

	// ADMIN COMMAND: the telnet port collects specs by submitting a
	// COMMAND event; return the spec as the hook payload. Su:true would
	// hide it from non-superusers.
	if err := ctx.Bus.RegisterHook(m.Name(), "COMMAND", func(*bus.Event) (bus.Handled, any) {
		return bus.None, admin.Spec{
			Name:  "example",
			Match: regexp.MustCompile(`^example$`),
			Help:  "Show the example module's counter",
			Run: func(_, _ string) string {
				var st stored
				m.ctx.Store.Get(m.Name(), "state", &st)
				return fmt.Sprintf("example counter is at {y}%d{/}", st.Counter)
			},
		}
	}); err != nil {
		return err
	}
	return nil
}

// Unload must undo every registration. Storage needs no cleanup; flush
// anything dirty here (we save on change, so nothing to do).
func (m *Module) Unload() error {
	m.ctx.Cmd.UnregisterModule(m.Name())
	m.ctx.Bus.UnregisterModule(m.Name())
	return nil
}

// cbExample: d.Command is "example", d.Data the text after it.
// d.Event has the full context (channel, sender, server). Reply via
// ctx.Privmsg for one-liners or ctx.Pager for anything that may exceed
// the flood budget.
func (m *Module) cbExample(d *cmd.Data) bool {
	switch {
	case d.Data == "count":
		// STORAGE: read-modify-write a value in our namespace. Get on a
		// missing key leaves the zero value; save on every change (or
		// mark dirty and save in Unload, but change-time saves survive
		// crashes - the Perl bot lost data by only saving on unload).
		var st stored
		m.ctx.Store.Get(m.Name(), "state", &st)
		st.Counter++
		if err := m.ctx.Store.Put(m.Name(), "state", st); err != nil {
			m.ctx.Privmsg(d.Event.Channel, "storage error: "+err.Error())
			return true
		}
		m.ctx.Privmsg(d.Event.Channel, fmt.Sprintf("counter: {g}%d{/}", st.Counter))

	case d.Data == "lines":
		// PAGER: multi-line replies. The first anti_flood_max_lines
		// (default 4) go out; the rest waits for !more. The command name
		// ties !more to this reply.
		lines := make([]string, 8)
		for i := range lines {
			lines[i] = fmt.Sprintf("line %d of 8", i+1)
		}
		m.ctx.Pager.EventMsg(d.Event, d.Command, lines...)

	case len(d.Data) > 6 && d.Data[:6] == "remind":
		// SCHED: timers run on the dispatcher too. After returns a tag
		// usable with Unschedule (keep it if the timer must not survive
		// Unload; see modules/pizza for the full pattern).
		channel, nick := d.Event.Channel, d.Event.Sender.Nick
		m.ctx.Sched.After(5*time.Second, func() {
			m.ctx.Privmsg(channel, nick+": "+d.Data[7:])
		})
		m.ctx.Privmsg(channel, "in 5 seconds...")

	case len(d.Data) > 5 && d.Data[:5] == "fetch":
		// FETCH: async HTTP. The callback is delivered ON THE DISPATCHER,
		// so it may touch module state freely. Returns false when the
		// same URL is already in flight (single-flight).
		channel := d.Event.Channel
		url := d.Data[6:]
		m.ctx.Fetch.Fetch(url, fetch.Options{}, func(res fetch.Result) {
			if res.Err != nil {
				m.ctx.Privmsg(channel, "fetch failed: "+res.Err.Error())
				return
			}
			m.ctx.Privmsg(channel, fmt.Sprintf("{c}%s{/}: %d bytes, status %d", url, len(res.Body), res.Status))
		})

	default:
		m.ctx.Privmsg(d.Event.Channel, "usage: !example count | lines | remind <msg> | fetch <url>")
	}
	return true
}

// cbDefault sees unclaimed !words; d.Command is empty, d.Data is the
// word plus arguments, without the bang ("greet ..."). Claim by
// returning true.
func (m *Module) cbDefault(d *cmd.Data) bool {
	if d.Data != "greet" {
		return false // not ours; let lower-priority defaults look at it
	}
	// CONF read: String/Int/Float/Bool panic on unknown names (a module
	// reading a setting it never created is a bug), so read what you
	// Create'd in Load.
	m.ctx.Privmsg(d.Event.Channel, m.ctx.Conf.String("example_greeting")+", "+d.Event.Sender.Nick+"!")
	return true
}

// onJoin greets joiners. SenderMe guards against reacting to the bot's
// own join. Returning bus.Replied with a payload feeds the caller's
// Submit collection; plain hooks return bus.None, nil. bus.Stop would
// block later modules' hooks for this event - almost never what you
// want.
func (m *Module) onJoin(ev *bus.Event) (bus.Handled, any) {
	if ev.SenderMe {
		return bus.None, nil
	}
	// stays quiet unless someone sets the conf value; an example module
	// should not spam real channels
	if greeting := m.ctx.Conf.String("example_greeting"); greeting == "loud" {
		m.ctx.Privmsg(ev.Channel, "Welcome, "+ev.Sender.Nick+"!")
	}
	return bus.None, nil
}
