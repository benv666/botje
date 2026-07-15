package weather

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"go-botje/internal/fetch"
	"go-botje/internal/irc/cmd"
)

// conditions is one place's current weather, whichever source supplied
// it (buienradar station or open-meteo); !weerdiff compares two of
// these. Nil fields were not measured (some stations skip humidity or
// wind) and their pair is left out of the diff. src names the supplier:
// two places on the same station give identical numbers, which without
// attribution reads as a bug ("geloof er niks van", live 2026-07-15).
type conditions struct {
	temp, feels, humidity, windBft *float64
	windDir                        string
	desc                           string
	src                            string
}

// withConditions fetches a resolved place's current conditions via the
// same station-or-open-meteo decision as !weer.
func (m *Module) withConditions(g geo, cb func(c conditions, ok bool)) {
	if g.Country != "NL" {
		m.meteoConditions(g, cb)
		return
	}
	m.withFeed(func(f *feed, ok bool) {
		if ok {
			if st, km := nearestStation(f.Actual.Stations, g); st != nil && km <= maxStationKm {
				cb(conditions{
					temp: st.Temperature, feels: st.FeelsLike,
					humidity: st.Humidity, windBft: st.WindBft,
					windDir: st.WindDir, desc: strings.ToLower(st.Description),
					src: fmt.Sprintf("meetstation %s, %dkm",
						strings.TrimPrefix(st.Name, "Meetstation "), km),
				}, true)
				return
			}
		}
		m.meteoConditions(g, cb) // no station in range: same fallback as !weer
	})
}

// meteoConditions fetches open-meteo current conditions.
func (m *Module) meteoConditions(g geo, cb func(c conditions, ok bool)) {
	u := fmt.Sprintf("%s?latitude=%.4f&longitude=%.4f&current=temperature_2m,apparent_temperature,"+
		"relative_humidity_2m,wind_speed_10m,wind_direction_10m,weather_code&wind_speed_unit=ms&timezone=auto",
		meteoURL, g.Lat, g.Lon)
	m.fetch(u, fetch.Options{}, func(res fetch.Result) {
		var out struct {
			Current struct {
				Temp     float64 `json:"temperature_2m"`
				Feels    float64 `json:"apparent_temperature"`
				Humidity float64 `json:"relative_humidity_2m"`
				WindMS   float64 `json:"wind_speed_10m"`
				WindDeg  float64 `json:"wind_direction_10m"`
				Code     int     `json:"weather_code"`
			} `json:"current"`
		}
		if res.Err != nil || json.Unmarshal(res.Body, &out) != nil {
			cb(conditions{}, false)
			return
		}
		c := out.Current
		bft := msToBft(c.WindMS)
		cb(conditions{
			temp: &c.Temp, feels: &c.Feels, humidity: &c.Humidity, windBft: &bft,
			windDir: degToDir(c.WindDeg), desc: wmoText(c.Code),
			src: "open-meteo",
		}, true)
	})
}

// splitPlaces splits the !weerdiff argument in two: a comma or " vs "
// separates multi-word places, exactly two words split on the space,
// and a single word compares against home.
func splitPlaces(arg, home string) (a, b string, ok bool) {
	for _, sep := range []string{",", " vs "} {
		if l, r, found := strings.Cut(arg, sep); found {
			l, r = strings.TrimSpace(l), strings.TrimSpace(r)
			return l, r, l != "" && r != ""
		}
	}
	switch fields := strings.Fields(arg); len(fields) {
	case 1:
		return home, fields[0], true
	case 2:
		return fields[0], fields[1], true
	}
	return "", "", false
}

// cbWeerdiff is Bram's "!weerdiff hauwert castricum" (2026-07-14):
// current conditions of two places side by side, with a verdict.
func (m *Module) cbWeerdiff(d *cmd.Data) bool {
	channel := d.Event.Channel
	arg, wantHelp := strings.TrimSpace(d.Data), false
	switch strings.ToLower(arg) {
	case "", "?", "help", "hulp":
		wantHelp = true
	}
	a, b, ok := "", "", false
	if !wantHelp {
		a, b, ok = splitPlaces(arg, m.ctx.Conf.String("weather_home"))
	}
	if wantHelp || !ok {
		m.ctx.Privmsg(channel, diffUsage)
		return true
	}
	m.resolve(a, func(ga geo, ok bool) {
		if !ok {
			m.ctx.Privmsg(channel, fmt.Sprintf("Ken ik niet: %s. %s", a, diffUsage))
			return
		}
		m.resolve(b, func(gb geo, ok bool) {
			if !ok {
				m.ctx.Privmsg(channel, fmt.Sprintf("Ken ik niet: %s. %s", b, diffUsage))
				return
			}
			m.withConditions(ga, func(ca conditions, ok bool) {
				if !ok {
					m.ctx.Privmsg(channel, "Het weer is even zoek.")
					return
				}
				m.withConditions(gb, func(cb conditions, ok bool) {
					if !ok {
						m.ctx.Privmsg(channel, "Het weer is even zoek.")
						return
					}
					m.ctx.Privmsg(channel, diffLine(ga, ca, gb, cb))
				})
			})
		})
	})
	return true
}

const diffUsage = "Gebruik: !weerdiff <plaats1> <plaats2> (komma voor namen met spaties: " +
	"!weerdiff wijk aan zee, den haag). Een plaats vergelijkt met thuis."

// diffLine renders the side-by-side: only pairs both sides measured.
// Each side is labeled with its source (same station twice = identical
// numbers, and that must be visible).
func diffLine(ga geo, a conditions, gb geo, b conditions) string {
	nameA, nameB := ga.Name, gb.Name
	var parts []string
	if a.temp != nil && b.temp != nil {
		parts = append(parts, fmt.Sprintf("%s vs %s", temp(*a.temp), temp(*b.temp)))
	}
	offTemp := func(c conditions) bool { return c.temp != nil && c.feels != nil && *c.feels != *c.temp }
	if a.feels != nil && b.feels != nil && (offTemp(a) || offTemp(b)) {
		parts = append(parts, fmt.Sprintf("voelt als %s vs %s", temp(*a.feels), temp(*b.feels)))
	}
	if a.humidity != nil && b.humidity != nil {
		parts = append(parts, fmt.Sprintf("%s vs %s vochtig", humidity(*a.humidity), humidity(*b.humidity)))
	}
	if a.windBft != nil && b.windBft != nil {
		parts = append(parts, fmt.Sprintf("wind %s %s vs %s %s",
			windDir(a.windDir), windBft(*a.windBft), windDir(b.windDir), windBft(*b.windBft)))
	}
	if a.desc != "" && b.desc != "" {
		if strings.EqualFold(a.desc, b.desc) {
			parts = append(parts, "allebei "+a.desc)
		} else {
			parts = append(parts, a.desc+" vs "+b.desc)
		}
	}
	line := fmt.Sprintf("%s vs %s: %s",
		placeLabel(ga, a.src), placeLabel(gb, b.src), strings.Join(parts, ", "))
	if a.temp != nil && b.temp != nil {
		line += " | " + verdict(nameA, *a.temp, nameB, *b.temp)
	}
	return line
}

// verdict names the warmer place; within a rounding step it is a draw.
func verdict(nameA string, ta float64, nameB string, tb float64) string {
	d := ta - tb
	switch {
	case math.Abs(d) < 0.05:
		return "{g}precies even warm{/}"
	case d > 0:
		return fmt.Sprintf("{y}%s is %.1f°C warmer{/}", nameA, d)
	default:
		return fmt.Sprintf("{y}%s is %.1f°C warmer{/}", nameB, -d)
	}
}
