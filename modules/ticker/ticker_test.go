package ticker

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

type fixture struct {
	m       *Module
	b       *bus.Bus
	cmds    *cmd.Registry
	cf      *conf.Conf
	sch     *sched.Sched
	clk     time.Time
	sent    []string
	fetched []string
	cbs     map[string]func(fetch.Result)
}

func newFixture(t *testing.T, store storage.Store) *fixture {
	t.Helper()
	f := &fixture{clk: time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
		cbs: map[string]func(fetch.Result){}}
	f.b = bus.New()
	f.b.RegisterEvent("IRC_PRIVMSG")
	f.cmds = cmd.New()
	f.cf = conf.New()
	f.sch = sched.New(func() time.Time { return f.clk })
	f.m = New()
	f.m.Now = func() time.Time { return f.clk }
	f.m.Rand = func() float64 { return 0 } // restore offset 0
	f.m.fetch = func(url string, opts fetch.Options, cb func(fetch.Result)) bool {
		if _, dup := f.cbs[url]; dup {
			return false
		}
		f.fetched = append(f.fetched, url)
		f.cbs[url] = cb
		return true
	}
	err := f.m.Load(&module.Context{
		Bus: f.b, Cmd: f.cmds, Conf: f.cf, Store: store, Sched: f.sch,
		Privmsg: func(ch, msg string) {
			for l := range strings.SplitSeq(msg, "\n") {
				f.sent = append(f.sent, ch+"|"+l)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) ticker(nick, data string) {
	msg := "!ticker"
	if data != "" {
		msg += " " + data
	}
	f.raw(nick, msg)
}

func (f *fixture) raw(nick, msg string) {
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: "#testing",
		Msg: msg, Extra: map[string]any{}}
	ev.Sender.Nick = nick
	f.b.Submit(ev)
	f.cmds.Handle(ev)
}

func (f *fixture) complete(t *testing.T, url, body string) {
	t.Helper()
	cb, ok := f.cbs[url]
	if !ok {
		t.Fatalf("no fetch in flight for %q (have %q)", url, f.fetched)
	}
	delete(f.cbs, url)
	cb(fetch.Result{URL: url, Status: 200, Body: []byte(body)})
}

func (f *fixture) advance(d time.Duration) {
	f.clk = f.clk.Add(d)
	f.sch.RunDue()
}

func (f *fixture) take() []string {
	s := f.sent
	f.sent = nil
	return s
}

const btcURL = "https://blockchain.info/ticker"

func btcJSON(last float64) string {
	return fmt.Sprintf(`{"EUR": {"symbol": "€", "buy": %.2f, "sell": %.2f, "15m": %.2f, "last": %.2f}}`,
		last+10, last-10, last, last)
}

// feed pumps one BTC price into the module (subscribing first if needed).
func (f *fixture) feedBTC(t *testing.T, price float64) {
	t.Helper()
	if _, ok := f.m.tickers["BTC"]; !ok {
		f.ticker("BenV", "add BTC")
	} else {
		f.advance(30 * time.Minute)
	}
	f.complete(t, btcURL, btcJSON(price))
}

func TestAddBTCBroadcasts(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.ticker("BenV", "add BTC")
	if len(f.fetched) != 1 || f.fetched[0] != btcURL {
		t.Fatalf("fetched = %q", f.fetched)
	}
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|BenV: Subscription added. It will be sent to #testing" {
		t.Fatalf("reply = %q", got)
	}
	f.complete(t, btcURL, btcJSON(95000))
	got = f.take()
	if len(got) == 0 {
		t.Fatal("no broadcast after first fetch")
	}
	head := got[0]
	if !strings.HasPrefix(head, "#testing|{y}BTC{/} just now: ") {
		t.Fatalf("broadcast head = %q", head)
	}
	if !strings.Contains(head, "{w}€{/}({r}95010{/}/{g}94990{/}/{c}95000{/}) ({r}B{/}/{g}S{/}/{c}15m{/})") {
		t.Fatalf("oneliner = %q", head)
	}
	if !strings.Contains(head, "Min:{g}95000{/} Max:{r}95000{/}") {
		t.Fatalf("minmax = %q", head)
	}
	// sparkline lines wrapped in the eighth-block bars
	if len(got) < 2 || !strings.Contains(got[1], "▕") || !strings.Contains(got[1], "▏") {
		t.Fatalf("graph lines = %q", got[1:])
	}
}

func TestNoRebroadcastWithinSixHours(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.feedBTC(t, 95000)
	f.take()
	f.feedBTC(t, 95001)
	if got := f.take(); len(got) != 0 {
		t.Fatalf("rebroadcast within 6h without jump: %q", got)
	}
	// after 6h of quiet it broadcasts again
	f.advance(7 * time.Hour) // fires the pending 30m refresh on the way
	f.complete(t, btcURL, btcJSON(95002))
	if got := f.take(); len(got) == 0 {
		t.Fatal("no rebroadcast after 6 hours")
	}
}

func TestJumpBroadcastsEarly(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	// build 21 stable points (last broadcast lands at the 6h mark),
	// then spike far outside the IQR fences well before the next
	// 6-hour slot: only jump detection can broadcast this
	for i := range 21 {
		f.feedBTC(t, 95000+float64(i%3))
	}
	f.take()
	f.advance(30 * time.Minute)
	f.complete(t, btcURL, btcJSON(120000))
	got := f.take()
	if len(got) == 0 {
		t.Fatal("jump not broadcast")
	}
	if !strings.Contains(got[0], "{c}120000{/}") {
		t.Fatalf("jump broadcast = %q", got[0])
	}
}

func TestShowAndBareSymbol(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.feedBTC(t, 95000)
	f.take()
	f.ticker("BenV", "show btc")
	got := f.take()
	if len(got) == 0 || !strings.HasPrefix(got[0], "#testing|BenV: {y}BTC{/} ") {
		t.Fatalf("show = %q (lowercase must work)", got)
	}
	f.raw("BenV", "!btc 5")
	got = f.take()
	if len(got) < 6 { // header + 5 graph rows
		t.Fatalf("bare symbol with height = %q", got)
	}
	f.raw("BenV", "!GME")
	if got := f.take(); len(got) != 0 {
		t.Fatalf("unknown bare symbol replied: %q", got)
	}
}

func TestShowUnknown(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.feedBTC(t, 95000)
	f.take()
	f.ticker("BenV", "show DOGE")
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "No such symbol. Try BTC for example. We currently have: BTC") {
		t.Fatalf("show unknown = %q", got)
	}
}

func TestETHSource(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.ticker("BenV", "add ETH")
	f.take()
	url := "https://api.kraken.com/0/public/Ticker?pair=XETHZEUR"
	if len(f.fetched) != 1 || f.fetched[0] != url {
		t.Fatalf("fetched = %q", f.fetched)
	}
	f.complete(t, url, `{"error":[],"result":{"XETHZEUR":{"a":["1800.5","35","35.000"],"b":["1799.1","1","1.000"],"c":["1800.0","20.4"]}}}`)
	got := f.take()
	if len(got) == 0 || !strings.Contains(got[0], "{w}€{/}({r}1800.5{/}/{g}1799.1{/}/{c}1800.0{/}) ({r}B{/}/{g}S{/}/{c}last trade{/})") {
		t.Fatalf("eth broadcast = %q", got)
	}
}

func TestStockNeedsAPIKey(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.ticker("BenV", "add GME")
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "no alphavantage API key configured") {
		t.Fatalf("keyless stock add = %q", got)
	}
	if len(f.fetched) != 0 {
		t.Fatalf("fetched without key: %q", f.fetched)
	}
}

func TestStockSource(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.cf.Set("ticker_alphavantage_key", "TESTKEY123")
	f.ticker("BenV", "add GME")
	f.take()
	if len(f.fetched) != 1 || !strings.Contains(f.fetched[0], "apikey=TESTKEY123") ||
		!strings.HasSuffix(f.fetched[0], "symbol=GME") {
		t.Fatalf("fetched = %q", f.fetched)
	}
	f.complete(t, f.fetched[0], `{
		"Meta Data": {"3. Last Refreshed": "2026-07-06 09:55:00", "6. Time Zone": "US/Eastern"},
		"Time Series (5min)": {
			"2026-07-06 09:55:00": {"1. open": "292.1000", "2. high": "294.4100", "3. low": "289.0000", "4. close": "292.0000", "5. volume": "65785"},
			"2026-07-06 09:50:00": {"1. open": "290.0000", "2. high": "292.0000", "3. low": "288.0000", "4. close": "291.0000", "5. volume": "60000"}
		}}`)
	got := f.take()
	if len(got) == 0 || !strings.Contains(got[0], "Open {w}292.1000{/}") ||
		!strings.Contains(got[0], "Close {W}292.0000{/}") {
		t.Fatalf("stock broadcast = %q", got)
	}
}

func TestAlarms(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.feedBTC(t, 95000)
	f.take()
	f.ticker("BenV", "setalarm BTC high=96000 low=90000")
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "Alarm SET for BTC@#testing") {
		t.Fatalf("setalarm = %q", got)
	}
	// crossing the rise threshold alerts channel and query
	f.feedBTC(t, 97000)
	got = f.take()
	found := 0
	for _, l := range got {
		if strings.Contains(l, "{R}ALARM{/} - {C}BTC{/} just rose from {y}95000{/} to {r}97000{/}") {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("alarm lines = %q, want channel + query", got)
	}
	// no repeat while above
	f.feedBTC(t, 97500)
	for _, l := range f.take() {
		if strings.Contains(l, "ALARM") {
			t.Fatalf("alarm re-fired without crossing: %q", l)
		}
	}
	f.ticker("BenV", "delalarm BTC")
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0], "alarms for BTC on channel #testing deleted") {
		t.Fatalf("delalarm = %q", got)
	}
}

func TestAlarmGibberish(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.feedBTC(t, 95000)
	f.take()
	f.ticker("BenV", "setalarm BTC high=96000 blabla")
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "Leaves this gibberish: {R}") {
		t.Fatalf("gibberish = %q", got)
	}
}

func TestDelAndList(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.feedBTC(t, 95000)
	f.take()
	f.ticker("BenV", "list")
	if got := f.take(); len(got) != 1 || got[0] != "#testing|Tickers in this channel: BTC" {
		t.Fatalf("list = %q", got)
	}
	f.ticker("BenV", "del BTC")
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0], "OK: subscription for BTC removed.") {
		t.Fatalf("del = %q", got)
	}
	f.advance(2 * time.Hour)
	if len(f.fetched) != 1 {
		t.Fatalf("still polling after del: %q", f.fetched)
	}
}

func TestRefreshSubstring(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.feedBTC(t, 95000)
	f.take()
	f.ticker("BenV", "refresh bt")
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|Refreshing ticker BTC" {
		t.Fatalf("refresh = %q", got)
	}
	if len(f.fetched) != 2 {
		t.Fatalf("refresh did not refetch: %q", f.fetched)
	}
}

func TestJSONErrorsAbandon(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.ticker("BenV", "add BTC")
	f.take()
	for range 11 {
		f.complete(t, btcURL, "geen json")
		f.advance(30 * time.Minute)
	}
	got := f.take()
	found := false
	for _, l := range got {
		if strings.Contains(l, "had too many fetcher errors - no longer retrying") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no abandon broadcast: %q", got)
	}
	f.advance(24 * time.Hour)
	if len(f.cbs) != 0 {
		t.Fatal("still fetching after abandon")
	}
}

func TestRestoreResumesPolling(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	f.feedBTC(t, 95000)
	f.take()
	f.m.Unload()

	f2 := newFixture(t, store)
	f2.advance(time.Second) // rand-0 offset
	if len(f2.fetched) != 1 || f2.fetched[0] != btcURL {
		t.Fatalf("restored ticker not polled: %q", f2.fetched)
	}
	// old graph data survived: show works before any new fetch
	f2.ticker("BenV", "show BTC")
	got := f2.take()
	if len(got) == 0 || !strings.Contains(got[0], "{c}95000{/}") {
		t.Fatalf("restored show = %q", got)
	}
}

func TestHelp(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.ticker("BenV", "")
	got := f.take()
	if len(got) != 4 || !strings.Contains(got[0], "Usage  : !ticker (add|del|show) SYM") {
		t.Fatalf("help = %q", got)
	}
}

func TestSigDigits(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0.000034, "0.000034"},
		{0.5, "0.50"},
		{12.345, "12.35"},
		{-3.0, "-3.00"},
	} {
		if got := firstSigDigits(tc.in, 2); got != tc.want {
			t.Errorf("firstSigDigits(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeSparkline(t *testing.T) {
	var pts []point
	for i := range 10 {
		pts = append(pts, point{v: float64(i), t: float64(i * 100)})
	}
	got := normalizeSparkline(pts, 5)
	if len(got) < 5 {
		t.Fatalf("normalized = %v", got)
	}
	// last raw value gets appended when it differs from the last bucket
	if got[len(got)-1] != 9 {
		t.Fatalf("normalized tail = %v", got)
	}
}
