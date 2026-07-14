// Package core assembles the bot: one dispatcher goroutine owning the
// bus, scheduler, config, storage, command registry, pager, and the
// IRC session, with the connection transport and fetcher feeding work
// back in through a channel. The Go counterpart of botje.pl's select
// loop plus IRC.pm's connection management.
//
// The package is split by concern: core.go holds Config, the core
// struct, Run, and the dispatcher loop; transport.go the IRC
// connection lifecycle and the outbound privmsg pipeline; builtins.go
// the telnet admin port and its builtin commands; metrics.go the
// Prometheus collectors and endpoint.
package core

import (
	"context"
	"log/slog"
	"net"
	"slices"
	"time"

	"go-botje/internal/auth"
	"go-botje/internal/bus"
	"go-botje/internal/conf"
	"go-botje/internal/fetch"
	"go-botje/internal/irc"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/irc/pager"
	"go-botje/internal/metrics"
	"go-botje/internal/module"
	"go-botje/internal/sched"
	"go-botje/internal/storage"
)

// Config is what standalone mode needs to run one network.
type Config struct {
	Network string
	Addr    string // host:port
	TLS     bool
	// CertFile/KeyFile: TLS client cert for standalone mode (under a
	// keeper the keeper presents it)
	CertFile, KeyFile string
	Nick              string
	Channels          []string
	Store             storage.Store
	Modules           []module.Module
	Dial              func() (net.Conn, error) // test hook; nil dials Addr
	// SaveInterval is the saver flush cadence; 0 means one minute.
	SaveInterval time.Duration

	// Admin is the telnet control port. AdminListener wins over
	// AdminAddr (tests); both empty means no admin port.
	Auth          *auth.Auth
	AdminAddr     string
	AdminListener net.Listener

	// SkipGoodbye suppresses the QUIT on shutdown. Set when running
	// under a keeper: a core restart must leave the IRC session up, so
	// the keeper (not the core) owns the real goodbye.
	SkipGoodbye bool

	// Metrics, when set, exposes a Prometheus endpoint on MetricsAddr.
	Metrics     *metrics.Registry
	MetricsAddr string
}

// ircEvents is every event name the session can emit.
var ircEvents = []string{
	"IRC_ERROR", "IRC_PRIVMSG", "IRC_NOTICE", "IRC_MODE", "IRC_JOIN",
	"IRC_KICK", "IRC_PART", "IRC_INVITE", "IRC_QUIT", "IRC_TOPIC",
	"IRC_SENT", // outbound privmsg, one event per line; logger food
	"config_changed", "QUIT", "COMMAND",
}

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
	saver   *storage.Saver

	// channels is the persisted autojoin set (storage core/channels).
	// The flags only seed it on the very first boot; after that telnet
	// join/part and invites manage it.
	channels []string
}

// Run connects and dispatches until ctx is cancelled, reconnecting
// with backoff on connection loss. It returns after the goodbye QUIT
// has been flushed.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Metrics != nil {
		// every storage operation (core and modules alike) reports its
		// latency; this is the evidence for the markov blob decision
		reg := cfg.Metrics
		cfg.Store = storage.Instrument(cfg.Store, func(op, ns string, seconds float64) {
			labels := map[string]string{"op": op, "ns": ns}
			reg.AddCounter("botje_storage_op_seconds_sum", labels, seconds)
			reg.IncCounter("botje_storage_op_seconds_count", labels)
		})
	}
	if cfg.SaveInterval == 0 {
		cfg.SaveInterval = time.Minute
	}
	c := &core{
		cfg:  cfg,
		work: make(chan func(), 256),
		sch:  sched.New(time.Now),
		bus:  bus.New(),
		conf: conf.New(),
		cmds: cmd.New(),
	}
	c.saver = storage.NewSaver(cfg.Store,
		func(fn func()) { c.work <- fn },
		func(err error) { slog.Error("core: saver", "err", err) })
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
	if cfg.Metrics != nil {
		reg := cfg.Metrics
		fetcher.Observe = func(host string, seconds float64, isErr bool) {
			labels := map[string]string{"host": host}
			reg.AddCounter("botje_fetch_duration_seconds_sum", labels, seconds)
			reg.IncCounter("botje_fetch_duration_seconds_count", labels)
			if isErr {
				reg.IncCounter("botje_fetch_errors_total", labels)
			}
		}
	}
	mctx := &module.Context{
		Bus: c.bus, Cmd: c.cmds, Pager: c.pager, Conf: c.conf,
		Store: cfg.Store, Saver: c.saver, Sched: c.sch, Fetch: fetcher,
		Metrics:   cfg.Metrics,
		Privmsg:   c.privmsg,
		SendRaw:   c.sendRaw,
		InChannel: c.inChannel,
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
	c.startMetrics(ctx)

	// the saver flush heartbeat; the final synchronous flush below
	// catches whatever is dirty at shutdown
	var flushLoop func()
	flushLoop = func() {
		c.saver.Flush()
		c.sch.After(c.cfg.SaveInterval, flushLoop)
	}
	c.sch.After(c.cfg.SaveInterval, flushLoop)

	c.connect()
	c.loop(ctx)
	c.shutdown()
	if err := c.saver.FlushSync(); err != nil {
		slog.Error("core: final saver flush", "err", err)
	}
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
