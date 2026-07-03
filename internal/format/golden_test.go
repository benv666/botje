package format

// Golden parity tests: testdata/golden.json is generated from the REAL
// Perl botje code by testdata/golden-gen.pl (needs the gitignored
// reference/ tree). Do not edit the json by hand.

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

type golden struct {
	Colorize []struct {
		In   string `json:"in"`
		ANSI string `json:"ansi"`
		MIRC string `json:"mirc"`
	} `json:"colorize"`
	WrapText []struct {
		In  string   `json:"in"`
		Max int      `json:"max"`
		Out []string `json:"out"`
	} `json:"wraptext"`
	SplitMessageData []struct {
		Prefix string   `json:"prefix"`
		Msg    string   `json:"msg"`
		Out    []string `json:"out"`
	} `json:"splitmessagedata"`
	Sparkline []struct {
		In   []float64 `json:"in"`
		Rows int       `json:"rows"`
		Out  []string  `json:"out"`
	} `json:"sparkline"`
	Tfcprint []struct {
		Format string   `json:"format"`
		Color  any      `json:"color"`
		Args   []string `json:"args"`
		Mode   any      `json:"mode"`
		Out    string   `json:"out"`
	} `json:"tfcprint"`
}

func loadGolden(t *testing.T) *golden {
	t.Helper()
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return &g
}

func TestColorizeGolden(t *testing.T) {
	for _, c := range loadGolden(t).Colorize {
		if got := ToANSI(c.In); got != c.ANSI {
			t.Errorf("ToANSI(%q) = %q, want %q", c.In, got, c.ANSI)
		}
		if got := ToIRC(c.In); got != c.MIRC {
			t.Errorf("ToIRC(%q) = %q, want %q", c.In, got, c.MIRC)
		}
	}
}

func TestWrapTextGolden(t *testing.T) {
	for _, c := range loadGolden(t).WrapText {
		if got := WrapText(c.In, c.Max); !slices.Equal(got, c.Out) {
			t.Errorf("WrapText(%q, %d) = %q, want %q", c.In, c.Max, got, c.Out)
		}
	}
}

func TestSplitMessageGolden(t *testing.T) {
	for _, c := range loadGolden(t).SplitMessageData {
		if got := SplitMessage(c.Prefix, c.Msg); !slices.Equal(got, c.Out) {
			t.Errorf("SplitMessage(%q, %d-byte msg) = %q, want %q", c.Prefix, len(c.Msg), got, c.Out)
		}
	}
}

func TestSparklineGolden(t *testing.T) {
	for _, c := range loadGolden(t).Sparkline {
		if got := Sparkline(c.In, c.Rows); !slices.Equal(got, c.Out) {
			t.Errorf("Sparkline(%v, %d) = %q, want %q", c.In, c.Rows, got, c.Out)
		}
	}
}

func TestTableGolden(t *testing.T) {
	for _, c := range loadGolden(t).Tfcprint {
		var got string
		if len(c.Args) > 0 {
			color, _ := c.Color.(string)
			got = TableRow(c.Format, color, c.Args)
		} else {
			got = TableSep(c.Format)
		}
		if got != c.Out {
			t.Errorf("table(%q, %v, %q) = %q, want %q", c.Format, c.Color, c.Args, got, c.Out)
		}
	}
}
