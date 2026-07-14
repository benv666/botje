// Package logger writes plain-text IRC logs, one file per channel per
// day, plus a per-network server log for events without a channel
// (quits, own-nick modes). NOT a port: the Perl Log.pm was console
// logging plumbing and its per-channel file logging stayed a TODO from
// 2007 to the end.
//
// Layout under logger_dir (default $BOTJE_LOG_DIR, empty = disabled):
//
//	<network>/<#channel>/YYYY-MM-DD.log   channel traffic
//	<network>/queries/<nick>/...          private messages
//	<network>/server/...                  quits and other channel-less events
//
// Daily files double as rotation. The bot's own lines arrive via the
// IRC_SENT event (emitted by core.privmsg); {x} tags and mIRC control
// codes are stripped so the files stay grep-clean. The ops.log next to
// these directories is written by the core (internal/teelog), not by
// this module.
package logger

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/format"
	"go-botje/internal/irc"
	"go-botje/internal/module"
)

type openFile struct {
	f    *os.File
	date string
}

// Module is the logger. Now is a test hook.
type Module struct {
	Now func() time.Time

	ctx   *module.Context
	files map[string]*openFile // rel dir -> current day's file
}

func New() *Module {
	return &Module{Now: time.Now, files: make(map[string]*openFile)}
}

func (m *Module) Name() string { return "logger" }

func (m *Module) Load(ctx *module.Context) error {
	m.ctx = ctx
	ctx.Conf.CreateString("logger_dir", os.Getenv("BOTJE_LOG_DIR"))
	for ev, h := range map[string]bus.Handler{
		"IRC_PRIVMSG": m.onPrivmsg,
		"IRC_SENT":    m.onPrivmsg,
		"IRC_NOTICE":  m.onNotice,
		"IRC_JOIN":    m.onJoin,
		"IRC_PART":    m.onPart,
		"IRC_QUIT":    m.onQuit,
		"IRC_KICK":    m.onKick,
		"IRC_MODE":    m.onMode,
		"IRC_TOPIC":   m.onTopic,
		"IRC_INVITE":  m.onInvite,
	} {
		if err := ctx.Bus.RegisterHook(m.Name(), ev, h); err != nil {
			return err
		}
	}
	return nil
}

func (m *Module) Unload() error {
	for _, of := range m.files {
		of.f.Close()
	}
	m.files = make(map[string]*openFile)
	m.ctx.Bus.UnregisterModule(m.Name())
	return nil
}

// who renders "nick [user@host]" for join/part/quit lines.
func who(ev *bus.Event) string {
	return fmt.Sprintf("%s [%s@%s]", ev.Sender.Nick, ev.Sender.User, ev.Sender.Host)
}

func (m *Module) onPrivmsg(ev *bus.Event) (bus.Handled, any) {
	msg := format.Strip(ev.Msg)
	line := fmt.Sprintf("<%s> %s", ev.Sender.Nick, msg)
	// CTCP ACTION renders irssi-style; other CTCPs are not chat
	if rest, ok := strings.CutPrefix(msg, "\x01ACTION "); ok {
		line = fmt.Sprintf("* %s %s", ev.Sender.Nick, strings.TrimSuffix(rest, "\x01"))
	} else if strings.HasPrefix(msg, "\x01") {
		return bus.None, nil
	}
	m.write(ev.Server, ev.Channel, line)
	return bus.None, nil
}

func (m *Module) onNotice(ev *bus.Event) (bus.Handled, any) {
	line := fmt.Sprintf("-%s- %s", ev.Sender.Nick, format.Strip(ev.Msg))
	// server notices: dotted sender (nicks cannot contain dots) or the
	// pre-registration "*" target
	if strings.Contains(ev.Sender.Nick, ".") || ev.Channel == "*" {
		m.writeServer(ev.Server, line)
		return bus.None, nil
	}
	target := ev.Channel
	if ev.TargetMe {
		target = ev.Sender.Nick // notices to us file under the sender's query
	}
	m.write(ev.Server, target, line)
	return bus.None, nil
}

func (m *Module) onJoin(ev *bus.Event) (bus.Handled, any) {
	m.write(ev.Server, ev.Channel, fmt.Sprintf("-!- %s has joined %s", who(ev), ev.Channel))
	return bus.None, nil
}

func (m *Module) onPart(ev *bus.Event) (bus.Handled, any) {
	// the session keeps the part reason out of the event (perl parity),
	// so dig it out of the raw params
	reason := ""
	if p := irc.SplitParams(ev.Raw.Params); len(p) > 1 {
		reason = p[1]
	}
	m.write(ev.Server, ev.Channel, fmt.Sprintf("-!- %s has left %s [%s]", who(ev), ev.Channel, format.Strip(reason)))
	return bus.None, nil
}

func (m *Module) onQuit(ev *bus.Event) (bus.Handled, any) {
	// quits carry no channel and we keep no user->channels map: they go
	// to the per-network server log
	msg := ev.Msg
	m.writeServer(ev.Server, fmt.Sprintf("-!- %s has quit [%s]", who(ev), format.Strip(msg)))
	return bus.None, nil
}

func (m *Module) onKick(ev *bus.Event) (bus.Handled, any) {
	// perl parity: the kick channel travels in extra, ev.Channel is empty
	channel, target, reason := ev.Channel, ev.Target, ev.Reason
	m.write(ev.Server, channel, fmt.Sprintf("-!- %s was kicked from %s by %s [%s]",
		target, channel, ev.Sender.Nick, format.Strip(reason)))
	return bus.None, nil
}

func (m *Module) onMode(ev *bus.Event) (bus.Handled, any) {
	mode, args := ev.Mode, ev.Args
	full := strings.TrimSpace(mode + " " + strings.Join(args, " "))
	if ev.TargetMe {
		m.writeServer(ev.Server, fmt.Sprintf("-!- mode/%s [%s] by %s", ev.Channel, full, ev.Sender.Nick))
	} else {
		m.write(ev.Server, ev.Channel, fmt.Sprintf("-!- mode/%s [%s] by %s", ev.Channel, full, ev.Sender.Nick))
	}
	return bus.None, nil
}

func (m *Module) onTopic(ev *bus.Event) (bus.Handled, any) {
	topic := ev.Topic
	m.write(ev.Server, ev.Channel, fmt.Sprintf("-!- %s changed the topic of %s to: %s",
		ev.Sender.Nick, ev.Channel, format.Strip(topic)))
	return bus.None, nil
}

func (m *Module) onInvite(ev *bus.Event) (bus.Handled, any) {
	channel := ev.Channel
	m.writeServer(ev.Server, fmt.Sprintf("-!- %s invited %s to %s", ev.Sender.Nick, ev.BotNick, channel))
	return bus.None, nil
}

func (m *Module) writeServer(network, line string) {
	m.append(filepath.Join(sanitize(network), "server"), line)
}

func (m *Module) write(network, target, line string) {
	target = strings.ToLower(target)
	rel := filepath.Join(sanitize(network), sanitize(target))
	if !strings.HasPrefix(target, "#") && !strings.HasPrefix(target, "&") {
		rel = filepath.Join(sanitize(network), "queries", sanitize(target))
	}
	m.append(rel, line)
}

// append writes one timestamped line to rel's current day file,
// rolling over (and creating directories) as the date changes.
func (m *Module) append(rel, line string) {
	dir := m.ctx.Conf.String("logger_dir")
	if dir == "" {
		return
	}
	now := m.Now()
	date := now.Format("2006-01-02")
	of := m.files[rel]
	if of == nil || of.date != date {
		if of != nil {
			of.f.Close()
		}
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(path, 0o755); err != nil {
			slog.Error("logger: mkdir", "path", path, "err", err)
			return
		}
		f, err := os.OpenFile(filepath.Join(path, date+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			slog.Error("logger: open", "path", path, "err", err)
			return
		}
		of = &openFile{f: f, date: date}
		m.files[rel] = of
	}
	if _, err := fmt.Fprintf(of.f, "%s %s\n", now.Format("15:04:05"), line); err != nil {
		slog.Error("logger: write", "rel", rel, "err", err)
	}
}

// sanitize makes a channel or nick safe as a single path element:
// separators become underscores (which also defuses any ".." between
// them), and a name that IS a dot element gets prefixed.
func sanitize(name string) string {
	name = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', 0:
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return "_" + name
	}
	return name
}
