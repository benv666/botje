// Package wolframalpha answers !wa <query> with the plaintext pods
// from the WolframAlpha query API. Ported from IRC_WolframAlpha.pm and
// its Custom::WolframAlpha helper; the appid moved from the source
// into conf (wolframalpha_appid).
package wolframalpha

import (
	"encoding/xml"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"go-botje/internal/bus"
	"go-botje/internal/fetch"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
)

type waResult struct {
	Success string `xml:"success,attr"`
	Pods    []struct {
		Subpods []struct {
			Plaintext string `xml:"plaintext"`
		} `xml:"subpod"`
	} `xml:"pod"`
}

var nlRe = regexp.MustCompile(`[\n\r]`)

// Module implements module.Module.
type Module struct {
	// fetch is the fetcher seam, replaced in tests.
	fetch func(url string, opts fetch.Options, cb func(fetch.Result)) bool

	ctx *module.Context
}

// New returns an unloaded wolframalpha module.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "wolframalpha" }

func (m *Module) Load(ctx *module.Context) error {
	m.ctx = ctx
	if m.fetch == nil {
		m.fetch = ctx.Fetch.Fetch
	}
	ctx.Conf.CreateString("wolframalpha_appid", "")
	ctx.Cmd.Register(m.Name(), "wa", m.cbWa)
	return nil
}

func (m *Module) Unload() error {
	m.ctx.Cmd.UnregisterModule(m.Name())
	m.ctx.Bus.UnregisterModule(m.Name())
	return nil
}

func (m *Module) cbWa(d *cmd.Data) bool {
	if d.Event.SenderMe {
		return false
	}
	ev := d.Event
	appID := m.ctx.Conf.String("wolframalpha_appid")
	if appID == "" {
		m.ctx.Privmsg(ev.Channel, ev.Sender.Nick+": Error - no wolframalpha appid configured (conf wolframalpha_appid).")
		return true
	}
	query := d.Data
	// QueryEscape encodes a real + as %2B; its form-style + for spaces
	// becomes %20 like the perl's uri_escape
	apiURL := "http://api.wolframalpha.com/v1/query.jsp?appid=" + url.QueryEscape(appID) +
		"&input=" + strings.ReplaceAll(url.QueryEscape(query), "+", "%20")
	m.fetch(apiURL, fetch.Options{}, func(res fetch.Result) { m.fetched(res, ev, query) })
	return true
}

func (m *Module) fetched(res fetch.Result, ev *bus.Event, query string) {
	var rows []string
	if res.Err == nil {
		var result waResult
		if xml.Unmarshal(res.Body, &result) == nil && result.Success == "true" {
			for _, pod := range result.Pods {
				for _, sub := range pod.Subpods {
					if sub.Plaintext != "" {
						rows = append(rows, sub.Plaintext)
					}
				}
			}
		}
	}

	colors := []string{"m", "r", "g", "w", "y", "c"}
	var parts []string
	length := 0
	if len(rows) == 0 {
		parts = append(parts, fmt.Sprintf("No results for {W}%s{/}.", query))
	}
	for i, row := range rows {
		row = strings.TrimSpace(row)
		row = nlRe.ReplaceAllString(row, " ")
		if len(row) > 80 {
			row = row[:75] + "{/}.."
		}
		row = "{" + colors[0] + "}" + row
		colors = append(colors[1:], colors[0])
		parts = append(parts, row)
		length += 5 + len(row)
		if i+1 > 4 && i+1 < len(rows) && length >= 250 {
			parts = append(parts, "++")
			break
		}
	}
	m.ctx.Privmsg(ev.Channel, ev.Sender.Nick+": "+strings.Join(parts, "{/}, "))
}
