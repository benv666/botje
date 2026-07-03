package format

// Hand-written tests for deliberate divergences from the Perl code and
// for behavior the goldens cannot cover (see doc.go in this package).

import (
	"slices"
	"strings"
	"testing"
)

// The Perl wrapText corrupts UTF-8 when a multibyte word is joined with
// a following word ('use bytes' concat, live bug). Fixed in Go: same
// split points, intact bytes.
func TestWrapTextMultibyteJoinFixed(t *testing.T) {
	got := WrapText("mixed 🍺 beer éééé words here", 10)
	want := []string{"mixed", "🍺 beer", "éééé", "words", "here"}
	if !slices.Equal(got, want) {
		t.Errorf("WrapText = %q, want %q", got, want)
	}
}

// {i}/{u}/{I} are debug-log tags; the Perl subColorTags would replace
// them with literal words like "ignore" (latent bug, no module hits it).
// Go strips them.
func TestIgnoreTagsStripped(t *testing.T) {
	for _, f := range []func(string) string{ToANSI, ToIRC} {
		if got := f("{i}a{u}b{I}c"); got != "abc" {
			t.Errorf("ignore tags: got %q, want abc", got)
		}
	}
}

// {n} (deterministic per-word color) is only used on the console log
// path in Perl; no IRC module output uses it. Go strips the tag and
// keeps the word.
func TestColoredNameTagStripped(t *testing.T) {
	if got := ToIRC("{n}BenV said hi"); got != "BenV said hi" {
		t.Errorf("ToIRC({n}...) = %q, want tag stripped", got)
	}
}

func TestTruncateIRC(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"short unchanged", "hello", 510, "hello"},
		{"exact fit", strings.Repeat("a", 10), 10, strings.Repeat("a", 10)},
		{"ascii cut", strings.Repeat("a", 20), 10, strings.Repeat("a", 10)},
		{"no mid-rune cut", "aaaaaaaaa🍺", 12, "aaaaaaaaa"},
		{"rune exact fit", "aaaaaaaa🍺", 12, "aaaaaaaa🍺"},
		{"no mid-cluster cut", "aaaaaaaaéx", 10, "aaaaaaaa"},
		{"cluster fits whole", "aaaaaaaaé", 11, "aaaaaaaaé"},
		{"empty", "", 510, ""},
	} {
		if got := TruncateIRC(tc.in, tc.max); got != tc.want {
			t.Errorf("%s: TruncateIRC(%q, %d) = %q, want %q", tc.name, tc.in, tc.max, got, tc.want)
		}
	}
}

func TestWrapTextEmpty(t *testing.T) {
	if got := WrapText("", 448); len(got) != 0 {
		t.Errorf("WrapText(\"\") = %q, want empty", got)
	}
}
