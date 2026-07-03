// Package lastseen tracks the last activity per nick per channel and
// answers !last <nick>. Ported from IRC_Lastseen.pm. Notes vs the
// Perl: its join/part/quit handlers existed but were never hooked, so
// only messages count here too; the fuzzy time-ago wording is a Go
// approximation of DateTime::Duration::Fuzzy; activity is saved every
// 100 messages and on unload (the Perl only saved on unload).
package lastseen

import (
	"fmt"
	"strings"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
)

const (
	storeKey     = "activity"
	saveInterval = 100
)

type act struct {
	Msg  string `json:"msg"`
	Time int64  `json:"time"` // unix seconds
}

// Module implements module.Module.
type Module struct {
	// Now is injectable time; nil means time.Now.
	Now func() time.Time

	ctx      *module.Context
	activity map[string]map[string]map[string]*act // server -> nick -> channel -> act
	msgCount int
}

// New returns an unloaded lastseen module.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "lastseen" }

func (m *Module) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Module) Load(ctx *module.Context) error {
	m.ctx = ctx
	m.activity = make(map[string]map[string]map[string]*act)
	if _, err := ctx.Store.Get(m.Name(), storeKey, &m.activity); err != nil {
		return fmt.Errorf("lastseen: load: %w", err)
	}
	ctx.Cmd.Register(m.Name(), "last", m.cbLast)
	return ctx.Bus.RegisterHook(m.Name(), "IRC_PRIVMSG", m.onPrivmsg)
}

func (m *Module) Unload() error {
	m.ctx.Bus.UnregisterModule(m.Name())
	m.ctx.Cmd.UnregisterModule(m.Name())
	return m.save()
}

func (m *Module) save() error {
	return m.ctx.Store.Put(m.Name(), storeKey, &m.activity)
}

func (m *Module) onPrivmsg(ev *bus.Event) (bus.Handled, any) {
	if ev.SenderMe || ev.Msg == "" || strings.HasPrefix(ev.Msg, "!") {
		return bus.None, nil
	}
	nick := strings.ToLower(ev.Sender.Nick)
	if m.activity[ev.Server] == nil {
		m.activity[ev.Server] = make(map[string]map[string]*act)
	}
	if m.activity[ev.Server][nick] == nil {
		m.activity[ev.Server][nick] = make(map[string]*act)
	}
	m.activity[ev.Server][nick][ev.Channel] = &act{Msg: ev.Msg, Time: m.now().Unix()}

	m.msgCount++
	if m.msgCount >= saveInterval {
		m.msgCount = 0
		m.save()
	}
	return bus.None, nil
}

func (m *Module) cbLast(d *cmd.Data) bool {
	if d.Event.SenderMe {
		return false
	}
	target := strings.TrimSpace(d.Data)
	if target == "" {
		m.ctx.Privmsg(d.Event.Channel, "!last <nick> Shows last known activity for <nick> on this channel")
		return true
	}
	m.ctx.Privmsg(d.Event.Channel, m.lastAct(target, d.Event.Server, d.Event.Channel))
	return true
}

// lastAct formats the last-seen line, the Perl getLastAct (including
// its quirk of pairing the other channel's clock time with this
// channel's message in the "Last here" case).
func (m *Module) lastAct(target, server, channel string) string {
	nick := strings.ToLower(target)
	byChan := m.activity[server][nick]
	notFound := target + " destroyed (never seen that nick anywhere)."
	if len(byChan) == 0 {
		return notFound
	}

	lastChan := ""
	var lastAct *act
	for ch, a := range byChan {
		if lastAct == nil || a.Time > lastAct.Time ||
			(a.Time == lastAct.Time && ch < lastChan) {
			lastChan, lastAct = ch, a
		}
	}
	when := timeAgo(m.now().Sub(time.Unix(lastAct.Time, 0)))
	hms := time.Unix(lastAct.Time, 0).Format("15:04:05")

	switch {
	case lastChan == channel:
		return fmt.Sprintf("%s was last seen %s on this channel: %s <%s> %s",
			target, when, hms, target, lastAct.Msg)
	case byChan[channel] == nil:
		return fmt.Sprintf("%s was last seen in %s %s. Never seen in this channel!",
			target, lastChan, when)
	default:
		local := byChan[channel]
		localWhen := timeAgo(m.now().Sub(time.Unix(local.Time, 0)))
		return fmt.Sprintf("%s was last seen in %s %s. Last here %s: %s <%s> %s",
			target, lastChan, when, localWhen, hms, target, local.Msg)
	}
}

// timeAgo approximates DateTime::Duration::Fuzzy.
func timeAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < 2*time.Minute:
		return "a minute ago"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 2*time.Hour:
		return "about an hour ago"
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "yesterday"
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	case d < 14*24*time.Hour:
		return "last week"
	case d < 31*24*time.Hour:
		return fmt.Sprintf("%d weeks ago", int(d.Hours()/(24*7)))
	case d < 61*24*time.Hour:
		return "last month"
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%d months ago", int(d.Hours()/(24*30)))
	case d < 2*365*24*time.Hour:
		return "last year"
	default:
		return fmt.Sprintf("%d years ago", int(d.Hours()/(24*365)))
	}
}
