// Package core assembles the bot: one dispatcher goroutine owning the
// bus, scheduler, config, storage, command registry, pager, and the
// IRC session, with the connection transport and fetcher feeding work
// back in through a channel. The Go counterpart of botje.pl's select
// loop plus IRC.pm's connection management.
package core

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net"
	"regexp"
	"strings"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/conf"
	"go-botje/internal/fetch"
	"go-botje/internal/format"
	"go-botje/internal/irc"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/irc/pager"
	"go-botje/internal/module"
	"go-botje/internal/sched"
	"go-botje/internal/storage"
)

// Config is what standalone mode needs to run one network.
type Config struct {
	Network   string
	Addr      string // host:port
	TLS       bool
	Nick      string
	Channels  []string
	Store     storage.Store
	Modules   []module.Module
	Dial      func() (net.Conn, error) // test hook; nil dials Addr
	JoinDelay time.Duration            // 0 means the Perl 10s
}

// ircEvents is every event name the session can emit.
var ircEvents = []string{
	"IRC_ERROR", "IRC_PRIVMSG", "IRC_NOTICE", "IRC_MODE", "IRC_JOIN",
	"IRC_KICK", "IRC_PART", "IRC_INVITE", "IRC_QUIT", "IRC_TOPIC",
	"config_changed", "QUIT",
}

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

var newlineRe = regexp.MustCompile(`\n\s*`)

type core struct {
	cfg   Config
	work  chan func()
	sch   *sched.Sched
	bus   *bus.Bus
	conf  *conf.Conf
	cmds  *cmd.Registry
	pager *pager.Pager

	conn    *irc.Conn // nil while disconnected
	session *irc.Session
	backoff irc.Backoff
}

// Run connects and dispatches until ctx is cancelled, reconnecting
// with backoff on connection loss. It returns after the goodbye QUIT
// has been flushed.
func Run(ctx context.Context, cfg Config) error {
	if cfg.JoinDelay == 0 {
		cfg.JoinDelay = 10 * time.Second
	}
	c := &core{
		cfg:  cfg,
		work: make(chan func(), 256),
		sch:  sched.New(time.Now),
		bus:  bus.New(),
		conf: conf.New(),
		cmds: cmd.New(),
	}
	for _, name := range ircEvents {
		c.bus.RegisterEvent(name)
	}
	c.conf.OnChange = func(name string) {
		c.bus.Submit(&bus.Event{Name: "config_changed", Msg: name, Extra: map[string]any{}})
	}
	c.conf.CreateInt("anti_flood_max_lines", 4)

	c.pager = pager.New(c.sch, func(channel, line string) { c.privmsg(channel, line) })
	c.pager.MaxLines = func() int { return c.conf.Int("anti_flood_max_lines") }
	c.cmds.Reply = func(ev *bus.Event, msg string) { c.privmsg(ev.Channel, msg) }
	c.cmds.Register("core", "more", func(d *cmd.Data) bool {
		c.pager.More(d.Event, d.Data)
		return true
	})

	fetcher := fetch.New(func(fn func()) { c.work <- fn })
	mctx := &module.Context{
		Bus: c.bus, Cmd: c.cmds, Pager: c.pager, Conf: c.conf,
		Store: cfg.Store, Sched: c.sch, Fetch: fetcher,
		Privmsg: c.privmsg,
	}
	for _, m := range cfg.Modules {
		if err := m.Load(mctx); err != nil {
			slog.Error("core: module load failed", "module", m.Name(), "err", err)
		} else {
			slog.Info("core: module loaded", "module", m.Name())
		}
	}

	c.connect()
	c.loop(ctx)
	c.shutdown()
	return nil
}

// loop is the dispatcher: everything that touches modules runs here.
func (c *core) loop(ctx context.Context) {
	for {
		var timerC <-chan time.Time
		var timer *time.Timer
		if d, ok := c.sch.NextIn(); ok {
			timer = time.NewTimer(d)
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case fn := <-c.work:
			fn()
		case <-timerC:
			c.sch.RunDue()
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// connect dials and wires a fresh session; failures schedule a retry.
func (c *core) connect() {
	sess := irc.NewSession(c.cfg.Network, c.cfg.Nick, time.Now)
	conn, err := irc.Connect(irc.ConnConfig{
		Network: c.cfg.Network,
		Addr:    c.cfg.Addr,
		TLS:     c.cfg.TLS,
		Dial:    c.cfg.Dial,
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
	sess.Emit = func(ev *bus.Event) {
		c.bus.Submit(ev)
		if ev.Name == "IRC_PRIVMSG" && !ev.SenderMe {
			c.cmds.Handle(ev)
		}
	}
	c.conn, c.session = conn, sess

	sess.Register()
	channels := c.cfg.Channels
	c.sch.After(c.cfg.JoinDelay, func() {
		if c.session == sess { // still this connection
			sess.JoinChannels(channels)
		}
	})
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
	delay := c.backoff.Next(time.Now())
	slog.Info("core: scheduling reconnect", "delay", delay)
	c.sch.After(delay, c.connect)
}

// privmsg is the Perl cmd_privmsg: strip whitespace/colons from the
// receiver, split the message on newlines, wrap long lines at the wire
// budget, and queue everything through flood control.
func (c *core) privmsg(receiver, msg string) {
	if c.conn == nil {
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
	for _, part := range newlineRe.Split(msg, -1) {
		for _, line := range format.SplitMessage("PRIVMSG "+receiver+" :", part) {
			c.conn.Write(line)
		}
	}
}

// shutdown says goodbye and gives the flood queue a moment to flush.
func (c *core) shutdown() {
	if c.conn == nil {
		return
	}
	c.bus.Submit(&bus.Event{Name: "QUIT", Extra: map[string]any{}})
	c.conn.Write("QUIT :" + quitMsgs[rand.IntN(len(quitMsgs))])
	time.Sleep(1500 * time.Millisecond)
	c.conn.Close()
}
