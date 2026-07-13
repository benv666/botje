package weather

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/conf"
	"go-botje/internal/fetch"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
	"go-botje/internal/sched"
	"go-botje/internal/storage"
)

const geoHauwert = `{"results":[{"name":"Hauwert","latitude":52.70833,"longitude":5.1,"country_code":"NL","admin1":"Noord-Holland"}]}`
const geoAlkmaar = `{"results":[{"name":"Alkmaar","latitude":52.63,"longitude":4.75,"country_code":"NL","admin1":"Noord-Holland"}]}`
const geoNiks = `{"generationtime_ms":0.1}`

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
	m    *Module
	b    *bus.Bus
	cmds *cmd.Registry
	cf   *conf.Conf
	sch  *sched.Sched
	clk  time.Time
	sent []string
	urls []string // fetched urls, in order
	body map[string]string
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
	f.m.fetch = func(url string, opts fetch.Options, cb func(fetch.Result)) bool {
		f.urls = append(f.urls, url)
		for prefix, body := range f.body {
			if strings.HasPrefix(url, prefix) {
				cb(fetch.Result{URL: url, Status: 200, Body: []byte(body)})
				return true
			}
		}
		cb(fetch.Result{URL: url, Err: fmt.Errorf("no fixture for %s", url)})
		return true
	}
	err := f.m.Load(&module.Context{
		Bus: f.b, Cmd: f.cmds, Conf: f.cf, Store: store, Sched: f.sch,
		Privmsg: func(ch, msg string) { f.sent = append(f.sent, ch+"|"+msg) },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.body[geoURL] = geoHauwert
	f.body[feedURL] = feedJSON
	f.body[rainURL] = rainDry
	return f
}

func (f *fixture) msg(nick, channel, text string) {
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: channel,
		Msg: text, Extra: map[string]any{}}
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
	// 24.6°C is warm = orange, and every color must be reset: an
	// unclosed tag paints the rest of the line navy (live complaint)
	for _, want := range []string{"{B}{b}Hauwert{/}", "Berkhout", "{y}24.6°C{/}", "NNO 3Bft", "vrijwel onbewolkt"} {
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
	f.b.Submit(&bus.Event{Name: "config_changed", Msg: "weather_report_channels", Extra: map[string]any{}})

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
