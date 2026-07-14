package markov

import (
	"reflect"
	"testing"
)

func TestCompactRoundTrip(t *testing.T) {
	n := &Node{Count: 5, Children: map[string]*Node{
		"noot": {Count: 3, Children: map[string]*Node{
			"mies": {Count: 2},
			".":    {Count: 1},
		}},
		"teun": {Count: 2},
	}}
	back := fromCompact(toCompact(n))
	if !reflect.DeepEqual(n, back) {
		t.Fatalf("round trip lost data:\n in: %+v\nout: %+v", n, back)
	}
}

func TestCompactChildOps(t *testing.T) {
	nd := &cnode{}
	// insert out of id order, read back
	c := wordTab.id("ccc")
	a := wordTab.id("aaa")
	nd.ensureChild(c).count = 3
	nd.ensureChild(a).count = 1
	if got := nd.child(a); got == nil || got.count != 1 {
		t.Fatalf("child(a) = %+v", got)
	}
	if got := nd.child(c); got == nil || got.count != 3 {
		t.Fatalf("child(c) = %+v", got)
	}
	if nd.child(wordTab.id("nope")) != nil {
		t.Fatal("missing child found")
	}
	// ensure is idempotent
	if nd.ensureChild(a).count != 1 {
		t.Fatal("ensureChild replaced an existing child")
	}
	if len(nd.kids) != 2 {
		t.Fatalf("kids = %d, want 2", len(nd.kids))
	}
}

// sortedKids orders alphabetically by WORD, not by arrival id: the
// weighted pick's rand-to-child mapping must not change with the
// representation.
func TestSortedKidsAlphabetical(t *testing.T) {
	nd := &cnode{}
	// intern in non-alphabetical order so id order != word order
	for _, w := range []string{"zebra", "aap", "mies"} {
		nd.ensureChild(wordTab.id(w))
	}
	kids := nd.sortedKids()
	got := []string{}
	for _, k := range kids {
		got = append(got, wordTab.str(k.word))
	}
	want := []string{"aap", "mies", "zebra"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sortedKids = %v, want %v", got, want)
	}
}

// toCompact must deliver kids sorted by id: child() binary-searches,
// and JSON map iteration order is random (a missing sort here works by
// luck on single-child fixtures and corrupts lookups on real data).
func TestToCompactSortsKids(t *testing.T) {
	kidWords := []string{"zzz-late", "mmm-mid", "aaa-early", "qqq-q", "bbb-b"}
	children := map[string]*Node{}
	for _, w := range kidWords {
		wordTab.id(w) // interning order != alphabetical order
		children[w] = &Node{Count: 1}
	}
	// map iteration order is random: repeat so an unsorted conversion
	// cannot pass by luck
	for range 20 {
		nd := toCompact(&Node{Count: 5, Children: children})
		for i := 1; i < len(nd.kids); i++ {
			if nd.kids[i-1].word >= nd.kids[i].word {
				t.Fatalf("kids not sorted by id: %v", nd.kids)
			}
		}
		for _, w := range kidWords {
			id, _ := wordTab.lookup(w)
			if nd.child(id) == nil {
				t.Fatalf("child lookup failed for %q", w)
			}
		}
	}
}
