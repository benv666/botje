package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"go-botje/modules/ego"
	"go-botje/modules/rss"
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
