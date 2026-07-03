package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"go-botje/internal/storage"
	"go-botje/modules/karma"
)

// perl-shaped karma fixture: global entries mixed into the server level
const karmaFixture = `{
  "karma": {
    "__GLOBAL_IRC_Karma__": {
      "beer":  {"karma": 12, "last": 1600000000, "reason": {"up": {"goede pils": 3}}},
      "kanker": {"karma": -5, "last": 1600000001}
    },
    "junerules": {
      "#rss": {
        "beer": {"karma": 7, "last": 1600000000, "reason": {"up": {"goede pils": 2}, "down": {"warm": 1}}}
      },
      "#test": {
        "beer":  {"karma": 5, "last": 1600000000},
        "kanker": {"karma": -5, "last": 1600000001}
      }
    },
    "mibbit": {
      "#chan": {
        "iets": {"karma": 1, "last": 1500000000}
      }
    }
  }
}`

func TestKarmaTransform(t *testing.T) {
	var dump map[string]any
	if err := json.Unmarshal([]byte(karmaFixture), &dump); err != nil {
		t.Fatal(err)
	}
	data, stats, err := karmaFromPerl(dump)
	if err != nil {
		t.Fatal(err)
	}

	if got := data.Global["beer"]; got == nil || got.Karma != 12 || got.Reason["up"]["goede pils"] != 3 {
		t.Fatalf("global beer = %+v", got)
	}
	if got := data.Servers["junerules"]["#rss"]["beer"]; got == nil || got.Karma != 7 ||
		got.Reason["down"]["warm"] != 1 || got.Last != 1600000000 {
		t.Fatalf("junerules #rss beer = %+v", got)
	}
	if got := data.Servers["mibbit"]["#chan"]["iets"]; got == nil || got.Karma != 1 {
		t.Fatalf("mibbit iets = %+v", got)
	}
	if stats.GlobalItems != 2 || stats.Servers != 2 || stats.Channels != 3 || stats.ChannelItems != 4 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestKarmaImportReadableByModule(t *testing.T) {
	var dump map[string]any
	json.Unmarshal([]byte(karmaFixture), &dump)
	data, _, err := karmaFromPerl(dump)
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemory()
	if err := store.Put("karma", "karma", data); err != nil {
		t.Fatal(err)
	}

	var back karma.Data
	found, err := store.Get("karma", "karma", &back)
	if err != nil || !found {
		t.Fatalf("Get: %v %v", found, err)
	}
	if back.Servers["junerules"]["#test"]["kanker"].Karma != -5 {
		t.Fatalf("round-trip = %+v", back.Servers["junerules"]["#test"])
	}
}

// TestKarmaRealData migrates the actual live snapshot when the
// gitignored reference tree is present, and verifies counts.
func TestKarmaRealData(t *testing.T) {
	dat := "../../reference/mounts/data/IRC_Karma.dat"
	if _, err := os.Stat(dat); err != nil {
		t.Skip("reference data not present")
	}
	out, err := exec.Command("perl", "dump.pl", dat).Output()
	if err != nil {
		t.Fatalf("dump.pl: %v", err)
	}
	var dump map[string]any
	if err := json.Unmarshal(out, &dump); err != nil {
		t.Fatalf("dump not valid json: %v", err)
	}
	data, stats, err := karmaFromPerl(dump)
	if err != nil {
		t.Fatal(err)
	}

	// counts seen live on 2026-07-03: 3889 global items, servers
	// fifo/junerules/mibbit
	if stats.GlobalItems < 3800 {
		t.Errorf("global items = %d, expected the live ~3889", stats.GlobalItems)
	}
	if stats.Servers != 3 {
		t.Errorf("servers = %d, want 3 (fifo junerules mibbit)", stats.Servers)
	}
	if len(data.Global) != stats.GlobalItems {
		t.Errorf("stats/global mismatch: %d vs %d", stats.GlobalItems, len(data.Global))
	}
	// nothing lost: recount the raw dump independently and compare
	// (the live data has a handful of channel items with no global
	// entry, predating the update-both-sides code, so counts are the
	// honest invariant)
	top := dump["karma"].(map[string]any)
	rawGlobal := len(top["__GLOBAL_IRC_Karma__"].(map[string]any))
	rawItems := 0
	for server, v := range top {
		if server == "__GLOBAL_IRC_Karma__" {
			continue
		}
		for _, cv := range v.(map[string]any) {
			rawItems += len(cv.(map[string]any))
		}
	}
	if rawGlobal != stats.GlobalItems || rawItems != stats.ChannelItems {
		t.Errorf("dump has %d global/%d channel items, transformed %d/%d",
			rawGlobal, rawItems, stats.GlobalItems, stats.ChannelItems)
	}
	t.Logf("migrated: %d global items, %d servers, %d channels, %d channel items",
		stats.GlobalItems, stats.Servers, stats.Channels, stats.ChannelItems)
}
