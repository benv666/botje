package tinyurl

import (
	"strings"
	"testing"

	"go-botje/internal/bus"
	"go-botje/internal/fetch"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
)

type fixture struct {
	m       *Module
	b       *bus.Bus
	cmds    *cmd.Registry
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
	f.m = New()
	f.m.fetch = func(url string, opts fetch.Options, cb func(fetch.Result)) bool {
		if _, dup := f.cbs[url]; dup {
			return false
		}
		f.fetched = append(f.fetched, url)
		f.cbs[url] = cb
		return true
	}
	err := f.m.Load(&module.Context{
		Bus: f.b, Cmd: f.cmds,
		Privmsg: func(ch, msg string) { f.sent = append(f.sent, ch+"|"+msg) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) msg(text string) {
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: "#testing",
		Msg: text}
	ev.Sender.Nick = "BenV"
	f.b.Submit(ev)
	f.cmds.Handle(ev)
}

func (f *fixture) complete(t *testing.T, apiURL, body string) {
	t.Helper()
	cb, ok := f.cbs[apiURL]
	if !ok {
		t.Fatalf("no fetch in flight for %q (have %q)", apiURL, f.fetched)
	}
	delete(f.cbs, apiURL)
	cb(fetch.Result{URL: apiURL, Status: 200, Body: []byte(body)})
}

func TestCommandShortensURL(t *testing.T) {
	f := newFixture(t)
	f.msg("!tinyurl http://example.com/a")
	wantAPI := "http://tinyurl.com/api-create.php?url=http%3A%2F%2Fexample.com%2Fa"
	if len(f.fetched) != 1 || f.fetched[0] != wantAPI {
		t.Fatalf("fetched = %q, want %q", f.fetched, wantAPI)
	}
	f.complete(t, wantAPI, "https://tinyurl.com/abc123")
	want := "#testing|TinyURL for {m}http://example.com/a{/}: {W}https://tinyurl.com/abc123{/}"
	if len(f.sent) != 1 || f.sent[0] != want {
		t.Fatalf("sent = %q, want %q", f.sent, want)
	}
}

func TestLongURLDisplayTruncated(t *testing.T) {
	f := newFixture(t)
	long := "http://example.com/heel/lang/pad/dat/maar/doorgaat"
	f.msg("!tinyurl " + long)
	f.complete(t, f.fetched[0], "https://tinyurl.com/x")
	if !strings.Contains(f.sent[0], "{m}example.com/heel/lang/...{/}") {
		t.Fatalf("sent = %q, want 22 chars after :// plus ellipsis", f.sent[0])
	}
}

func TestBareCommandShowsHelp(t *testing.T) {
	f := newFixture(t)
	f.msg("!tinyurl")
	if len(f.sent) != 1 || !strings.Contains(f.sent[0], "Usage  : !tinyurl <longurl>") {
		t.Fatalf("sent = %q, want help", f.sent)
	}
}

func TestSyntaxError(t *testing.T) {
	f := newFixture(t)
	f.msg("!tinyurl geen url")
	if len(f.sent) != 2 || f.sent[0] != "#testing|Syntax error!" ||
		!strings.Contains(f.sent[1], "Usage  : !tinyurl") {
		t.Fatalf("sent = %q, want syntax error + help", f.sent)
	}
}

func TestDuplicateWhilePending(t *testing.T) {
	f := newFixture(t)
	f.msg("!tinyurl http://example.com/a")
	f.msg("!tinyurl http://example.com/a")
	if len(f.fetched) != 1 {
		t.Fatalf("fetched %d times, want single-flight", len(f.fetched))
	}
	want := "#testing|Already fetching that url you asshole! Have some patience. [added you to the list]"
	if len(f.sent) != 1 || f.sent[0] != want {
		t.Fatalf("sent = %q, want %q", f.sent, want)
	}
	f.sent = nil
	f.complete(t, f.fetched[0], "https://tinyurl.com/abc")
	if len(f.sent) != 2 {
		t.Fatalf("sent = %q, want replies for both waiters", f.sent)
	}
}

func TestFetchFailure(t *testing.T) {
	f := newFixture(t)
	f.msg("!tinyurl http://example.com/a")
	f.complete(t, f.fetched[0], "")
	want := "#testing|TinyURL failed for {m}http://example.com/a{/}, don't ask me why."
	if len(f.sent) != 1 || f.sent[0] != want {
		t.Fatalf("sent = %q, want %q", f.sent, want)
	}
	f.msg("!tinyurl http://example.com/b")
	f.complete(t, f.fetched[1], strings.Repeat("x", 300))
	if !strings.Contains(f.sent[1], "TinyURL failed") {
		t.Fatalf("sent = %q, want failure for oversized body", f.sent)
	}
}

func TestAutoShortenLongChannelURLs(t *testing.T) {
	f := newFixture(t)
	f.msg("kijk hier: http://example.com/dit/is/een/behoorlijk/lange/url/van/ruim/veertig/tekens grappig he")
	if len(f.fetched) != 1 {
		t.Fatalf("fetched = %q, want auto-shorten", f.fetched)
	}
	if len(f.sent) != 0 {
		t.Fatalf("sent = %q, auto mode is silent until the result", f.sent)
	}
	f.complete(t, f.fetched[0], "https://tinyurl.com/kort")
	if len(f.sent) != 1 || !strings.Contains(f.sent[0], "{W}https://tinyurl.com/kort{/}") {
		t.Fatalf("sent = %q", f.sent)
	}
}

func TestShortChannelURLsIgnored(t *testing.T) {
	f := newFixture(t)
	f.msg("zie http://example.com/kort ok")
	if len(f.fetched) != 0 {
		t.Fatalf("fetched = %q for a short url", f.fetched)
	}
}

func TestURLExtractionTrailingPunctuation(t *testing.T) {
	f := newFixture(t)
	f.msg("(zie http://example.com/dit/is/een/behoorlijk/lange/url/van/ruim/veertig/xx). ok?")
	if len(f.fetched) != 1 || strings.Contains(f.fetched[0], "xx%29") || !strings.Contains(f.fetched[0], "veertig%2Fxx") {
		t.Fatalf("fetched = %q, want trailing ). stripped", f.fetched)
	}
}
