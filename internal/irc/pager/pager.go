// Package pager is the reply pager, the Go counterpart of the Perl
// cmd_eventmsg + !more: replies send at most anti_flood_max_lines
// (default 4) lines, the last visible line gets a {W}(+N) suffix, the
// remainder is stashed per (channel, nick, command) with a 600s expiry
// and paged out by !more. One fix vs Perl: replacing a stash cancels
// its old expiry timer (the Perl left it running, killing the new
// stash early). Dispatcher goroutine only.
package pager

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/sched"
)

// expiry is how long an unread stash survives.
const expiry = 600 * time.Second

type stashKey struct {
	channel, nick, command string
}

type stash struct {
	msgs      []string
	displayed int
	timer     sched.Tag
}

// Pager stashes overflow reply lines per (channel, nick, command).
type Pager struct {
	// MaxLines returns the per-burst line budget; nil means 4. Read
	// fresh on every call so a config change applies immediately.
	MaxLines func() int

	sched   *sched.Sched
	send    func(channel, line string)
	stashes map[stashKey]*stash
}

// New returns a pager that schedules expiries on s and sends lines
// through send(channel, line). Lines may contain {x} color tags; the
// send layer colorizes.
func New(s *sched.Sched, send func(channel, line string)) *Pager {
	return &Pager{sched: s, send: send, stashes: make(map[stashKey]*stash)}
}

func (p *Pager) maxLines() int {
	if p.MaxLines != nil {
		return p.MaxLines()
	}
	return 4
}

// channelOf cleans the event channel the way the Perl does
// (s/[\s:]//g); empty means no reply target.
func channelOf(ev *bus.Event) string {
	ch := strings.Map(func(r rune) rune {
		if r == ':' || r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, ev.Channel)
	return ch
}

// EventMsg replies to the event's channel with msgs (each split on
// newlines, blank lines dropped), paging beyond the line budget.
// command keys the stash for !more.
func (p *Pager) EventMsg(ev *bus.Event, command string, msgs ...string) {
	channel := channelOf(ev)
	if channel == "" {
		return
	}
	max := p.maxLines()

	var lines []string
	for _, m := range msgs {
		for l := range strings.SplitSeq(m, "\n") {
			if strings.TrimSpace(l) != "" {
				lines = append(lines, l)
			}
		}
	}

	shown := min(len(lines), max)
	for i := range shown {
		line := lines[i]
		if i+1 == shown && i+1 < len(lines) {
			line += fmt.Sprintf(" {W}(+%d)", len(lines)-i-1)
			lines[i] = line
		}
		p.send(channel, line)
	}

	key := stashKey{channel, ev.Sender.Nick, command}
	p.drop(key)
	if len(lines) > max {
		p.stashes[key] = &stash{
			msgs:      lines,
			displayed: max,
			timer:     p.sched.After(expiry, func() { delete(p.stashes, key) }),
		}
	}
}

// More pages out a stash for the event's (channel, nick): the !more
// handler. requested picks the command when several are pending.
func (p *Pager) More(ev *bus.Event, requested string) {
	channel := channelOf(ev)
	if channel == "" {
		return
	}
	nick := ev.Sender.Nick
	max := p.maxLines()

	pending := p.pendingCommands(channel, nick)
	if len(pending) == 0 {
		p.send(channel, "There is nothing more to display for you.")
		return
	}
	command := requested
	if command == "" {
		if len(pending) > 1 {
			p.send(channel, "Multiple commands are waiting for you to display more. Use !more "+
				strings.Join(pending, "|")+" (choose one) to specify for which one you want to see more!")
			return
		}
		command = pending[0]
	}

	key := stashKey{channel, nick, command}
	st, ok := p.stashes[key]
	if !ok {
		p.send(channel, "There is nothing (more) to display for "+command+" for you.")
		return
	}

	end := st.displayed + max
	for i := st.displayed; i < end && i < len(st.msgs); i++ {
		line := st.msgs[i]
		if i+1 == end && i+1 < len(st.msgs) {
			line += fmt.Sprintf(" {W}(+%d)", len(st.msgs)-i-1)
			st.msgs[i] = line
		}
		p.send(channel, line)
	}
	if end < len(st.msgs) {
		st.displayed = end
	} else {
		p.drop(key)
	}
}

// pendingCommands lists stashed commands for (channel, nick), sorted
// (the Perl left them in hash order).
func (p *Pager) pendingCommands(channel, nick string) []string {
	var out []string
	for key := range p.stashes {
		if key.channel == channel && key.nick == nick {
			out = append(out, key.command)
		}
	}
	slices.Sort(out)
	return out
}

// drop removes a stash and cancels its expiry timer.
func (p *Pager) drop(key stashKey) {
	if st, ok := p.stashes[key]; ok {
		p.sched.Unschedule(st.timer)
		delete(p.stashes, key)
	}
}
