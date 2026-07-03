// Package pizza is the timer module: !pizza / !timer alarms with the
// full Perl timespec (absolute h:m[:s] and d-m-yyyy, relative
// +/-N[smhdwy|mo], weekday names, repeats r{...} with a 1800s minimum),
// greek-letter ids, a stopwatch, and restore-on-reload with near-past
// timers delayed 20s. Ported from Pizza.pm. Divergences: a running
// stopwatch survives reload as a stopwatch (the Perl restored it as a
// broken timer), relative offsets apply largest-unit-first (the Perl
// used hash order), and timers are saved after firing too.
package pizza

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"time"

	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
	"go-botje/internal/sched"
)

const storeKey = "timers"

// timer is one scheduled alarm (persisted).
type timer struct {
	ID      string       `json:"id"`
	Nick    string       `json:"nick"` // original capitalization
	When    int64        `json:"when"`
	Channel string       `json:"channel"`
	Server  string       `json:"server"`
	Message string       `json:"message"`
	Repeat  []repeatPart `json:"repeat,omitempty"`

	tag sched.Tag
}

// persisted is the storage shape: timers per lowercased nick, plus the
// stopwatch starts (the Perl hid those inside the timer map as
// __count).
type persisted struct {
	Timers    map[string]map[string]*timer `json:"timers"`
	Stopwatch map[string]int64             `json:"stopwatch,omitempty"`
}

var (
	whenRe  = regexp.MustCompile(`^(?i)\s*when\s+([\w\-]+)$`)
	startRe = regexp.MustCompile(`^(?i)\s*start\s*$`)
	stopRe  = regexp.MustCompile(`^(?i)\s*stop\s*$`)
	clearRe = regexp.MustCompile(`^(?i)clear(?:\s+(.+?))?$`)

	// list colors cycle, raw ANSI like the Perl (the send pipeline
	// translates them to mIRC codes)
	listColors = []string{"\x1b[35;1m", "\x1b[31;1m", "\x1b[32;1m", "\x1b[33;1m", "\x1b[34;1m"}
)

var helpLines = []string{
	"!(timer|pizza) [+?/-n[y,mo,w,d,h,m,s]]+ (d+-m+-yyyy)|[monday-sunday]|, h+:m+<:s+> r{n[y,mo,w,d,h,m,s]+} [id]: Sets timer.",
	"!(timer|pizza) clear [id]: Clears all or id.",
	"!(timer|pizza) [when id] : Show time remaining.",
	"!(timer|pizza) [start]|[stop] : Start/stop stopwatch.",
}

// Module implements module.Module.
type Module struct {
	// Now and Rand are injectable for tests; nil means the real ones.
	Now  func() time.Time
	Rand func() float64

	ctx        *module.Context
	timers     map[string]map[string]*timer // lc nick -> id -> timer
	stopwatch  map[string]int64             // lc nick -> start epoch
	clearCheck map[string]time.Time         // lc nick -> first "clear" time
	ids        *idPool
}

// New returns an unloaded pizza module.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "pizza" }

func (m *Module) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Module) Load(ctx *module.Context) error {
	m.ctx = ctx
	rnd := m.Rand
	if rnd == nil {
		rnd = rand.Float64
	}
	m.ids = newIDPool(rnd)
	m.timers = make(map[string]map[string]*timer)
	m.stopwatch = make(map[string]int64)
	m.clearCheck = make(map[string]time.Time)

	var stored persisted
	if _, err := ctx.Store.Get(m.Name(), storeKey, &stored); err != nil {
		return fmt.Errorf("pizza: load: %w", err)
	}
	if stored.Stopwatch != nil {
		m.stopwatch = stored.Stopwatch
	}
	for _, byID := range stored.Timers {
		for id, t := range byID {
			m.ids.mark(id)
			when := time.Unix(t.When, 0)
			if when.Sub(m.now()) < 30*time.Second {
				// probably enough time to connect to irc + channel
				when = m.now().Add(20 * time.Second)
			}
			m.schedule(t.Nick, t.Channel, t.Server, when, id, t.Message, t.Repeat)
		}
	}

	ctx.Cmd.Register(m.Name(), "pizza", m.cbPizza)
	ctx.Cmd.Register(m.Name(), "timer", m.cbPizza)
	return nil
}

func (m *Module) Unload() error {
	for _, byID := range m.timers {
		for _, t := range byID {
			m.ctx.Sched.Unschedule(t.tag)
		}
	}
	m.ctx.Cmd.UnregisterModule(m.Name())
	return m.save()
}

func (m *Module) save() error {
	return m.ctx.Store.Put(m.Name(), storeKey, &persisted{
		Timers: m.timers, Stopwatch: m.stopwatch,
	})
}

func (m *Module) cbPizza(d *cmd.Data) bool {
	if d.Event.SenderMe {
		return false
	}
	nick := d.Event.Sender.Nick
	channel := d.Event.Channel
	server := d.Event.Server
	data := strings.TrimSpace(d.Data)

	switch {
	case data == "":
		m.list(nick, channel)
	case whenRe.MatchString(data):
		m.when(nick, channel, whenRe.FindStringSubmatch(data)[1])
	case startRe.MatchString(data):
		m.start(nick, channel)
	case stopRe.MatchString(data):
		m.stop(nick, channel)
	case strings.EqualFold(data, "help"):
		for _, l := range helpLines {
			m.ctx.Privmsg(channel, l)
		}
	case clearRe.MatchString(data):
		m.clear(nick, channel, clearRe.FindStringSubmatch(data)[1])
	default:
		m.set(nick, channel, server, data)
	}
	return true
}

func (m *Module) set(nick, channel, server, spec string) {
	ti, errMsg := parseTime(spec, m.now(), m.ids)
	if ti == nil {
		m.ctx.Privmsg(channel, errMsg)
		return
	}
	if !ti.abs.After(m.now()) {
		m.ctx.Privmsg(channel, "YEah that's not anytime soon, it's in the past.")
		return
	}
	m.schedule(nick, channel, server, ti.abs, ti.id, ti.message, ti.repeat)
	m.ctx.Privmsg(channel, timerString(nick, ti))
	m.save()
}

// schedule arms one timer and tracks it (the Perl scheduleTimer).
func (m *Module) schedule(nick, channel, server string, at time.Time, id, message string, repeat []repeatPart) {
	t := &timer{
		ID: id, Nick: nick, When: at.Unix(), Channel: channel,
		Server: server, Message: message, Repeat: repeat,
	}
	if !at.After(m.now()) {
		m.fire(t)
		return
	}
	t.tag = m.ctx.Sched.Schedule(at, func() { m.fire(t) })
	ln := strings.ToLower(nick)
	if m.timers[ln] == nil {
		m.timers[ln] = make(map[string]*timer)
	}
	m.timers[ln][id] = t
}

// fire announces a due timer and reschedules repeats (the Perl cbTimer).
func (m *Module) fire(t *timer) {
	m.ctx.Privmsg(t.Channel, t.Nick+": "+t.Message)

	if len(t.Repeat) > 0 {
		next := nextRepeat(time.Unix(t.When, 0), t.Repeat)
		if diff := next.Sub(m.now()); diff < 1800*time.Second {
			m.ctx.Privmsg(t.Channel,
				fmt.Sprintf("%s: Rescheduling of %s denied, period too short, FU.", t.Nick, t.ID))
		} else {
			m.schedule(t.Nick, t.Channel, t.Server, next, t.ID, t.Message, t.Repeat)
			m.ctx.Privmsg(t.Channel, timerString(t.Nick, &timeInfo{
				abs: next, diff: diff, id: t.ID, message: t.Message, repeat: t.Repeat,
			}))
			m.save()
			return
		}
	}
	m.ids.clear(t.ID)
	delete(m.timers[strings.ToLower(t.Nick)], t.ID)
	m.save()
}

func (m *Module) list(nick, channel string) {
	byID := m.timers[strings.ToLower(nick)]
	if len(byID) == 0 {
		m.ctx.Privmsg(channel, nick+": You have no timers set.")
		return
	}
	type line struct {
		t    int64
		text string
	}
	var lines []line
	nowT := m.now()
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		t := byID[id]
		secs := t.When - nowT.Unix()
		var tLeft string
		switch {
		case secs >= 3600:
			tLeft = fmt.Sprintf("%.1fh", float64(secs)/3600)
		case secs >= 60:
			tLeft = fmt.Sprintf("%dm", secs/60)
		default:
			tLeft = fmt.Sprintf("%ds", secs)
		}
		days := ""
		if d := secs / 86400; d > 0 {
			days = fmt.Sprintf("+%dd ", d)
		}
		at := time.Unix(t.When, 0).Format("15:04:05")
		lines = append(lines, line{secs,
			fmt.Sprintf("%s: %sat %s (in %s) %s ", id, days, at, tLeft, t.Message)})
	}
	slices.SortStableFunc(lines, func(a, b line) int {
		if a.t != b.t {
			return int(a.t - b.t)
		}
		return strings.Compare(a.text, b.text)
	})
	var b strings.Builder
	colors := slices.Clone(listColors)
	for _, l := range lines {
		b.WriteString(colors[0])
		b.WriteString(l.text)
		colors = append(colors[1:], colors[0]) // cycle
	}
	m.ctx.Privmsg(channel, nick+": Time remaining: "+b.String()+"\x1b[0m")
}

func (m *Module) when(nick, channel, id string) {
	t := m.timers[strings.ToLower(nick)][id]
	if t == nil {
		m.ctx.Privmsg(channel, fmt.Sprintf("%s: You have no timer set with id %s", nick, id))
		return
	}
	at := time.Unix(t.When, 0)
	m.ctx.Privmsg(channel, fmt.Sprintf("%s: {M}%s: %s %s at %s{/}",
		nick, id, at.Weekday(), at.Format("02-01-2006"), at.Format("15:04:05")))
}

func (m *Module) start(nick, channel string) {
	ln := strings.ToLower(nick)
	if _, ok := m.stopwatch[ln]; ok {
		m.ctx.Privmsg(channel, ln+": Already started one, use stop to stop.")
		return
	}
	m.stopwatch[ln] = m.now().Unix()
	m.save()
	m.ctx.Privmsg(channel, ln+": Time stored, use stop to get time elapsed.")
}

func (m *Module) stop(nick, channel string) {
	ln := strings.ToLower(nick)
	started, ok := m.stopwatch[ln]
	if !ok {
		m.ctx.Privmsg(channel, ln+": Use start to start a timer first.")
		return
	}
	delete(m.stopwatch, ln)
	m.save()
	diff := m.now().Unix() - started
	var parts []string
	if d := diff / 86400; d > 0 {
		parts = append(parts, fmt.Sprintf("%d days", d))
	}
	if h := diff / 3600 % 24; h > 0 {
		parts = append(parts, fmt.Sprintf("%d hours", h))
	}
	if mm := diff / 60 % 60; mm > 0 {
		parts = append(parts, fmt.Sprintf("%d minutes", mm))
	}
	parts = append(parts, fmt.Sprintf("%d seconds", diff%60))
	m.ctx.Privmsg(channel, ln+": Time elapsed: "+strings.Join(parts, ", ")+".")
}

func (m *Module) clear(nick, channel, id string) {
	ln := strings.ToLower(nick)
	byID := m.timers[ln]
	if id != "" {
		t := byID[id]
		if t == nil {
			m.ctx.Privmsg(channel, fmt.Sprintf("%s: No %s for you..", nick, id))
			return
		}
		m.ctx.Sched.Unschedule(t.tag)
		m.ids.clear(id)
		delete(byID, id)
		m.ctx.Privmsg(channel, nick+": Removed timer: "+id)
		m.save()
		return
	}
	// clear-all wants a confirmation within the minute
	if first, ok := m.clearCheck[ln]; ok && m.now().Sub(first) < 60*time.Second {
		delete(m.clearCheck, ln)
		ids := make([]string, 0, len(byID))
		for tid := range byID {
			ids = append(ids, tid)
		}
		slices.Sort(ids)
		for _, tid := range ids {
			m.ctx.Sched.Unschedule(byID[tid].tag)
			m.ids.clear(tid)
			delete(byID, tid)
		}
		if len(ids) == 0 {
			m.ctx.Privmsg(channel, nick+": you had no timers, idiot")
		} else {
			m.ctx.Privmsg(channel, nick+": Removed timers: "+strings.Join(ids, ", "))
			m.save()
		}
		return
	}
	m.clearCheck[ln] = m.now()
	m.ctx.Privmsg(channel, nick+": Say again?")
}

// timerString is the alarm-set confirmation (the Perl getTimerString).
func timerString(nick string, ti *timeInfo) string {
	secs := int64(ti.diff / time.Second)
	var left string
	if secs/3600 > 24 {
		left = fmt.Sprintf("in \x1b[36m%d\x1b[0m days", secs/86400)
	} else {
		left = fmt.Sprintf("+%dh %dm %ds", secs/3600, secs%3600/60, secs%60)
	}
	repeat := ""
	if len(ti.repeat) > 0 {
		var parts []string
		for _, p := range ti.repeat {
			parts = append(parts, fmt.Sprintf("+%d%s", p.N, p.Unit))
		}
		repeat = ", repeat{m}{" + strings.Join(parts, " ") + "}{/}"
	}
	return fmt.Sprintf("%s: Alarm set for {g}%s{/} (%s)%s, {r}%s{/}: {M}%s{/}",
		nick, ti.abs.Format("Mon Jan _2 15:04:05 2006"), left, repeat, ti.id, ti.message)
}
