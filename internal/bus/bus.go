// Package bus is the event bus: the Go counterpart of the Perl
// ModuleLoader dispatch. One dispatcher goroutine owns all module event
// dispatch (Run); module handlers run synchronously one at a time, so
// module state needs no locks. Semantics carried over from Perl:
// one hook per (module, event) pair, return values collected, handler
// code 2 stops propagation, callchain tracking refuses re-entry of the
// same (module, event) already on the chain, a panicking handler gets
// its module force-unloaded, per-hook call timing stats.
package bus

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"
)

// Handled is the Perl handler return protocol.
type Handled int

const (
	None    Handled = 0 // nothing, or handled silently
	Replied Handled = 1 // handled and produced a message (payload = importance or data)
	Stop    Handled = 2 // handled, stop propagation to later hooks
)

// Sender is who sent an IRC event.
type Sender struct {
	Nick, User, Host string
	UserID           string // set by the auth nick handler, empty otherwise
}

// RawCmd is the raw IRC command behind an event. Params is the unparsed
// params string, like the Perl rawcmd; split it with irc.SplitParams.
type RawCmd struct {
	Prefix string
	Cmd    string
	Params string
}

// Event is the Perl event hashref as a struct. Modules written against
// the Perl shape port 1:1.
type Event struct {
	Name     string // PRIVMSG, JOIN, COMMAND, config_changed, ...
	BotNick  string
	Server   string
	Sender   Sender
	SenderMe bool   // sender is the bot
	Channel  string // channel, or sender nick for queries
	TargetMe bool
	Query    bool // private message to the bot
	Msg      string
	Raw      RawCmd
	Extra    map[string]any // topic, mode, reason, target, netsplit, ...
}

// Handler is a module hook. The payload (second return) is collected by
// Submit when non-nil: importance for IRC replies, command specs for the
// COMMAND event.
type Handler func(ev *Event) (Handled, any)

// HookID keys call stats.
type HookID struct {
	Module string
	Event  string
}

// CallStats is per-hook call timing, in nanoseconds.
type CallStats struct {
	Count           int64
	Min, Max, Total int64
}

type hook struct {
	module string
	fn     Handler
}

// Bus is the event bus. RegisterEvent/RegisterHook/Submit must only be
// called from the dispatcher goroutine (or before Run starts); Publish is
// safe from any goroutine.
type Bus struct {
	// OnPanic, when set, is called after a handler panic with the module
	// name, event name, and recovered value, right after the module's
	// hooks have been removed.
	OnPanic func(module, event string, v any)

	events map[string][]hook // declared event -> hooks in registration order
	chain  map[HookID]bool   // (module,event) currently on the call chain
	stats  map[HookID]CallStats
	queue  chan *Event
	now    func() time.Time
}

// New returns an empty bus.
func New() *Bus {
	return &Bus{
		events: make(map[string][]hook),
		chain:  make(map[HookID]bool),
		stats:  make(map[HookID]CallStats),
		queue:  make(chan *Event, 256),
		now:    time.Now,
	}
}

// RegisterEvent declares an event name. Hooks can only be registered on
// declared events.
func (b *Bus) RegisterEvent(name string) {
	if _, ok := b.events[name]; !ok {
		b.events[name] = nil
	}
}

// RegisterHook registers module's hook for event, replacing any previous
// hook for the same (module, event) pair. Errors on undeclared events.
func (b *Bus) RegisterHook(module, event string, h Handler) error {
	hooks, ok := b.events[event]
	if !ok {
		return fmt.Errorf("bus: event %q not declared", event)
	}
	for i := range hooks {
		if hooks[i].module == module {
			hooks[i].fn = h
			return nil
		}
	}
	b.events[event] = append(hooks, hook{module: module, fn: h})
	return nil
}

// UnregisterModule removes all hooks of module.
func (b *Bus) UnregisterModule(module string) {
	for event, hooks := range b.events {
		b.events[event] = slices.DeleteFunc(hooks, func(h hook) bool {
			return h.module == module
		})
	}
}

// Modules lists modules that currently have hooks registered.
func (b *Bus) Modules() []string {
	var out []string
	for _, hooks := range b.events {
		for _, h := range hooks {
			if !slices.Contains(out, h.module) {
				out = append(out, h.module)
			}
		}
	}
	slices.Sort(out)
	return out
}

// Submit dispatches ev synchronously to every hook registered for
// ev.Name, in registration order, and returns the collected non-nil
// payloads. Dispatcher goroutine only.
func (b *Bus) Submit(ev *Event) []any {
	var collected []any
	// iterate over a copy: hooks may register/unregister during dispatch
	for _, h := range slices.Clone(b.events[ev.Name]) {
		id := HookID{Module: h.module, Event: ev.Name}
		if b.chain[id] {
			continue // refuse re-entry of the same (module,event)
		}
		code, payload, panicked := b.call(id, h, ev)
		if panicked {
			continue
		}
		if payload != nil {
			collected = append(collected, payload)
		}
		if code == Stop {
			break
		}
	}
	return collected
}

// call runs one hook with chain tracking, timing, and panic recovery.
func (b *Bus) call(id HookID, h hook, ev *Event) (code Handled, payload any, panicked bool) {
	b.chain[id] = true
	start := b.now()
	defer func() {
		delete(b.chain, id)
		b.record(id, b.now().Sub(start).Nanoseconds())
		if r := recover(); r != nil {
			panicked = true
			slog.Error("bus: handler panicked, force-unloading module",
				"module", id.Module, "event", id.Event, "panic", r)
			b.UnregisterModule(id.Module)
			if b.OnPanic != nil {
				b.OnPanic(id.Module, id.Event, r)
			}
		}
	}()
	code, payload = h.fn(ev)
	return code, payload, false
}

func (b *Bus) record(id HookID, ns int64) {
	cs := b.stats[id]
	if cs.Count == 0 || ns < cs.Min {
		cs.Min = ns
	}
	cs.Max = max(cs.Max, ns)
	cs.Total += ns
	cs.Count++
	b.stats[id] = cs
}

// Publish enqueues ev for dispatch by the Run loop. Safe from any
// goroutine.
func (b *Bus) Publish(ev *Event) {
	b.queue <- ev
}

// Run is the dispatcher loop: dispatches published events until ctx is
// cancelled.
func (b *Bus) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-b.queue:
			b.Submit(ev)
		}
	}
}

// Stats returns per-hook call timing stats.
func (b *Bus) Stats() map[HookID]CallStats {
	return maps.Clone(b.stats)
}
