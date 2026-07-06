package wolframalpha

import (
	"strings"
	"testing"

	"go-botje/internal/bus"
	"go-botje/internal/conf"
	"go-botje/internal/fetch"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
	"go-botje/internal/storage"
)

type fixture struct {
	m       *Module
	b       *bus.Bus
	cmds    *cmd.Registry
	cf      *conf.Conf
	sent    []string
	fetched []string
	cbs     map[string]func(fetch.Result)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{cbs: map[string]func(fetch.Result){}}
	f.b = bus.New()
	f.b.RegisterEvent("IRC_PRIVMSG")
	f.cmds = cmd.New()
	f.cf = conf.New()
	f.m = New()
	f.m.fetch = func(url string, opts fetch.Options, cb func(fetch.Result)) bool {
		f.fetched = append(f.fetched, url)
		f.cbs[url] = cb
		return true
	}
	err := f.m.Load(&module.Context{
		Bus: f.b, Cmd: f.cmds, Conf: f.cf, Store: storage.NewMemory(),
		Privmsg: func(ch, msg string) { f.sent = append(f.sent, ch+"|"+msg) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) wa(nick, query string) {
	msg := "!wa"
	if query != "" {
		msg += " " + query
	}
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: "#testing",
		Msg: msg, Extra: map[string]any{}}
	ev.Sender.Nick = nick
	f.b.Submit(ev)
	f.cmds.Handle(ev)
}

func (f *fixture) take() []string {
	s := f.sent
	f.sent = nil
	return s
}

const waXML = `<?xml version="1.0"?>
<queryresult success="true" error="false">
  <pod title="Input interpretation">
    <subpod title=""><plaintext>2+2</plaintext></subpod>
  </pod>
  <pod title="Result">
    <subpod title=""><plaintext>4</plaintext></subpod>
  </pod>
  <pod title="Number line">
    <subpod title=""><plaintext></plaintext></subpod>
  </pod>
</queryresult>`

func TestNoAppIDRefuses(t *testing.T) {
	f := newFixture(t)
	f.wa("BenV", "2+2")
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "no wolframalpha appid configured") {
		t.Fatalf("keyless = %q", got)
	}
	if len(f.fetched) != 0 {
		t.Fatalf("fetched without appid: %q", f.fetched)
	}
}

func TestQuery(t *testing.T) {
	f := newFixture(t)
	f.cf.Set("wolframalpha_appid", "APP-ID")
	f.wa("BenV", "2+2")
	wantURL := "http://api.wolframalpha.com/v1/query.jsp?appid=APP-ID&input=2%2B2"
	if len(f.fetched) != 1 || f.fetched[0] != wantURL {
		t.Fatalf("fetched = %q", f.fetched)
	}
	f.cbs[wantURL](fetch.Result{Status: 200, Body: []byte(waXML)})
	got := f.take()
	// empty plaintexts skipped, rows color-cycled, joined with {/},
	want := "#testing|BenV: {m}2+2{/}, {r}4"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("reply = %q\nwant %q", got, want)
	}
}

func TestLongRowTruncated(t *testing.T) {
	f := newFixture(t)
	f.cf.Set("wolframalpha_appid", "A")
	f.wa("BenV", "lang")
	long := strings.Repeat("x", 100)
	f.cbs[f.fetched[0]](fetch.Result{Status: 200, Body: []byte(
		`<queryresult success="true"><pod><subpod><plaintext>` + long + `</plaintext></subpod></pod></queryresult>`)})
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], strings.Repeat("x", 75)+"{/}..") {
		t.Fatalf("reply = %q", got)
	}
	if strings.Contains(got[0], strings.Repeat("x", 76)) {
		t.Fatalf("row not truncated at 75: %q", got)
	}
}

func TestRowCap(t *testing.T) {
	f := newFixture(t)
	f.cf.Set("wolframalpha_appid", "A")
	f.wa("BenV", "veel")
	var pods strings.Builder
	for range 12 {
		pods.WriteString("<pod><subpod><plaintext>" + strings.Repeat("y", 60) + "</plaintext></subpod></pod>")
	}
	f.cbs[f.fetched[0]](fetch.Result{Status: 200, Body: []byte(
		`<queryresult success="true">` + pods.String() + `</queryresult>`)})
	got := f.take()
	if len(got) != 1 || !strings.HasSuffix(got[0], "{/}, ++") {
		t.Fatalf("reply = %q, want ++ cap", got)
	}
}

func TestNoResults(t *testing.T) {
	f := newFixture(t)
	f.cf.Set("wolframalpha_appid", "A")
	f.wa("BenV", "gibberish")
	f.cbs[f.fetched[0]](fetch.Result{Status: 200, Body: []byte(
		`<queryresult success="false" error="false"></queryresult>`)})
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|BenV: No results for {W}gibberish{/}." {
		t.Fatalf("reply = %q", got)
	}
}
