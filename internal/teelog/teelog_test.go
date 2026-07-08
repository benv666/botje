package teelog

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// records go to both sinks; the file side survives handler decoration
// (WithAttrs/WithGroup).
func TestTee(t *testing.T) {
	var a, b bytes.Buffer
	h := New(
		slog.NewTextHandler(&a, nil),
		slog.NewTextHandler(&b, nil),
	)
	log := slog.New(h).With("component", "test")
	log.Info("hello", "k", "v")
	for name, buf := range map[string]*bytes.Buffer{"primary": &a, "secondary": &b} {
		out := buf.String()
		if !strings.Contains(out, "hello") || !strings.Contains(out, "k=v") ||
			!strings.Contains(out, "component=test") {
			t.Errorf("%s sink missing record parts: %q", name, out)
		}
	}
}

// OpsLog opens (creating parents) an append-mode ops.log in dir and
// returns a handler teeing to it; a second run appends, not truncates.
func TestOpsLogAppends(t *testing.T) {
	dir := t.TempDir()
	var stderr bytes.Buffer
	for i := 0; i < 2; i++ {
		h, closefn, err := OpsLog(slog.NewTextHandler(&stderr, nil), filepath.Join(dir, "sub"))
		if err != nil {
			t.Fatal(err)
		}
		slog.New(h).Info("boot", "run", i)
		closefn()
	}
	data, err := os.ReadFile(filepath.Join(dir, "sub", "ops.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "run=0") || !strings.Contains(string(data), "run=1") {
		t.Fatalf("ops.log = %q, want both runs", data)
	}
	if strings.Count(stderr.String(), "boot") != 2 {
		t.Fatalf("stderr sink missed records: %q", stderr.String())
	}
}
