package ticker

// The data source table, ported from the Perl @dataSources: BTC via
// blockchain.info, ETH via Kraken, everything else via alphavantage.
// The alphavantage API key comes from conf (ticker_alphavantage_key);
// the Perl hardcoded it in the source. Another fix: the Perl stock
// oneliner labeled every stock "IRC_Ticker (last 5 min)" because it
// interpolated the module name instead of the symbol.

import (
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// point is one stored data sample: the transformed fetch JSON plus a
// "time" epoch field.
type sample = map[string]any

type source struct {
	handles   func(sym string) bool
	url       func(sym, apiKey string) string
	oneLiner  func(sym string, data sample) string
	lastValue func(data sample) float64
	// transform validates and reshapes raw fetch JSON into the stored
	// sample; nil result drops the fetch.
	transform func(raw sample, sym string, now time.Time) sample
}

// numStr renders a JSON value the way Perl interpolation would:
// strings verbatim, numbers without trailing zeros.
func numStr(v any) string {
	switch n := v.(type) {
	case string:
		return n
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	case nil:
		return ""
	default:
		return fmt.Sprint(n)
	}
}

// num extracts a float from a JSON value (number or numeric string).
func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}

// dig walks nested JSON maps.
func dig(data sample, path ...string) any {
	var cur any = data
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

var sources = []*source{
	{ // BTC via blockchain.info
		handles: func(sym string) bool { return sym == "BTC" },
		url:     func(_, _ string) string { return "https://blockchain.info/ticker" },
		oneLiner: func(_ string, data sample) string {
			eur, ok := data["EUR"].(map[string]any)
			if !ok {
				return "{r}No EUR data retrieved.{/}({r}B{/}/{g}S{/}/{c}15m{/})"
			}
			return fmt.Sprintf("{w}%s{/}({r}%s{/}/{g}%s{/}/{c}%s{/}) ({r}B{/}/{g}S{/}/{c}15m{/})",
				numStr(eur["symbol"]), numStr(eur["buy"]), numStr(eur["sell"]), numStr(eur["15m"]))
		},
		lastValue: func(data sample) float64 { return num(dig(data, "EUR", "last")) },
		transform: func(raw sample, _ string, _ time.Time) sample {
			if num(dig(raw, "EUR", "last")) <= 0 {
				return nil
			}
			return raw
		},
	},
	{ // ETH via kraken
		handles: func(sym string) bool { return sym == "ETH" },
		url:     func(_, _ string) string { return "https://api.kraken.com/0/public/Ticker?pair=XETHZEUR" },
		oneLiner: func(_ string, data sample) string {
			a, _ := dig(data, "result", "XETHZEUR", "a").([]any)
			b, _ := dig(data, "result", "XETHZEUR", "b").([]any)
			c, _ := dig(data, "result", "XETHZEUR", "c").([]any)
			if len(a) == 0 || len(b) == 0 || len(c) == 0 {
				return "{r}No ETH data retrieved.{/}"
			}
			return fmt.Sprintf("{w}€{/}({r}%s{/}/{g}%s{/}/{c}%s{/}) ({r}B{/}/{g}S{/}/{c}last trade{/})",
				numStr(a[0]), numStr(b[0]), numStr(c[0]))
		},
		lastValue: func(data sample) float64 {
			if a, _ := dig(data, "result", "XETHZEUR", "a").([]any); len(a) > 0 {
				return num(a[0])
			}
			return 0
		},
		transform: func(raw sample, _ string, _ time.Time) sample {
			a, _ := dig(raw, "result", "XETHZEUR", "a").([]any)
			if len(a) == 0 || num(a[0]) <= 0 {
				return nil
			}
			return raw
		},
	},
	{ // default: stocks via alphavantage
		handles: func(sym string) bool { return false }, // default source
		url: func(sym, apiKey string) string {
			return "https://www.alphavantage.co/query?function=TIME_SERIES_INTRADAY&interval=5min&apikey=" +
				url.QueryEscape(apiKey) + "&symbol=" + url.QueryEscape(sym)
		},
		oneLiner: func(sym string, data sample) string {
			return fmt.Sprintf("{y}%s (last 5 min){/} Open {w}%s{/} Close {W}%s{/} High {g}%s{/} Low {r}%s{/} Volume {B}%s{/} ",
				sym, numStr(data["1. open"]), numStr(data["4. close"]),
				numStr(data["2. high"]), numStr(data["3. low"]), numStr(data["5. volume"]))
		},
		lastValue: func(data sample) float64 { return num(data["4. close"]) },
		transform: func(raw sample, sym string, _ time.Time) sample {
			series, _ := raw["Time Series (5min)"].(map[string]any)
			key, _ := dig(raw, "Meta Data", "3. Last Refreshed").(string)
			if len(series) == 0 || key == "" {
				return nil
			}
			entry, ok := series[key].(map[string]any)
			if !ok {
				return nil
			}
			tz, _ := dig(raw, "Meta Data", "6. Time Zone").(string)
			loc, err := time.LoadLocation(tz)
			if err != nil {
				loc, _ = time.LoadLocation("US/Eastern")
			}
			at, err := time.ParseInLocation("2006-01-02 15:04:05", key, loc)
			if err != nil {
				return nil
			}
			entry["time"] = float64(at.Unix())
			return entry
		},
	},
}

// sourceFor picks the handler for a symbol; the last entry is the
// default.
func sourceFor(sym string) *source {
	for _, s := range sources {
		if s.handles(sym) {
			return s
		}
	}
	return sources[len(sources)-1]
}

// isStock reports whether sym falls through to the alphavantage
// default source (which needs the API key).
func isStock(sym string) bool {
	return sourceFor(sym) == sources[len(sources)-1]
}
