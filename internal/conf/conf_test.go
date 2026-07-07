package conf

import (
	"testing"
)

func TestCreateAndGetDefaults(t *testing.T) {
	c := New()
	c.CreateInt("anti_flood_max_lines", 4)
	c.CreateFloat("backoff_factor", 1.5)
	c.CreateString("nick", "Meretrix")
	c.CreateBool("autoconnect", true)

	if got := c.Int("anti_flood_max_lines"); got != 4 {
		t.Errorf("Int = %d, want 4", got)
	}
	if got := c.Float("backoff_factor"); got != 1.5 {
		t.Errorf("Float = %v, want 1.5", got)
	}
	if got := c.String("nick"); got != "Meretrix" {
		t.Errorf("String = %q, want Meretrix", got)
	}
	if got := c.Bool("autoconnect"); got != true {
		t.Errorf("Bool = %v, want true", got)
	}
}

func TestSetValidFiresOnChange(t *testing.T) {
	c := New()
	var changed []string
	c.OnChange = func(name string) { changed = append(changed, name) }
	c.CreateInt("lines", 4)

	if err := c.Set("lines", "6"); err != nil {
		t.Fatal(err)
	}
	if got := c.Int("lines"); got != 6 {
		t.Errorf("Int after Set = %d, want 6", got)
	}
	if len(changed) != 1 || changed[0] != "lines" {
		t.Errorf("OnChange calls = %v, want [lines]", changed)
	}
}

func TestSetInvalidValueRejected(t *testing.T) {
	c := New()
	fired := false
	c.OnChange = func(string) { fired = true }
	c.CreateInt("lines", 4)

	if err := c.Set("lines", "banana"); err == nil {
		t.Fatal("Set int=banana did not error")
	}
	if got := c.Int("lines"); got != 4 {
		t.Errorf("value changed to %d after rejected Set", got)
	}
	if fired {
		t.Error("OnChange fired for rejected Set")
	}
}

func TestSetUnknownName(t *testing.T) {
	c := New()
	if err := c.Set("nosuch", "1"); err == nil {
		t.Fatal("Set on unknown setting did not error")
	}
}

func TestBoolParsing(t *testing.T) {
	c := New()
	c.CreateBool("flag", false)
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"true", true}, {"1", true}, {"false", false}, {"0", false},
	} {
		if err := c.Set("flag", tc.in); err != nil {
			t.Fatalf("Set flag=%q: %v", tc.in, err)
		}
		if got := c.Bool("flag"); got != tc.want {
			t.Errorf("Bool after Set %q = %v, want %v", tc.in, got, tc.want)
		}
	}
	if err := c.Set("flag", "maybe"); err == nil {
		t.Error("Set bool=maybe did not error")
	}
}

func TestFloatSet(t *testing.T) {
	c := New()
	c.CreateFloat("f", 0.5)
	if err := c.Set("f", "2.25"); err != nil {
		t.Fatal(err)
	}
	if got := c.Float("f"); got != 2.25 {
		t.Errorf("Float = %v, want 2.25", got)
	}
}

func TestStoredValueAppliedAtCreate(t *testing.T) {
	c := New()
	c.LoadStored(map[string]string{"lines": "9"})
	c.CreateInt("lines", 4)
	if got := c.Int("lines"); got != 9 {
		t.Errorf("Int = %d, want stored 9 over default 4", got)
	}
}

func TestStoredInvalidFallsBackToDefault(t *testing.T) {
	c := New()
	c.LoadStored(map[string]string{"lines": "banana"})
	c.CreateInt("lines", 4)
	if got := c.Int("lines"); got != 4 {
		t.Errorf("Int = %d, want default 4 when stored value invalid", got)
	}
}

func TestFileOverrideWins(t *testing.T) {
	c := New()
	c.LoadStored(map[string]string{"nick": "stored"})
	c.SetFileOverrides(map[string]string{"nick": "filewins"})
	c.CreateString("nick", "default")

	if got := c.String("nick"); got != "filewins" {
		t.Errorf("String = %q, want file override", got)
	}
	// runtime Set still updates the stored value, but the override keeps winning
	if err := c.Set("nick", "runtime"); err != nil {
		t.Fatal(err)
	}
	if got := c.String("nick"); got != "filewins" {
		t.Errorf("String after Set = %q, want file override to keep winning", got)
	}
	if got := c.Dump()["nick"]; got != "runtime" {
		t.Errorf("Dump nick = %q, want runtime value persisted", got)
	}
}

func TestDumpLoadRoundTrip(t *testing.T) {
	c := New()
	c.CreateInt("lines", 4)
	c.CreateBool("flag", false)
	c.Set("lines", "7")
	c.Set("flag", "true")

	c2 := New()
	c2.LoadStored(c.Dump())
	c2.CreateInt("lines", 4)
	c2.CreateBool("flag", false)
	if got := c2.Int("lines"); got != 7 {
		t.Errorf("round-trip lines = %d, want 7", got)
	}
	if got := c2.Bool("flag"); got != true {
		t.Errorf("round-trip flag = %v, want true", got)
	}
}

func TestCreateIdempotentKeepsCurrentValue(t *testing.T) {
	c := New()
	c.CreateInt("lines", 4)
	c.Set("lines", "8")
	c.CreateInt("lines", 4) // module reload re-creates its settings
	if got := c.Int("lines"); got != 8 {
		t.Errorf("Int = %d, want 8 preserved across re-create", got)
	}
}

func TestWrongTypeGetterPanics(t *testing.T) {
	c := New()
	c.CreateString("nick", "x")
	defer func() {
		if recover() == nil {
			t.Error("Int on a string setting did not panic")
		}
	}()
	c.Int("nick")
}

func TestList(t *testing.T) {
	c := New()
	c.CreateInt("b", 1)
	c.CreateInt("a", 2)
	c.CreateInt("c", 3)
	got := c.List()
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("List = %v, want sorted [a b c]", got)
	}
}

// Stored returns everything a next run must LoadStored to keep telnet
// conf changes alive: explicitly Set values plus loaded values whose
// setting has not been re-created yet. Defaults never leak in.
func TestStoredRoundTrip(t *testing.T) {
	c := New()
	c.LoadStored(map[string]string{"sleeping_module_key": "abc"})
	c.CreateInt("lines", 4)
	c.CreateString("touched", "x")
	if err := c.Set("touched", "y"); err != nil {
		t.Fatal(err)
	}
	st := c.Stored()
	if st["touched"] != "y" {
		t.Fatalf("touched = %q, want y", st["touched"])
	}
	if st["sleeping_module_key"] != "abc" {
		t.Fatalf("unclaimed stored value lost: %v", st)
	}
	if _, ok := st["lines"]; ok {
		t.Fatalf("default leaked into stored: %v", st)
	}

	// second run: Stored feeds LoadStored, values survive
	c2 := New()
	c2.LoadStored(st)
	c2.CreateString("touched", "x")
	if got := c2.String("touched"); got != "y" {
		t.Fatalf("after roundtrip touched = %q, want y", got)
	}
}
