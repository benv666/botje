// Package karma is the karma module: !item++ / !item-- (optionally
// with "# reason"), !item? queries, !wku/!wkd reason lists, and the
// automatic -1 for kicking the bot. Ported from IRC_Karma.pm.
//
// Storage shape differs from the Perl (which mixed global entries into
// the server level of one hash): here servers and global live in
// separate maps under namespace "karma", key "karma". The migration
// tool maps the old Storable layout onto this.
package karma

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
)

const storeKey = "karma"

// Entry is one item's karma and reason tallies.
type Entry struct {
	Karma  int                       `json:"karma"`
	Last   int64                     `json:"last"`
	Reason map[string]map[string]int `json:"reason,omitempty"` // "up"/"down" -> reason -> count
}

type Data struct {
	// Servers: server -> channel -> item -> entry
	Servers map[string]map[string]map[string]*Entry `json:"servers"`
	// Global: item -> entry (the Perl __GLOBAL_IRC_Karma__)
	Global map[string]*Entry `json:"global"`
}

var (
	// "!<item>++ / -- / ?" with an optional trailing smiley, and the
	// "# reason" form; both verbatim from the Perl
	plainRe  = regexp.MustCompile(`^!(.+)([+]{2}|[-]{2}|\?)\s*(?:\(-?:|\(-?;|;-?\)|:-?\))?\s*$`)
	reasonRe = regexp.MustCompile(`^!(.+)([+]{2}|[-]{2})\s*#\s*(.*?)\s*$`)
	wordRe   = regexp.MustCompile(`^!(\w+)`)
)

// Module implements module.Module.
type Module struct {
	ctx   *module.Context
	karma Data
}

// New returns an unloaded karma module.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "karma" }

func (m *Module) Load(ctx *module.Context) error {
	m.ctx = ctx
	m.karma = Data{}
	if _, err := ctx.Store.Get(m.Name(), storeKey, &m.karma); err != nil {
		return fmt.Errorf("karma: load: %w", err)
	}
	if m.karma.Servers == nil {
		m.karma.Servers = make(map[string]map[string]map[string]*Entry)
	}
	if m.karma.Global == nil {
		m.karma.Global = make(map[string]*Entry)
	}

	ctx.Cmd.Register(m.Name(), "wku", m.whyKarma)
	ctx.Cmd.Register(m.Name(), "wkd", m.whyKarma)
	if err := ctx.Bus.RegisterHook(m.Name(), "IRC_PRIVMSG", m.onPrivmsg); err != nil {
		return err
	}
	return ctx.Bus.RegisterHook(m.Name(), "IRC_KICK", m.onKick)
}

func (m *Module) Unload() error {
	m.ctx.Bus.UnregisterModule(m.Name())
	m.ctx.Cmd.UnregisterModule(m.Name())
	return m.save()
}

func (m *Module) save() error {
	return m.ctx.Store.Put(m.Name(), storeKey, &m.karma)
}

func (m *Module) onKick(ev *bus.Event) (bus.Handled, any) {
	if !ev.TargetMe {
		return bus.None, nil
	}
	channel := ev.Channel
	m.adjust(ev.Sender.Nick, -1, ev.Server, channel, "For kicking defenseless bots")
	return bus.None, nil
}

func (m *Module) onPrivmsg(ev *bus.Event) (bus.Handled, any) {
	if ev.SenderMe || ev.Msg == "" {
		return bus.None, nil
	}
	// leave registered commands alone (and their typo'd passwords)
	if w := wordRe.FindStringSubmatch(ev.Msg); w != nil && m.ctx.Cmd.Has(w[1]) {
		return bus.None, nil
	}

	var item, op, reason string
	if g := plainRe.FindStringSubmatch(ev.Msg); g != nil {
		item, op = g[1], g[2]
	} else if g := reasonRe.FindStringSubmatch(ev.Msg); g != nil {
		item, op, reason = g[1], g[2], g[3]
	} else {
		return bus.None, nil
	}

	var timeWord string
	var ck, gk int
	if op == "?" {
		timeWord = "currently"
		ck, gk = m.get(item, ev.Server, ev.Channel)
	} else {
		timeWord = "now"
		amount := 1
		if op == "--" {
			amount = -1
		}
		ck, gk = m.adjust(item, amount, ev.Server, ev.Channel, reason)
	}

	m.ctx.Privmsg(ev.Channel, fmt.Sprintf(
		"Karma for {m}%s{/} in this channel is %s {m}%d{/} (global karma is {m}%d{/}).",
		item, timeWord, ck, gk))
	return bus.Replied, nil
}

// whyKarma is !wku/!wkd: list the recorded reasons, most-counted first.
func (m *Module) whyKarma(d *cmd.Data) bool {
	if d.Event.SenderMe {
		return false
	}
	item := strings.TrimSpace(strings.ToLower(d.Data))

	ud, incdec, sign := "down", "decrease", "{R}-"
	if d.Command == "wku" {
		ud, incdec, sign = "up", "increase", "{G}+"
	}

	var reasons map[string]int
	if ch := m.karma.Servers[d.Event.Server][d.Event.Channel]; ch != nil && ch[item] != nil {
		reasons = ch[item].Reason[ud]
	}
	if len(reasons) == 0 {
		m.ctx.Pager.EventMsg(d.Event, d.Command,
			fmt.Sprintf("No reasons were found for the karma %s of {m}%s{/}.", incdec, item))
		return true
	}

	names := make([]string, 0, len(reasons))
	for r := range reasons {
		names = append(names, r)
	}
	sort.Slice(names, func(i, j int) bool {
		if reasons[names[i]] != reasons[names[j]] {
			return reasons[names[i]] > reasons[names[j]]
		}
		return names[i] < names[j]
	})
	lines := make([]string, 0, len(names)+1)
	lines = append(lines,
		fmt.Sprintf("The following reasons were found for the karma %s of {m}%s{/}:", incdec, item))
	for _, r := range names {
		lines = append(lines, fmt.Sprintf("%s: %s%d{/}", r, sign, reasons[r]))
	}
	m.ctx.Pager.EventMsg(d.Event, d.Command, lines...)
	return true
}

func (m *Module) entryFor(server, channel, item string) *Entry {
	if m.karma.Servers[server] == nil {
		m.karma.Servers[server] = make(map[string]map[string]*Entry)
	}
	if m.karma.Servers[server][channel] == nil {
		m.karma.Servers[server][channel] = make(map[string]*Entry)
	}
	e := m.karma.Servers[server][channel][item]
	if e == nil {
		e = &Entry{Last: time.Now().Unix()}
		m.karma.Servers[server][channel][item] = e
	}
	return e
}

func (m *Module) globalFor(item string) *Entry {
	e := m.karma.Global[item]
	if e == nil {
		e = &Entry{Last: time.Now().Unix()}
		m.karma.Global[item] = e
	}
	return e
}

// adjust bumps item karma on (server, channel) and globally, recording
// the reason (3+ chars, like the Perl) and saving immediately.
func (m *Module) adjust(item string, amount int, server, channel, reason string) (int, int) {
	item = strings.ToLower(item)
	e := m.entryFor(server, channel, item)
	g := m.globalFor(item)
	e.Karma += amount
	g.Karma += amount

	if len(reason) >= 3 {
		ud := "down"
		if amount > 0 {
			ud = "up"
		}
		for _, target := range []*Entry{e, g} {
			if target.Reason == nil {
				target.Reason = make(map[string]map[string]int)
			}
			if target.Reason[ud] == nil {
				target.Reason[ud] = make(map[string]int)
			}
			target.Reason[ud][reason]++
		}
	}
	if amount != 0 {
		m.save()
	}
	return e.Karma, g.Karma
}

func (m *Module) get(item, server, channel string) (int, int) {
	item = strings.ToLower(item)
	ck, gk := 0, 0
	if ch := m.karma.Servers[server][channel]; ch != nil && ch[item] != nil {
		ck = ch[item].Karma
	}
	if g := m.karma.Global[item]; g != nil {
		gk = g.Karma
	}
	return ck, gk
}
