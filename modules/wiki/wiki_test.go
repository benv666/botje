package wiki

import (
	"strings"
	"testing"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/fetch"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
	"go-botje/internal/storage"
)

type fixture struct {
	m       *Module
	b       *bus.Bus
	cmds    *cmd.Registry
	clk     time.Time
	sent    []string
	fetched []string
	headers []map[string]string
	cbs     map[string]func(fetch.Result)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{clk: time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
		cbs: map[string]func(fetch.Result){}}
	f.b = bus.New()
	f.b.RegisterEvent("IRC_PRIVMSG")
	f.cmds = cmd.New()
	f.m = New()
	f.m.Now = func() time.Time { return f.clk }
	f.m.fetch = func(url string, opts fetch.Options, cb func(fetch.Result)) bool {
		f.fetched = append(f.fetched, url)
		f.headers = append(f.headers, opts.Headers)
		f.cbs[url] = cb
		return true
	}
	err := f.m.Load(&module.Context{
		Bus: f.b, Cmd: f.cmds, Store: storage.NewMemory(),
		Privmsg: func(ch, msg string) { f.sent = append(f.sent, ch+"|"+msg) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) wiki(nick, data string) {
	msg := "!wiki"
	if data != "" {
		msg += " " + data
	}
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: "#testing",
		Msg: msg}
	ev.Sender.Nick = nick
	f.b.Submit(ev)
	f.cmds.Handle(ev)
}

func (f *fixture) take() []string {
	s := f.sent
	f.sent = nil
	return s
}

const pagesJSON = `{"pages":[
  {"title":"Earth","excerpt":"<span class=\"searchmatch\">Earth</span> is the third   planet"},
  {"title":"Earth science","excerpt":"study of &quot;the&quot; planet"},
  {"title":"Flat Earth","excerpt":"an archaic conception"}]}`

func TestWikiSearch(t *testing.T) {
	f := newFixture(t)
	f.wiki("BenV", "earth")
	wantURL := "https://en.wikipedia.org/w/rest.php/v1/search/page?limit=3&utf8=0&q=earth"
	if len(f.fetched) != 1 || f.fetched[0] != wantURL {
		t.Fatalf("fetched = %q", f.fetched)
	}
	if f.headers[0]["User-Agent"] != "IRC Bot/4.0 (botjevanirc@gmail.com) Fetcher/1.0" {
		t.Fatalf("headers = %v", f.headers[0])
	}
	f.cbs[wantURL](fetch.Result{Status: 200, Body: []byte(pagesJSON)})
	got := f.take()
	want := "#testing| {r}Earth {R}Earth is the third planet{/}" +
		" {g}Earth science {G}study of \"the\" planet{/}" +
		" {m}Flat Earth {M}an archaic conception{/}"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("reply = %q\nwant %q", got, want)
	}
}

func TestWikiEscaping(t *testing.T) {
	f := newFixture(t)
	f.wiki("BenV", "foo bar&baz")
	if len(f.fetched) != 1 || !strings.HasSuffix(f.fetched[0], "q=foo%20bar%26baz") {
		t.Fatalf("fetched = %q", f.fetched)
	}
}

func TestWikiEmpty(t *testing.T) {
	f := newFixture(t)
	f.wiki("BenV", "")
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|~ If I only had a brain..." {
		t.Fatalf("reply = %q", got)
	}
}

func TestWikiGarbage(t *testing.T) {
	f := newFixture(t)
	f.wiki("BenV", "iets")
	f.cbs[f.fetched[0]](fetch.Result{Status: 200, Body: []byte("geen json")})
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|BenV: Wiki returned some garbage data for {W}iets{/}.. too bad." {
		t.Fatalf("reply = %q", got)
	}
	// empty result set counts as garbage too
	f.wiki("BenV", "niks")
	f.cbs[f.fetched[1]](fetch.Result{Status: 200, Body: []byte(`{"pages":[]}`)})
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0], "garbage data for {W}niks{/}") {
		t.Fatalf("reply = %q", got)
	}
}

func TestWikiSpamCheck(t *testing.T) {
	f := newFixture(t)
	f.wiki("BenV", "earth")
	f.take()
	f.wiki("Other", "earth") // same channel+query within 15s
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|T)#$@)%@)#%)%@24@($2 stop spamming" {
		t.Fatalf("spam reply = %q", got)
	}
	if len(f.fetched) != 1 {
		t.Fatalf("spam still fetched: %q", f.fetched)
	}
	f.clk = f.clk.Add(16 * time.Second)
	f.wiki("BenV", "earth")
	f.take()
	if len(f.fetched) != 2 {
		t.Fatalf("query after cooldown not fetched: %q", f.fetched)
	}
}
