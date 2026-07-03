// Package tinyurl shortens URLs through the tinyurl.com API: !tinyurl
// <url>, plus silent auto-shortening of URLs over 40 characters seen
// in channel. Ported from IRC_TinyURL.pm. Fixes vs the Perl: the
// target URL is query-escaped (the Perl concatenated it raw, breaking
// on &), and URL extraction is a regexp approximation of URI::Find.
package tinyurl

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"go-botje/internal/bus"
	"go-botje/internal/fetch"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
)

const (
	baseURL   = "http://tinyurl.com/api-create.php?url="
	minAuto   = 40  // auto-shorten channel URLs longer than this
	maxResult = 300 // longer bodies are error pages, not short urls
)

var (
	urlRe     = regexp.MustCompile(`\b(?:https?|ftp)://[^\s<>"']+`)
	cmdURLRe  = regexp.MustCompile(`^(?:ht|f)tps?://`)
	displayRe = regexp.MustCompile(`://(.{0,22})`)
)

// waiters tracks who asked for an in-flight API url (the Perl $active).
type waiters struct {
	original string
	events   []*bus.Event
}

// Module implements module.Module.
type Module struct {
	// fetch is the fetcher seam, replaced in tests.
	fetch func(url string, opts fetch.Options, cb func(fetch.Result)) bool

	ctx    *module.Context
	active map[string]*waiters
}

// New returns an unloaded tinyurl module.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "tinyurl" }

func (m *Module) Load(ctx *module.Context) error {
	m.ctx = ctx
	m.active = make(map[string]*waiters)
	if m.fetch == nil {
		m.fetch = ctx.Fetch.Fetch
	}
	ctx.Cmd.Register(m.Name(), "tinyurl", m.cbTinyURL)
	return ctx.Bus.RegisterHook(m.Name(), "IRC_PRIVMSG", m.onPrivmsg)
}

func (m *Module) Unload() error {
	m.ctx.Bus.UnregisterModule(m.Name())
	m.ctx.Cmd.UnregisterModule(m.Name())
	return nil
}

const help = "Usage  : !tinyurl <longurl>\nExample: !tinyurl http://example.com/krijg/kanker.php?a=123&jemoeder=iseenhoer"

func (m *Module) cbTinyURL(d *cmd.Data) bool {
	if d.Event.SenderMe {
		return false
	}
	switch {
	case d.Data == "":
		m.ctx.Privmsg(d.Event.Channel, help)
	case cmdURLRe.MatchString(d.Data):
		m.handleURL(d.Data, d.Event, false)
	default:
		m.ctx.Privmsg(d.Event.Channel, "Syntax error!")
		m.ctx.Privmsg(d.Event.Channel, help)
	}
	return true
}

func (m *Module) onPrivmsg(ev *bus.Event) (bus.Handled, any) {
	// the ^! skip is a fix: the Perl hook also ran on "!tinyurl <url>"
	// itself, racing its own command into an "Already fetching" nag and
	// a doubled reply
	if ev.SenderMe || ev.Msg == "" || strings.HasPrefix(ev.Msg, "!") {
		return bus.None, nil
	}
	for _, u := range extractURLs(ev.Msg) {
		if len(u) > minAuto {
			m.handleURL(u, ev, true) // silent: no nagging, just the result
		}
	}
	return bus.None, nil
}

// handleURL starts (or joins) a shorten request for one URL.
func (m *Module) handleURL(target string, ev *bus.Event, silent bool) {
	api := baseURL + url.QueryEscape(target)
	if w, ok := m.active[api]; ok {
		if !silent {
			m.ctx.Privmsg(ev.Channel,
				"Already fetching that url you asshole! Have some patience. [added you to the list]")
		}
		w.events = append(w.events, ev)
		return
	}
	m.active[api] = &waiters{original: target, events: []*bus.Event{ev}}
	m.fetch(api, fetch.Options{}, func(res fetch.Result) { m.fetched(api, res) })
}

func (m *Module) fetched(api string, res fetch.Result) {
	w, ok := m.active[api]
	if !ok {
		return
	}
	delete(m.active, api)

	short := w.original
	if len(short) > 25 {
		if g := displayRe.FindStringSubmatch(short); g != nil {
			short = g[1] + "..."
		} else {
			short = short[:22] + "..."
		}
	}
	body := string(res.Body)
	reply := fmt.Sprintf("TinyURL failed for {m}%s{/}, don't ask me why.", short)
	if res.Err == nil && body != "" && len(body) < maxResult {
		reply = fmt.Sprintf("TinyURL for {m}%s{/}: {W}%s{/}", short, body)
	}
	for _, ev := range w.events {
		m.ctx.Privmsg(ev.Channel, reply)
	}
}

// extractURLs approximates URI::Find: grab URL-looking spans, strip
// trailing punctuation.
func extractURLs(s string) []string {
	var out []string
	for _, u := range urlRe.FindAllString(s, -1) {
		out = append(out, strings.TrimRight(u, ".,;:!?)]}'\""))
	}
	return out
}
