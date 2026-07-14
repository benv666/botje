package rvf

import (
	"strings"
	"testing"
)

// the shipped corpus parses, is big enough to be fun, and stays within
// the board alphabet (a stray diacritic would render as an unguessable
// masked cell forever).
func TestBuiltinCorpus(t *testing.T) {
	min := map[string]int{"nl": 150, "en": 90}
	for lang, want := range min {
		puzzles, err := builtinPuzzles(lang)
		if err != nil {
			t.Fatalf("%s: %v", lang, err)
		}
		if len(puzzles) < want {
			t.Errorf("%s corpus has %d puzzles, want >= %d", lang, len(puzzles), want)
		}
		for _, p := range puzzles {
			if p.Category == "" {
				t.Fatalf("%s: empty category for %q", lang, p.Text)
			}
			up := strings.ToUpper(p.Text)
			for _, r := range up {
				ok := (r >= 'A' && r <= 'Z') || r == ' ' || r == ',' || r == '\''
				if !ok {
					t.Errorf("%s: %q contains %q, outside the board alphabet", lang, p.Text, r)
				}
			}
		}
	}
}

func TestParseCorpusRejectsGarbage(t *testing.T) {
	if _, err := parseCorpus("no separator here\n"); err == nil {
		t.Fatal("line without category accepted")
	}
	puzzles, err := parseCorpus("# comment\n\nDing: Een fiets\n")
	if err != nil || len(puzzles) != 1 || puzzles[0].Category != "Ding" {
		t.Fatalf("puzzles = %+v, err %v", puzzles, err)
	}
}
