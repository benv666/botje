// Package guard is the spam gatekeeper, stage 1: the toggle and the
// residents table. Enforcement (oper kickban, timed glines, the auth
// flow for newcomers mid-wave) is the next stage; design in CLAUDE.md.
//
// The module idles by default and passively learns who belongs on the
// network: every user@host seen joining or talking while the guard is
// OFF becomes a resident (aged out after guard_resident_days). When
// spammers show up, `guard on` via telnet freezes the trust set - no
// new residents are learned - and non-resident joins are counted and
// logged to the ops log, so BenV can watch what enforcement WOULD have
// acted on before it exists.
//
// Identity is user@host, not nick!user@host: nick changes must not
// reset residency, and drive-by spammers randomize nicks anyway.
package guard

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"go-botje/internal/admin"
	"go-botje/internal/bus"
	"go-botje/internal/module"
	"go-botje/internal/sched"
)

const flushEvery = 5 * time.Minute

// lastSeen updates within this window stay in memory only, keeping
// chatty channels from marking the table dirty on every line.
const seenSlack = time.Hour

// Module is the guard. Now is a test hook.
type Module struct {
	Now func() time.Time

	ctx       *module.Context
	residents map[string]map[string]int64 // server -> user@host -> last seen unix
	dirty     bool
	strangers int // non-resident joins seen while enabled, since load
	flushTag  sched.Tag
	flushSet  bool
}

func New() *Module { return &Module{Now: time.Now} }

func (m *Module) Name() string { return "guard" }

var guardRe = regexp.MustCompile(`^(?i)guard(?:\s+(on|off|status))?\s*$`)

func (m *Module) Load(ctx *module.Context) error {
	m.ctx = ctx
	m.residents = make(map[string]map[string]int64)
	m.strangers = 0
	m.dirty = false

	// the toggle lives in conf so it persists across restarts (core
	// stores runtime conf changes); days control resident aging
	ctx.Conf.CreateBool("guard_enabled", false)
	ctx.Conf.CreateInt("guard_resident_days", 90)

	if _, err := ctx.Store.Get(m.Name(), "residents", &m.residents); err != nil {
		return fmt.Errorf("guard: load residents: %w", err)
	}
	if m.residents == nil {
		m.residents = make(map[string]map[string]int64)
	}
	m.age()

	for ev, h := range map[string]bus.Handler{
		"IRC_PRIVMSG": m.onSeen,
		"IRC_JOIN":    m.onJoin,
		"QUIT":        m.onShutdown,
		"COMMAND":     m.onCommand,
	} {
		if err := ctx.Bus.RegisterHook(m.Name(), ev, h); err != nil {
			return err
		}
	}
	m.scheduleFlush()
	return nil
}

func (m *Module) Unload() error {
	if m.flushSet {
		m.ctx.Sched.Unschedule(m.flushTag)
		m.flushSet = false
	}
	m.flush()
	m.ctx.Bus.UnregisterModule(m.Name())
	return nil
}

func (m *Module) enabled() bool { return m.ctx.Conf.Bool("guard_enabled") }

// mask is the resident identity for an event sender; empty means the
// sender does not count (servers, the bot itself).
func mask(ev *bus.Event) string {
	if ev.SenderMe || ev.Sender.User == "" || ev.Sender.Host == "" {
		return ""
	}
	return strings.ToLower(ev.Sender.User + "@" + ev.Sender.Host)
}

func (m *Module) isResident(server, mask string) bool {
	_, ok := m.residents[server][mask]
	return ok
}

// record notes a sighting. While the guard is ON the trust set is
// frozen: a spam wave must not teach the guard its spammers. Existing
// residents' lastSeen still advances (throttled to once per hour so
// chatter does not keep the table permanently dirty).
func (m *Module) record(server, mask string) {
	if mask == "" {
		return
	}
	known := m.isResident(server, mask)
	if !known && m.enabled() {
		return
	}
	now := m.Now().Unix()
	if known && now-m.residents[server][mask] < int64(seenSlack/time.Second) {
		return
	}
	if m.residents[server] == nil {
		m.residents[server] = make(map[string]int64)
	}
	m.residents[server][mask] = now
	m.dirty = true
}

func (m *Module) onSeen(ev *bus.Event) (bus.Handled, any) {
	m.record(ev.Server, mask(ev))
	return bus.None, nil
}

func (m *Module) onJoin(ev *bus.Event) (bus.Handled, any) {
	mk := mask(ev)
	if mk != "" && m.enabled() && !m.isResident(ev.Server, mk) {
		// observability before enforcement exists: this line in the ops
		// log is what stage 2 will act on
		m.strangers++
		slog.Info("guard: non-resident join", "mask", mk, "nick", ev.Sender.Nick,
			"channel", ev.Channel, "server", ev.Server)
	}
	m.record(ev.Server, mk)
	return bus.None, nil
}

func (m *Module) onShutdown(*bus.Event) (bus.Handled, any) {
	m.flush()
	return bus.None, nil
}

func (m *Module) onCommand(*bus.Event) (bus.Handled, any) {
	return bus.None, admin.Spec{
		Name:  "guard",
		Match: guardRe,
		Help:  "Spam guard: status, or toggle with on/off",
		Args:  []string{"[on|off|status]"},
		Su:    true,
		Run: func(_, line string) string {
			g := guardRe.FindStringSubmatch(line)
			switch strings.ToLower(g[1]) {
			case "on":
				if err := m.ctx.Conf.Set("guard_enabled", "true"); err != nil {
					return fmt.Sprintf("{r}Error:{/} %v", err)
				}
				slog.Info("guard: enabled")
				return "{r}Guard is ON.{/} Trust set frozen; non-resident joins are logged.\n" + m.status()
			case "off":
				if err := m.ctx.Conf.Set("guard_enabled", "false"); err != nil {
					return fmt.Sprintf("{r}Error:{/} %v", err)
				}
				slog.Info("guard: disabled")
				return "{g}Guard is off.{/} Back to learning residents.\n" + m.status()
			default:
				return m.status()
			}
		},
	}
}

func (m *Module) status() string {
	state := "{g}off{/} (learning residents)"
	if m.enabled() {
		state = "{r}ON{/} (trust set frozen)"
	}
	total := 0
	for _, byMask := range m.residents {
		total += len(byMask)
	}
	plural := "s"
	if total == 1 {
		plural = ""
	}
	return fmt.Sprintf("Guard: %s. %d resident%s known, %d stranger joins seen while on (since load).",
		state, total, plural, m.strangers)
}

// age drops residents not seen within guard_resident_days.
func (m *Module) age() {
	cutoff := m.Now().Add(-time.Duration(m.ctx.Conf.Int("guard_resident_days")) * 24 * time.Hour).Unix()
	for server, byMask := range m.residents {
		for mask, seen := range byMask {
			if seen < cutoff {
				delete(byMask, mask)
				m.dirty = true
			}
		}
		if len(byMask) == 0 {
			delete(m.residents, server)
		}
	}
}

func (m *Module) scheduleFlush() {
	m.flushTag = m.ctx.Sched.After(flushEvery, func() {
		m.flush()
		m.scheduleFlush()
	})
	m.flushSet = true
}

func (m *Module) flush() {
	if !m.dirty {
		return
	}
	if err := m.ctx.Store.Put(m.Name(), "residents", m.residents); err != nil {
		slog.Error("guard: save residents", "err", err)
		return
	}
	m.dirty = false
}
