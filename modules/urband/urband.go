// Package urband answers !ud <term> from the RapidAPI Urban
// Dictionary, definitions and examples color-cycled through the reply
// pager. Ported from IRC_UrbanD.pm; the RapidAPI key moved from the
// source into conf (urband_rapidapi_key). The doubled color tag on
// non-first lines is the Perl's, kept for parity (it is invisible on
// the wire).
package urband

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"go-botje/internal/fetch"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
)

const apiHost = "mashape-community-urban-dictionary.p.rapidapi.com"

type entry struct {
	Definition string `json:"definition"`
	Example    string `json:"example"`
}

// Module implements module.Module.
type Module struct {
	// fetch is the fetcher seam, replaced in tests.
	fetch func(url string, opts fetch.Options, cb func(fetch.Result)) bool

	ctx *module.Context
}

// New returns an unloaded urband module.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "urband" }

func (m *Module) Load(ctx *module.Context) error {
	m.ctx = ctx
	if m.fetch == nil {
		m.fetch = ctx.Fetch.Fetch
	}
	ctx.Conf.CreateString("urband_rapidapi_key", "")
	ctx.Cmd.Register(m.Name(), "ud", m.cbUD)
	return nil
}

func (m *Module) Unload() error {
	m.ctx.Cmd.UnregisterModule(m.Name())
	m.ctx.Bus.UnregisterModule(m.Name())
	return nil
}

func (m *Module) cbUD(d *cmd.Data) bool {
	if d.Event.SenderMe {
		return false
	}
	ev := d.Event
	term := strings.TrimSpace(d.Data)
	key := m.ctx.Conf.String("urband_rapidapi_key")
	if key == "" {
		m.ctx.Privmsg(ev.Channel, ev.Sender.Nick+": Error - no rapidapi key configured (conf urband_rapidapi_key).")
		return true
	}

	apiURL := "https://" + apiHost + "/define?term=" +
		strings.ReplaceAll(url.QueryEscape(term), "+", "%20")
	ok := m.fetch(apiURL, fetch.Options{
		Timeout: 10 * time.Second,
		Headers: map[string]string{"X-Rapidapi-Host": apiHost, "X-Rapidapi-Key": key},
	}, func(res fetch.Result) { m.fetched(res, d, term) })
	if !ok {
		m.ctx.Privmsg(ev.Channel, ev.Sender.Nick+": DERrrrr, something went wrong with fetching ;)")
	}
	return true
}

func (m *Module) fetched(res fetch.Result, d *cmd.Data, term string) {
	ev := d.Event
	nick := ev.Sender.Nick
	if res.Err != nil {
		m.ctx.Privmsg(ev.Channel, fmt.Sprintf("%s: Timeout for: {W}%s{/}. Boohoo!", nick, term))
		return
	}
	var result struct {
		List []entry `json:"list"`
	}
	if err := json.Unmarshal(res.Body, &result); err != nil {
		m.ctx.Privmsg(ev.Channel, nick+": Hm, got some garbage data.. too bad!")
		return
	}
	if len(result.List) == 0 {
		m.ctx.Privmsg(ev.Channel, fmt.Sprintf("%s: No entry for: {W}%s", nick, term))
		return
	}

	clean := strings.NewReplacer("\r\n", "", "..", ".")
	colors := []string{"m", "g", "r", "y", "c"}
	var out []string
	for i, e := range result.List {
		def := clean.Replace(e.Definition)
		c := colors[0]
		colors = append(colors[1:], c)
		c2 := colors[0]
		colors = append(colors[1:], c2)
		if i == 0 {
			def = fmt.Sprintf("%s: {%s}%s", nick, c, def)
		} else {
			def = fmt.Sprintf("{%s}%s", c, def)
		}
		if ex := strings.ReplaceAll(e.Example, "\r\n", ""); ex != "" {
			def += fmt.Sprintf(" {%s}%s", c2, ex)
		}
		out = append(out, fmt.Sprintf("{%s}%s", c, def))
	}
	m.ctx.Pager.EventMsg(ev, d.Command, out...)
}
