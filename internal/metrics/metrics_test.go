package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExposition(t *testing.T) {
	r := New()
	r.SetGauge("botje_connected", nil, 1)
	r.IncCounter("botje_reconnects_total", nil)
	r.IncCounter("botje_reconnects_total", nil)
	r.SetCounter("botje_hook_calls_total", map[string]string{"module": "karma", "event": "IRC_PRIVMSG"}, 42)
	r.AddCounter("botje_admin_logins_total", map[string]string{"result": "ok"}, 3)

	var b strings.Builder
	r.WriteText(&b)
	out := b.String()

	for _, want := range []string{
		"# TYPE botje_connected gauge",
		"botje_connected 1",
		"# TYPE botje_reconnects_total counter",
		"botje_reconnects_total 2",
		`botje_hook_calls_total{event="IRC_PRIVMSG",module="karma"} 42`,
		`botje_admin_logins_total{result="ok"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition missing %q in:\n%s", want, out)
		}
	}
	// labels sorted for stable output; each TYPE line appears once
	if strings.Count(out, "# TYPE botje_hook_calls_total") != 1 {
		t.Errorf("duplicate TYPE line:\n%s", out)
	}
}

func TestCollectorRunsPerScrape(t *testing.T) {
	r := New()
	n := 0
	r.AddCollector(func() { n++; r.SetGauge("dynamic", nil, float64(n)) })

	var b1 strings.Builder
	r.WriteText(&b1)
	var b2 strings.Builder
	r.WriteText(&b2)
	if !strings.Contains(b1.String(), "dynamic 1") || !strings.Contains(b2.String(), "dynamic 2") {
		t.Fatalf("collector not run per scrape:\n%s---\n%s", b1.String(), b2.String())
	}
}

func TestLabelEscaping(t *testing.T) {
	r := New()
	r.SetGauge("g", map[string]string{"path": `a\b"c` + "\n"}, 1)
	var b strings.Builder
	r.WriteText(&b)
	if !strings.Contains(b.String(), `g{path="a\\b\"c\n"} 1`) {
		t.Fatalf("bad escaping: %s", b.String())
	}
}

func TestHandler(t *testing.T) {
	r := New()
	r.SetGauge("up", nil, 1)
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "up 1") {
		t.Fatalf("handler: %d %q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q", ct)
	}
}
