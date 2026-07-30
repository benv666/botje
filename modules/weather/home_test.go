package weather

import (
	"strings"
	"testing"

	"go-botje/internal/storage"
)

// homeFixture keys the geocoder per place (the catch-all fixture races
// the specific keys in map order) and shares a store so persistence can
// be checked across a reload.
func homeFixture(t *testing.T, store storage.Store) *fixture {
	f := newFixture(t, store)
	delete(f.body, geoURL)
	f.body["name=hauwert"] = geoHauwert
	f.body["name=alkmaar"] = geoAlkmaar
	f.body["name=utrecht"] = geoUtrecht
	f.body["name=nergenshuizen"] = geoNiks
	return f
}

func TestParseHome(t *testing.T) {
	for _, tc := range []struct {
		arg    string
		place  string
		set    bool
		isHome bool
	}{
		{"home=alkmaar", "alkmaar", true, true},
		{"HOME=Alkmaar", "Alkmaar", true, true},
		{"home alkmaar", "alkmaar", true, true},
		{"thuis=wijk aan zee", "wijk aan zee", true, true},
		{"home = alkmaar", "alkmaar", true, true},
		{"home=", "", true, true},   // clear
		{"home", "", false, true},   // show
		{"thuis", "", false, true},  // show
		{"homerville", "", false, false}, // a place that merely starts with home
		{"alkmaar", "", false, false},
		{"", "", false, false},
	} {
		place, set, isHome := parseHome(tc.arg)
		if place != tc.place || set != tc.set || isHome != tc.isHome {
			t.Fatalf("parseHome(%q) = (%q, %v, %v), want (%q, %v, %v)",
				tc.arg, place, set, isHome, tc.place, tc.set, tc.isHome)
		}
	}
}

// Bram's request (live 2026-07-16: "zorg eens dat die bot rekening
// houdt met nick oid"): your own place, set once, used by every weather
// command. Other nicks keep the configured default.
func TestHomePerNick(t *testing.T) {
	f := homeFixture(t, storage.NewMemory())
	f.msg("Bram", "#testing", "!weer home=alkmaar")
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "Alkmaar") {
		t.Fatalf("set home: %q", got)
	}
	// bare {b} is navy paint (the live complaint, twice); {B}{b} is the
	// name highlight, and every tag closes
	if strings.Count(got[0], "{b}") != strings.Count(got[0], "{B}{b}") ||
		strings.Count(got[0], "{/}") != strings.Count(got[0], "{B}{b}") {
		t.Fatalf("unbalanced color tags: %q", got[0])
	}
	f.msg("Bram", "#testing", "!weer")
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0], "{B}{b}Alkmaar{/}") {
		t.Fatalf("weer after home=alkmaar: %q", got)
	}
	// nicks are matched case-insensitively: same person either way
	f.msg("bram", "#testing", "!weer")
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0], "Alkmaar") {
		t.Fatalf("weer for lowercased nick: %q", got)
	}
	f.msg("BenV", "#testing", "!weer")
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0], "Hauwert") {
		t.Fatalf("other nick should still get conf home: %q", got)
	}
	// the other faces default to it too
	f.msg("Bram", "#testing", "!weer full")
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0], "Alkmaar") {
		t.Fatalf("weer full: %q", got)
	}
	f.msg("Bram", "#testing", "!regen")
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0], "Alkmaar") {
		t.Fatalf("regen: %q", got)
	}
	f.msg("Bram", "#testing", "!weerdiff utrecht")
	got = f.take()
	if len(got) != 1 || !strings.Contains(got[0], "Alkmaar") {
		t.Fatalf("weerdiff: %q", got)
	}
	if strings.Index(got[0], "Alkmaar") > strings.Index(got[0], "Utrecht") {
		t.Fatalf("own place should read first: %q", got[0])
	}
}

func TestHomeSurvivesReload(t *testing.T) {
	store := storage.NewMemory()
	f := homeFixture(t, store)
	f.msg("Bram", "#testing", "!weer home=alkmaar")
	f.take()

	f2 := homeFixture(t, store)
	f2.msg("Bram", "#testing", "!weer")
	if got := f2.take(); len(got) != 1 || !strings.Contains(got[0], "Alkmaar") {
		t.Fatalf("home did not survive a reload: %q", got)
	}
}

func TestHomeShowAndClear(t *testing.T) {
	f := homeFixture(t, storage.NewMemory())
	f.msg("Bram", "#testing", "!weer home")
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "Hauwert") || !strings.Contains(got[0], "home=") {
		t.Fatalf("show without own place: %q", got)
	}
	f.msg("Bram", "#testing", "!weer home=alkmaar")
	f.take()
	f.msg("Bram", "#testing", "!regen thuis")
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0], "Alkmaar") {
		t.Fatalf("show own place: %q", got)
	}
	f.msg("Bram", "#testing", "!weer home=")
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0], "Hauwert") {
		t.Fatalf("clear should name the fallback: %q", got)
	}
	f.msg("Bram", "#testing", "!weer")
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0], "Hauwert") {
		t.Fatalf("weer after clear: %q", got)
	}
}

// A home that does not geocode is refused, not stored: otherwise every
// later !weer answers "Ken ik niet".
func TestHomeUnknownPlaceRefused(t *testing.T) {
	f := homeFixture(t, storage.NewMemory())
	f.msg("Bram", "#testing", "!weer home=nergenshuizen")
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0], "Ken ik niet: nergenshuizen") {
		t.Fatalf("unknown home: %q", got)
	}
	f.msg("Bram", "#testing", "!weer")
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0], "Hauwert") {
		t.Fatalf("refused home must not stick: %q", got)
	}
}

// The usage line points at the setting, and names the caller's own
// place once they have one.
func TestUsageMentionsHome(t *testing.T) {
	f := homeFixture(t, storage.NewMemory())
	f.msg("Bram", "#testing", "!weer ?")
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "home=") || !strings.Contains(got[0], "Hauwert") {
		t.Fatalf("usage: %q", got)
	}
	f.msg("Bram", "#testing", "!weer home=alkmaar")
	f.take()
	f.msg("Bram", "#testing", "!weer ?")
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0], "Alkmaar") {
		t.Fatalf("usage should name the caller's own place: %q", got)
	}
}
