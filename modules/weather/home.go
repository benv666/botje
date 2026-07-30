package weather

import (
	"fmt"
	"log/slog"
	"strings"

	"go-botje/internal/bus"
)

// Per-nick default place, Bram's request (live 2026-07-16: "zorg eens
// dat die bot rekening houdt met nick oid"). "!weer home=alkmaar" once,
// and every later !weer / !weer full / !regen / !weerdiff <plaats> for
// that nick reads Alkmaar instead of conf weather_home. Junerules has no
// accounts, so the key is just the nick: whoever holds it holds the
// setting. The daily report keeps using weather_home, it belongs to the
// channel and not to a nick.

// homeKey identifies a nick's setting per server.
func homeKey(ev *bus.Event) string {
	return strings.ToLower(ev.Server + " " + ev.Sender.Nick)
}

// homeOf is the caller's default place: their own if they set one, conf
// weather_home otherwise.
func (m *Module) homeOf(ev *bus.Event) string {
	if p := m.homes[homeKey(ev)]; p != "" {
		return p
	}
	return m.ctx.Conf.String("weather_home")
}

// parseHome recognises the home argument in the spellings people
// actually type: "home=alkmaar", "home alkmaar", "thuis = wijk aan
// zee", bare "home" (show) and "home=" (clear). The boundary check
// keeps a place that merely starts with the keyword ("Homerville") out
// of it.
func parseHome(arg string) (place string, set, isHome bool) {
	arg = strings.TrimSpace(arg)
	for _, w := range []string{"home", "thuis"} {
		if len(arg) < len(w) || !strings.EqualFold(arg[:len(w)], w) {
			continue
		}
		rest := strings.TrimSpace(arg[len(w):])
		switch {
		case rest == "" && len(arg) == len(w):
			return "", false, true
		case strings.HasPrefix(rest, "=") || len(arg) > len(w) && arg[len(w)] == ' ':
			return strings.TrimSpace(strings.TrimPrefix(rest, "=")), true, true
		}
	}
	return "", false, false
}

// handleHome answers a home argument, reporting whether it was one so
// the command handlers can stop. Setting resolves the place first: a
// stored place that does not geocode would make every later !weer
// answer "Ken ik niet".
func (m *Module) handleHome(ev *bus.Event, arg string) bool {
	place, set, isHome := parseHome(arg)
	if !isHome {
		return false
	}
	channel, nick := ev.Channel, ev.Sender.Nick
	own := m.homes[homeKey(ev)]
	switch {
	case !set && own == "":
		m.ctx.Privmsg(channel, fmt.Sprintf("Je hebt geen eigen plaats, ik gebruik {B}{b}%s{/}. "+
			"Instellen: !weer home=<plaats>.", m.ctx.Conf.String("weather_home")))
	case !set:
		m.ctx.Privmsg(channel, fmt.Sprintf("Jouw plaats is {B}{b}%s{/}. "+
			"Wijzigen met !weer home=<plaats>, wissen met !weer home=.", own))
	case place == "":
		delete(m.homes, homeKey(ev))
		m.saveHomes()
		m.ctx.Privmsg(channel, fmt.Sprintf("%s: je eigen plaats is gewist, ik gebruik weer {B}{b}%s{/}.",
			nick, m.ctx.Conf.String("weather_home")))
	default:
		m.resolve(place, func(g geo, ok bool) {
			if !ok {
				m.ctx.Privmsg(channel, fmt.Sprintf("Ken ik niet: %s. %s", place, m.usage(ev)))
				return
			}
			// store the geocoded name, not what was typed: it renders
			// properly in later output and resolves to the same cache entry
			m.homes[homeKey(ev)] = g.Name
			m.saveHomes()
			m.ctx.Privmsg(channel, fmt.Sprintf("%s: %s is nu jouw plaats voor !weer, !regen en !weerdiff.",
				nick, placeLabel(g, "")))
		})
	}
	return true
}

func (m *Module) saveHomes() {
	if err := m.ctx.Store.Put(m.Name(), "homes", m.homes); err != nil {
		slog.Error("weather: save homes", "err", err)
	}
}
