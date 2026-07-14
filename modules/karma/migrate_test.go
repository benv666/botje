package karma

import (
	"encoding/json"
	"testing"

	"go-botje/internal/storage"
)

// perl-shaped karma fixture: global entries mixed into the server level
const migrateFixture = `{
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

func TestMigrateTransform(t *testing.T) {
	var dump map[string]any
	if err := json.Unmarshal([]byte(migrateFixture), &dump); err != nil {
		t.Fatal(err)
	}
	data, stats, err := MigrateFromPerl(dump)
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

func TestMigrateImportReadable(t *testing.T) {
	var dump map[string]any
	json.Unmarshal([]byte(migrateFixture), &dump)
	data, _, err := MigrateFromPerl(dump)
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemory()
	if err := store.Put("karma", "karma", data); err != nil {
		t.Fatal(err)
	}

	var back Data
	found, err := store.Get("karma", "karma", &back)
	if err != nil || !found {
		t.Fatalf("Get: %v %v", found, err)
	}
	if back.Servers["junerules"]["#test"]["kanker"].Karma != -5 {
		t.Fatalf("round-trip = %+v", back.Servers["junerules"]["#test"])
	}
}
