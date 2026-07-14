// Package ticker tracks financial symbols: BTC and ETH prices and
// alphavantage stocks, with 24h of history (96 half-hour points),
// sparkline graphs, IQR-based jump detection, a 6-hour broadcast cap,
// and per-user price alarms. Ported from IRC_Ticker.pm. Fixes vs the
// Perl: the alphavantage key lives in conf instead of the source, the
// IQR quartiles sort numerically (the Perl string-sorted prices), show
// works in lowercase, the add options query/#channel work as
// advertised (the Perl had them inverted), alarm queries go to the
// nick (not the bogus &nick channel), and alarm changes save
// immediately.
package ticker

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go-botje/internal/fetch"
	"go-botje/internal/format"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
	"go-botje/internal/sched"
)

const (
	maxData       = 96 // 48*0.5h = 24 hours
	maxJSONErrors = 10
	refreshEvery  = 30 * time.Minute
	broadcastGap  = 6 * time.Hour
)

// subInfo is one user's subscription (alarms live here).
type subInfo struct {
	Alarms map[string]float64 `json:"alarms,omitempty"` // rise drop uprate downrate
}

type tickerSub struct {
	// Subscriptions: server -> channel -> user
	Subscriptions map[string]map[string]map[string]*subInfo `json:"subscriptions"`
}

// fetcher is the runtime fetch state per symbol.
type fetcher struct {
	data          []sample
	timer         sched.Tag
	timerSet      bool
	tries         int
	errorCount    int
	noReschedule  bool
	lastBroadcast time.Time
	lastShown     float64
	hasShown      bool
}

type stored struct {
	Tickers map[string]*tickerSub `json:"tickers"`
	Data    map[string][]sample   `json:"tickerdata"`
}

// symState is the persisted per-symbol broadcast state. Without it a
// core restart reset the 6h broadcast cap (and the delta baseline) and
// every boot re-announced the same price to the subscribed channels
// (BenV, 2026-07-14, after a day of dev restarts).
type symState struct {
	LastBroadcast int64   `json:"last_broadcast"`
	LastShown     float64 `json:"last_shown"`
	HasShown      bool    `json:"has_shown"`
}

var (
	addRe      = regexp.MustCompile(`^add\s+(\S+)(?:\s+(.+))?\s*$`)
	showRe     = regexp.MustCompile(`^show\s+(\S+)\s*(\d+)?`)
	delRe      = regexp.MustCompile(`^del\s+(\S+)`)
	setAlarmRe = regexp.MustCompile(`^setalarm\s+(\S+)(?:\s+(.+))?\s*$`)
	delAlarmRe = regexp.MustCompile(`^delalarm\s+(\S+)`)
	refreshRe  = regexp.MustCompile(`^refresh\s+(\S+)`)
	listRe     = regexp.MustCompile(`^list($|\s)`)
	bareRe     = regexp.MustCompile(`^(\S+)\s*(\d+)?`)
	targetRe   = regexp.MustCompile(`(?:\s|^)(query|#(\S+))(?:\s|$)`)

	alarmOptRes = map[string]*regexp.Regexp{
		"drop":     regexp.MustCompile(`(?i)low=(\d+(\.\d+)?)`),
		"rise":     regexp.MustCompile(`(?i)high=(\d+(\.\d+)?)`),
		"uprate":   regexp.MustCompile(`(?i)up=(\d+(\.\d+)?)`),
		"downrate": regexp.MustCompile(`(?i)down=(\d+(\.\d+)?)`),
	}
)

var tickerHelp = strings.Join([]string{
	"Usage  : !ticker (add|del|show) SYM <graph heigh>, i.e. !ticker add AAPL to show apple stock info every 30 minutes here if changed. Symbol BTC is special for BITCOIN. ETH for Ethereum.",
	"Set alarms: !ticker setalarm SYM high=321 low=123 up=10 down=10",
	"This will set an alarm that compares the price going over 321 or under 123 or if the update change rate is greater than 10.",
	"Remove this alarm with !delalarm SYM, in the channel you set it.",
}, "\n")

// Module implements module.Module.
type Module struct {
	// Now and Rand are injectable for tests.
	Now  func() time.Time
	Rand func() float64

	// fetch is the fetcher seam, replaced in tests.
	fetch func(url string, opts fetch.Options, cb func(fetch.Result)) bool

	ctx      *module.Context
	tickers  map[string]*tickerSub
	fetchers map[string]*fetcher
}

// New returns an unloaded ticker module.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "ticker" }

func (m *Module) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Module) rand() float64 {
	if m.Rand != nil {
		return m.Rand()
	}
	return rand.Float64()
}

func (m *Module) Load(ctx *module.Context) error {
	m.ctx = ctx
	if m.fetch == nil {
		m.fetch = ctx.Fetch.Fetch
	}
	ctx.Conf.CreateString("ticker_alphavantage_key", "")

	m.tickers = make(map[string]*tickerSub)
	m.fetchers = make(map[string]*fetcher)

	var st stored
	if _, err := ctx.Store.Get(m.Name(), "tickers", &st.Tickers); err != nil {
		return fmt.Errorf("ticker: load: %w", err)
	}
	if _, err := ctx.Store.Get(m.Name(), "tickerdata", &st.Data); err != nil {
		return fmt.Errorf("ticker: load data: %w", err)
	}
	if st.Tickers != nil {
		m.tickers = st.Tickers
	}
	for sym, data := range st.Data {
		m.fetcherFor(sym).data = data
	}
	var state map[string]*symState
	if _, err := ctx.Store.Get(m.Name(), "tickerstate", &state); err != nil {
		return fmt.Errorf("ticker: load state: %w", err)
	}
	for sym, s := range state {
		f := m.fetcherFor(sym)
		f.lastBroadcast = time.Unix(s.LastBroadcast, 0)
		f.lastShown, f.hasShown = s.LastShown, s.HasShown
	}
	for sym := range m.tickers {
		m.scheduleRefresh(sym, time.Duration(m.rand()*30)*time.Second)
	}

	ctx.Cmd.Register(m.Name(), "ticker", m.cbTicker)
	ctx.Cmd.RegisterDefault(m.Name(), 10, false, m.cbDefault)
	return nil
}

func (m *Module) Unload() error {
	for _, f := range m.fetchers {
		if f.timerSet {
			m.ctx.Sched.Unschedule(f.timer)
		}
	}
	m.ctx.Cmd.UnregisterModule(m.Name())
	m.ctx.Bus.UnregisterModule(m.Name())
	return m.save()
}

// save persists subscriptions and graph data (the Perl wiped the data
// on every intermediate save; here both always land).
func (m *Module) save() error {
	if err := m.ctx.Store.Put(m.Name(), "tickers", m.tickers); err != nil {
		return err
	}
	data := make(map[string][]sample, len(m.fetchers))
	state := make(map[string]*symState, len(m.fetchers))
	for sym, f := range m.fetchers {
		if len(f.data) > 0 {
			data[sym] = f.data
		}
		state[sym] = &symState{
			LastBroadcast: f.lastBroadcast.Unix(),
			LastShown:     f.lastShown, HasShown: f.hasShown,
		}
	}
	if err := m.ctx.Store.Put(m.Name(), "tickerdata", data); err != nil {
		return err
	}
	return m.ctx.Store.Put(m.Name(), "tickerstate", state)
}

func (m *Module) fetcherFor(sym string) *fetcher {
	f := m.fetchers[sym]
	if f == nil {
		f = &fetcher{}
		m.fetchers[sym] = f
	}
	return f
}

// --- commands

// cbDefault renders "!SYM [height]" graphs for known symbols.
func (m *Module) cbDefault(d *cmd.Data) bool {
	if d.Event.SenderMe {
		return false
	}
	g := bareRe.FindStringSubmatch(d.Data)
	if g == nil {
		return false
	}
	sym := strings.ToUpper(g[1])
	if m.tickers[sym] == nil {
		return false
	}
	m.ctx.Privmsg(d.Event.Channel, d.Event.Sender.Nick+": "+m.printTicker(sym, atoi(g[2])))
	return true
}

func (m *Module) cbTicker(d *cmd.Data) bool {
	if d.Event.SenderMe {
		return false
	}
	server, channel, nick := d.Event.Server, d.Event.Channel, d.Event.Sender.Nick
	msg := d.Data

	switch {
	case msg == "":
		m.ctx.Privmsg(channel, tickerHelp)
	case addRe.MatchString(msg):
		g := addRe.FindStringSubmatch(msg)
		m.addTicker(server, channel, nick, strings.ToUpper(g[1]), g[2])
	case showRe.MatchString(msg):
		g := showRe.FindStringSubmatch(msg)
		sym := strings.ToUpper(g[1])
		if m.tickers[sym] != nil {
			m.ctx.Privmsg(channel, nick+": "+m.printTicker(sym, atoi(g[2])))
		} else {
			m.ctx.Privmsg(channel, "No such symbol. Try BTC for example. We currently have: "+
				strings.Join(m.symbols(), ", "))
		}
	case delRe.MatchString(msg):
		m.delTicker(strings.ToUpper(delRe.FindStringSubmatch(msg)[1]), server, channel, nick)
	case setAlarmRe.MatchString(msg):
		g := setAlarmRe.FindStringSubmatch(msg)
		m.addAlarm(strings.ToUpper(g[1]), server, channel, nick, g[2])
	case delAlarmRe.MatchString(msg):
		m.delAlarm(strings.ToUpper(delAlarmRe.FindStringSubmatch(msg)[1]), server, channel, nick)
	case refreshRe.MatchString(msg):
		m.refreshCmd(strings.ToUpper(refreshRe.FindStringSubmatch(msg)[1]), channel)
	case listRe.MatchString(msg):
		var list []string
		for _, sym := range m.symbols() {
			if t := m.tickers[sym]; t.Subscriptions[server] != nil && t.Subscriptions[server][channel] != nil {
				list = append(list, sym)
			}
		}
		m.ctx.Privmsg(channel, "Tickers in this channel: "+strings.Join(list, " "))
	default:
		m.ctx.Privmsg(channel, "Syntax error!")
		m.ctx.Privmsg(channel, tickerHelp)
	}
	return true
}

func atoi(s string) int {
	var n int
	fmt.Sscanf(s, "%d", &n)
	return n
}

func (m *Module) symbols() []string {
	out := make([]string, 0, len(m.tickers))
	for sym := range m.tickers {
		out = append(out, sym)
	}
	slices.Sort(out)
	return out
}

func (m *Module) addTicker(server, channel, user, sym, options string) {
	target := channel
	if g := targetRe.FindStringSubmatch(options); g != nil {
		if g[1] == "query" {
			target = user
		} else {
			target = g[1]
		}
	}
	if isStock(sym) && m.ctx.Conf.String("ticker_alphavantage_key") == "" {
		m.ctx.Privmsg(channel, user+": Error - no alphavantage API key configured (conf ticker_alphavantage_key).")
		return
	}

	t := m.tickers[sym]
	if t != nil && t.Subscriptions[server] != nil && t.Subscriptions[server][target] != nil {
		m.ctx.Privmsg(channel, fmt.Sprintf("%s: Subscription for that %s@%s combination already exists - ignoring.",
			user, sym, target))
		return
	}
	if t == nil {
		t = &tickerSub{Subscriptions: make(map[string]map[string]map[string]*subInfo)}
		m.tickers[sym] = t
	}
	if t.Subscriptions[server] == nil {
		t.Subscriptions[server] = make(map[string]map[string]*subInfo)
	}
	if t.Subscriptions[server][target] == nil {
		t.Subscriptions[server][target] = make(map[string]*subInfo)
	}
	t.Subscriptions[server][target][user] = &subInfo{}
	m.ctx.Privmsg(channel, user+": Subscription added. It will be sent to "+target)
	m.save()
	m.refreshTicker(sym)
}

func (m *Module) delTicker(sym, server, channel, user string) {
	t := m.tickers[sym]
	if t != nil && t.Subscriptions[server] == nil {
		if len(t.Subscriptions) == 0 {
			m.dropTicker(sym)
			m.ctx.Privmsg(channel, user+": Ticker had no subscriptions on any server: deleted.")
		} else {
			m.ctx.Privmsg(channel, fmt.Sprintf("%s: %s not subscribed to ticker %s", user, channel, sym))
		}
		return
	}
	if t == nil || t.Subscriptions[server] == nil || t.Subscriptions[server][channel] == nil {
		m.ctx.Privmsg(channel, user+": Error: no such subscription found for this ticker")
		return
	}
	delete(t.Subscriptions[server], channel)
	if len(t.Subscriptions[server]) == 0 {
		delete(t.Subscriptions, server)
	}
	if len(t.Subscriptions) == 0 {
		m.dropTicker(sym)
	}
	m.ctx.Privmsg(channel, fmt.Sprintf("%s: OK: subscription for %s removed.", user, sym))
	m.save()
}

func (m *Module) dropTicker(sym string) {
	if f := m.fetchers[sym]; f != nil && f.timerSet {
		m.ctx.Sched.Unschedule(f.timer)
	}
	delete(m.tickers, sym)
	delete(m.fetchers, sym)
}

func (m *Module) addAlarm(sym, server, channel, user, options string) {
	if options == "" {
		m.ctx.Privmsg(channel, fmt.Sprintf("%s: You want to set WHAT alarm for %s@%s?!", user, sym, channel))
		return
	}
	alarms := make(map[string]float64)
	rest := options
	for kind, re := range alarmOptRes {
		if g := re.FindStringSubmatch(rest); g != nil {
			alarms[kind] = num(g[1])
			rest = re.ReplaceAllString(rest, "")
		}
	}
	parts := make([]string, 0, len(alarms))
	for _, kind := range []string{"rise", "drop", "uprate", "downrate"} {
		if v, ok := alarms[kind]; ok {
			parts = append(parts, fmt.Sprintf("%s=%s", kind, numStr(v)))
		}
	}
	parsed := strings.Join(parts, ", ")
	if strings.TrimSpace(rest) != "" {
		m.ctx.Privmsg(channel, fmt.Sprintf(
			"%s: You want an alarm for %s@%s, I parsed these options: %s. Leaves this gibberish: {R}%s",
			user, sym, channel, parsed, strings.TrimSpace(rest)))
		return
	}
	t := m.tickers[sym]
	if t == nil {
		m.ctx.Privmsg(channel, fmt.Sprintf("%s: no ticker %s to set alarms on", user, sym))
		return
	}
	if t.Subscriptions[server] == nil {
		t.Subscriptions[server] = make(map[string]map[string]*subInfo)
	}
	if t.Subscriptions[server][channel] == nil {
		t.Subscriptions[server][channel] = make(map[string]*subInfo)
	}
	t.Subscriptions[server][channel][user] = &subInfo{Alarms: alarms}
	m.ctx.Privmsg(channel, fmt.Sprintf(
		"%s: Alarm SET for %s@%s, with these options: %s. Alarms will also be queried to you.",
		user, sym, channel, parsed))
	m.save()
}

func (m *Module) delAlarm(sym, server, channel, user string) {
	t := m.tickers[sym]
	if t != nil && t.Subscriptions[server] != nil && t.Subscriptions[server][channel] != nil {
		if sub := t.Subscriptions[server][channel][user]; sub != nil && sub.Alarms != nil {
			sub.Alarms = nil
			m.ctx.Privmsg(channel, fmt.Sprintf("%s: alarms for %s on channel %s deleted", user, sym, channel))
			m.save()
			return
		}
	}
	m.ctx.Privmsg(channel, fmt.Sprintf("%s: you have no set alarms for %s on channel %s", user, sym, channel))
}

func (m *Module) refreshCmd(sym, channel string) {
	if m.tickers[sym] == nil {
		for _, t := range m.symbols() {
			if strings.Contains(strings.ToLower(t), strings.ToLower(sym)) {
				sym = t
				break
			}
		}
	}
	if m.tickers[sym] == nil {
		m.ctx.Privmsg(channel, fmt.Sprintf("Can't find ticker %s -- not refreshing!", sym))
		return
	}
	m.ctx.Privmsg(channel, "Refreshing ticker "+sym)
	m.refreshTicker(sym)
}

// --- polling

func (m *Module) scheduleRefresh(sym string, d time.Duration) {
	f := m.fetcherFor(sym)
	if f.timerSet {
		m.ctx.Sched.Unschedule(f.timer)
		f.timerSet = false
	}
	if f.noReschedule {
		return
	}
	f.timer = m.ctx.Sched.After(d, func() { m.refreshTicker(sym) })
	f.timerSet = true
}

func (m *Module) refreshTicker(sym string) {
	if m.tickers[sym] == nil {
		delete(m.fetchers, sym)
		return
	}
	f := m.fetcherFor(sym)
	if f.timerSet {
		m.ctx.Sched.Unschedule(f.timer)
		f.timerSet = false
	}
	url := sourceFor(sym).url(sym, m.ctx.Conf.String("ticker_alphavantage_key"))
	ok := m.fetch(url, fetch.Options{Timeout: 30 * time.Second}, func(res fetch.Result) {
		m.fetched(sym, res)
	})
	if !ok {
		m.scheduleRefresh(sym, time.Minute)
	}
}

func (m *Module) fetched(sym string, res fetch.Result) {
	f := m.fetchers[sym]
	if f == nil || m.tickers[sym] == nil {
		return // deleted while fetching
	}
	if res.Err != nil || res.Status >= 400 {
		// the perl cbCheckFetch cadence: two quick retries, then an hour
		f.tries++
		if f.tries > 2 {
			f.tries = 0
			m.scheduleRefresh(sym, time.Hour)
		} else {
			m.scheduleRefresh(sym, time.Minute)
		}
		return
	}
	f.tries = 0
	m.scheduleRefresh(sym, refreshEvery) // fail or not, next slot is set

	var raw sample
	if err := json.Unmarshal(res.Body, &raw); err != nil {
		f.errorCount++
		if f.errorCount > maxJSONErrors {
			f.noReschedule = true
			m.scheduleRefresh(sym, 0) // unschedules and stays down
			m.broadcastMsg(sym, fmt.Sprintf(
				"{R}Error!{/} Ticker {y}%s{/} had too many fetcher errors - no longer retrying without user intervention!", sym))
		}
		return
	}
	f.errorCount = 0

	data := sourceFor(sym).transform(raw, sym, m.now())
	if data == nil {
		return
	}
	if _, ok := data["time"]; !ok {
		data["time"] = float64(m.now().Unix())
	}
	f.data = append(f.data, data)
	if len(f.data) > maxData {
		f.data = f.data[1:]
	}
	m.broadcastTicker(sym)
	m.checkAlarms(sym)
	m.save()
}

// --- broadcasting

func (m *Module) broadcastTicker(sym string) {
	f := m.fetchers[sym]
	if m.now().Sub(f.lastBroadcast) < broadcastGap && !m.hasJumped(sym) {
		return
	}
	f.lastShown = m.currentValue(sym)
	f.hasShown = true
	f.lastBroadcast = m.now()
	m.broadcastMsg(sym, m.printTicker(sym, 0))
}

func (m *Module) broadcastMsg(sym, msg string) {
	t := m.tickers[sym]
	if t == nil {
		return
	}
	for _, byChannel := range t.Subscriptions {
		for channel := range byChannel {
			m.ctx.Privmsg(channel, msg)
		}
	}
}

func (m *Module) currentValue(sym string) float64 {
	f := m.fetchers[sym]
	return sourceFor(sym).lastValue(f.data[len(f.data)-1])
}

// hasJumped is the IQR outlier check (numeric sort; the Perl sorted
// prices as strings).
func (m *Module) hasJumped(sym string) bool {
	f := m.fetchers[sym]
	if len(f.data) < 20 {
		return false // no real data, who cares
	}
	src := sourceFor(sym)
	values := make([]float64, len(f.data))
	for i, d := range f.data {
		values[i] = src.lastValue(d)
	}
	slices.Sort(values)
	n := len(values)
	q2 := quartile(values, float64(n)/2)
	_ = q2 // computed for parity; the fences only use Q1/Q3
	q1 := quartile(values, float64(n)/4)
	q3 := quartile(values, 3*float64(n)/4)
	iqr := q3 - q1
	lower, upper := q1-iqr*1.3, q3+iqr*1.3

	v := m.currentValue(sym)
	if v < lower || v > upper {
		if !f.hasShown {
			return true
		}
		dt := v - f.lastShown
		// perl parity: dt is not abs()'d, so drops below the last
		// shown value never trip this branch
		if iqr > 0 && dt/iqr > 1 {
			return true
		}
	}
	return false
}

// quartile is the Perl index-plus-average-when-integral dance.
func quartile(sorted []float64, pos float64) float64 {
	i := int(pos)
	q := sorted[i]
	if math.Abs(pos-math.Trunc(pos)) < 0.00001 && i > 0 {
		q = (q + sorted[i-1]) / 2
	}
	return q
}

// --- alarms

func (m *Module) checkAlarms(sym string) {
	f := m.fetchers[sym]
	if len(f.data) < 2 {
		return
	}
	src := sourceFor(sym)
	price := src.lastValue(f.data[len(f.data)-1])
	lastPrice := src.lastValue(f.data[len(f.data)-2])
	rate := price - lastPrice
	if rate == 0 {
		return
	}
	t := m.tickers[sym]
	for server, byChannel := range t.Subscriptions {
		_ = server
		for channel, byUser := range byChannel {
			for user, sub := range byUser {
				a := sub.Alarms
				if a == nil {
					continue
				}
				alert := func(verb, kind string, at float64) {
					msg := fmt.Sprintf("{R}ALARM{/} - {C}%s{/} just %s from {y}%s{/} to {r}%s{/} (rate: {w}%s{/}), you had %s set at %s",
						sym, verb, numStr(lastPrice), numStr(price), numStr(rate), kind, numStr(at))
					m.ctx.Privmsg(channel, user+" "+msg)
					m.ctx.Privmsg(user, msg) // query, not the perl's &nick
				}
				if at, ok := a["rise"]; ok && price >= at && lastPrice < at {
					alert("rose", "an alarm", at)
				}
				if at, ok := a["drop"]; ok && price <= at && lastPrice > at {
					alert("dropped", "an alarm", at)
				}
				if at, ok := a["uprate"]; ok && rate >= at {
					alert("rose", "an uprate alarm", at)
				}
				// "uprate" in the drop message is the perl's copy-paste
				if at, ok := a["downrate"]; ok && rate < 0 && math.Abs(rate) >= math.Abs(at) {
					alert("dropped", "an uprate alarm", at)
				}
			}
		}
	}
}

// --- rendering

type point struct{ v, t float64 }

func (m *Module) printTicker(sym string, graphHeight int) string {
	if graphHeight <= 0 {
		graphHeight = 3
	}
	if graphHeight > 8 {
		graphHeight = 8 // don't be rediculous
	}
	if m.tickers[sym] == nil {
		return fmt.Sprintf("No such ticker '%s', known keys: %s", sym, strings.Join(m.symbols(), ", "))
	}
	f := m.fetchers[sym]
	if f == nil || len(f.data) == 0 {
		return fmt.Sprintf("No data for ticker '%s'", sym)
	}
	last := f.data[len(f.data)-1]
	src := sourceFor(sym)
	oneliner := src.oneLiner(sym, last)

	minV, maxV := math.Inf(1), math.Inf(-1)
	sum, dxSum := 0.0, 0.0
	prev := src.lastValue(f.data[0])
	pts := make([]point, 0, len(f.data))
	for _, d := range f.data {
		v := src.lastValue(d)
		pts = append(pts, point{v: v, t: num(d["time"])})
		sum += v
		minV = math.Min(minV, v)
		maxV = math.Max(maxV, v)
		dxSum += v - prev
		prev = v
	}
	count := max(len(f.data), 1)
	oneliner += fmt.Sprintf(" Min:{g}%s{/} Max:{r}%s{/} d-Avg:{c}%s{/}",
		numStr(minV), numStr(maxV), firstSigDigits(sum/float64(count), 2))
	oneliner += fmt.Sprintf(" ∑DX:{y}%.2f{/} DXavg:%s{/}", dxSum, firstSigDigits(dxSum/float64(count), 2))

	rows := format.Sparkline(normalizeSparkline(pts, 64), graphHeight)
	left, right := "{m}▕", "{m}▏"
	oneliner += "\n" + left + strings.Join(rows, right+"\n"+left) + right

	return fmt.Sprintf("{y}%s{/} %s: %s", sym, timeAgo(m.now().Sub(time.Unix(int64(num(last["time"])), 0))), oneliner)
}

// normalizeSparkline spreads samples over hRes time buckets, averaging
// within a bucket and carrying the last average over empty ones; the
// final raw value is appended when it differs (the Perl kept big
// last-minute moves visible that way).
func normalizeSparkline(data []point, hRes int) []float64 {
	if len(data) == 0 {
		return nil
	}
	tStart, tEnd := data[0].t, data[len(data)-1].t
	if hRes == 0 {
		hRes = 1
	}
	if tStart == tEnd {
		out := make([]float64, len(data))
		for i, d := range data {
			out[i] = d.v
		}
		return out
	}
	dt := (tEnd - tStart) / float64(hRes)
	tCur := tStart
	n, sum, last := 0, 0.0, 0.0
	var res []float64
	for _, d := range data {
		for d.t > tCur+dt {
			if n > 0 {
				last = sum / float64(n)
			}
			res = append(res, last)
			n, sum = 0, 0
			tCur += dt
		}
		sum += d.v
		n++
	}
	if data[len(data)-1].v != last {
		res = append(res, data[len(data)-1].v)
	}
	return res
}

var sigRe = regexp.MustCompile(`^-?([0.]+)([^0]{0,2})`)

// firstSigDigits keeps leading zeros plus n significant characters for
// tiny values, plain %.2f otherwise (the Perl _getFirstSigDigits).
func firstSigDigits(v float64, n int) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if g := sigRe.FindStringSubmatch(s); g != nil {
		return g[1] + g[2] + strings.Repeat("0", n-len(g[2]))
	}
	return fmt.Sprintf("%.*f", n, v)
}

// timeAgo approximates DateTime::Duration::Fuzzy (same as lastseen's).
func timeAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < 2*time.Minute:
		return "a minute ago"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 2*time.Hour:
		return "about an hour ago"
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "yesterday"
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}
