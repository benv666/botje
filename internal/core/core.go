// Package core assembles the bot: one dispatcher goroutine owning the
// bus, scheduler, config, storage, command registry, pager, and the
// IRC session, with the connection transport and fetcher feeding work
// back in through a channel. The Go counterpart of botje.pl's select
// loop plus IRC.pm's connection management.
package core

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"

	"go-botje/internal/admin"
	"go-botje/internal/auth"
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

	// Admin is the telnet control port. AdminListener wins over
	// AdminAddr (tests); both empty means no admin port.
	Auth          *auth.Auth
	AdminAddr     string
	AdminListener net.Listener

	// SkipGoodbye suppresses the QUIT on shutdown. Set when running
	// under a keeper: a core restart must leave the IRC session up, so
	// the keeper (not the core) owns the real goodbye.
	SkipGoodbye bool
}

// ircEvents is every event name the session can emit.
var ircEvents = []string{
	"IRC_ERROR", "IRC_PRIVMSG", "IRC_NOTICE", "IRC_MODE", "IRC_JOIN",
	"IRC_KICK", "IRC_PART", "IRC_INVITE", "IRC_QUIT", "IRC_TOPIC",
	"IRC_SENT", // outbound privmsg, one event per line; logger food
	"config_changed", "QUIT", "COMMAND",
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

	// channels is the persisted autojoin set (storage core/channels).
	// The flags only seed it on the very first boot; after that telnet
	// join/part and invites manage it.
	channels []string
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
	// conf values set at runtime (telnet conf x=y) persist in storage;
	// load them before any setting is created so they win over defaults.
	var storedConf map[string]string
	if _, err := cfg.Store.Get("core", "conf", &storedConf); err != nil {
		slog.Error("core: load stored conf", "err", err)
	} else if storedConf != nil {
		c.conf.LoadStored(storedConf)
	}
	c.conf.OnChange = func(name string) {
		if err := cfg.Store.Put("core", "conf", c.conf.Stored()); err != nil {
			slog.Error("core: save conf", "err", err)
		}
		c.bus.Submit(&bus.Event{Name: "config_changed", Msg: name, Extra: map[string]any{}})
	}
	c.conf.CreateInt("anti_flood_max_lines", 4)

	// the persisted channel set wins over the flags; the flags seed it
	// on the very first boot only
	if ok, err := cfg.Store.Get("core", "channels", &c.channels); err != nil {
		slog.Error("core: load channels", "err", err)
	} else if !ok {
		c.channels = slices.Clone(cfg.Channels)
		c.saveChannels()
	}
	c.bus.RegisterHook("core", "IRC_INVITE", func(ev *bus.Event) (bus.Handled, any) {
		ch, _ := ev.Extra["channel"].(string)
		if ch == "" {
			return bus.None, nil
		}
		slog.Info("core: invited, joining", "channel", ch, "by", ev.Sender.Nick)
		c.addChannel(ch)
		if c.session != nil {
			c.session.JoinChannels([]string{ch})
		}
		return bus.None, nil
	})

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

	if err := c.startAdmin(ctx); err != nil {
		return err
	}
	c.connect()
	c.loop(ctx)
	c.shutdown()
	return nil
}

// startAdmin brings up the telnet control port when configured. Admin
// commands run on the dispatcher: the server's Exec does a synchronous
// round-trip through the work channel.
func (c *core) startAdmin(ctx context.Context) error {
	ln := c.cfg.AdminListener
	if ln == nil && c.cfg.AdminAddr != "" {
		var err error
		if ln, err = net.Listen("tcp", c.cfg.AdminAddr); err != nil {
			return fmt.Errorf("core: admin listen: %w", err)
		}
	}
	if ln == nil {
		return nil
	}
	srv := &admin.Server{
		Auth: c.cfg.Auth,
		Exec: func(fn func()) {
			done := make(chan struct{})
			c.work <- func() { defer close(done); fn() }
			<-done
		},
		Commands: c.adminCommands,
	}
	slog.Info("core: admin port up", "addr", ln.Addr())
	go srv.Serve(ln)
	go func() { <-ctx.Done(); ln.Close() }()
	return nil
}

// adminCommands collects module specs via the COMMAND event and adds
// the core builtins. Runs on the dispatcher.
func (c *core) adminCommands() []admin.Spec {
	var specs []admin.Spec
	for _, payload := range c.bus.Submit(&bus.Event{Name: "COMMAND", Extra: map[string]any{}}) {
		switch v := payload.(type) {
		case admin.Spec:
			specs = append(specs, v)
		case []admin.Spec:
			specs = append(specs, v...)
		}
	}
	return append(specs, c.builtinSpecs()...)
}

// addChannel puts a channel in the autojoin set (case-insensitive,
// like the ircd) and persists. Reports whether it was new.
func (c *core) addChannel(ch string) bool {
	if slices.ContainsFunc(c.channels, func(have string) bool {
		return strings.EqualFold(have, ch)
	}) {
		return false
	}
	c.channels = append(c.channels, ch)
	c.saveChannels()
	return true
}

// removeChannel drops a channel from the autojoin set and persists.
// Reports whether it was present.
func (c *core) removeChannel(ch string) bool {
	n := len(c.channels)
	c.channels = slices.DeleteFunc(c.channels, func(have string) bool {
		return strings.EqualFold(have, ch)
	})
	if len(c.channels) == n {
		return false
	}
	c.saveChannels()
	return true
}

func (c *core) saveChannels() {
	if err := c.cfg.Store.Put("core", "channels", c.channels); err != nil {
		slog.Error("core: save channels", "err", err)
	}
}

var (
	confRe = regexp.MustCompile(`^(?i)conf(?:\s+([^=\s]+)\s*(?:=\s*(.+))?)?$`)
	joinRe = regexp.MustCompile(`^(?i)join\s+(\S+)$`)
	partRe = regexp.MustCompile(`^(?i)part\s+(\S+)$`)
)

func (c *core) builtinSpecs() []admin.Spec {
	return []admin.Spec{
		{
			Name:  "conf",
			Match: confRe,
			Help:  "Change or display config setting. No args for conf dump.",
			Args:  []string{"<setting>", "<newvalue>"},
			Su:    true,
			Run: func(_, line string) string {
				g := confRe.FindStringSubmatch(line)
				switch {
				case g[1] == "":
					var b strings.Builder
					dump := c.conf.Dump()
					for _, name := range c.conf.List() {
						fmt.Fprintf(&b, "{y}%s{/} = %s\n", name, dump[name])
					}
					return b.String()
				case g[2] == "":
					if v, ok := c.conf.Dump()[g[1]]; ok {
						return fmt.Sprintf("{y}%s{/} = %s", g[1], v)
					}
					return fmt.Sprintf("{r}Error:{/} no such setting %q", g[1])
				default:
					if err := c.conf.Set(g[1], g[2]); err != nil {
						return fmt.Sprintf("{r}Error:{/} %v", err)
					}
					return fmt.Sprintf("{g}%s = %s{/}", g[1], g[2])
				}
			},
		},
		{
			Name:  "join",
			Match: joinRe,
			Help:  "Join a channel and add it to the autojoin set",
			Args:  []string{"<channel>"},
			Su:    true,
			Run: func(_, line string) string {
				ch := joinRe.FindStringSubmatch(line)[1]
				added := c.addChannel(ch)
				if c.session != nil {
					c.session.JoinChannels([]string{ch})
				}
				if !added {
					return fmt.Sprintf("{y}%s{/} was already in the autojoin set, join sent anyway.", ch)
				}
				return fmt.Sprintf("{g}Joining %s{/} (persisted).", ch)
			},
		},
		{
			Name:  "part",
			Match: partRe,
			Help:  "Leave a channel and drop it from the autojoin set",
			Args:  []string{"<channel>"},
			Su:    true,
			Run: func(_, line string) string {
				ch := partRe.FindStringSubmatch(line)[1]
				if !c.removeChannel(ch) {
					return fmt.Sprintf("{r}Error:{/} %s is not in the autojoin set.", ch)
				}
				if c.session != nil && c.conn != nil {
					c.conn.Write("PART " + ch)
				}
				return fmt.Sprintf("{g}Left %s{/} (removed from autojoin).", ch)
			},
		},
		{
			Name:  "status",
			Match: regexp.MustCompile(`^status$`),
			Help:  "Connection and module status",
			Run: func(_, _ string) string {
				var b strings.Builder
				if c.session != nil {
					fmt.Fprintf(&b, "Connected to {c}%s{/} as {g}%s{/}, channels: %s\n",
						c.cfg.Network, c.cfg.Nick, strings.Join(c.session.Channels(), " "))
				} else {
					b.WriteString("{r}Not connected.{/}\n")
				}
				fmt.Fprintf(&b, "Modules with hooks: %s\n", strings.Join(c.bus.Modules(), " "))
				return b.String()
			},
		},
		{
			Name:  "callstats",
			Match: regexp.MustCompile(`^callstats$`),
			Help:  "Per-hook call timing stats",
			Run: func(_, _ string) string {
				var b strings.Builder
				stats := c.bus.Stats()
				ids := make([]bus.HookID, 0, len(stats))
				for id := range stats {
					ids = append(ids, id)
				}
				slices.SortFunc(ids, func(a, b bus.HookID) int {
					if a.Module != b.Module {
						return strings.Compare(a.Module, b.Module)
					}
					return strings.Compare(a.Event, b.Event)
				})
				for _, id := range ids {
					cs := stats[id]
					fmt.Fprintf(&b, "{y}%s{/}/{c}%s{/}: %d calls, min %s avg %s max %s\n",
						id.Module, id.Event, cs.Count,
						time.Duration(cs.Min), time.Duration(cs.Total/max(cs.Count, 1)), time.Duration(cs.Max))
				}
				if b.Len() == 0 {
					return "No calls recorded yet."
				}
				return b.String()
			},
		},
	}
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
	c.sch.After(c.cfg.JoinDelay, func() {
		if c.session == sess { // still this connection
			sess.JoinChannels(c.channels)
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
		// IRC_SENT lets the logger see the bot's own lines. A separate
		// event, NOT an IRC_PRIVMSG with SenderMe: modules keep the perl
		// behavior of never seeing bot output (markov must not learn its
		// own lines). Nested Submit is fine, chain tracking is per
		// (module,event); an IRC_SENT hook calling privmsg again is
		// refused by that same tracking rather than recursing.
		c.bus.Submit(&bus.Event{
			Name: "IRC_SENT", BotNick: c.cfg.Nick, Server: c.cfg.Network,
			Sender: bus.Sender{Nick: c.cfg.Nick}, SenderMe: true,
			Channel: receiver, Msg: part, Extra: map[string]any{},
		})
	}
}

// shutdown lets modules say goodbye and, unless running under a keeper,
// sends the IRC QUIT. Under a keeper the session must survive a core
// restart, so the QUIT is suppressed and the connection just closes;
// the keeper keeps the IRC link and the next core resumes.
func (c *core) shutdown() {
	if c.conn == nil {
		return
	}
	c.bus.Submit(&bus.Event{Name: "QUIT", Extra: map[string]any{}})
	if !c.cfg.SkipGoodbye {
		c.conn.Write("QUIT :" + quitMsgs[rand.IntN(len(quitMsgs))])
		time.Sleep(1500 * time.Millisecond)
	}
	c.conn.Close()
}
