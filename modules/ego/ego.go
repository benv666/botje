// Package ego counts self-references (I/me/mine/my, NL: ik/mijn/mij)
// per nick, per channel and globally. !ego <nick> reports; every 200
// hits it reports on its own. Ported from IRC_Ego.pm; bare !ego stays
// silent like the Perl.
package ego

import (
	"fmt"
	"regexp"
	"strings"

	"go-botje/internal/bus"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
)

const (
	storeKey     = "ego"
	saveInterval = 100 // save every 100 msgs
	reportRate   = 200 // auto-report once every x hits
)

var egoWordRe = regexp.MustCompile(`\b(?:i|me|mine|my|ik|mijn|mij)\b`)

type chanStats struct {
	EgoCount  int `json:"egoCount"`
	Sentences int `json:"sentences"`
}

type nickStats struct {
	EgoCount  int                   `json:"egoCount"`
	Sentences int                   `json:"sentences"`
	Channels  map[string]*chanStats `json:"channels"`
}

// Module implements module.Module.
type Module struct {
	ctx      *module.Context
	ego      map[string]map[string]*nickStats // server -> nick -> stats
	msgCount int
}

// New returns an unloaded ego module.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "ego" }

func (m *Module) Load(ctx *module.Context) error {
	m.ctx = ctx
	m.ego = make(map[string]map[string]*nickStats)
	if _, err := ctx.Store.Get(m.Name(), storeKey, &m.ego); err != nil {
		return fmt.Errorf("ego: load: %w", err)
	}
	ctx.Cmd.Register(m.Name(), "ego", m.cbEgo)
	return ctx.Bus.RegisterHook(m.Name(), "IRC_PRIVMSG", m.onPrivmsg)
}

func (m *Module) Unload() error {
	m.ctx.Bus.UnregisterModule(m.Name())
	m.ctx.Cmd.UnregisterModule(m.Name())
	return m.save()
}

func (m *Module) save() error {
	return m.ctx.Store.Put(m.Name(), storeKey, &m.ego)
}

func (m *Module) onPrivmsg(ev *bus.Event) (bus.Handled, any) {
	if ev.SenderMe || ev.Msg == "" {
		return bus.None, nil
	}
	count := len(egoWordRe.FindAllString(strings.ToLower(ev.Msg), -1))
	report := m.adjust(ev.Server, ev.Channel, ev.Sender.Nick, count)

	m.msgCount++
	if m.msgCount >= saveInterval {
		m.msgCount = 0
		m.save()
	}
	if report {
		m.report(ev.Server, ev.Channel, ev.Sender.Nick)
	}
	return bus.Replied, nil
}

// adjust adds count hits (and one sentence) for nick; reports true when
// the channel counter crosses the report boundary (perl adjustEgo).
func (m *Module) adjust(server, channel, nick string, count int) bool {
	nick = strings.ToLower(nick)
	if m.ego[server] == nil {
		m.ego[server] = make(map[string]*nickStats)
	}
	ns := m.ego[server][nick]
	if ns == nil {
		ns = &nickStats{Channels: make(map[string]*chanStats)}
		m.ego[server][nick] = ns
	}
	if ns.Channels == nil {
		ns.Channels = make(map[string]*chanStats)
	}
	cs := ns.Channels[channel]
	if cs == nil {
		cs = &chanStats{}
		ns.Channels[channel] = cs
	}

	report := false
	if count > 0 {
		if (cs.EgoCount%reportRate)+count >= reportRate {
			report = true
		}
		ns.EgoCount += count
		cs.EgoCount += count
	}
	ns.Sentences++
	cs.Sentences++
	return report
}

func (m *Module) cbEgo(d *cmd.Data) bool {
	if d.Event.SenderMe {
		return false
	}
	target := strings.TrimSpace(d.Data)
	if target == "" {
		return false // perl parity: bare !ego says nothing
	}
	m.report(d.Event.Server, d.Event.Channel, target)
	return true
}

func (m *Module) report(server, channel, nick string) {
	nick = strings.ToLower(nick)
	ns := m.ego[server][nick]
	if ns == nil {
		m.ctx.Privmsg(channel, fmt.Sprintf("No data found for [{m}%s{/}]", nick))
		return
	}
	var cEgo, cSent int
	if cs := ns.Channels[channel]; cs != nil {
		cEgo, cSent = cs.EgoCount, cs.Sentences
	}
	m.ctx.Privmsg(channel, fmt.Sprintf(
		"Channel ego for {m}%s{/}: %s. Global ego: %s",
		nick, statsMsg(cEgo, cSent), statsMsg(ns.EgoCount, ns.Sentences)))
}

func statsMsg(ego, sentences int) string {
	ratio := ""
	if sentences != 0 {
		ratio = fmt.Sprintf(", ratio:{g}%.1f{/}%%", 100*float64(ego)/float64(sentences))
	}
	return fmt.Sprintf("{r}%d{/}x in {r}%d{/} messages%s", ego, sentences, ratio)
}
