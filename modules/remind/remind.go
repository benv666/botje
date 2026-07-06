// Package remind is cron reminders plus the remember/recall/forget
// notepad, ported from Remind.pm (promoted from inactive by decision).
// Reminders take standard five-field cron lines with the Perl's
// day/month name shorthands; the notepad keeps the last three values
// per (channel, nick, title). Raw ANSI colors ported verbatim; the
// send pipeline translates them. Cron math by robfig/cron instead of
// DateTime::Event::Cron; su/sun map to 0 instead of the Perl's 7
// (same day, robfig rejects 7).
package remind

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"go-botje/internal/bus"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
	"go-botje/internal/sched"
)

const maxRemembered = 3

// reminder is one cron entry (persisted).
type reminder struct {
	ID       int    `json:"id"`
	Nick     string `json:"nick"`
	Channel  string `json:"channel"`
	Server   string `json:"server"`
	Message  string `json:"message"`
	CronLine string `json:"cronLine"`
	TimeDef  string `json:"timeDef"`

	schedule cron.Schedule
	timer    sched.Tag
	timerSet bool
}

// note is one remembered title with its value stack (persisted).
type note struct {
	Title string   `json:"title"`
	Data  []string `json:"data"`
}

var (
	cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

	showRe     = regexp.MustCompile(`^\s*show\s*$`)
	clearRe    = regexp.MustCompile(`^\s*clear\s+((\d+)(\s+\d+)*)$`)
	cronRe     = regexp.MustCompile(`^\s*(\S+)\s+(\S+)\s+(\S+)\s+(\S+)\s+(\S+)\s+(.+?)\s*$`)
	recallRe   = regexp.MustCompile(`^\s*(.*?)(?:\s+(\d+))?$`)
	rememberRe = regexp.MustCompile(`^\s*(\S+)\s+(.*)$`)

	dayNameRe   = regexp.MustCompile(`(mon|tue|wed|thu|fri|sat|sun|mo|tu|we|th|fr|sa|su)`)
	monthNameRe = regexp.MustCompile(`(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)`)
)

var dayNums = map[string]string{
	"mo": "1", "mon": "1", "tu": "2", "tue": "2", "we": "3", "wed": "3",
	"th": "4", "thu": "4", "fr": "5", "fri": "5", "sa": "6", "sat": "6",
	"su": "0", "sun": "0",
}

var monthNums = map[string]string{
	"jan": "1", "feb": "2", "mar": "3", "apr": "4", "may": "5", "jun": "6",
	"jul": "7", "aug": "8", "sep": "9", "oct": "10", "nov": "11", "dec": "12",
}

var helpLines = []string{
	"!\x1b[33mremind\x1b[32m (*|min)[/step] hour day mon dow \x1b[35mmsg\x1b[0m (eg: !remind 0/5 12-23 10 jan-mar mo REMIND ME).",
	"!\x1b[33mremind\x1b[34m show\x1b[0m: Shows active reminders with matching ids.",
	"!\x1b[33mremind\x1b[34m clear\x1b[32m id1 id2 id3 id4\x1b[0m: Clears reminders specified.",
}

// Module implements module.Module.
type Module struct {
	// Now is injectable time; nil means time.Now.
	Now func() time.Time

	ctx       *module.Context
	reminders map[string]map[int]*reminder // nick -> id
	// notes: server -> channel -> nick -> lc(title), the perl shape
	notes map[string]map[string]map[string]map[string]*note
	maxID int
}

// New returns an unloaded remind module.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "remind" }

func (m *Module) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Module) Load(ctx *module.Context) error {
	m.ctx = ctx
	m.reminders = make(map[string]map[int]*reminder)
	m.notes = make(map[string]map[string]map[string]map[string]*note)

	if _, err := ctx.Store.Get(m.Name(), "reminders", &m.reminders); err != nil {
		return fmt.Errorf("remind: load reminders: %w", err)
	}
	if _, err := ctx.Store.Get(m.Name(), "remembers", &m.notes); err != nil {
		return fmt.Errorf("remind: load remembers: %w", err)
	}
	m.maxID = 0
	for _, byID := range m.reminders {
		for id, r := range byID {
			m.maxID = max(m.maxID, id)
			if !m.setTimer(r) {
				delete(byID, id)
			}
		}
	}
	m.maxID++

	ctx.Cmd.Register(m.Name(), "remind", m.cbRemind)
	ctx.Cmd.Register(m.Name(), "forget", m.cbForget)
	ctx.Cmd.Register(m.Name(), "remember", m.cbRemember)
	ctx.Cmd.Register(m.Name(), "recall", m.cbRecall)
	return nil
}

func (m *Module) Unload() error {
	for _, byID := range m.reminders {
		for _, r := range byID {
			if r.timerSet {
				m.ctx.Sched.Unschedule(r.timer)
			}
		}
	}
	m.ctx.Cmd.UnregisterModule(m.Name())
	m.ctx.Bus.UnregisterModule(m.Name())
	return m.save()
}

func (m *Module) save() error {
	if err := m.ctx.Store.Put(m.Name(), "reminders", m.reminders); err != nil {
		return err
	}
	return m.ctx.Store.Put(m.Name(), "remembers", m.notes)
}

// --- remember / recall / forget

func (m *Module) noteFor(ev *bus.Event, title string, create bool) *note {
	server, channel, nick := ev.Server, ev.Channel, ev.Sender.Nick
	if m.notes[server] == nil {
		if !create {
			return nil
		}
		m.notes[server] = make(map[string]map[string]map[string]*note)
	}
	if m.notes[server][channel] == nil {
		if !create {
			return nil
		}
		m.notes[server][channel] = make(map[string]map[string]*note)
	}
	if m.notes[server][channel][nick] == nil {
		if !create {
			return nil
		}
		m.notes[server][channel][nick] = make(map[string]*note)
	}
	n := m.notes[server][channel][nick][strings.ToLower(title)]
	if n == nil && create {
		n = &note{Title: title}
		m.notes[server][channel][nick][strings.ToLower(title)] = n
	}
	return n
}

func (m *Module) notesFor(ev *bus.Event) map[string]*note {
	return m.notes[ev.Server][ev.Channel][ev.Sender.Nick]
}

func (m *Module) cbRemember(d *cmd.Data) bool {
	if d.Event.SenderMe {
		return false
	}
	g := rememberRe.FindStringSubmatch(d.Data)
	if g == nil {
		return m.cbRecall(d) // handle retards :)
	}
	title, stuff := g[1], strings.TrimSpace(g[2])
	n := m.noteFor(d.Event, title, true)
	n.Data = append([]string{stuff}, n.Data...)
	removed := ""
	if len(n.Data) > maxRemembered {
		removed = " Removed: \x1b[33m" + n.Data[len(n.Data)-1]
		n.Data = n.Data[:maxRemembered]
	}
	m.ctx.Privmsg(d.Event.Channel, fmt.Sprintf(
		"Sure %s, \x1b[31m%s\x1b[0m: added \x1b[32m%s\x1b[0m.%s",
		d.Event.Sender.Nick, n.Title, stuff, removed))
	m.save()
	return true
}

func (m *Module) cbRecall(d *cmd.Data) bool {
	if d.Event.SenderMe {
		return false
	}
	g := recallRe.FindStringSubmatch(d.Data)
	title := g[1]
	index := 1
	if g[2] != "" {
		index, _ = strconv.Atoi(g[2])
	}
	if index < 1 {
		m.ctx.Privmsg(d.Event.Channel,
			"*cough* ~\x1b[32mFUCK YOU FUCK YOU FUCK YOU BURN IN HELL STUPID FUCKING WHORE TRALALAALALALAALAAAAAAA\x1b[0m \\o,")
		return true
	}
	n := m.noteFor(d.Event, title, false)
	if n == nil {
		m.showKeys(d.Event)
		return true
	}
	if index > len(n.Data) {
		m.ctx.Privmsg(d.Event.Channel, fmt.Sprintf(
			"*cough* \x1b[31m%s\x1b[0m: ~\x1b[32mFUCK YOU FUCK YOU FUCK YOU BURN IN HELL STUPID FUCKING WHORE TRALALAALALALAALAAAAAAA\x1b[0m \\o,",
			n.Title))
		return true
	}
	m.ctx.Privmsg(d.Event.Channel, fmt.Sprintf("\x1b[31m%s\x1b[0m: \x1b[32m%s\x1b[0m [%d/%d]",
		n.Title, n.Data[index-1], index, len(n.Data)))
	return true
}

func (m *Module) cbForget(d *cmd.Data) bool {
	if d.Event.SenderMe {
		return false
	}
	title := strings.TrimSpace(d.Data)
	n := m.noteFor(d.Event, title, false)
	if n == nil {
		m.showKeys(d.Event)
		return true
	}
	delete(m.notesFor(d.Event), strings.ToLower(title))
	m.save()
	m.ctx.Privmsg(d.Event.Channel, fmt.Sprintf(
		"\x1b[31m%s\x1b[0m: \x1b[33mForgot what it was...\x1b[0m", n.Title))
	return true
}

func (m *Module) showKeys(ev *bus.Event) {
	byTitle := m.notesFor(ev)
	if len(byTitle) == 0 {
		m.ctx.Privmsg(ev.Channel, "No AND you had nothing stored to begin with.")
		return
	}
	titles := make([]string, 0, len(byTitle))
	for _, n := range byTitle {
		titles = append(titles, fmt.Sprintf("%s\x1b[0m(%d)", n.Title, len(n.Data)))
	}
	slices.SortFunc(titles, func(a, b string) int {
		return strings.Compare(strings.ToLower(a), strings.ToLower(b))
	})
	m.ctx.Privmsg(ev.Channel, "No such thing, choose from: \x1b[31m"+strings.Join(titles, ",\x1b[31m "))
}

// --- cron reminders

func (m *Module) cbRemind(d *cmd.Data) bool {
	if d.Event.SenderMe {
		return false
	}
	ev := d.Event
	msg := d.Data
	switch {
	case strings.TrimSpace(msg) == "" || strings.EqualFold(strings.TrimSpace(msg), "help"):
		for _, l := range helpLines {
			m.ctx.Privmsg(ev.Channel, l)
		}
	case showRe.MatchString(msg):
		var lines []string
		for _, id := range m.reminderIDs(ev.Sender.Nick) {
			r := m.reminders[ev.Sender.Nick][id]
			if r.Server == ev.Server && r.Channel == ev.Channel {
				lines = append(lines, fmt.Sprintf("\x1b[31m%d\x1b[0m: \x1b[32m%s\x1b[0m ==> \x1b[33m%s\x1b[0m",
					id, r.TimeDef, r.Message))
			}
		}
		if len(lines) == 0 {
			m.ctx.Privmsg(ev.Channel, "No relevant reminds found.")
		} else {
			m.ctx.Privmsg(ev.Channel, strings.Join(lines, "\n"))
		}
	case clearRe.MatchString(msg):
		wanted := strings.Fields(clearRe.FindStringSubmatch(msg)[1])
		found := false
		for _, idStr := range wanted {
			id, _ := strconv.Atoi(idStr)
			r := m.reminders[ev.Sender.Nick][id]
			if r == nil || r.Server != ev.Server || r.Channel != ev.Channel {
				continue
			}
			if r.timerSet {
				m.ctx.Sched.Unschedule(r.timer)
			}
			delete(m.reminders[ev.Sender.Nick], id)
			m.ctx.Privmsg(ev.Channel, fmt.Sprintf("Deleted reminder \x1b[31m%d\x1b[0m.", id))
			found = true
		}
		if !found {
			m.ctx.Privmsg(ev.Channel, "No matching reminds found.")
		} else {
			m.save()
		}
	case cronRe.MatchString(msg):
		m.addReminder(ev, cronRe.FindStringSubmatch(msg))
	default:
		m.ctx.Privmsg(ev.Channel, "..What?")
	}
	return true
}

func (m *Module) reminderIDs(nick string) []int {
	ids := make([]int, 0, len(m.reminders[nick]))
	for id := range m.reminders[nick] {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (m *Module) addReminder(ev *bus.Event, g []string) {
	cronLine := strings.ToLower(strings.Join(g[1:6], " "))
	cronLine = dayNameRe.ReplaceAllStringFunc(cronLine, func(s string) string { return dayNums[s] })
	cronLine = monthNameRe.ReplaceAllStringFunc(cronLine, func(s string) string { return monthNums[s] })

	schedule, err := cronParser.Parse(cronLine)
	if err != nil {
		m.ctx.Privmsg(ev.Channel, "BOooo: "+err.Error())
		return
	}
	id := m.maxID
	m.maxID++
	r := &reminder{
		ID: id, Nick: ev.Sender.Nick, Channel: ev.Channel, Server: ev.Server,
		Message: g[6], CronLine: cronLine,
		TimeDef:  fmt.Sprintf("m:%s, h:%s, d:%s, m:%s, dow:%s", g[1], g[2], g[3], g[4], g[5]),
		schedule: schedule,
	}
	if m.reminders[r.Nick] == nil {
		m.reminders[r.Nick] = make(map[int]*reminder)
	}
	m.reminders[r.Nick][id] = r
	m.setTimer(r)
	m.ctx.Privmsg(ev.Channel, fmt.Sprintf("Reminder set, ID: \x1b[32m%d\x1b[0m.", id))
	m.save()
}

// setTimer arms the next cron occurrence.
func (m *Module) setTimer(r *reminder) bool {
	if r.schedule == nil {
		schedule, err := cronParser.Parse(r.CronLine)
		if err != nil {
			return false
		}
		r.schedule = schedule
	}
	next := r.schedule.Next(m.now())
	r.timer = m.ctx.Sched.Schedule(next, func() { m.fire(r) })
	r.timerSet = true
	return true
}

func (m *Module) fire(r *reminder) {
	m.ctx.Privmsg(r.Channel, fmt.Sprintf("%s: \x1b[31m%s\x1b[0m.", r.Nick, r.Message))
	if !m.setTimer(r) {
		m.ctx.Privmsg(r.Channel, "Unable to reschedule.. report bug ;)")
	}
}
