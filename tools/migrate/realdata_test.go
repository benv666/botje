package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"go-botje/modules/ego"
	"go-botje/modules/rss"
	"go-botje/modules/ticker"
)

// dumpDat runs dump.pl on a reference .dat, skipping when the gitignored
// tree is absent.
func dumpDat(t *testing.T, name string) map[string]any {
	t.Helper()
	path := "../../reference/mounts/data/" + name
	if _, err := os.Stat(path); err != nil {
		t.Skip("reference data not present")
	}
	out, err := exec.Command("perl", "dump.pl", path).Output()
	if err != nil {
		t.Fatalf("dump.pl %s: %v", name, err)
	}
	var dump map[string]any
	if err := json.Unmarshal(out, &dump); err != nil {
		t.Fatalf("%s not valid json: %v", name, err)
	}
	return dump
}

func TestEgoRealData(t *testing.T) {
	dump := dumpDat(t, "IRC_Ego.dat")
	data, n, err := ego.MigrateFromPerl(dump)
	if err != nil {
		t.Fatal(err)
	}
	// independent recount: total nicks across all servers
	raw := dump["ego"].(map[string]any)
	want := 0
	for _, nicks := range raw {
		want += len(nicks.(map[string]any))
	}
	got := 0
	for _, nicks := range data {
		got += len(nicks)
	}
	if got != want || n != want {
		t.Errorf("nicks: transformed %d (n=%d), dump has %d", got, n, want)
	}
	t.Logf("migrated %d ego nicks", n)
}

func TestRSSRealData(t *testing.T) {
	dump := dumpDat(t, "IRC_RSS.dat")
	data, n, err := rss.MigrateFromPerl(dump)
	if err != nil {
		t.Fatal(err)
	}
	raw := dump["feeds"].(map[string]any)
	if n != len(raw) {
		t.Errorf("feeds: transformed %d, dump has %d", n, len(raw))
	}
	// every history item kept its short id and a timestamp
	items, withID := 0, 0
	for _, f := range data.Feeds {
		for _, it := range f.History {
			items++
			if it.ID != "" && it.Time > 0 {
				withID++
			}
		}
	}
	if items == 0 || withID != items {
		t.Errorf("items = %d, with id+time = %d", items, withID)
	}
	t.Logf("migrated %d feeds, %d history items", n, items)
}

func TestTickerRealData(t *testing.T) {
	dump := dumpDat(t, "IRC_Ticker.dat")
	data, stats, err := ticker.MigrateFromPerl(dump)
	if err != nil {
		t.Fatal(err)
	}
	rawTickers := dump["tickers"].(map[string]any)
	if stats.Tickers != len(rawTickers) {
		t.Errorf("tickers: transformed %d, dump has %d", stats.Tickers, len(rawTickers))
	}
	// independent recount of data points that carry a time key
	want := 0
	for _, pts := range dump["tickerdata"].(map[string]any) {
		for _, p := range pts.([]any) {
			if _, ok := p.(map[string]any)["time"]; ok {
				want++
			}
		}
	}
	got := 0
	for _, pts := range data.Data {
		got += len(pts)
	}
	if got != want || stats.Points != want {
		t.Errorf("points: transformed %d (stats %d), dump has %d timed points", got, stats.Points, want)
	}
	// every subscription's alarms became numbers > 0 (the live data
	// has rise/drop/up/down set on both tickers)
	alarms := 0
	for _, ts := range data.Tickers {
		for _, byChan := range ts.Subscriptions {
			for _, byUser := range byChan {
				for _, si := range byUser {
					for name, v := range si.Alarms {
						if v <= 0 {
							t.Errorf("alarm %s = %v, want > 0", name, v)
						}
						alarms++
					}
				}
			}
		}
	}
	if stats.Subscriptions == 0 || alarms == 0 {
		t.Errorf("subs = %d, alarms = %d, want > 0", stats.Subscriptions, alarms)
	}
	t.Logf("migrated %d tickers, %d subscriptions, %d points, %d alarms",
		stats.Tickers, stats.Subscriptions, stats.Points, alarms)
}
