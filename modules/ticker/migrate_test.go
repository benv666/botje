package ticker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go-botje/internal/storage"
)

// tickerFixture mirrors the real IRC_Ticker.dat shape: alarms are
// strings (perl), the fetcher subtree is runtime junk, tickerdata
// points are raw API responses with an added integer "time".
const tickerFixture = `{
  "tickers": {
    "BTC": {
      "fetcher": {},
      "subscriptions": {"junerules": {"#testing": {"BenV": {
        "alarms": {"rise": "50000", "drop": "15000", "uprate": "1000", "downrate": "1000"}
      }}}}
    },
    "ETH": {
      "fetcher": {"junk": 1},
      "subscriptions": {"junerules": {"#testing": {"BenV": {}}}}
    }
  },
  "tickerdata": {
    "BTC": [
      {"time": 1780492197, "EUR": {"last": "90000.5", "symbol": "EUR"}},
      {"EUR": {"last": "90001", "symbol": "EUR"}},
      {"time": 1780492797, "EUR": {"last": "90002.5", "symbol": "EUR"}}
    ],
    "ETH": [
      {"time": 1780492197, "error": [], "result": {"XETHZEUR": {"c": ["1613.57000", "0.16"]}}}
    ]
  }
}`

func TestMigrateFromPerl(t *testing.T) {
	var dump map[string]any
	if err := json.Unmarshal([]byte(tickerFixture), &dump); err != nil {
		t.Fatal(err)
	}
	out, st, err := MigrateFromPerl(dump)
	if err != nil {
		t.Fatal(err)
	}
	if st.Tickers != 2 || st.Subscriptions != 2 || st.Points != 3 {
		t.Fatalf("stats = %+v, want 2 tickers, 2 subs, 3 points", st)
	}
	// string alarms became numbers
	btc := out.Tickers["BTC"].Subscriptions["junerules"]["#testing"]["BenV"]
	if btc.Alarms["rise"] != 50000 || btc.Alarms["downrate"] != 1000 {
		t.Fatalf("BTC alarms = %v", btc.Alarms)
	}
	// subscription without alarms survives
	if out.Tickers["ETH"].Subscriptions["junerules"]["#testing"]["BenV"] == nil {
		t.Fatal("ETH BenV subscription lost")
	}
	// point without a time key is dropped (would sort as epoch 0),
	// the rest pass through raw with time as float64
	if len(out.Data["BTC"]) != 2 {
		t.Fatalf("BTC points = %d, want 2", len(out.Data["BTC"]))
	}
	if tm, ok := out.Data["BTC"][0]["time"].(float64); !ok || tm != 1780492197 {
		t.Fatalf("time = %v (%T)", out.Data["BTC"][0]["time"], out.Data["BTC"][0]["time"])
	}
	if v := dig(out.Data["BTC"][1], "EUR", "last"); numStr(v) != "90002.5" {
		t.Fatalf("second kept point EUR last = %v", v)
	}
}

func TestMigrateFromPerlRejectsGarbage(t *testing.T) {
	var dump map[string]any
	json.Unmarshal([]byte(`{"tickers": {"BTC": "nope"}}`), &dump)
	if _, _, err := MigrateFromPerl(dump); err == nil {
		t.Fatal("want error for non-map ticker")
	}
}

// the migrated value must be readable by a fresh module: restore
// starts the BTC/ETH fetchers and !ticker list shows both symbols.
func TestMigratedReadableByModule(t *testing.T) {
	var dump map[string]any
	json.Unmarshal([]byte(tickerFixture), &dump)
	out, _, err := MigrateFromPerl(dump)
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemory()
	if err := store.Put("ticker", "tickers", out.Tickers); err != nil {
		t.Fatal(err)
	}
	if err := store.Put("ticker", "tickerdata", out.Data); err != nil {
		t.Fatal(err)
	}

	f := newFixture(t, store)
	f.advance(time.Second) // rand-0 restore offset
	if len(f.fetched) != 2 {
		t.Fatalf("restore fetched %v, want BTC+ETH sources", f.fetched)
	}
	f.ticker("BenV", "list")
	joined := strings.Join(f.sent, " ")
	if !strings.Contains(joined, "BTC") || !strings.Contains(joined, "ETH") {
		t.Fatalf("!ticker list after migration = %q", f.sent)
	}
}
