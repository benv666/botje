package core

import (
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/format"
	"go-botje/internal/irc"
)

// the Perl bye() quit messages, verbatim
var quitMsgs = []string{
	"Bye bye morons!",
	"*knock* *knock* OPEN UP, FBI!!! Uh oh 'rm -rf / ; reboot'",
	"Just... one... more... turn....",
	"Page fault, going to library",
	"Connection reset by asshole.",
	"*KABOOOOOOOOOM*",
	"He's DEAD, Jim. You grab his tricorder, I'll get his wallet",
	"Found another bug! Applying bug tape",
	"I'm sure this is just a minor upgrade, I'll be back in a few seconds!",
	"Rebooting your machine",
	"Upgrading to IPv5...",
	"I've had it with you guys!",
}

// splitLines breaks a message into IRC lines on newlines only,
// dropping blank lines. The Perl cmd_privmsg split on /\n\s*/, which
// ate the leading whitespace of every continuation line and mangled
// ASCII art (pacman); this matches Perl's own cmd_eventmsg, which
// splits on \n and greps out blank lines, preserving indentation.
func splitLines(msg string) []string {
	var out []string
	for line := range strings.SplitSeq(msg, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// isChannel reports whether an IRC target names a channel (RFC prefixes).
func isChannel(target string) bool {
	if target == "" {
		return false
	}
	switch target[0] {
	case '#', '&', '+', '!':
		return true
	}
	return false
}

// connect dials and wires a fresh session; failures schedule a retry.
func (c *core) connect() {
	sess := irc.NewSession(c.cfg.Network, c.cfg.Nick, time.Now)
	conn, err := irc.Connect(irc.ConnConfig{
		Network:  c.cfg.Network,
		Addr:     c.cfg.Addr,
		TLS:      c.cfg.TLS,
		CertFile: c.cfg.CertFile,
		KeyFile:  c.cfg.KeyFile,
		Dial:     c.cfg.Dial,
		OnLine: func(line string) {
			c.work <- func() { sess.HandleLine(line) }
		},
		OnDisconnect: func(err error) {
			c.work <- func() { c.disconnected(err) }
		},
	})
	if err != nil {
		slog.Error("core: connect failed", "addr", c.cfg.Addr, "err", err)
		c.scheduleReconnect()
		return
	}
	slog.Info("core: connected", "network", c.cfg.Network, "addr", c.cfg.Addr, "nick", c.cfg.Nick)

	sess.Send = conn.Write
	sess.SendHigh = conn.WriteHigh
	dispatch := func(ev *bus.Event) {
		c.bus.Submit(ev)
		if ev.Name == "IRC_PRIVMSG" && !ev.SenderMe {
			c.cmds.Handle(ev)
		}
	}
	// user messages arriving before the welcome are held and replayed
	// after it: the keeper buffers inbound while a core restarts, and
	// flushing that buffer into an unregistered core made modules
	// mutate state (a rad-van-fortuin spin!) while every reply was
	// eaten by the pre-welcome outbound guard (BenV's silent "draai",
	// 2026-07-14). The cap keeps a hostile backlog from ballooning;
	// anything beyond it is dropped like it always was.
	var held []*bus.Event
	sess.Emit = func(ev *bus.Event) {
		if ev.Name == "IRC_PRIVMSG" && !ev.SenderMe && !sess.Welcomed() {
			if len(held) < 64 {
				held = append(held, ev)
			}
			return
		}
		dispatch(ev)
	}
	c.conn, c.session = conn, sess

	// join when the server confirms registration (001, or 462 on a
	// keeper-resume), never on a timer: a JOIN sent before the server
	// registers us dies with ERR_NOTREGISTERED (2026-07-13, JOINs
	// queued in the keeper flushed ahead of registration completing)
	sess.Welcome = func() {
		if c.session != sess { // superseded connection
			return
		}
		sess.JoinChannels(c.channels)
		for _, ev := range held {
			dispatch(ev)
		}
		held = nil
	}
	sess.Register()
}

func (c *core) disconnected(err error) {
	slog.Warn("core: connection lost", "network", c.cfg.Network, "err", err)
	if c.conn != nil {
		c.conn.Close()
	}
	c.conn, c.session = nil, nil
	c.scheduleReconnect()
}

func (c *core) scheduleReconnect() {
	if c.cfg.Metrics != nil {
		c.cfg.Metrics.IncCounter("botje_reconnects_total", nil)
	}
	delay := c.backoff.Next(time.Now())
	slog.Info("core: scheduling reconnect", "delay", delay)
	c.sch.After(delay, c.connect)
}

// privmsg is the Perl cmd_privmsg: strip whitespace/colons from the
// receiver, split the message on newlines, wrap long lines at the wire
// budget, and queue everything through flood control.
func (c *core) privmsg(receiver, msg string) {
	// nothing goes out before the server confirms registration: the
	// ircd would eat it with ERR_NOTREGISTERED after it burned flood
	// budget (and keeper buffer, when the IRC side is down)
	if c.conn == nil || c.session == nil || !c.session.Welcomed() {
		return
	}
	receiver = strings.Map(func(r rune) rune {
		if r == ':' || r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, receiver)
	if receiver == "" {
		return
	}
	// drop channel messages to channels we are not in: the ircd would
	// reject them with ERR_CANNOTSENDTOCHAN, but only after they burn
	// the shared 1 msg/s flood budget and delay real replies (RSS and
	// ticker broadcast to their target channels whether we joined or
	// not). Queries to nicks are unaffected.
	if isChannel(receiver) && c.session != nil && !c.session.InChannel(receiver) {
		slog.Debug("core: dropping message to un-joined channel", "channel", receiver)
		return
	}
	for _, part := range splitLines(msg) {
		for _, line := range format.SplitMessage("PRIVMSG "+receiver+" :", part) {
			c.conn.Write(line)
		}
		// IRC_SENT lets the logger see the bot's own lines. A separate
		// event, NOT an IRC_PRIVMSG with SenderMe: modules keep the perl
		// behavior of never seeing bot output (markov must not learn its
		// own lines). Nested Submit is fine, chain tracking is per
		// (module,event); an IRC_SENT hook calling privmsg again is
		// refused by that same tracking rather than recursing.
		c.bus.Submit(&bus.Event{
			Name: "IRC_SENT", BotNick: c.cfg.Nick, Server: c.cfg.Network,
			Sender: bus.Sender{Nick: c.cfg.Nick}, SenderMe: true,
			Channel: receiver, Msg: part,
		})
	}
}

// sendRaw queues a raw IRC line at high priority (the module SendRaw).
// No-op while disconnected: a spam-wave kickban that misses is better
// than a panic, and the guard re-evaluates on the next event.
func (c *core) sendRaw(line string) {
	if c.conn == nil {
		return
	}
	c.conn.WriteHigh(line)
}

// inChannel reports whether the bot is in a channel (the module
// InChannel). False while disconnected.
func (c *core) inChannel(channel string) bool {
	return c.session != nil && c.session.InChannel(channel)
}

// members lists a channel's tracked nicks (the module Members). Empty
// while disconnected.
func (c *core) members(channel string) []string {
	if c.session == nil {
		return nil
	}
	return c.session.Members(channel)
}

// shutdown lets modules say goodbye and, unless running under a keeper,
// sends the IRC QUIT. Under a keeper the session must survive a core
// restart, so the QUIT is suppressed and the connection just closes;
// the keeper keeps the IRC link and the next core resumes.
func (c *core) shutdown() {
	if c.conn == nil {
		return
	}
	c.bus.Submit(&bus.Event{Name: "QUIT"})
	if !c.cfg.SkipGoodbye {
		c.conn.Write("QUIT :" + quitMsgs[rand.IntN(len(quitMsgs))])
		time.Sleep(1500 * time.Millisecond)
	}
	c.conn.Close()
}
