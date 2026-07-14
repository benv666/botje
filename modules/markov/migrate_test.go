package markov

import (
	"encoding/json"
	"testing"

	"go-botje/internal/storage"
)

const migrateFixture = `{
  "dictionary_3_default": {
    "aap": {
      "__count": 3,
      "noot": {
        "__count": 2,
        "mies": {"__count": 2, ".": {"__count": 2}}
      },
      "teun": {"__count": 1}
    },
    "noot": {"__count": 5, "mies": {"__count": 4}}
  }
}`

func TestMigrateTransform(t *testing.T) {
	var dump map[string]any
	if err := json.Unmarshal([]byte(migrateFixture), &dump); err != nil {
		t.Fatal(err)
	}
	dicts, stats, err := MigrateFromPerl(dump)
	if err != nil {
		t.Fatal(err)
	}
	chains := dicts["dictionary_3_default"]
	if chains == nil || len(chains) != 2 {
		t.Fatalf("dicts = %+v", stats.Dictionaries)
	}
	aap := chains["aap"]
	if aap.Count != 3 || aap.Children["noot"].Count != 2 ||
		aap.Children["noot"].Children["mies"].Children["."].Count != 2 {
		t.Fatalf("aap chain = %+v", aap)
	}
	if stats.TopWords != 2 || stats.TotalCount != 8 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestMigrateImportReadable(t *testing.T) {
	var dump map[string]any
	json.Unmarshal([]byte(migrateFixture), &dump)
	dicts, _, err := MigrateFromPerl(dump)
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemory()
	if err := store.Put("markov", "dictionary_3_default", dicts["dictionary_3_default"]); err != nil {
		t.Fatal(err)
	}
	var back map[string]*Node
	found, err := store.Get("markov", "dictionary_3_default", &back)
	if err != nil || !found {
		t.Fatalf("Get: %v %v", found, err)
	}
	if back["aap"].Children["noot"].Children["mies"].Count != 2 {
		t.Fatalf("round-trip = %+v", back["aap"])
	}
}
