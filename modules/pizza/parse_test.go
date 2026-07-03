package pizza

import (
	"strings"
	"testing"
	"time"
)

// noon on a Friday, local time
var now = time.Date(2026, 7, 3, 12, 0, 0, 0, time.Local)

func parseAt(t *testing.T, spec string) (*timeInfo, string) {
	t.Helper()
	ids := newIDPool(func() float64 { return 0 }) // alpha-0 forever
	ti, errMsg := parseTime(spec, now, ids)
	return ti, errMsg
}

func TestParseRelative(t *testing.T) {
	ti, errMsg := parseAt(t, "+5m ")
	if ti == nil {
		t.Fatal(errMsg)
	}
	if want := now.Add(5 * time.Minute); !ti.abs.Equal(want) {
		t.Fatalf("abs = %v, want %v", ti.abs, want)
	}
	if ti.message != "QUICK! Pizza alpha-0 is burning!" {
		t.Fatalf("message = %q", ti.message)
	}
}

func TestParseRelativeCombination(t *testing.T) {
	// 1d + 2h - 30m, sign accumulates per unit like the perl
	ti, errMsg := parseAt(t, "+1d 2h -30m ")
	if ti == nil {
		t.Fatal(errMsg)
	}
	if want := now.Add(24*time.Hour + 2*time.Hour - 30*time.Minute); !ti.abs.Equal(want) {
		t.Fatalf("abs = %v, want %v", ti.abs, want)
	}
}

func TestParseAbsoluteTimeToday(t *testing.T) {
	ti, errMsg := parseAt(t, "18:30 eten halen")
	if ti == nil {
		t.Fatal(errMsg)
	}
	if want := time.Date(2026, 7, 3, 18, 30, 0, 0, time.Local); !ti.abs.Equal(want) {
		t.Fatalf("abs = %v, want %v", ti.abs, want)
	}
	if ti.message != "eten halen" {
		t.Fatalf("message = %q", ti.message)
	}
}

func TestParseAbsoluteTimePastRollsToTomorrow(t *testing.T) {
	ti, errMsg := parseAt(t, "9:15 ochtend")
	if ti == nil {
		t.Fatal(errMsg)
	}
	if want := time.Date(2026, 7, 4, 9, 15, 0, 0, time.Local); !ti.abs.Equal(want) {
		t.Fatalf("abs = %v, want tomorrow %v", ti.abs, want)
	}
}

func TestParseAbsoluteDateAndTime(t *testing.T) {
	ti, errMsg := parseAt(t, "24-12-2026 20:00 kerst")
	if ti == nil {
		t.Fatal(errMsg)
	}
	if want := time.Date(2026, 12, 24, 20, 0, 0, 0, time.Local); !ti.abs.Equal(want) {
		t.Fatalf("abs = %v, want %v", ti.abs, want)
	}
}

func TestParsePastDateRefused(t *testing.T) {
	ti, errMsg := parseAt(t, "1-1-2020 12:00 te laat")
	if ti != nil {
		t.Fatalf("parsed %v, want refusal", ti.abs)
	}
	if errMsg != "This bot travels forward in time." {
		t.Fatalf("error = %q", errMsg)
	}
}

func TestParseWeekday(t *testing.T) {
	// now is Friday; monday means next monday
	ti, errMsg := parseAt(t, "monday 9:00 standup")
	if ti == nil {
		t.Fatal(errMsg)
	}
	if want := time.Date(2026, 7, 6, 9, 0, 0, 0, time.Local); !ti.abs.Equal(want) {
		t.Fatalf("abs = %v, want next monday %v", ti.abs, want)
	}
	// today's weekday goes to next week
	ti, errMsg = parseAt(t, "fr 9:00 volgende week")
	if ti == nil {
		t.Fatal(errMsg)
	}
	if want := time.Date(2026, 7, 10, 9, 0, 0, 0, time.Local); !ti.abs.Equal(want) {
		t.Fatalf("abs = %v, want friday next week %v", ti.abs, want)
	}
}

func TestParseRepeat(t *testing.T) {
	ti, errMsg := parseAt(t, "+1h r{1d} dagelijks ding")
	if ti == nil {
		t.Fatal(errMsg)
	}
	if len(ti.repeat) != 1 || ti.repeat[0].Unit != "d" || ti.repeat[0].N != 1 {
		t.Fatalf("repeat = %+v", ti.repeat)
	}
	if ti.message != "dagelijks ding" {
		t.Fatalf("message = %q", ti.message)
	}
}

func TestParseRepeatInvalid(t *testing.T) {
	ti, errMsg := parseAt(t, "+1h r{kaas} x")
	if ti != nil || errMsg != "Unable to parse repeat part" {
		t.Fatalf("ti=%v err=%q", ti, errMsg)
	}
}

func TestParseGarbage(t *testing.T) {
	for _, in := range []string{"volkomen onzin", "", "later misschien"} {
		ti, errMsg := parseAt(t, in)
		if ti != nil {
			t.Fatalf("parsed %q", in)
		}
		if errMsg != "Unable to parse date/time part" {
			t.Fatalf("error for %q = %q", in, errMsg)
		}
	}
}

func TestParseBadToken(t *testing.T) {
	// time-part regex accepts the chars, token parse then rejects
	ti, errMsg := parseAt(t, "12:34:56:78 x")
	if ti != nil || !strings.HasPrefix(errMsg, "Unable to parse ") {
		t.Fatalf("ti=%v err=%q", ti, errMsg)
	}
}

func TestIDPool(t *testing.T) {
	seq := []float64{0, 0, 0.5, 0.99}
	i := 0
	ids := newIDPool(func() float64 { v := seq[i%len(seq)]; i++; return v })
	first := ids.get()
	if first != "alpha-0" {
		t.Fatalf("first id = %q", first)
	}
	second := ids.get() // rand 0.5,0.99 -> different name
	if second == first {
		t.Fatalf("second id %q collides", second)
	}
	ids.clear(first)
	i = 0
	if got := ids.get(); got != "alpha-0" {
		t.Fatalf("id not reusable after clear: %q", got)
	}
}
