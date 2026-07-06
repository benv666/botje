package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"go-botje/internal/storage"
	"go-botje/modules/markov"
)

const markovFixture = `{
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

func TestMarkovTransform(t *testing.T) {
	var dump map[string]any
	if err := json.Unmarshal([]byte(markovFixture), &dump); err != nil {
		t.Fatal(err)
	}
	dicts, stats, err := markovFromPerl(dump)
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

func TestMarkovImportReadableByModule(t *testing.T) {
	var dump map[string]any
	json.Unmarshal([]byte(markovFixture), &dump)
	dicts, _, err := markovFromPerl(dump)
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemory()
	if err := store.Put("markov", "dictionary_3_default", dicts["dictionary_3_default"]); err != nil {
		t.Fatal(err)
	}
	var back map[string]*markov.Node
	found, err := store.Get("markov", "dictionary_3_default", &back)
	if err != nil || !found {
		t.Fatalf("Get: %v %v", found, err)
	}
	if back["aap"].Children["noot"].Children["mies"].Count != 2 {
		t.Fatalf("round-trip = %+v", back["aap"])
	}
}

// TestMarkovRealData migrates the 29 MB live dictionary when the
// reference tree is present and verifies counts.
func TestMarkovRealData(t *testing.T) {
	dat := "../../reference/mounts/data/IRC_Markov.dat"
	if _, err := os.Stat(dat); err != nil {
		t.Skip("reference data not present")
	}
	if testing.Short() {
		t.Skip("short mode")
	}
	out, err := exec.Command("perl", "dump.pl", dat).Output()
	if err != nil {
		t.Fatalf("dump.pl: %v", err)
	}
	var dump map[string]any
	if err := json.Unmarshal(out, &dump); err != nil {
		t.Fatalf("dump not valid json: %v", err)
	}
	dicts, stats, err := markovFromPerl(dump)
	if err != nil {
		t.Fatal(err)
	}
	// live facts 2026-07-06: one dictionary, 27617 top words
	if len(stats.Dictionaries) != 1 || stats.Dictionaries[0] != "dictionary_3_default" {
		t.Errorf("dictionaries = %v", stats.Dictionaries)
	}
	if stats.TopWords < 27000 {
		t.Errorf("top words = %d, expected the live ~27617", stats.TopWords)
	}
	// recount the raw dump independently: every top word carried over
	raw := dump["dictionary_3_default"].(map[string]any)
	rawWords := 0
	for w := range raw {
		if len(w) < 2 || w[:2] != "__" {
			rawWords++
		}
	}
	if rawWords != len(dicts["dictionary_3_default"]) {
		t.Errorf("dump has %d words, transformed %d", rawWords, len(dicts["dictionary_3_default"]))
	}
	t.Logf("migrated: %d top words, total count %d", stats.TopWords, stats.TotalCount)
}
