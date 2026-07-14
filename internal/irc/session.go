package irc

import (
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"go-botje/internal/bus"
)

// channelState is what the bot tracks per joined channel.
type channelState struct {
	joined time.Time
	topic  string
}

// Session is the per-network protocol state machine: parsed lines in,
// Perl-shaped bus events and outbound lines out, plus channel/topic/
// mode/motd tracking. Pure: no sockets, no goroutines; the connection
// shell owns the I/O and feeds HandleLine. Dispatcher goroutine only.
type Session struct {
	// Send queues a normal outbound line, SendHigh a high-priority one
	// (PONG). Emit receives constructed events.
	Send     func(line string)
	SendHigh func(line string)
	Emit     func(ev *bus.Event)

	// UserName and RealName go into USER at registration.
	UserName string
	RealName string

	// Welcome fires once when the server confirms registration: 001 on
	// a fresh connection, or 462 when a restarted core re-registers
	// over a keeper's live session (the ircd rejects the second USER,
	// which proves we are already in). The owner joins channels here.
	Welcome func()

	network  string
	nick     string
	now      func() time.Time
	welcomed bool

	channels map[string]*channelState
	mode     string
	motd     strings.Builder

	// prefixless lines inherit the last seen sender (Perl ircParser)
	lastNick, lastUser, lastHost string
}

// NewSession returns a session for network with the given bot nick,
// reading time from now.
func NewSession(network, nick string, now func() time.Time) *Session {
	return &Session{
		UserName: "Botje",
		RealName: "BenV's Test Bot",
		network:  network,
		nick:     nick,
		now:      now,
		channels: make(map[string]*channelState),
	}
}

// Welcomed reports whether the server has confirmed registration.
func (s *Session) Welcomed() bool { return s.welcomed }

// Register sends the NICK/USER registration.
func (s *Session) Register() {
	s.Send("NICK " + s.nick)
	s.Send("USER " + s.UserName + " i * :" + s.RealName)
}

// JoinChannels sends a JOIN per channel (the owner schedules this 10s
// after connect, like the Perl) and marks each as joined optimistically.
//
// The optimistic mark matters under a keeper: a restarted core re-JOINs
// channels the live IRC session is already in, and the ircd sends no
// JOIN echo for an already-joined channel, so membership tracking would
// otherwise stay empty and every channel message would be dropped. A
// real echo later just refreshes the timestamp; a genuinely failed join
// (banned, +i) leaves a stale mark, which at worst wastes flood budget
// on that one channel, the pre-existing behavior.
func (s *Session) JoinChannels(channels []string) {
	for _, ch := range channels {
		s.Send("JOIN " + ch)
		if _, ok := s.channels[ch]; !ok {
			s.channels[ch] = &channelState{joined: s.now()}
		}
	}
}

// HandleLine parses and processes one inbound line.
func (s *Session) HandleLine(raw string) {
	line, ok := ParseLine(raw)
	if !ok {
		slog.Debug("irc: unparsed line", "network", s.network, "line", raw)
		return
	}

	nick, user, host := s.lastNick, s.lastUser, s.lastHost
	if line.Prefix != "" {
		pn, pu, ph := ParsePrefix(line.Prefix)
		nick, user, host = pn, pu, ph
		s.lastNick = pn
		if pu != "" {
			s.lastUser = pu
		}
		if ph != "" {
			s.lastHost = ph
		}
	}

	ev := &bus.Event{
		BotNick:  s.nick,
		Server:   s.network,
		SenderMe: nick == s.nick,
		Raw:      bus.RawCmd{Prefix: line.Prefix, Cmd: line.Name, Params: line.Params},
	}
	ev.Sender.Nick, ev.Sender.User, ev.Sender.Host = nick, user, host

	switch line.Name {
	case "ERROR":
		s.onError(ev, line)
	case "PING":
		s.onPing(line)
	case "PRIVMSG":
		s.onPrivmsg(ev, line)
	case "NOTICE":
		s.onNotice(ev, line)
	case "MODE":
		s.onMode(ev, line)
	case "JOIN":
		s.onJoin(ev, line)
	case "PART":
		s.onPart(ev, line)
	case "KICK":
		s.onKick(ev, line)
	case "INVITE":
		s.onInvite(ev, line)
	case "QUIT":
		s.onQuit(ev, line)
	case "TOPIC":
		s.onTopic(ev, line)
	case "RPL_TOPIC":
		s.onRplTopic(line)
	case "ERR_NICKNAMEINUSE":
		// divergence from the Perl (which sat nickless forever): retry
		// with an underscore, the classic client move
		s.nick += "_"
		slog.Warn("irc: nick in use, retrying", "network", s.network, "nick", s.nick)
		s.Send("NICK " + s.nick)
	case "RPL_WELCOME", "ERR_ALREADYREGISTRED":
		// registration confirmed; joining is anchored here, not on a
		// timer: a JOIN sent before the server confirms registration
		// dies with ERR_NOTREGISTERED (2026-07-13: pre-welcome JOINs
		// buffered in the keeper were silently eaten that way)
		if !s.welcomed {
			s.welcomed = true
			if s.Welcome != nil {
				s.Welcome()
			}
		}
	case "RPL_MOTDSTART":
		s.motd.Reset()
	case "RPL_MOTD":
		if p := SplitParams(line.Params); len(p) > 0 {
			s.motd.WriteString(p[len(p)-1])
			s.motd.WriteString("\n")
		}
	default:
		// RPL_ENDOFMOTD, RPL_NOTOPIC, unknown numerics: log only
		slog.Debug("irc: unhandled command", "network", s.network, "cmd", line.Name)
	}
}

func (s *Session) emit(name string, ev *bus.Event) {
	ev.Name = name
	s.Emit(ev)
}

func (s *Session) onError(ev *bus.Event, line Line) {
	ev.Msg = strings.Join(SplitParams(line.Params), ", ")
	s.emit("IRC_ERROR", ev)
	s.Send("QUIT :I can't deal with ERRORs, Bye bye!")
}

func (s *Session) onPing(line Line) {
	pong := "PONG"
	if p := SplitParams(line.Params); len(p) > 0 && p[0] != "" {
		magic := p[0]
		if strings.ContainsAny(magic, " \t:") {
			magic = ":" + magic
		}
		pong += " " + magic
	}
	s.SendHigh(pong)
}

func (s *Session) onPrivmsg(ev *bus.Event, line Line) {
	p := SplitParams(line.Params)
	if len(p) < 2 {
		return
	}
	target, msg := p[0], p[1]
	if target == s.nick {
		// query: rewrite the reply target to the sender
		ev.TargetMe = true
		ev.Query = true
		target = ev.Sender.Nick
	}
	ev.Channel = target
	ev.Msg = msg
	ev.Args = p[2:]
	s.emit("IRC_PRIVMSG", ev)
}

func (s *Session) onNotice(ev *bus.Event, line Line) {
	p := SplitParams(line.Params)
	if len(p) < 2 {
		return
	}
	// perl parity: the notice channel is the target, never rewritten
	ev.Channel = p[0]
	ev.TargetMe = p[0] == s.nick
	ev.Msg = p[1]
	s.emit("IRC_NOTICE", ev)
}

func (s *Session) onMode(ev *bus.Event, line Line) {
	p := SplitParams(line.Params)
	if len(p) < 2 {
		return
	}
	target, mode := p[0], p[1]
	ev.Channel = target
	ev.TargetMe = target == s.nick
	if ev.TargetMe {
		s.mode = mode
	}
	ev.Mode = mode
	ev.Args = p[2:]
	s.emit("IRC_MODE", ev)
}

func (s *Session) onJoin(ev *bus.Event, line Line) {
	p := SplitParams(line.Params)
	if len(p) < 1 {
		return
	}
	channel := p[0]
	if ev.SenderMe {
		s.channels[channel] = &channelState{joined: s.now()}
		ev.TargetMe = true // technically it targets us... right?
	}
	ev.Channel = channel
	s.emit("IRC_JOIN", ev)
}

// onPart tracks and emits parts. The Perl efPart has a typo
// ($event->{$channel} instead of {channel}) so its PART events carry no
// channel; fixed here.
func (s *Session) onPart(ev *bus.Event, line Line) {
	p := SplitParams(line.Params)
	if len(p) < 1 {
		return
	}
	channel := p[0]
	if ev.SenderMe {
		delete(s.channels, channel)
	}
	ev.Channel = channel
	s.emit("IRC_PART", ev)
}

// onKick emits kicks with the channel in Channel (a deliberate
// divergence: the Perl left the event channel unset for KICK and hid
// it in extra). Being kicked does not drop the channel from tracking,
// same as the Perl.
func (s *Session) onKick(ev *bus.Event, line Line) {
	p := SplitParams(line.Params)
	if len(p) < 2 {
		return
	}
	channel, target := p[0], p[1]
	reason := ""
	if len(p) > 2 {
		reason = p[2]
	}
	ev.TargetMe = target == s.nick
	ev.Channel = channel
	ev.Target = target
	ev.Reason = reason
	s.emit("IRC_KICK", ev)
}

func (s *Session) onInvite(ev *bus.Event, line Line) {
	p := SplitParams(line.Params)
	if len(p) < 2 {
		return
	}
	ev.Channel = p[1]
	ev.TargetMe = true // always us
	s.emit("IRC_INVITE", ev)
}

var netsplitRe = regexp.MustCompile(`^\S+\s+.+`)

// onQuit classifies quits the way the Perl does: "Quit: x" keeps x,
// "Ping timeout"/"EOF..." keep the message, anything else with a space
// in it counts as a netsplit (the Irssi-grade heuristic never got
// finished), the rest passes through.
func (s *Session) onQuit(ev *bus.Event, line Line) {
	msg := strings.TrimPrefix(line.Params, ":")
	lower := strings.ToLower(msg)
	switch {
	case strings.HasPrefix(lower, "quit:"):
		ev.Msg = strings.TrimLeft(msg[len("quit:"):], " \t")
	case strings.HasPrefix(lower, "ping timeout"):
		ev.Msg = msg[:len("Ping timeout")]
	case strings.HasPrefix(lower, "eof"):
		ev.Msg = msg
	case netsplitRe.MatchString(msg):
		ev.Msg = msg
		ev.NetSplit = true
	default:
		ev.Msg = msg
	}
	s.emit("IRC_QUIT", ev)
}

func (s *Session) onTopic(ev *bus.Event, line Line) {
	p := SplitParams(line.Params)
	if len(p) < 2 {
		return
	}
	channel, topic := p[0], p[1]
	if st, ok := s.channels[channel]; ok {
		st.topic = topic
	} else {
		s.channels[channel] = &channelState{topic: topic}
	}
	ev.Channel = channel
	ev.Topic = topic
	s.emit("IRC_TOPIC", ev)
}

// onRplTopic (332) tracks the topic silently.
func (s *Session) onRplTopic(line Line) {
	p := SplitParams(line.Params)
	if len(p) < 3 {
		return
	}
	channel, topic := p[1], p[2]
	if st, ok := s.channels[channel]; ok {
		st.topic = topic
	} else {
		s.channels[channel] = &channelState{topic: topic}
	}
}

// Channels lists channels the bot has joined, sorted.
// InChannel reports whether the bot currently sits in a channel.
func (s *Session) InChannel(channel string) bool {
	_, ok := s.channels[channel]
	return ok
}

func (s *Session) Channels() []string {
	out := slices.Collect(maps.Keys(s.channels))
	slices.Sort(out)
	return out
}

// Topic returns the last seen topic for a channel.
func (s *Session) Topic(channel string) string {
	if st, ok := s.channels[channel]; ok {
		return st.topic
	}
	return ""
}

// Mode returns the bot's own user mode as last reported.
func (s *Session) Mode() string { return s.mode }

// Motd returns the last received message of the day.
func (s *Session) Motd() string { return s.motd.String() }

// Backoff computes reconnect delays: 3, 60, 180, 300 seconds,
// escalating per attempt, resetting after more than 300s of peace
// beyond the scheduled attempt. Divergence from Perl: repeated fast
// failures stay at 300s (the Perl fell off the list and never
// reconnected again).
type Backoff struct {
	delay time.Duration
	when  time.Time // when the scheduled attempt happens (call time + delay)
}

// Next returns the delay to wait before the next reconnect attempt.
// The caller is assumed to sleep the returned delay before attempting,
// so the quiet-period reset measures from the end of that sleep: a
// 300s sleep by itself must not reset the ladder (it did before
// 2026-07-13, hammering a connectban'd ircd at 3s again).
func (b *Backoff) Next(now time.Time) time.Duration {
	if !b.when.IsZero() && b.when.Add(300*time.Second).Before(now) {
		b.delay = 0 // last attempt long ago, start over
	}
	for _, d := range []time.Duration{3 * time.Second, 60 * time.Second, 180 * time.Second, 300 * time.Second} {
		if d > b.delay {
			b.delay = d
			b.when = now.Add(d)
			return d
		}
	}
	b.when = now.Add(300 * time.Second)
	return 300 * time.Second
}
