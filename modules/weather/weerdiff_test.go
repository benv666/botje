package weather

import (
	"strings"
	"testing"

	"go-botje/internal/storage"
)

const geoUtrecht = `{"results":[{"name":"Utrecht","latitude":52.09,"longitude":5.12,"country_code":"NL","admin1":"Utrecht"}]}`
const geoWijkAanZee = `{"results":[{"name":"Wijk aan Zee","latitude":52.49,"longitude":4.59,"country_code":"NL","admin1":"Noord-Holland"}]}`

// diffFixture swaps the catch-all geocode fixture for per-place ones:
// the catch-all would race the specific keys in map order.
func diffFixture(t *testing.T) *fixture {
	f := newFixture(t, storage.NewMemory())
	delete(f.body, geoURL)
	f.body["name=hauwert"] = geoHauwert
	f.body["name=utrecht"] = geoUtrecht
	f.body["name=wijk+aan+zee"] = geoWijkAanZee
	f.body["name=barcelona"] = geoBarcelona
	return f
}

// two dutch places, both on stations: every variable pairs up and the
// warmer place wins the verdict.
func TestWeerdiffTwoStations(t *testing.T) {
	f := diffFixture(t)
	f.msg("Bram", "#testing", "!weerdiff hauwert utrecht")
	got := f.take()
	if len(got) != 1 {
		t.Fatalf("sent = %q", got)
	}
	for _, want := range []string{
		"{B}{b}Hauwert{/}", "{B}{b}Utrecht{/}",
		"24.6°C", "21.5°C", // Berkhout vs De Bilt
		"39%", "80%",
		"NNO", "ZW",
		"vrijwel onbewolkt vs zwaar bewolkt",
		"Hauwert is 3.1°C warmer",
	} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("diff missing %q: %q", want, got[0])
		}
	}
}

// a single argument compares against weather_home.
func TestWeerdiffDefaultsToHome(t *testing.T) {
	f := diffFixture(t)
	f.msg("Bram", "#testing", "!weerdiff utrecht")
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "Hauwert") || !strings.Contains(got[0], "Utrecht") {
		t.Fatalf("home default: %q", got)
	}
	if strings.Index(got[0], "Hauwert") > strings.Index(got[0], "Utrecht") {
		t.Fatalf("home should read first: %q", got[0])
	}
}

// comma separates multi-word places; the same station on both sides
// means equal numbers and a draw.
func TestWeerdiffCommaAndDraw(t *testing.T) {
	f := diffFixture(t)
	f.msg("Bram", "#testing", "!weerdiff hauwert, wijk aan zee")
	got := f.take()
	if len(got) != 1 {
		t.Fatalf("sent = %q", got)
	}
	for _, want := range []string{"Wijk aan Zee", "allebei vrijwel onbewolkt", "even warm"} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("draw diff missing %q: %q", want, got[0])
		}
	}
	// identical numbers made BenV cry foul ("geloof er niks van",
	// !weerdiff hauwert aartswoud live 2026-07-15): both sides must show
	// their source, which here is the same station twice
	if strings.Count(got[0], "meetstation Berkhout") != 2 {
		t.Fatalf("per-side station attribution missing: %q", got[0])
	}
}

// abroad goes through open-meteo, mixed with a station on the other
// side.
func TestWeerdiffAbroad(t *testing.T) {
	f := diffFixture(t)
	f.msg("Bram", "#testing", "!weerdiff hauwert barcelona")
	got := f.take()
	if len(got) != 1 {
		t.Fatalf("sent = %q", got)
	}
	for _, want := range []string{
		"24.6°C", "29.2°C", "Barcelona is 4.6°C warmer",
		"(meetstation Berkhout, 10km)", "(Catalonia, Spanje, open-meteo)",
	} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("abroad diff missing %q: %q", want, got[0])
		}
	}
}

// garbage gets usage, unknown places explain themselves.
func TestWeerdiffUsage(t *testing.T) {
	f := diffFixture(t)
	for _, in := range []string{"!weerdiff", "!weerdiff ?", "!weerdiff a b c d"} {
		f.msg("Bram", "#testing", in)
		got := f.take()
		if len(got) != 1 || !strings.Contains(got[0], "weerdiff") {
			t.Fatalf("%q -> %q, want usage", in, got)
		}
	}
	f.body["name=nergenshuizen"] = geoNiks
	f.msg("Bram", "#testing", "!weerdiff hauwert nergenshuizen")
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0], "Ken ik niet: nergenshuizen") {
		t.Fatalf("unknown place: %q", got)
	}
}
