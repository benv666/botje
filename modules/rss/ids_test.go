package rss

import "testing"

func TestIDEncoding(t *testing.T) {
	// counter 0 is A, then B, ...; base-32 alphabet A-Z 0 2 4 6 8 9
	alloc := newIDAlloc(0, nil)
	for _, want := range []string{"A", "B", "C"} {
		if got := alloc.next(); got != want {
			t.Fatalf("next = %q, want %q", got, want)
		}
	}
	// 32 -> two digits: "BA" (1,0)
	alloc = newIDAlloc(32, nil)
	if got := alloc.next(); got != "BA" {
		t.Fatalf("id(32) = %q, want BA", got)
	}
	// index 26..31 map to 0 2 4 6 8 9
	alloc = newIDAlloc(26, nil)
	if got := alloc.next(); got != "0" {
		t.Fatalf("id(26) = %q, want 0", got)
	}
	alloc = newIDAlloc(31, nil)
	if got := alloc.next(); got != "9" {
		t.Fatalf("id(31) = %q, want 9", got)
	}
}

func TestIDWrapsAt32768(t *testing.T) {
	alloc := newIDAlloc(32767, nil)
	alloc.next()
	if got := alloc.counter(); got != 1 {
		t.Fatalf("counter after wrap = %d, want 1", got)
	}
	if got := alloc.next(); got != "B" {
		t.Fatalf("first id after wrap = %q, want B (counter 1)", got)
	}
}

func TestIDSkipsCommandWords(t *testing.T) {
	// find the counter that would encode to "add" and start just before
	// it; the allocator must skip it. Alphabet index: a maps from 'A';
	// easier: scan a full cycle and assert no reserved word comes out.
	alloc := newIDAlloc(0, nil)
	for range 40000 {
		got := alloc.next()
		if rssCommands[got] {
			t.Fatalf("allocator produced reserved word %q", got)
		}
	}
}

// The !LI6 fix: ids still referenced by live items are skipped after
// the counter wraps (the perl reused them, and recall resolved to the
// first feed alphabetically).
func TestIDSkipsLiveIDs(t *testing.T) {
	live := map[string]bool{"B": true, "C": true}
	alloc := newIDAlloc(1, func(id string) bool { return live[id] })
	if got := alloc.next(); got != "D" {
		t.Fatalf("next = %q, want D (B and C are live)", got)
	}
}
