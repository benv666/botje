package core

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"

	"go-botje/internal/admin"
	"go-botje/internal/bus"
)

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
	if c.cfg.Metrics != nil {
		srv.Metric = c.cfg.Metrics.IncCounter
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
	for _, payload := range c.bus.Submit(&bus.Event{Name: "COMMAND"}) {
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
