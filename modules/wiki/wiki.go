// Package wiki answers !wiki <query> with the top three Wikipedia
// search hits. Ported from IRC_Wiki.pm: same REST endpoint, same
// User-Agent, same 15-second per-channel spam brake.
package wiki

import (
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"time"

	"go-botje/internal/fetch"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
)

const (
	baseURL   = "https://en.wikipedia.org/w/rest.php/v1/search/page?limit=3&utf8=0&q="
	userAgent = "IRC Bot/4.0 (botjevanirc@gmail.com) Fetcher/1.0"
	spamGap   = 15 * time.Second
)

var (
	tagRe = regexp.MustCompile(`</?[^>]+>`)
	wsRe  = regexp.MustCompile(`\s{2,}`)
)

type page struct {
	Title   string `json:"title"`
	Excerpt string `json:"excerpt"`
}

// Module implements module.Module.
type Module struct {
	// Now is injectable time; nil means time.Now.
	Now func() time.Time
	// fetch is the fetcher seam, replaced in tests.
	fetch func(url string, opts fetch.Options, cb func(fetch.Result)) bool

	ctx  *module.Context
	spam map[string]time.Time // channel|command|msg -> last ask
}

// New returns an unloaded wiki module.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "wiki" }

func (m *Module) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Module) Load(ctx *module.Context) error {
	m.ctx = ctx
	if m.fetch == nil {
		m.fetch = ctx.Fetch.Fetch
	}
	m.spam = make(map[string]time.Time)
	ctx.Cmd.Register(m.Name(), "wiki", m.cbWiki)
	return nil
}

func (m *Module) Unload() error {
	m.ctx.Cmd.UnregisterModule(m.Name())
	m.ctx.Bus.UnregisterModule(m.Name())
	return nil
}

func (m *Module) cbWiki(d *cmd.Data) bool {
	if d.Event.SenderMe {
		return false
	}
	channel, sender := d.Event.Channel, d.Event.Sender.Nick
	msg := strings.TrimSpace(d.Data)
	if msg == "" {
		m.ctx.Privmsg(channel, "~ If I only had a brain...")
		return true
	}

	key := channel + "|" + d.Command + "|" + msg
	if last, ok := m.spam[key]; ok && m.now().Sub(last) < spamGap {
		m.ctx.Privmsg(channel, "T)#$@)%@)#%)%@24@($2 stop spamming")
		return true
	}
	for k, last := range m.spam {
		if m.now().Sub(last) > spamGap {
			delete(m.spam, k)
		}
	}
	m.spam[key] = m.now()

	// %20 for spaces like the perl's uri_escape, not the form-style +
	wikiURL := baseURL + strings.ReplaceAll(url.QueryEscape(msg), "+", "%20")
	m.fetch(wikiURL, fetch.Options{
		Headers: map[string]string{"User-Agent": userAgent},
	}, func(res fetch.Result) { m.fetched(res, channel, sender, msg) })
	return true
}

func (m *Module) fetched(res fetch.Result, channel, sender, msg string) {
	var result struct {
		Pages []page `json:"pages"`
	}
	valid := res.Err == nil && json.Unmarshal(res.Body, &result) == nil && len(result.Pages) > 0
	for _, p := range result.Pages {
		if p.Title == "" || p.Excerpt == "" {
			valid = false
		}
	}
	if !valid {
		m.ctx.Privmsg(channel, fmt.Sprintf(
			"%s: Wiki returned some garbage data for {W}%s{/}.. too bad.", sender, msg))
		return
	}

	colors := []string{"r", "g", "m"}
	var b strings.Builder
	for i, p := range result.Pages {
		excerpt := html.UnescapeString(tagRe.ReplaceAllString(p.Excerpt, ""))
		excerpt = wsRe.ReplaceAllString(excerpt, " ")
		c := colors[i%len(colors)]
		fmt.Fprintf(&b, " {%s}%s {%s}%s{/}", c, p.Title, strings.ToUpper(c), excerpt)
	}
	m.ctx.Privmsg(channel, b.String())
}
