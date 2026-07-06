package rss

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go-botje/internal/module"
	"go-botje/internal/storage"
)

const rssFixture = `{"maxItemId":9129,"feeds":{
  "http://feeds.example/nieuws.xml":{
    "tag":"Nieuws","refresh":30,"title":"Testfeed","description":"desc","link":"http://x",
    "lastupdate":1697279000,
    "fetcher":{"refresh":45},
    "history":{
      "g-1":{"_id":"A5","title":"Eerste","link":"http://x/1","guid":"g-1","description":"desc een","datetime":1697279236,"updatetime":1697279236}
    },
    "subscriptions":{"junerules":{"#rss":{"user":"BenV","received":{"g-1":1697279300}}}}
  }
}}`

func TestMigrateFromPerl(t *testing.T) {
	var dump map[string]any
	if err := json.Unmarshal([]byte(rssFixture), &dump); err != nil {
		t.Fatal(err)
	}
	out, count, err := MigrateFromPerl(dump)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || out.MaxID != 9129 {
		t.Fatalf("count=%d maxid=%d", count, out.MaxID)
	}
	f := out.Feeds["http://feeds.example/nieuws.xml"]
	if f.Tag != "Nieuws" || f.Refresh != 45 { // fetcher.refresh wins
		t.Fatalf("feed = %+v", f)
	}
	it := f.History["g-1"]
	if it.ID != "A5" || it.Time != 1697279236 {
		t.Fatalf("item = %+v", it)
	}
	sub := f.Subscriptions["junerules"]["#rss"]
	if sub.User != "BenV" || sub.Received["g-1"] != 1697279300 {
		t.Fatalf("sub = %+v", sub)
	}
}

// migrated feeds must be readable by a fresh module: list shows the
// tag, recall by short id shows the item.
func TestMigratedReadableByModule(t *testing.T) {
	var dump map[string]any
	json.Unmarshal([]byte(rssFixture), &dump)
	out, _, err := MigrateFromPerl(dump)
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemory()
	if err := store.Put("rss", "feeds", out); err != nil {
		t.Fatal(err)
	}

	f := newFixtureAt(t, store, time.Date(2026, 7, 6, 12, 0, 0, 0, time.Local))
	// the feed and its #rss subscription survived
	sub := f.m.feeds["http://feeds.example/nieuws.xml"].Subscriptions["junerules"]["#rss"]
	if sub == nil || sub.User != "BenV" {
		t.Fatalf("migrated subscription missing: %+v", sub)
	}
	// recall by the migrated short id works (global, any channel)
	f.rss("BenV", "A5") // recall shows the item description under its id
	if got := f.take(); len(got) == 0 || !strings.Contains(got[0], "{r}A5{/}, Nieuws: desc een") {
		t.Fatalf("migrated recall = %q", got)
	}
}

var _ = module.Context{}
