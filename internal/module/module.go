// Package module defines what a botje module is: a compiled-in package
// registered with the core, loaded and unloaded at runtime (no dynamic
// loading, by decision). The Perl getModuleInfo/load/unload contract,
// Go-shaped.
package module

import (
	"go-botje/internal/bus"
	"go-botje/internal/conf"
	"go-botje/internal/fetch"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/irc/pager"
	"go-botje/internal/sched"
	"go-botje/internal/storage"
)

// Context is what a module gets at load time. All of it is owned by
// the dispatcher goroutine; modules must not call it from goroutines
// they spawn (route results through Fetch-style delivery instead).
type Context struct {
	Bus   *bus.Bus
	Cmd   *cmd.Registry
	Pager *pager.Pager
	Conf  *conf.Conf
	Store storage.Store
	// Saver batches hot writes: Mark (ns, name) dirty with a value
	// func; the core flushes every minute and at shutdown. Use it
	// instead of Store.Put when saving on every event would hammer the
	// database (markov learns on every channel line).
	Saver *storage.Saver
	Sched *sched.Sched
	Fetch *fetch.Fetcher
	// Privmsg sends directly, bypassing the pager (the Perl
	// cmd_privmsg): newline-split, wrapped, flood-queued, colorized.
	Privmsg func(channel, msg string)
	// SendRaw queues a raw IRC line at high priority, ahead of the
	// flood queue. For oper commands (KICK, MODE +b, GLINE) that must
	// not sit behind chatter during a spam wave. No colorize/wrap: the
	// caller writes a complete, correct IRC command. No-op while
	// disconnected.
	SendRaw func(line string)
	// InChannel reports whether the bot is currently in a channel.
	InChannel func(channel string) bool
}

// Module is one feature module. Name doubles as the storage namespace
// and the bus/cmd registration owner, Perl convention.
type Module interface {
	Name() string
	Load(*Context) error
	Unload() error
}
