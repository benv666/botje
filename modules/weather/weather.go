// Package weather is the regen/weer module. Not a port: the Perl never
// had weather. Four faces:
//
//   - !weer [plaats]: current conditions. Inside the Netherlands from
//     the nearest buienradar station with a thermometer (some stations
//     only measure wind); anywhere else, or when the nearest station is
//     over maxStationKm away, from open-meteo. Reporting Barcelona from
//     Maastricht, 1090km up the road, is how the first version shipped.
//   - !regen [plaats]: the next two hours of precipitation as a
//     sparkline. Benelux from buienradar raintext (5-minute steps, log
//     scale: mm/h = 10^((value-109)/32)), elsewhere from open-meteo's
//     15-minute precipitation, rendered through the same math.
//   - !weeralarm: every active code geel/oranje/rood, from the
//     meteoalarm CAP feed. Active warnings for a place's province are
//     also appended to its !weer line, and new ones for conf
//     weather_warn_areas are broadcast to the report channels.
//   - a daily report at conf weather_report_time to conf
//     weather_report_channels (empty = off).
//
// The default place is conf weather_home. Geocoding (open-meteo, no
// key) is cached in storage forever: villages rarely move.
//
// Warning source: KNMI's own public warnings RSS is dead (frozen on
// storm Ciarán, October 2023) and their Open Data API needs a
// registered key, so this reads meteoalarm, which carries the same KNMI
// warnings for the Netherlands.
package weather

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/fetch"
	"go-botje/internal/format"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
)

const (
	geoURL   = "https://geocoding-api.open-meteo.com/v1/search"
	feedURL  = "https://data.buienradar.nl/2.0/feed/json"
	rainURL  = "https://gpsgadget.buienradar.nl/data/raintext"
	meteoURL = "https://api.open-meteo.com/v1/forecast"
	warnURL  = "https://feeds.meteoalarm.org/feeds/meteoalarm-legacy-atom-netherlands"

	feedTTL = 5 * time.Minute
	rainTTL = 3 * time.Minute
	warnTTL = 10 * time.Minute
	// warnPoll is how often the warning feed is checked for the
	// broadcast; the code geel/oranje/rood alarm BenV cares about.
	warnPoll = 15 * time.Minute
	// maxStationKm: beyond this the nearest buienradar station says
	// nothing about the place (Barcelona was reported from Maastricht,
	// 1090km away, live on 2026-07-13). Fall back to open-meteo.
	maxStationKm = 50
)

type rainEntry struct {
	pts []rainPoint
	at  time.Time
}

type geo struct {
	Name    string  `json:"name"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	Country string  `json:"country"` // ISO code, "NL" for the buienradar path
	Area    string  `json:"area"`    // province, matched against warning areas
}

type station struct {
	Name        string   `json:"stationname"`
	Regio       string   `json:"regio"`
	Lat         float64  `json:"lat"`
	Lon         float64  `json:"lon"`
	Temperature *float64 `json:"temperature"`
	FeelsLike   *float64 `json:"feeltemperature"`
	Description string   `json:"weatherdescription"`
	WindDir     string   `json:"winddirection"`
	WindBft     *float64 `json:"windspeedBft"`
	Humidity    *float64 `json:"humidity"`
	RainLastHr  *float64 `json:"rainFallLastHour"`
}

type feed struct {
	Actual struct {
		Stations []station `json:"stationmeasurements"`
	} `json:"actual"`
	Forecast struct {
		Report struct {
			Title string `json:"title"`
		} `json:"weatherreport"`
		FiveDay []struct {
			MinTemp     string  `json:"mintemperature"`
			MaxTemp     string  `json:"maxtemperature"`
			RainChance  float64 `json:"rainChance"`
			Description string  `json:"weatherdescription"`
			WindBft     float64 `json:"wind"`
			WindDir     string  `json:"windDirection"`
		} `json:"fivedayforecast"`
	} `json:"forecast"`
}

type rainPoint struct {
	Value int
	Time  string // HH:MM
}

// Module implements module.Module.
type Module struct {
	Now func() time.Time // injectable for tests

	ctx          *module.Context
	fetch        func(url string, opts fetch.Options, cb func(fetch.Result)) bool
	geoCache     map[string]geo
	rainCache    map[string]rainEntry
	lastFeed     *feed
	feedAt       time.Time
	warnings     []warning
	warnAt       time.Time
	seenWarnings map[string]time.Time // broadcast already, by warning id
	reportGen    int
	unloaded     bool
}

// New returns an unloaded weather module.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "weather" }

func (m *Module) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Module) Load(ctx *module.Context) error {
	m.ctx = ctx
	if m.fetch == nil {
		m.fetch = ctx.Fetch.Fetch
	}
	ctx.Conf.CreateString("weather_home", "Hauwert")
	ctx.Conf.CreateString("weather_report_time", "07:00")
	ctx.Conf.CreateString("weather_report_channels", "")
	// areas to broadcast code geel/oranje/rood for; meteoalarm names
	// them by province ("Noord-Holland") or coastal region
	ctx.Conf.CreateString("weather_warn_areas", "")

	m.geoCache = make(map[string]geo)
	m.rainCache = make(map[string]rainEntry)
	m.seenWarnings = make(map[string]time.Time)
	if _, err := ctx.Store.Get(m.Name(), "seen_warnings", &m.seenWarnings); err != nil {
		return fmt.Errorf("weather: load seen warnings: %w", err)
	}
	ctx.Cmd.Register(m.Name(), "weer", m.cbWeer)
	ctx.Cmd.Register(m.Name(), "regen", m.cbRegen)
	ctx.Cmd.Register(m.Name(), "weeralarm", m.cbWeeralarm)
	if err := ctx.Bus.RegisterHook(m.Name(), "config_changed", m.onConfChanged); err != nil {
		return err
	}
	m.scheduleReport()
	// warm the warning cache right away: !weer reads it, and a fresh
	// core would otherwise be blind to code geel for a whole poll
	// interval. Re-broadcasting is prevented by the persisted seen set,
	// not by staying quiet.
	m.checkWarnings()
	m.pollWarnings()
	return nil
}

func (m *Module) Unload() error {
	m.unloaded = true
	m.ctx.Bus.UnregisterModule(m.Name())
	m.ctx.Cmd.UnregisterModule(m.Name())
	return nil
}

// resolve turns a place name into coordinates: RAM cache, then the
// storage cache, then a geocoding fetch. cb runs on the dispatcher
// either way; a failed lookup calls cb with a zero geo.
func (m *Module) resolve(place string, cb func(g geo, ok bool)) {
	key := strings.ToLower(strings.TrimSpace(place))
	if key == "" {
		cb(geo{}, false)
		return
	}
	if g, ok := m.geoCache[key]; ok {
		cb(g, true)
		return
	}
	var g geo
	if found, err := m.ctx.Store.Get(m.Name(), "geo "+key, &g); err == nil && found {
		m.geoCache[key] = g
		cb(g, true)
		return
	}
	u := geoURL + "?name=" + url.QueryEscape(key) + "&count=1&language=nl&format=json"
	m.fetch(u, fetch.Options{}, func(res fetch.Result) {
		var out struct {
			Results []struct {
				Name    string  `json:"name"`
				Lat     float64 `json:"latitude"`
				Lon     float64 `json:"longitude"`
				Country string  `json:"country_code"`
				Admin1  string  `json:"admin1"`
			} `json:"results"`
		}
		if res.Err != nil || json.Unmarshal(res.Body, &out) != nil || len(out.Results) == 0 {
			cb(geo{}, false)
			return
		}
		r := out.Results[0]
		g := geo{Name: r.Name, Lat: r.Lat, Lon: r.Lon, Country: r.Country, Area: r.Admin1}
		m.geoCache[key] = g
		if err := m.ctx.Store.Put(m.Name(), "geo "+key, g); err != nil {
			// cache miss next boot, nothing worse
			_ = err
		}
		cb(g, true)
	})
}

// withFeed hands cb the buienradar feed, cached for a few minutes.
func (m *Module) withFeed(cb func(f *feed, ok bool)) {
	if m.lastFeed != nil && m.now().Sub(m.feedAt) < feedTTL {
		cb(m.lastFeed, true)
		return
	}
	m.fetch(feedURL, fetch.Options{}, func(res fetch.Result) {
		f := &feed{}
		if res.Err != nil || json.Unmarshal(res.Body, f) != nil || len(f.Actual.Stations) == 0 {
			cb(nil, false)
			return
		}
		m.lastFeed, m.feedAt = f, m.now()
		cb(f, true)
	})
}

const usage = "Gebruik: !weer [plaats] voor het actuele weer, !regen [plaats] voor de komende twee uur neerslag. Bijv: !weer alkmaar. Zonder plaats: %s."

func (m *Module) usage() string {
	return fmt.Sprintf(usage, m.ctx.Conf.String("weather_home"))
}

// place resolves the command argument; help-ish arguments ("?", "help")
// return wantHelp because users absolutely will type "!regen ?".
func (m *Module) place(arg string) (place string, wantHelp bool) {
	arg = strings.TrimSpace(arg)
	switch strings.ToLower(arg) {
	case "?", "help", "hulp":
		return "", true
	case "":
		return m.ctx.Conf.String("weather_home"), false
	}
	return arg, false
}

func (m *Module) cbWeer(d *cmd.Data) bool {
	channel := d.Event.Channel
	place, wantHelp := m.place(d.Data)
	if wantHelp {
		m.ctx.Privmsg(channel, m.usage())
		return true
	}
	m.resolve(place, func(g geo, ok bool) {
		if !ok {
			m.ctx.Privmsg(channel, fmt.Sprintf("Ken ik niet: %s. %s", place, m.usage()))
			return
		}
		if g.Country != "NL" {
			m.openMeteoWeer(channel, g) // no dutch station will do
			return
		}
		m.withFeed(func(f *feed, ok bool) {
			if !ok {
				m.ctx.Privmsg(channel, "Buienradar doet het even niet.")
				return
			}
			st, km := nearestStation(f.Actual.Stations, g)
			if st == nil || km > maxStationKm {
				m.openMeteoWeer(channel, g) // nearest station is too far to mean anything
				return
			}
			m.ctx.Privmsg(channel, weerLine(g.Name, st, km)+m.warnSuffix(g))
		})
	})
	return true
}

// openMeteoWeer reports current conditions from open-meteo: anything
// outside the buienradar station network (abroad, or a Dutch spot with
// no station within maxStationKm).
func (m *Module) openMeteoWeer(channel string, g geo) {
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
			m.ctx.Privmsg(channel, "Het weer is even zoek.")
			return
		}
		c := out.Current
		var b strings.Builder
		fmt.Fprintf(&b, "{B}{b}%s{/}: %s", g.Name, temp(c.Temp))
		if c.Feels != c.Temp {
			fmt.Fprintf(&b, " (voelt als %s)", temp(c.Feels))
		}
		fmt.Fprintf(&b, ", %s, wind %s %s, %s vochtig",
			wmoText(c.Code), windDir(degToDir(c.WindDeg)), windBft(msToBft(c.WindMS)), humidity(c.Humidity))
		m.ctx.Privmsg(channel, b.String()+m.warnSuffix(g))
	})
}

// degToDir turns a wind bearing into the Dutch compass points the rest
// of the module speaks.
func degToDir(deg float64) string {
	dirs := []string{"N", "NNO", "NO", "ONO", "O", "OZO", "ZO", "ZZO", "Z", "ZZW", "ZW", "WZW", "W", "WNW", "NW", "NNW"}
	i := int(math.Round(deg/22.5)) % len(dirs)
	if i < 0 {
		i += len(dirs)
	}
	return dirs[i]
}

// msToBft is the Beaufort scale from m/s (the classic thresholds).
func msToBft(ms float64) float64 {
	for bft, upper := range []float64{0.3, 1.6, 3.4, 5.5, 8.0, 10.8, 13.9, 17.2, 20.8, 24.5, 28.5, 32.7} {
		if ms < upper {
			return float64(bft)
		}
	}
	return 12
}

// wmoText renders an open-meteo WMO weather code in Dutch.
func wmoText(code int) string {
	switch {
	case code == 0:
		return "onbewolkt"
	case code <= 2:
		return "licht bewolkt"
	case code == 3:
		return "bewolkt"
	case code <= 48:
		return "mistig"
	case code <= 57:
		return "motregen"
	case code <= 67:
		return "regen"
	case code <= 77:
		return "sneeuw"
	case code <= 82:
		return "buien"
	case code <= 86:
		return "sneeuwbuien"
	case code <= 99:
		return "onweer"
	}
	return "onbekend weer"
}

func (m *Module) cbRegen(d *cmd.Data) bool {
	channel := d.Event.Channel
	place, wantHelp := m.place(d.Data)
	if wantHelp {
		m.ctx.Privmsg(channel, m.usage())
		return true
	}
	m.resolve(place, func(g geo, ok bool) {
		if !ok {
			m.ctx.Privmsg(channel, fmt.Sprintf("Ken ik niet: %s. %s", place, m.usage()))
			return
		}
		if !benelux(g) {
			m.openMeteoRegen(channel, g) // raintext only covers the benelux
			return
		}
		m.withRain(g, func(pts []rainPoint, ok bool) {
			if !ok {
				m.ctx.Privmsg(channel, "Buienradar doet het even niet.")
				return
			}
			m.ctx.Privmsg(channel, regenLine(g.Name, pts))
		})
	})
	return true
}

// benelux reports whether buienradar's raintext covers this spot.
func benelux(g geo) bool {
	switch g.Country {
	case "NL", "BE", "LU":
		return true
	}
	return false
}

// openMeteoRegen is !regen abroad: 15-minute precipitation for the next
// two hours (raintext is Benelux-only).
func (m *Module) openMeteoRegen(channel string, g geo) {
	u := fmt.Sprintf("%s?latitude=%.4f&longitude=%.4f&minutely_15=precipitation&forecast_minutely_15=8&timezone=auto",
		meteoURL, g.Lat, g.Lon)
	m.fetch(u, fetch.Options{}, func(res fetch.Result) {
		var out struct {
			Minutely struct {
				Time          []string  `json:"time"`
				Precipitation []float64 `json:"precipitation"`
			} `json:"minutely_15"`
		}
		if res.Err != nil || json.Unmarshal(res.Body, &out) != nil || len(out.Minutely.Time) == 0 {
			m.ctx.Privmsg(channel, "Het weer is even zoek.")
			return
		}
		// reuse the buienradar rendering: mm/h back onto the raintext
		// log scale, so one regenLine serves both sources
		pts := make([]rainPoint, 0, len(out.Minutely.Time))
		for i, t := range out.Minutely.Time {
			mm := 0.0
			if i < len(out.Minutely.Precipitation) {
				mm = out.Minutely.Precipitation[i] * 4 // 15-min total -> mm/h
			}
			pts = append(pts, rainPoint{Value: mmToRaintext(mm), Time: clockOf(t)})
		}
		m.ctx.Privmsg(channel, regenLine(g.Name, pts))
	})
}

// mmToRaintext is the inverse of mmPerHour, so open-meteo values render
// through the same sparkline and peak math.
func mmToRaintext(mm float64) int {
	if mm <= 0 {
		return 0
	}
	return int(math.Round(109 + 32*math.Log10(mm)))
}

// clockOf takes HH:MM out of an ISO timestamp.
func clockOf(iso string) string {
	if _, hm, ok := strings.Cut(iso, "T"); ok && len(hm) >= 5 {
		return hm[:5]
	}
	return iso
}

// withRain hands cb the raintext points for a location, cached a few
// minutes per rounded coordinate so command spam does not hammer
// buienradar (the data only refreshes every 5 minutes anyway).
func (m *Module) withRain(g geo, cb func(pts []rainPoint, ok bool)) {
	key := fmt.Sprintf("%.2f,%.2f", g.Lat, g.Lon)
	if c, ok := m.rainCache[key]; ok && m.now().Sub(c.at) < rainTTL {
		cb(c.pts, true)
		return
	}
	u := fmt.Sprintf("%s?lat=%.2f&lon=%.2f", rainURL, g.Lat, g.Lon)
	m.fetch(u, fetch.Options{}, func(res fetch.Result) {
		pts := parseRain(res.Body)
		if res.Err != nil || len(pts) == 0 {
			cb(nil, false)
			return
		}
		for k, c := range m.rainCache { // keep the cache bounded
			if m.now().Sub(c.at) >= rainTTL {
				delete(m.rainCache, k)
			}
		}
		m.rainCache[key] = rainEntry{pts: pts, at: m.now()}
		cb(pts, true)
	})
}

// nearestStation prefers stations that measure temperature (a handful
// only do wind) and returns the distance in km.
func nearestStation(stations []station, g geo) (*station, int) {
	var best *station
	bestD := math.MaxFloat64
	for pass := range 2 { // pass 0: with thermometer; pass 1: anything
		for i := range stations {
			st := &stations[i]
			if pass == 0 && st.Temperature == nil {
				continue
			}
			if d := distKm(g.Lat, g.Lon, st.Lat, st.Lon); d < bestD {
				best, bestD = st, d
			}
		}
		if best != nil {
			break
		}
	}
	return best, int(math.Round(bestD))
}

// distKm is the flat-earth approximation: good enough within NL.
func distKm(lat1, lon1, lat2, lon2 float64) float64 {
	dy := (lat2 - lat1) * 110.57
	dx := (lon2 - lon1) * 111.32 * math.Cos((lat1+lat2)/2*math.Pi/180)
	return math.Sqrt(dx*dx + dy*dy)
}

// tempColor picks a tag by temperature: cold blues to hot red. Every
// use closes with {/}: an unclosed tag paints the rest of the line.
func tempColor(t float64) string {
	switch {
	case t < 0:
		return "{C}"
	case t < 10:
		return "{c}"
	case t < 18:
		return "{g}"
	case t < 25:
		return "{y}"
	default:
		return "{R}"
	}
}

func temp(t float64) string {
	return fmt.Sprintf("%s%.1f°C{/}", tempColor(t), t)
}

// colorTempStr colors a temperature the feed hands us as a string
// (the fivedayforecast values); non-numbers pass through unstyled.
func colorTempStr(s string) string {
	var v float64
	if _, err := fmt.Sscanf(s, "%f", &v); err != nil {
		return s
	}
	return tempColor(v) + s + "{/}"
}

// windDir colors a Dutch compass direction by what it brings: noord
// cold blue, zuid warm red, oost dry continental cyan, west mild sea
// green. The first letter is the dominant component.
func windDir(dir string) string {
	if dir == "" {
		return dir
	}
	tag := ""
	switch dir[0] {
	case 'N', 'n':
		tag = "{c}"
	case 'Z', 'z', 'S', 's':
		tag = "{R}"
	case 'O', 'o', 'E', 'e':
		tag = "{C}"
	case 'W', 'w':
		tag = "{g}"
	default:
		return dir
	}
	return tag + strings.ToUpper(dir) + "{/}"
}

// windBft colors wind force: calm green, stiff orange, storm red.
func windBft(bft float64) string {
	tag := "{g}"
	switch {
	case bft >= 7:
		tag = "{R}"
	case bft >= 4:
		tag = "{y}"
	}
	return fmt.Sprintf("%s%.0fBft{/}", tag, bft)
}

// humidity colors relative humidity: dry orange, comfortable green,
// clammy cyan.
func humidity(h float64) string {
	tag := "{g}"
	switch {
	case h < 40:
		tag = "{y}"
	case h > 75:
		tag = "{c}"
	}
	return fmt.Sprintf("%s%.0f%%{/}", tag, h)
}

// rainChance colors the odds: dry green, maybe orange, bring a coat
// cyan.
func rainChance(pct float64) string {
	tag := "{g}"
	switch {
	case pct > 60:
		tag = "{c}"
	case pct > 30:
		tag = "{y}"
	}
	return fmt.Sprintf("%s%.0f%%{/}", tag, pct)
}

func weerLine(place string, st *station, km int) string {
	name := strings.TrimPrefix(st.Name, "Meetstation ")
	var b strings.Builder
	fmt.Fprintf(&b, "{B}{b}%s{/} (meetstation %s, %dkm):", place, name, km)
	if st.Temperature != nil {
		fmt.Fprintf(&b, " %s", temp(*st.Temperature))
		if st.FeelsLike != nil && *st.FeelsLike != *st.Temperature {
			fmt.Fprintf(&b, " (voelt als %s)", temp(*st.FeelsLike))
		}
	}
	if st.Description != "" {
		fmt.Fprintf(&b, ", %s", strings.ToLower(st.Description[:1])+st.Description[1:])
	}
	if st.WindBft != nil {
		fmt.Fprintf(&b, ", wind %s %s", windDir(st.WindDir), windBft(*st.WindBft))
	}
	if st.Humidity != nil {
		fmt.Fprintf(&b, ", %s vochtig", humidity(*st.Humidity))
	}
	if st.RainLastHr != nil && *st.RainLastHr > 0 {
		fmt.Fprintf(&b, ", {c}%.1fmm{/} regen in het laatste uur", *st.RainLastHr)
	}
	return b.String()
}

// parseRain reads the raintext body: one "VVV|HH:MM" per line.
func parseRain(body []byte) []rainPoint {
	var pts []rainPoint
	for line := range strings.SplitSeq(strings.TrimSpace(string(body)), "\n") {
		val, at, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok {
			continue
		}
		var v int
		if _, err := fmt.Sscanf(val, "%d", &v); err != nil {
			continue
		}
		pts = append(pts, rainPoint{Value: v, Time: at})
	}
	return pts
}

// mmPerHour converts a raintext value (log scale) to mm/h.
func mmPerHour(v int) float64 {
	if v <= 0 {
		return 0
	}
	return math.Pow(10, float64(v-109)/32)
}

func regenLine(place string, pts []rainPoint) string {
	firstWet, peak := -1, 0
	peakAt := ""
	values := make([]float64, len(pts))
	for i, p := range pts {
		values[i] = float64(p.Value)
		if p.Value > 0 && firstWet == -1 {
			firstWet = i
		}
		if p.Value > peak {
			peak, peakAt = p.Value, p.Time
		}
	}
	last := pts[len(pts)-1].Time
	if firstWet == -1 {
		return fmt.Sprintf("{B}{b}%s{/}: droog tot zeker %s.", place, last)
	}
	spark := format.Sparkline(values, 1)[0]
	when := "nu regen"
	if firstWet > 0 {
		when = "regen vanaf " + pts[firstWet].Time
	}
	return fmt.Sprintf("{B}{b}%s{/}: {C}%s{/} (tot %s), %s, piek {C}%.1fmm/u{/} om %s",
		place, spark, last, when, mmPerHour(peak), peakAt)
}

// --- warnings (code geel/oranje/rood) ---

// warning is one active meteoalarm alert.
type warning struct {
	ID      string
	Area    string
	Event   string
	Level   string // geel, oranje, rood
	Expires time.Time
}

// warnColor tags a warning by level.
func warnColor(level string) string {
	switch level {
	case "rood":
		return "{R}"
	case "oranje":
		return "{y}"
	default:
		return "{Y}"
	}
}

func (w warning) String() string {
	return fmt.Sprintf("%scode %s{/}: %s (%s)", warnColor(w.Level), w.Level, w.Event, w.Area)
}

// parseWarnings reads the meteoalarm legacy atom feed. Levels come from
// cap:severity (Moderate/Severe/Extreme = geel/oranje/rood); events are
// the English cap:event, translated where it matters.
func parseWarnings(body []byte, now time.Time) []warning {
	var feed struct {
		Entries []struct {
			Area     string `xml:"areaDesc"`
			Event    string `xml:"event"`
			Severity string `xml:"severity"`
			Expires  string `xml:"expires"`
			ID       string `xml:"identifier"`
		} `xml:"entry"`
	}
	if xml.Unmarshal(body, &feed) != nil {
		return nil
	}
	var out []warning
	for _, e := range feed.Entries {
		level := ""
		switch e.Severity {
		case "Moderate":
			level = "geel"
		case "Severe":
			level = "oranje"
		case "Extreme":
			level = "rood"
		default:
			continue // Minor/Unknown: not a code
		}
		exp, err := time.Parse(time.RFC3339, e.Expires)
		if err == nil && exp.Before(now) {
			continue // stale
		}
		out = append(out, warning{
			ID: e.ID, Area: e.Area, Event: warnEvent(e.Event), Level: level, Expires: exp,
		})
	}
	return out
}

// warnEvent translates the meteoalarm event names to Dutch.
func warnEvent(event string) string {
	e := strings.ToLower(event)
	for eng, nl := range map[string]string{
		"wind": "wind", "thunderstorm": "onweer", "rain": "regen", "snow": "sneeuw",
		"ice": "ijzel", "fog": "mist", "high-temperature": "hitte", "low-temperature": "kou",
		"coastal": "kustweer", "forest": "natuurbrandgevaar", "flood": "overstroming",
		"avalanche": "lawine", "rain-flood": "wateroverlast",
	} {
		if strings.Contains(e, eng) {
			return nl
		}
	}
	return event
}

// withWarnings hands cb the active warnings, cached.
func (m *Module) withWarnings(cb func(ws []warning, ok bool)) {
	if m.warnAt.After(m.now().Add(-warnTTL)) {
		cb(m.warnings, true)
		return
	}
	m.fetch(warnURL, fetch.Options{}, func(res fetch.Result) {
		if res.Err != nil {
			cb(nil, false)
			return
		}
		m.warnings, m.warnAt = parseWarnings(res.Body, m.now()), m.now()
		cb(m.warnings, true)
	})
}

// warnSuffix is the active-warning tail appended to a !weer line. It
// reads the cache only: the poller keeps it fresh, and a weather
// lookup should not wait on a second fetch.
func (m *Module) warnSuffix(g geo) string {
	if g.Area == "" {
		return ""
	}
	for _, w := range m.warnings {
		if strings.EqualFold(w.Area, g.Area) {
			return " | " + w.String()
		}
	}
	return ""
}

// cbWeeralarm lists every active warning.
func (m *Module) cbWeeralarm(d *cmd.Data) bool {
	channel := d.Event.Channel
	m.withWarnings(func(ws []warning, ok bool) {
		if !ok {
			m.ctx.Privmsg(channel, "Meteoalarm doet het even niet.")
			return
		}
		if len(ws) == 0 {
			m.ctx.Privmsg(channel, "Geen waarschuwingen van kracht. {g}Code groen{/}.")
			return
		}
		var lines []string
		for _, w := range ws {
			lines = append(lines, w.String())
		}
		m.ctx.Pager.EventMsg(d.Event, "weeralarm", strings.Join(lines, "\n"))
	})
	return true
}

// pollWarnings refreshes the warnings and broadcasts new ones for the
// configured areas to the report channels. Rearms itself.
func (m *Module) pollWarnings() {
	m.ctx.Sched.After(warnPoll, func() {
		if m.unloaded {
			return
		}
		m.checkWarnings()
		m.pollWarnings()
	})
}

func (m *Module) checkWarnings() {
	areas := splitList(m.ctx.Conf.String("weather_warn_areas"))
	channels := splitList(m.ctx.Conf.String("weather_report_channels"))
	m.warnAt = time.Time{} // force a refresh: this is the poll
	m.withWarnings(func(ws []warning, ok bool) {
		if !ok || len(areas) == 0 || len(channels) == 0 {
			return
		}
		for _, w := range ws {
			if _, seen := m.seenWarnings[w.ID]; seen || !matchesAny(w.Area, areas) {
				continue
			}
			m.seenWarnings[w.ID] = m.now()
			for _, ch := range channels {
				m.ctx.Privmsg(ch, "{B}{b}Weeralarm{/} | "+w.String())
			}
		}
		// forget warnings that expired long ago so the map stays small
		for id, seen := range m.seenWarnings {
			if m.now().Sub(seen) > 7*24*time.Hour {
				delete(m.seenWarnings, id)
			}
		}
		m.ctx.Saver.Mark(m.Name(), "seen_warnings", func() any { return m.seenWarnings })
	})
}

func splitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' })
}

func matchesAny(area string, wanted []string) bool {
	for _, w := range wanted {
		if strings.EqualFold(area, w) {
			return true
		}
	}
	return false
}

// --- daily report ---

func (m *Module) onConfChanged(ev *bus.Event) (bus.Handled, any) {
	if ev.Msg == "weather_report_time" {
		m.scheduleReport()
	}
	return bus.None, nil
}

// scheduleReport (re)arms the daily report timer for the next
// occurrence of weather_report_time. The generation counter makes a
// reschedule orphan any previously armed timer.
func (m *Module) scheduleReport() {
	m.reportGen++
	gen := m.reportGen
	spec := m.ctx.Conf.String("weather_report_time")
	t, err := time.Parse("15:04", spec)
	if err != nil {
		return // no valid time, no report
	}
	now := m.now()
	next := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	m.ctx.Sched.After(next.Sub(now), func() {
		if m.unloaded || gen != m.reportGen {
			return
		}
		m.report()
		m.scheduleReport()
	})
}

// report sends the morning forecast to the configured channels.
func (m *Module) report() {
	channels := strings.FieldsFunc(m.ctx.Conf.String("weather_report_channels"), func(r rune) bool {
		return r == ' ' || r == ','
	})
	if len(channels) == 0 {
		return
	}
	home := m.ctx.Conf.String("weather_home")
	m.withFeed(func(f *feed, ok bool) {
		if !ok || len(f.Forecast.FiveDay) == 0 {
			return // no data, no spam; tomorrow is another day
		}
		day := f.Forecast.FiveDay[0]
		var b strings.Builder
		fmt.Fprintf(&b, "Goedemorgen! Vandaag: %s, %s-%s°C, regenkans %s, wind %s %s.",
			day.Description, colorTempStr(day.MinTemp), colorTempStr(day.MaxTemp),
			rainChance(day.RainChance), windDir(day.WindDir), windBft(day.WindBft))
		if f.Forecast.Report.Title != "" {
			fmt.Fprintf(&b, " %s.", strings.TrimSuffix(f.Forecast.Report.Title, "."))
		}
		line := b.String()
		for _, ch := range channels {
			m.ctx.Privmsg(ch, line)
		}
		// tack on the rain graph when something is coming at home
		m.resolve(home, func(g geo, ok bool) {
			if !ok {
				return
			}
			m.withRain(g, func(pts []rainPoint, ok bool) {
				wet := false
				for _, p := range pts {
					if p.Value > 0 {
						wet = true
						break
					}
				}
				if !ok || !wet {
					return
				}
				line := regenLine(g.Name, pts)
				for _, ch := range channels {
					m.ctx.Privmsg(ch, line)
				}
			})
		})
	})
}
