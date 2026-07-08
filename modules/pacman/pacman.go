// Package pacman replies with ASCII pacman art to messages starting
// with two or more dots (exactly two: 70% chance to let it slide).
// Ported from IRC_Pacman.pm, art verbatim.
package pacman

import (
	"math/rand/v2"
	"regexp"
	"strings"

	"go-botje/internal/bus"
	"go-botje/internal/module"
)

// the five templates, verbatim: %l is the left margin, %s the nick slot
var pacmans = []string{
	"%l /~~\\\n%l( o <  . . . . . . %s\n%l \\__/\n",
	// fancy UTF-8 pac-man: shaded left edge (soft/anti-aliased look),
	// rounded disc, eye, wedge mouth chomping filled dots toward the nick
	"%l░▟▀▙\n%l▓█▝◣  ● ● ● %s\n%l░▜▄▛\n",
	"%l .--.\n%l/ _.-' .-.   .-.  .-.   .''.\n%l\\  '-. '-'   '-'  '-'   '..'\n%l '--'\n",
	"%l .-.   .-.     .--.\n%l| OO| | OO|   / _.-' .-.   .-.  .-.   .''.\n%l|   | |   |   \\  '-. '-'   '-'  '-'   '..'\n%l'^^^' '^^^'    '--'",
	"%l (*<   .   .   .   %s\n",
	"%l   _..._                    _.../|__\n%l .'     `.                .'    `~\\/\n%l:  o   _.-'              '-._   o= :\n%l:     `-._   o  o  o  o   _.-'     :\n%l`.       .'              `.       .'\n%l  `-...-'                  `-...-'\n",
}

var dotsRe = regexp.MustCompile(`^\s*(\.{2,})`)

// Module implements module.Module.
type Module struct {
	// Rand is injectable randomness in [0,1); nil means math/rand.
	Rand func() float64

	ctx *module.Context
}

// New returns an unloaded pacman module.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "pacman" }

func (m *Module) Load(ctx *module.Context) error {
	m.ctx = ctx
	return ctx.Bus.RegisterHook(m.Name(), "IRC_PRIVMSG", m.onPrivmsg)
}

func (m *Module) Unload() error {
	m.ctx.Bus.UnregisterModule(m.Name())
	return nil
}

func (m *Module) rand() float64 {
	if m.Rand != nil {
		return m.Rand()
	}
	return rand.Float64()
}

func (m *Module) onPrivmsg(ev *bus.Event) (bus.Handled, any) {
	if ev.SenderMe || ev.Msg == "" {
		return bus.None, nil
	}
	// not when addressed, not for command attempts
	if strings.HasPrefix(strings.ToLower(ev.Msg), strings.ToLower(ev.BotNick)+":") ||
		strings.HasPrefix(ev.Msg, "!") {
		return bus.None, nil
	}
	g := dotsRe.FindStringSubmatch(ev.Msg)
	if g == nil {
		return bus.None, nil
	}
	if len(g[1]) == 2 && m.rand() > 0.3 {
		return bus.None, nil // muhaha surprise random attack (not this time)
	}

	art := pacmans[int(m.rand()*float64(len(pacmans)))%len(pacmans)]
	art = strings.ReplaceAll(art, "%l", "   ")
	art = strings.ReplaceAll(art, "%s", " "+ev.Sender.Nick)
	m.ctx.Privmsg(ev.Channel, art)
	return bus.Replied, nil
}
