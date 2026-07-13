package storage

import (
	"encoding/json"
	"slices"
	"testing"
)

// conformance runs the Store contract tests against any backend.
func conformance(t *testing.T, open func(t *testing.T) Store) {
	t.Run("GetMissing", func(t *testing.T) {
		s := open(t)
		var v map[string]int
		found, err := s.Get("karma", "nosuch", &v)
		if err != nil {
			t.Fatal(err)
		}
		if found {
			t.Fatal("Get of missing name reported found")
		}
	})

	t.Run("PutGetRoundTrip", func(t *testing.T) {
		s := open(t)
		type inner struct {
			Score int      `json:"score"`
			Tags  []string `json:"tags"`
		}
		in := map[string]inner{
			"beer":    {Score: 42, Tags: []string{"drink", "good"}},
			"unicode": {Score: -1, Tags: []string{"héhé", "ünïcode", "🍺"}},
		}
		if err := s.Put("karma", "items", in); err != nil {
			t.Fatal(err)
		}
		var out map[string]inner
		found, err := s.Get("karma", "items", &out)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatal("Get after Put: not found")
		}
		if out["beer"].Score != 42 || out["unicode"].Tags[2] != "🍺" {
			t.Fatalf("round-trip mangled data: %+v", out)
		}
	})

	t.Run("PutManyGetAll", func(t *testing.T) {
		s := open(t)
		batch := map[string]any{
			"w:beer": map[string]int{"c": 3},
			"w:kaas": map[string]int{"c": 1},
		}
		if err := s.PutMany("markov", batch); err != nil {
			t.Fatal(err)
		}
		if err := s.Put("other", "unrelated", 1); err != nil {
			t.Fatal(err)
		}
		all, err := s.GetAll("markov")
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 2 {
			t.Fatalf("GetAll = %d entries, want 2 (namespaces leaked?)", len(all))
		}
		var n map[string]int
		if err := json.Unmarshal(all["w:beer"], &n); err != nil || n["c"] != 3 {
			t.Fatalf("GetAll[w:beer] = %s (%v)", all["w:beer"], err)
		}
		// PutMany upserts: overwrite one, add one
		if err := s.PutMany("markov", map[string]any{
			"w:beer": map[string]int{"c": 4},
			"w:bier": map[string]int{"c": 9},
		}); err != nil {
			t.Fatal(err)
		}
		all, err = s.GetAll("markov")
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(all["w:beer"], &n); err != nil || n["c"] != 4 || len(all) != 3 {
			t.Fatalf("after upsert: %d entries, w:beer=%s", len(all), all["w:beer"])
		}
	})

	t.Run("GetAllEmpty", func(t *testing.T) {
		s := open(t)
		all, err := s.GetAll("void")
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 0 {
			t.Fatalf("GetAll of empty ns = %v", all)
		}
	})

	t.Run("PutOverwrites", func(t *testing.T) {
		s := open(t)
		if err := s.Put("ns", "k", 1); err != nil {
			t.Fatal(err)
		}
		if err := s.Put("ns", "k", 2); err != nil {
			t.Fatal(err)
		}
		var v int
		if _, err := s.Get("ns", "k", &v); err != nil {
			t.Fatal(err)
		}
		if v != 2 {
			t.Fatalf("v = %d, want 2 (upsert)", v)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		s := open(t)
		s.Put("ns", "k", 1)
		if err := s.Delete("ns", "k"); err != nil {
			t.Fatal(err)
		}
		var v int
		found, _ := s.Get("ns", "k", &v)
		if found {
			t.Fatal("value survived Delete")
		}
		if err := s.Delete("ns", "k"); err != nil {
			t.Fatal("Delete of missing name errored (must be idempotent):", err)
		}
	})

	t.Run("NamesSorted", func(t *testing.T) {
		s := open(t)
		for _, n := range []string{"zeta", "alpha", "mid"} {
			s.Put("rss", n, 1)
		}
		s.Put("other", "x", 1)
		got, err := s.Names("rss")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, []string{"alpha", "mid", "zeta"}) {
			t.Fatalf("Names = %v, want sorted names of ns only", got)
		}
		empty, err := s.Names("void")
		if err != nil {
			t.Fatal(err)
		}
		if len(empty) != 0 {
			t.Fatalf("Names of empty ns = %v", empty)
		}
	})

	t.Run("NamespaceIsolation", func(t *testing.T) {
		s := open(t)
		s.Put("karma", "k", "from-karma")
		s.Put("ego", "k", "from-ego")
		var v string
		s.Get("ego", "k", &v)
		if v != "from-ego" {
			t.Fatalf("ego/k = %q, namespaces leak", v)
		}
		s.Delete("karma", "k")
		found, _ := s.Get("ego", "k", &v)
		if !found {
			t.Fatal("delete in one namespace removed the other's value")
		}
	})
}

func TestMemoryConformance(t *testing.T) {
	conformance(t, func(t *testing.T) Store {
		return NewMemory()
	})
}

func TestInstrument(t *testing.T) {
	type call struct {
		op, ns string
	}
	var calls []call
	s := Instrument(NewMemory(), func(op, ns string, seconds float64) {
		if seconds < 0 {
			t.Fatalf("negative duration for %s/%s", op, ns)
		}
		calls = append(calls, call{op, ns})
	})

	if err := s.Put("karma", "x", 1); err != nil {
		t.Fatal(err)
	}
	var v int
	if ok, err := s.Get("karma", "x", &v); !ok || err != nil || v != 1 {
		t.Fatalf("instrumented Get = %v %v %d, passthrough broken", ok, err, v)
	}
	if _, err := s.Names("karma"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("karma", "x"); err != nil {
		t.Fatal(err)
	}
	want := []call{{"put", "karma"}, {"get", "karma"}, {"names", "karma"}, {"delete", "karma"}}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("call %d = %v, want %v", i, calls[i], want[i])
		}
	}
}
