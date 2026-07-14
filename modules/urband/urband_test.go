package urband

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/conf"
	"go-botje/internal/fetch"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/irc/pager"
	"go-botje/internal/module"
	"go-botje/internal/sched"
	"go-botje/internal/storage"
)

type fixture struct {
	m       *Module
	b       *bus.Bus
	cmds    *cmd.Registry
	cf      *conf.Conf
	sent    []string
	fetched []string
	headers []map[string]string
	cbs     map[string]func(fetch.Result)
	refuse  bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{cbs: map[string]func(fetch.Result){}}
	f.b = bus.New()
	f.b.RegisterEvent("IRC_PRIVMSG")
	f.cmds = cmd.New()
	f.cf = conf.New()
	sch := sched.New(time.Now)
	f.m = New()
	f.m.fetch = func(url string, opts fetch.Options, cb func(fetch.Result)) bool {
		if f.refuse {
			return false
		}
		f.fetched = append(f.fetched, url)
		f.headers = append(f.headers, opts.Headers)
		f.cbs[url] = cb
		return true
	}
	pg := pager.New(sch, func(ch, line string) { f.sent = append(f.sent, ch+"|"+line) })
	err := f.m.Load(&module.Context{
		Bus: f.b, Cmd: f.cmds, Conf: f.cf, Store: storage.NewMemory(),
		Sched: sch, Pager: pg,
		Privmsg: func(ch, msg string) {
			for l := range strings.SplitSeq(msg, "\n") {
				f.sent = append(f.sent, ch+"|"+l)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) ud(nick, term string) {
	msg := "!ud"
	if term != "" {
		msg += " " + term
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

func TestNoKeyRefuses(t *testing.T) {
	f := newFixture(t)
	f.ud("BenV", "hoer")
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "no rapidapi key configured") {
		t.Fatalf("keyless = %q", got)
	}
	if len(f.fetched) != 0 {
		t.Fatalf("fetched without key: %q", f.fetched)
	}
}

func TestDefine(t *testing.T) {
	f := newFixture(t)
	f.cf.Set("urband_rapidapi_key", "KEY123")
	f.ud("BenV", "hoer")
	wantURL := "https://mashape-community-urban-dictionary.p.rapidapi.com/define?term=hoer"
	if len(f.fetched) != 1 || f.fetched[0] != wantURL {
		t.Fatalf("fetched = %q", f.fetched)
	}
	h := f.headers[0]
	if h["X-Rapidapi-Key"] != "KEY123" ||
		h["X-Rapidapi-Host"] != "mashape-community-urban-dictionary.p.rapidapi.com" {
		t.Fatalf("headers = %v", h)
	}
	f.cbs[wantURL](fetch.Result{Status: 200, Body: []byte(`{"list":[
		{"definition":"eerste..def\r\n", "example":"voorbeeld\r\neen"},
		{"definition":"tweede def", "example":""}]}`)})
	got := f.take()
	if len(got) != 2 {
		t.Fatalf("lines = %q", got)
	}
	if got[0] != "#testing|{m}BenV: {m}eerste.def {g}voorbeeldeen" {
		t.Fatalf("first = %q", got[0])
	}
	if got[1] != "#testing|{r}{r}tweede def" {
		t.Fatalf("second = %q (perl doubles the tag on non-first lines)", got[1])
	}
}

func TestNoEntry(t *testing.T) {
	f := newFixture(t)
	f.cf.Set("urband_rapidapi_key", "K")
	f.ud("BenV", "znxqw")
	f.cbs[f.fetched[0]](fetch.Result{Status: 200, Body: []byte(`{"list":[]}`)})
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|BenV: No entry for: {W}znxqw" {
		t.Fatalf("reply = %q", got)
	}
}

func TestGarbage(t *testing.T) {
	f := newFixture(t)
	f.cf.Set("urband_rapidapi_key", "K")
	f.ud("BenV", "iets")
	f.cbs[f.fetched[0]](fetch.Result{Status: 200, Body: []byte("html error page")})
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|BenV: Hm, got some garbage data.. too bad!" {
		t.Fatalf("reply = %q", got)
	}
}

func TestTimeout(t *testing.T) {
	f := newFixture(t)
	f.cf.Set("urband_rapidapi_key", "K")
	f.ud("BenV", "traag")
	f.cbs[f.fetched[0]](fetch.Result{Err: fmt.Errorf("context deadline exceeded")})
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|BenV: Timeout for: {W}traag{/}. Boohoo!" {
		t.Fatalf("reply = %q", got)
	}
}

func TestFetchRefused(t *testing.T) {
	f := newFixture(t)
	f.cf.Set("urband_rapidapi_key", "K")
	f.refuse = true
	f.ud("BenV", "dubbel")
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|BenV: DERrrrr, something went wrong with fetching ;)" {
		t.Fatalf("reply = %q", got)
	}
}
