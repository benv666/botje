package weather

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/conf"
	"go-botje/internal/fetch"
	"go-botje/internal/format"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/irc/pager"
	"go-botje/internal/module"
	"go-botje/internal/sched"
	"go-botje/internal/storage"
)

const geoHauwert = `{"results":[{"name":"Hauwert","latitude":52.70833,"longitude":5.1,"country_code":"NL","admin1":"Noord-Holland"}]}`
const geoAlkmaar = `{"results":[{"name":"Alkmaar","latitude":52.63,"longitude":4.75,"country_code":"NL","admin1":"Noord-Holland"}]}`
const geoBarcelona = `{"results":[{"name":"Barcelona","latitude":41.39,"longitude":2.16,"country_code":"ES","admin1":"Catalonia","country":"Spanje"}]}`
const geoNiks = `{"generationtime_ms":0.1}`

// open-meteo current weather for Barcelona; weather_code 3 = bewolkt
const meteoJSON = `{"current":{"time":"2026-07-13T17:45","temperature_2m":29.2,"apparent_temperature":32.8,
 "relative_humidity_2m":66,"wind_speed_10m":2.96,"wind_direction_10m":102,"weather_code":3,"precipitation":0.0}}`

const meteoRainJSON = `{"minutely_15":{"time":["2026-07-13T17:45","2026-07-13T18:00","2026-07-13T18:15"],
 "precipitation":[0.0,0.5,1.2]}}`

// forecast at 12:00 local. The 11:00 hour is HOTTER (27.9) and WETTER
// (95%) than anything still to come: it is history, so it must not be
// reported. What is still ahead: 26.4 at 15:00 and a 70% shower at
// 17:00, sunset 21:58. Tomorrow 15-22 and wet.
const forecastJSON = `{"current":{"time":"2026-07-13T12:00"},
 "hourly":{"time":["2026-07-13T11:00","2026-07-13T13:00","2026-07-13T15:00","2026-07-13T17:00","2026-07-13T21:00","2026-07-14T09:00"],
  "temperature_2m":[27.9,23.0,26.4,24.0,18.0,16.0],
  "precipitation_probability":[95,10,20,70,5,80],
  "weather_code":[80,2,3,80,1,61]},
 "daily":{"time":["2026-07-13","2026-07-14"],
  "sunset":["2026-07-13T21:58","2026-07-14T21:57"],
  "temperature_2m_min":[14.0,15.0],"temperature_2m_max":[26.4,22.0],
  "precipitation_probability_max":[70,80],"weather_code":[80,61]}}`

// one yellow wind warning for Noord-Holland, one orange for Limburg
const warnXML = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom" xmlns:cap="urn:oasis:names:tc:emergency:cap:1.2">
 <entry>
  <cap:areaDesc>Noord-Holland</cap:areaDesc>
  <cap:event>Moderate Wind</cap:event>
  <cap:severity>Moderate</cap:severity>
  <cap:expires>2126-07-14T14:39:29+00:00</cap:expires>
  <cap:identifier>id-nh-wind-1</cap:identifier>
  <title>Yellow Wind Warning issued for The Netherlands - Noord-Holland</title>
 </entry>
 <entry>
  <cap:areaDesc>Limburg</cap:areaDesc>
  <cap:event>Severe Thunderstorm</cap:event>
  <cap:severity>Severe</cap:severity>
  <cap:expires>2126-07-14T14:39:29+00:00</cap:expires>
  <cap:identifier>id-li-onweer-1</cap:identifier>
  <title>Orange Thunderstorm Warning issued for The Netherlands - Limburg</title>
 </entry>
</feed>`

// two stations: Berkhout near Hauwert, De Bilt far; Houtribdijk is
// nearest of all but has no temperature and must be skipped
const feedJSON = `{"actual":{"stationmeasurements":[
 {"stationname":"Meetstation De Bilt","regio":"Utrecht","lat":52.1,"lon":5.18,"temperature":21.5,"feeltemperature":21.0,"weatherdescription":"Zwaar bewolkt","winddirection":"ZW","windspeedBft":4,"humidity":80.0,"rainFallLastHour":0.4},
 {"stationname":"Meetstation Houtribdijk","regio":"Houtribdijk","lat":52.7,"lon":5.1,"weatherdescription":"","winddirection":"W","windspeedBft":5},
 {"stationname":"Meetstation Berkhout","regio":"Berkhout","lat":52.65,"lon":4.98,"temperature":24.6,"feeltemperature":25.1,"weatherdescription":"Vrijwel onbewolkt","winddirection":"NNO","windspeedBft":3,"humidity":39.0,"rainFallLastHour":0.0}
]},"forecast":{"weatherreport":{"title":"Komende dagen hoogzomers"},"fivedayforecast":[
 {"day":"2026-07-13T00:00:00","mintemperature":"15","maxtemperature":"26","rainChance":10,"sunChance":70,"weatherdescription":"Zonnig met wolkjes","wind":3,"windDirection":"no"}
]}}`

const rainDry = "000|17:00\n000|17:05\n000|17:10\n"
const rainWet = "000|17:00\n077|17:05\n141|17:10\n000|17:15\n"

type fixture struct {
	m     *Module
	b     *bus.Bus
	cmds  *cmd.Registry
	cf    *conf.Conf
	sch   *sched.Sched
	saver *storage.Saver
	pg    *pager.Pager
	clk   time.Time
	sent  []string
	urls  []string // fetched urls, in order
	body  map[string]string
}

func newFixture(t *testing.T, store storage.Store) *fixture {
	t.Helper()
	f := &fixture{
		clk:  time.Date(2026, 7, 13, 12, 0, 0, 0, time.Local),
		body: map[string]string{},
	}
	f.b = bus.New()
	f.b.RegisterEvent("IRC_PRIVMSG")
	f.b.RegisterEvent("config_changed")
	f.cmds = cmd.New()
	f.cf = conf.New()
	f.sch = sched.New(func() time.Time { return f.clk })
	f.m = New()
	f.m.Now = func() time.Time { return f.clk }
	// fixtures are keyed by a distinctive substring of the url: the
	// open-meteo endpoints differ only by query (current / minutely_15 /
	// hourly), so a prefix match cannot tell them apart
	f.m.fetch = func(url string, opts fetch.Options, cb func(fetch.Result)) bool {
		f.urls = append(f.urls, url)
		for key, body := range f.body {
			if strings.Contains(url, key) {
				cb(fetch.Result{URL: url, Status: 200, Body: []byte(body)})
				return true
			}
		}
		cb(fetch.Result{URL: url, Err: fmt.Errorf("no fixture for %s", url)})
		return true
	}
	// bodies must exist before Load: the module warms its warning cache
	// there (a fresh core is otherwise blind to code geel for a whole
	// poll interval)
	f.body[geoURL] = geoHauwert
	f.body[feedURL] = feedJSON
	f.body[rainURL] = rainDry
	f.body[warnURL] = warnXML
	f.body["apparent_temperature"] = meteoJSON
	f.body["hourly="] = forecastJSON
	f.pg = pager.New(f.sch, func(ch, msg string) { f.sent = append(f.sent, ch+"|"+msg) })
	f.pg.MaxLines = func() int { return 4 }
	f.saver = storage.NewSaver(store,
		func(fn func()) { fn() },
		func(err error) { t.Errorf("saver: %v", err) })
	err := f.m.Load(&module.Context{
		Bus: f.b, Cmd: f.cmds, Conf: f.cf, Store: store, Sched: f.sch,
		Saver: f.saver, Pager: f.pg,
		Privmsg: func(ch, msg string) { f.sent = append(f.sent, ch+"|"+msg) },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.urls = nil // the load-time warm-up is not what the tests count
	return f
}

func (f *fixture) msg(nick, channel, text string) {
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: channel,
		Msg: text}
	ev.Sender.Nick = nick
	f.b.Submit(ev)
	f.cmds.Handle(ev)
}

func (f *fixture) take() []string {
	s := f.sent
	f.sent = nil
	return s
}

func TestWeerDefaultsToHome(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "!weer")
	got := f.take()
	if len(got) != 1 {
		t.Fatalf("sent = %q", got)
	}
	// every variable gets a relevant color (24.6 warm orange, NNO cold
	// cyan, 3Bft calm green, 39% dry orange), and every color must be
	// reset: an unclosed tag paints the rest of the line navy (live
	// complaint, twice)
	for _, want := range []string{"{B}{b}Hauwert{/}", "Berkhout", "{y}24.6°C{/}", "wind {c}NNO{/} {g}3Bft{/}", "{y}39%{/} vochtig", "vrijwel onbewolkt"} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("weer output missing %q: %q", want, got[0])
		}
	}
	if strings.Contains(got[0], "Houtribdijk") {
		t.Fatalf("picked a station without temperature: %q", got[0])
	}
	// bare {b} is navy paint (the live complaint); {B}{b} is the light
	// blue name highlight and fine
	if strings.Count(got[0], "{b}") != strings.Count(got[0], "{B}{b}") {
		t.Fatalf("navy paint in output: %q", got[0])
	}
}

func TestTempColor(t *testing.T) {
	for _, tc := range []struct {
		temp float64
		want string
	}{{-5, "{C}"}, {5, "{c}"}, {15, "{g}"}, {20, "{y}"}, {30, "{R}"}} {
		if got := tempColor(tc.temp); got != tc.want {
			t.Fatalf("tempColor(%v) = %q, want %q", tc.temp, got, tc.want)
		}
	}
}

func TestValueColors(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{windDir("NNO"), "{c}NNO{/}"}, // noord = koud
		{windDir("zw"), "{R}ZW{/}"},   // zuid = warm
		{windDir("ONO"), "{C}ONO{/}"}, // oost = droog continentaal
		{windDir("W"), "{g}W{/}"},     // west = zacht zeeklimaat
		{windBft(2), "{g}2Bft{/}"},    // kalm
		{windBft(5), "{y}5Bft{/}"},    // stevig
		{windBft(9), "{R}9Bft{/}"},    // storm
		{humidity(30), "{y}30%{/}"},   // droog
		{humidity(55), "{g}55%{/}"},   // prima
		{humidity(90), "{c}90%{/}"},   // klam
		{rainChance(10), "{g}10%{/}"}, // droog
		{rainChance(45), "{y}45%{/}"}, // misschien
		{rainChance(80), "{c}80%{/}"}, // jas mee
	} {
		if tc.got != tc.want {
			t.Fatalf("colored value = %q, want %q", tc.got, tc.want)
		}
	}
}

// The Barcelona bug (live, 2026-07-13): a foreign place reported the
// nearest DUTCH station (Maastricht, 1090km) as if it were the local
// weather. Abroad must come from open-meteo instead.
func TestForeignPlaceUsesOpenMeteo(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.body[geoURL] = geoBarcelona
	f.msg("BenV", "#testing", "!weer barcelona")
	got := f.take()
	if len(got) != 1 {
		t.Fatalf("sent = %q", got)
	}
	if strings.Contains(got[0], "Maastricht") || strings.Contains(got[0], "meetstation") {
		t.Fatalf("foreign place reported from a dutch station: %q", got[0])
	}
	// real Barcelona numbers, plus the wind direction converted from degrees
	for _, want := range []string{"Barcelona", "29.2°C", "32.8°C", "66%", "bewolkt"} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("open-meteo output missing %q: %q", want, got[0])
		}
	}
}

// Planet trolling (live 2026-07-15: !weer venus/pluto/jupiter answered
// from US villages as if they were the planets, !weer wouter from a
// Belgian hamlet): a foreign reply says where the resolved place is
// and which source supplied the numbers.
func TestForeignPlaceLabeled(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.body[geoURL] = geoBarcelona
	f.msg("BenV", "#testing", "!weer barcelona")
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "{B}{b}Barcelona{/} (Catalonia, Spanje, open-meteo)") {
		t.Fatalf("foreign weer not labeled with region/country/source: %q", got)
	}
}

// A geo cached before the country-name field has Country but no
// CountryName: foreign entries get re-geocoded once so the label can
// name the country.
func TestStaleForeignGeoCacheRefreshed(t *testing.T) {
	store := storage.NewMemory()
	if err := store.Put("weather", "geo barcelona", map[string]any{
		"name": "Barcelona", "lat": 41.39, "lon": 2.16,
		"country": "ES", "area": "Catalonia", // no country_name
	}); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, store)
	f.body[geoURL] = geoBarcelona
	f.msg("BenV", "#testing", "!weer barcelona")
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "Spanje") {
		t.Fatalf("stale foreign cache entry not refreshed: %q", got)
	}
}

// Same trap for rain: buienradar's raintext only covers the Benelux.
func TestForeignRainUsesOpenMeteo(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.body[geoURL] = geoBarcelona
	f.body["minutely_15"] = meteoRainJSON
	f.msg("BenV", "#testing", "!regen barcelona")
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "Barcelona") {
		t.Fatalf("foreign regen = %q", got)
	}
	if strings.Contains(strings.Join(f.urls, " "), rainURL) {
		t.Fatalf("used buienradar raintext for a foreign place: %v", f.urls)
	}
	if !strings.Contains(got[0], "18:00") { // rain starts at the second point
		t.Fatalf("foreign regen missing the rain start: %q", got[0])
	}
	if !strings.Contains(got[0], "open-meteo") {
		t.Fatalf("foreign regen not source-labeled: %q", got[0])
	}
}

// A dutch place too far from any station is still nonsense; the guard
// is the distance, not just the country.
func TestFarDutchPlaceFallsBack(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	// a "NL" place way out in the North Sea: no station within 50km
	f.body[geoURL] = `{"results":[{"name":"Doggersbank","latitude":54.9,"longitude":3.0,"country_code":"NL","admin1":""}]}`
	f.msg("BenV", "#testing", "!weer doggersbank")
	got := f.take()
	if len(got) != 1 || strings.Contains(got[0], "meetstation") {
		t.Fatalf("far dutch place still used a station: %q", got)
	}
	if !strings.Contains(got[0], "(open-meteo)") {
		t.Fatalf("station-less reply not source-labeled: %q", got[0])
	}
}

// Code geel/oranje/rood: an active warning for the place's area shows
// up in !weer, and !weeralarm lists everything active.
func TestWarningInWeerAndList(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "!weer") // Hauwert = Noord-Holland
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "code geel") || !strings.Contains(got[0], "wind") {
		t.Fatalf("weer output lacks the yellow warning: %q", got)
	}

	f.msg("BenV", "#testing", "!weeralarm")
	got = f.take()
	all := strings.Join(got, "\n")
	if !strings.Contains(all, "Noord-Holland") || !strings.Contains(all, "Limburg") ||
		!strings.Contains(all, "code geel") || !strings.Contains(all, "code oranje") {
		t.Fatalf("weeralarm list = %q", got)
	}
}

// All severities map to their code and color on the wire: Moderate =
// geel (mIRC 08), Severe = oranje (07), Extreme = rood (04). Minor is
// not a code and is dropped.
func TestWarningSeverityLevelsAndColors(t *testing.T) {
	feed := `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom" xmlns:cap="urn:oasis:names:tc:emergency:cap:1.2">
 <entry><cap:areaDesc>Groningen</cap:areaDesc><cap:event>fog</cap:event><cap:severity>Minor</cap:severity><cap:expires>2126-07-14T14:39:29+00:00</cap:expires><cap:identifier>id-min</cap:identifier></entry>
 <entry><cap:areaDesc>Noord-Holland</cap:areaDesc><cap:event>Moderate Wind</cap:event><cap:severity>Moderate</cap:severity><cap:expires>2126-07-14T14:39:29+00:00</cap:expires><cap:identifier>id-mod</cap:identifier></entry>
 <entry><cap:areaDesc>Limburg</cap:areaDesc><cap:event>Severe Thunderstorm</cap:event><cap:severity>Severe</cap:severity><cap:expires>2126-07-14T14:39:29+00:00</cap:expires><cap:identifier>id-sev</cap:identifier></entry>
 <entry><cap:areaDesc>Zeeland</cap:areaDesc><cap:event>Extreme Wind</cap:event><cap:severity>Extreme</cap:severity><cap:expires>2126-07-14T14:39:29+00:00</cap:expires><cap:identifier>id-ext</cap:identifier></entry>
</feed>`
	ws := parseWarnings([]byte(feed), time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC))
	if len(ws) != 3 {
		t.Fatalf("want 3 warnings (Minor dropped), got %d: %v", len(ws), ws)
	}
	wire := map[string]string{ // level -> mIRC color code
		"geel": "\x0308", "oranje": "\x0307", "rood": "\x0304",
	}
	for i, want := range []string{"geel", "oranje", "rood"} {
		if ws[i].Level != want {
			t.Fatalf("warning %d level = %q, want %q", i, ws[i].Level, want)
		}
		irc := format.ToIRC(ws[i].String())
		if !strings.Contains(irc, wire[want]+"code "+want) {
			t.Fatalf("code %s not colored %q on the wire: %q", want, wire[want], irc)
		}
	}
}

// When a province has more than one active warning, !weer shows the
// most severe one, not whichever the feed lists first.
func TestWeerShowsMostSevereWarning(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.body[warnURL] = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom" xmlns:cap="urn:oasis:names:tc:emergency:cap:1.2">
 <entry><cap:areaDesc>Noord-Holland</cap:areaDesc><cap:event>fog</cap:event><cap:severity>Moderate</cap:severity><cap:expires>2126-07-14T14:39:29+00:00</cap:expires><cap:identifier>id-nh-mist</cap:identifier></entry>
 <entry><cap:areaDesc>Noord-Holland</cap:areaDesc><cap:event>Extreme Wind</cap:event><cap:severity>Extreme</cap:severity><cap:expires>2126-07-14T14:39:29+00:00</cap:expires><cap:identifier>id-nh-storm</cap:identifier></entry>
</feed>`
	f.m.warnAt = time.Time{}                // drop the cache Load warmed with the default fixture
	f.msg("BenV", "#testing", "!weeralarm") // refill via withWarnings
	f.take()
	f.msg("BenV", "#testing", "!weer") // Hauwert = Noord-Holland
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "code rood") {
		t.Fatalf("weer did not show the most severe warning: %q", got)
	}
	if strings.Contains(got[0], "code geel") {
		t.Fatalf("weer shows the milder warning too: %q", got)
	}
}

// New warnings for the configured areas are broadcast once, not on
// every poll.
func TestWarningBroadcastOnce(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.cf.Set("weather_report_channels", "#testing")
	f.cf.Set("weather_warn_areas", "Noord-Holland")

	f.clk = f.clk.Add(warnPoll + time.Second)
	f.sch.RunDue()
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "code geel") || !strings.Contains(got[0], "Noord-Holland") {
		t.Fatalf("first broadcast = %q", got)
	}
	if strings.Contains(got[0], "Limburg") {
		t.Fatalf("broadcast an area we did not ask for: %q", got[0])
	}

	f.clk = f.clk.Add(warnPoll + time.Second)
	f.sch.RunDue()
	if got := f.take(); len(got) != 0 {
		t.Fatalf("same warning broadcast twice: %q", got)
	}
}

// !weer full: the rest of today, only the bits still ahead. At 12:00
// the 11:00 hour is history and must not appear; the 26.4 peak at
// 15:00, the 70% shower at 17:00, and sunset do.
func TestWeerFullRestOfDay(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "!weer full")
	got := f.take()
	if len(got) != 1 {
		t.Fatalf("sent = %q", got)
	}
	for _, want := range []string{"Hauwert", "vandaag nog", "26", "15:00", "70%", "17:00", "21:58", "Morgen", "22"} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("full forecast missing %q: %q", want, got[0])
		}
	}
	// the 11:00 hour was hotter and wetter, but it already happened
	for _, gone := range []string{"27.9", "95%", "11:00"} {
		if strings.Contains(got[0], gone) {
			t.Fatalf("full forecast reports the hour already past (%s): %q", gone, got[0])
		}
	}
}

// Late in the evening there is no day left: skip today, lead with
// tomorrow ("only the bits that are still relevant").
func TestWeerFullLateEveningSkipsToday(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.body["hourly="] = strings.Replace(forecastJSON,
		`"current":{"time":"2026-07-13T12:00"}`, `"current":{"time":"2026-07-13T23:30"}`, 1)
	f.msg("BenV", "#testing", "!weer full")
	got := f.take()
	if len(got) != 1 || strings.Contains(got[0], "vandaag") {
		t.Fatalf("late evening still reports today: %q", got)
	}
	if !strings.Contains(got[0], "Morgen") || !strings.Contains(got[0], "22") {
		t.Fatalf("late evening lacks tomorrow: %q", got)
	}
}

// "!weer full <plaats>" works for a place too, and abroad.
func TestWeerFullWithPlace(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.body[geoURL] = geoBarcelona
	f.msg("BenV", "#testing", "!weer full barcelona")
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "Barcelona") || !strings.Contains(got[0], "Morgen") {
		t.Fatalf("full forecast for a place = %q", got)
	}
}

// Trolls will type "!regen ?" and "!weer help"; they get usage, and an
// unknown place explains itself instead of a bare "ken ik niet".
func TestHelpAndUnknownPlaceExplain(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	for _, line := range []string{"!weer ?", "!regen help", "!weer atlantis"} {
		if line == "!weer atlantis" {
			f.body[geoURL] = geoNiks
		}
		f.msg("BenV", "#testing", line)
		got := f.take()
		if len(got) != 1 || !strings.Contains(got[0], "!weer [plaats]") {
			t.Fatalf("%s reply lacks usage: %q", line, got)
		}
	}
}

// raintext is cached per location so command spam does not hammer
// buienradar (the feed already had its own cache).
func TestRainCached(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "!regen")
	f.msg("BenV", "#testing", "!regen")
	rainCalls := 0
	for _, u := range f.urls {
		if strings.HasPrefix(u, rainURL) {
			rainCalls++
		}
	}
	if rainCalls != 1 {
		t.Fatalf("raintext fetched %d times, want 1 (cache)", rainCalls)
	}
	// cache expires
	f.clk = f.clk.Add(10 * time.Minute)
	f.msg("BenV", "#testing", "!regen")
	rainCalls = 0
	for _, u := range f.urls {
		if strings.HasPrefix(u, rainURL) {
			rainCalls++
		}
	}
	if rainCalls != 2 {
		t.Fatalf("raintext fetched %d times after TTL, want 2", rainCalls)
	}
}

func TestWeerPlaceArgument(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.body[geoURL] = geoAlkmaar
	f.msg("BenV", "#testing", "!weer alkmaar")
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "Alkmaar") {
		t.Fatalf("weer alkmaar = %q", got)
	}
	if !strings.Contains(strings.Join(f.urls, " "), "name=alkmaar") {
		t.Fatalf("geocode url missing place: %v", f.urls)
	}
}

func TestGeocodeCached(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	f.msg("BenV", "#testing", "!weer")
	f.msg("BenV", "#testing", "!weer")
	geoCalls := 0
	for _, u := range f.urls {
		if strings.HasPrefix(u, geoURL) {
			geoCalls++
		}
	}
	if geoCalls != 1 {
		t.Fatalf("geocode fetched %d times, want 1 (cache)", geoCalls)
	}
	// and the cache survives a reload through storage
	f2 := newFixture(t, store)
	f2.msg("BenV", "#testing", "!weer")
	for _, u := range f2.urls {
		if strings.HasPrefix(u, geoURL) {
			t.Fatalf("geocode fetched after reload: %v", f2.urls)
		}
	}
}

// A geo cached by the pre-country version has no Country/Area, which
// made every cached place look foreign (Hauwert reported from
// open-meteo instead of its station, live 2026-07-13). Stale entries
// must be re-geocoded.
func TestStaleGeoCacheRefreshed(t *testing.T) {
	store := storage.NewMemory()
	if err := store.Put("weather", "geo hauwert", map[string]any{
		"name": "Hauwert", "lat": 52.70833, "lon": 5.1, // no country, no area
	}); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, store)
	f.msg("BenV", "#testing", "!weer")
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "meetstation Berkhout") {
		t.Fatalf("stale cache entry not refreshed: %q", got)
	}
	var g geo
	if _, err := store.Get("weather", "geo hauwert", &g); err != nil || g.Country != "NL" {
		t.Fatalf("stored geo not rewritten: %+v (%v)", g, err)
	}
}

func TestWeerUnknownPlace(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.body[geoURL] = geoNiks
	f.msg("BenV", "#testing", "!weer atlantis")
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "atlantis") {
		t.Fatalf("unknown place reply = %q", got)
	}
}

func TestRegenDry(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.msg("BenV", "#testing", "!regen")
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "droog") || !strings.Contains(got[0], "17:10") {
		t.Fatalf("dry regen = %q", got)
	}
}

func TestRegenWet(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.body[rainURL] = rainWet
	f.msg("BenV", "#testing", "!regen")
	got := f.take()
	if len(got) != 1 {
		t.Fatalf("wet regen = %q", got)
	}
	// 141 -> 10^((141-109)/32) = 10 mm/u peak at 17:10, rain from 17:05
	for _, want := range []string{"17:05", "10.0", "17:10", "{/}"} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("wet regen missing %q: %q", want, got[0])
		}
	}
}

func TestDailyReport(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.cf.Set("weather_report_channels", "#testing #rss")
	f.b.Submit(&bus.Event{Name: "config_changed", Msg: "weather_report_channels"})

	// clock starts at 12:00; the report is due at 07:00 tomorrow
	f.clk = f.clk.Add(19 * time.Hour) // 07:00 next day
	f.sch.RunDue()
	got := f.take()
	if len(got) != 2 {
		t.Fatalf("report sent to %d channels: %q", len(got), got)
	}
	for _, want := range []string{"#testing|", "15", "26", "Zonnig met wolkjes", "hoogzomers"} {
		if !strings.Contains(strings.Join(got, "\n"), want) {
			t.Fatalf("report missing %q: %q", want, got)
		}
	}
	// rearmed for the day after
	f.take()
	f.clk = f.clk.Add(24 * time.Hour)
	f.sch.RunDue()
	if got := f.take(); len(got) != 2 {
		t.Fatalf("report did not rearm: %q", got)
	}
}

func TestNoReportWithoutChannels(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.clk = f.clk.Add(19 * time.Hour)
	f.sch.RunDue()
	if got := f.take(); len(got) != 0 {
		t.Fatalf("report sent with empty channel conf: %q", got)
	}
}

func TestMmPerHour(t *testing.T) {
	for _, tc := range []struct {
		v    int
		want float64
	}{{0, 0}, {77, 0.1}, {109, 1.0}, {141, 10.0}, {255, 36307.8}} {
		got := mmPerHour(tc.v)
		if diff := got - tc.want; diff > 0.1*tc.want+0.001 || diff < -0.1*tc.want-0.001 {
			t.Fatalf("mmPerHour(%d) = %v, want ~%v", tc.v, got, tc.want)
		}
	}
}
