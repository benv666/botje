package guard

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go-botje/internal/bus"
)

// suspect is the in-memory tracking for one non-resident mask while the
// guard is enabled. Discarded when the guard turns off.
type suspect struct {
	nick     string
	host     string
	lines    []stamp         // recent messages (rate + cross-channel dup)
	joins    []stamp         // recent channel joins (mass-join)
	channels map[string]bool // channels we share with them, for kicking
	actioned bool            // enforced already; do not re-fire
}

// stamp is a text sighting at a time (channel for joins, message for lines).
type stamp struct {
	text string
	when time.Time
}

func (m *Module) enforceSettings() {
	m.ctx.Conf.CreateInt("guard_gline_seconds", 3600)
	m.ctx.Conf.CreateInt("guard_dup_channels", 2)  // same line to N channels
	m.ctx.Conf.CreateInt("guard_join_channels", 4) // joins to N channels...
	m.ctx.Conf.CreateInt("guard_join_window_sec", 20)
	m.ctx.Conf.CreateInt("guard_line_rate", 6) // N lines...
	m.ctx.Conf.CreateInt("guard_line_window_sec", 4)
	m.ctx.Conf.CreateString("guard_auth_password", "")
}

// suspectFor returns the tracked suspect for a mask, creating it. Only
// called while enabled and for non-residents.
func (m *Module) suspectFor(mask, nick, host string) *suspect {
	s := m.suspects[mask]
	if s == nil {
		s = &suspect{channels: make(map[string]bool)}
		m.suspects[mask] = s
	}
	s.nick, s.host = nick, host
	return s
}

// watchLine feeds a message into enforcement. Returns after acting if a
// heuristic trips.
func (m *Module) watchLine(ev *bus.Event, mask string) {
	if mask == "" || !m.enabled() || m.isResident(ev.Server, mask) {
		return
	}
	s := m.suspectFor(mask, ev.Sender.Nick, ev.Sender.Host)
	if s.actioned {
		return
	}
	now := m.Now()
	s.channels[ev.Channel] = true
	s.lines = append(s.lines, stamp{text: ev.Msg, when: now})
	s.lines = prune(s.lines, now, time.Duration(m.ctx.Conf.Int("guard_line_window_sec"))*time.Second)

	// line rate: many lines in a short window
	if len(s.lines) >= m.ctx.Conf.Int("guard_line_rate") {
		m.enforce(mask, s, "line flooding")
		return
	}
	// cross-channel duplicate: the same line in >= N distinct channels
	if m.dupChannels(s, ev.Msg) >= m.ctx.Conf.Int("guard_dup_channels") {
		m.enforce(mask, s, "same message to multiple channels")
		return
	}
}

// watchJoin feeds a join into enforcement (mass-join detection).
func (m *Module) watchJoin(ev *bus.Event, mask string) {
	if mask == "" || !m.enabled() || m.isResident(ev.Server, mask) {
		return
	}
	s := m.suspectFor(mask, ev.Sender.Nick, ev.Sender.Host)
	if s.actioned {
		return
	}
	now := m.Now()
	s.channels[ev.Channel] = true
	s.joins = append(s.joins, stamp{text: ev.Channel, when: now})
	s.joins = prune(s.joins, now, time.Duration(m.ctx.Conf.Int("guard_join_window_sec"))*time.Second)
	if distinct(s.joins) >= m.ctx.Conf.Int("guard_join_channels") {
		m.enforce(mask, s, "joining many channels at once")
	}
}

// dupChannels counts the distinct channels the suspect sent msg to
// within the line window.
func (m *Module) dupChannels(s *suspect, msg string) int {
	// lines carry text but not their channel; the cross-channel signal
	// is that the same text recurred and the suspect is in >1 channel.
	// We approximate with: identical text seen this many times, capped
	// by the number of channels we share with them.
	same := 0
	for _, l := range s.lines {
		if l.text == msg {
			same++
		}
	}
	if same > len(s.channels) {
		same = len(s.channels)
	}
	return same
}

// enforce glines the host and kicks the suspect from every shared
// channel. Idempotent via s.actioned.
func (m *Module) enforce(mask string, s *suspect, why string) {
	s.actioned = true
	m.actions++
	reason := "spam guard: " + why
	dur := m.ctx.Conf.Int("guard_gline_seconds")
	m.ctx.SendRaw(fmt.Sprintf("GLINE *@%s %ds :%s", s.host, dur, reason))
	for ch := range s.channels {
		m.ctx.SendRaw(fmt.Sprintf("KICK %s %s :%s", ch, s.nick, reason))
	}
	slog.Warn("guard: enforced", "mask", mask, "nick", s.nick, "reason", why,
		"channels", len(s.channels), "gline_seconds", dur)
}

// tryAuth handles the query escape hatch: a non-resident proves they
// are human with the shared password and joins the residents table, so
// legit newcomers arriving mid-wave are not swept up. Returns whether
// the query was an auth attempt (handled).
func (m *Module) tryAuth(ev *bus.Event) bool {
	rest, ok := strings.CutPrefix(strings.TrimSpace(ev.Msg), "auth")
	if !ok {
		return false
	}
	pass := strings.TrimSpace(rest)
	want := m.ctx.Conf.String("guard_auth_password")
	mk := mask(ev)
	if want == "" || pass != want {
		slog.Info("guard: failed auth", "nick", ev.Sender.Nick, "mask", mk)
		m.ctx.Privmsg(ev.Sender.Nick, "Guard: authentication failed.")
		return true
	}
	// promote to resident (proven human) and clear any suspicion
	if m.residents[ev.Server] == nil {
		m.residents[ev.Server] = make(map[string]int64)
	}
	m.residents[ev.Server][mk] = m.Now().Unix()
	m.dirty = true
	delete(m.suspects, mk)
	slog.Info("guard: authed", "nick", ev.Sender.Nick, "mask", mk)
	m.ctx.Privmsg(ev.Sender.Nick, "Guard: you are cleared. Welcome.")
	return true
}

// prune drops stamps older than window from the front (stamps are
// append-only in time order).
func prune(stamps []stamp, now time.Time, window time.Duration) []stamp {
	cutoff := now.Add(-window)
	i := 0
	for i < len(stamps) && stamps[i].when.Before(cutoff) {
		i++
	}
	return stamps[i:]
}

// distinct counts distinct texts (channel names) in stamps.
func distinct(stamps []stamp) int {
	seen := make(map[string]bool, len(stamps))
	for _, s := range stamps {
		seen[s.text] = true
	}
	return len(seen)
}
