package rss

// Feed XML parsing (RSS 2.0, RDF, Atom) and the HTML-to-IRC text
// cleanup, ported from the IRC_RSS.pm XML::LibXML/XML::Atom code with
// encoding/xml. Same repairs: naked & gets escaped before parsing,
// unknown timezones fall back to UTC, a missing guid becomes
// title+link.

import (
	"encoding/xml"
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"
)

// feedHead is the per-feed metadata from the last fetch.
type feedHead struct {
	Title       string
	Description string
	Link        string
}

// feedItem is one parsed entry, pre-history.
type feedItem struct {
	Title       string
	Link        string
	Guid        string
	Description string
	Time        time.Time
}

type xmlRSS struct {
	XMLName xml.Name
	Channel struct {
		Title       string    `xml:"title"`
		Description string    `xml:"description"`
		Link        string    `xml:"link"`
		Items       []xmlItem `xml:"item"`
	} `xml:"channel"`
	Items []xmlItem `xml:"item"` // rdf puts items at the top level
}

type xmlItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Guid        string `xml:"guid"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
	DCDate      string `xml:"date"` // dc:date fallback
}

type xmlAtom struct {
	Title   string `xml:"title"`
	Updated string `xml:"updated"`
	Links   []struct {
		Href string `xml:"href,attr"`
	} `xml:"link"`
	Entries []struct {
		Title string `xml:"title"`
		Links []struct {
			Href string `xml:"href,attr"`
		} `xml:"link"`
		ID        string `xml:"id"`
		Published string `xml:"published"`
		Updated   string `xml:"updated"`
		Content   string `xml:"content"`
	} `xml:"entry"`
}

var entityRe = regexp.MustCompile(`^&(amp|lt|gt|quot|apos|#[0-9]+|#x[0-9a-fA-F]+);`)

// fixAmp escapes naked ampersands (invalid XML but common in feeds).
// Go regexp has no lookahead, so scan manually.
func fixAmp(data []byte) []byte {
	var out []byte
	for i := range data {
		if data[i] != '&' {
			out = append(out, data[i])
			continue
		}
		end := min(i+12, len(data))
		if entityRe.Match(data[i:end]) {
			out = append(out, '&')
			continue
		}
		out = append(out, []byte("&amp;")...)
	}
	return out
}

// parseFeed detects Atom vs RSS/RDF and parses. Items come back in
// document order, text decoded.
func parseFeed(data []byte) (feedHead, []feedItem, error) {
	data = fixAmp(data)

	var probe struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal(data, &probe); err != nil {
		return feedHead{}, nil, fmt.Errorf("rss: parse: %w", err)
	}
	if probe.XMLName.Local == "feed" {
		return parseAtom(data)
	}
	return parseRSS(data)
}

func parseRSS(data []byte) (feedHead, []feedItem, error) {
	var doc xmlRSS
	if err := xml.Unmarshal(data, &doc); err != nil {
		return feedHead{}, nil, fmt.Errorf("rss: parse: %w", err)
	}
	head := feedHead{
		Title:       decode(doc.Channel.Title),
		Description: decode(doc.Channel.Description),
		Link:        decode(doc.Channel.Link),
	}
	raw := doc.Channel.Items
	if len(raw) == 0 {
		raw = doc.Items
	}
	var items []feedItem
	for _, it := range raw {
		item := feedItem{
			Title:       decode(it.Title),
			Link:        decode(it.Link),
			Description: decode(it.Description),
		}
		guid := it.Guid
		if guid == "" {
			guid = it.Title + it.Link
		}
		item.Guid = decode(guid)
		pub := it.PubDate
		if pub == "" {
			pub = it.DCDate
		}
		item.Time = parsePubDate(pub)
		items = append(items, item)
	}
	return head, items, nil
}

func parseAtom(data []byte) (feedHead, []feedItem, error) {
	var doc xmlAtom
	if err := xml.Unmarshal(data, &doc); err != nil {
		return feedHead{}, nil, fmt.Errorf("rss: atom parse: %w", err)
	}
	head := feedHead{
		Title:       decode(doc.Title),
		Description: "Last update: " + decode(doc.Updated),
	}
	if len(doc.Links) > 0 {
		head.Link = decode(doc.Links[0].Href)
	}
	var items []feedItem
	for _, e := range doc.Entries {
		item := feedItem{
			Title: decode(e.Title),
			Guid:  decode(e.ID),
		}
		if len(e.Links) > 0 {
			item.Link = decode(e.Links[0].Href)
		}
		if e.Content != "" {
			item.Description = decode(e.Content)
		} else {
			item.Description = item.Title
		}
		pub := e.Published
		if pub == "" {
			pub = e.Updated
		}
		item.Time = parsePubDate(pub)
		items = append(items, item)
	}
	return head, items, nil
}

// parsePubDate handles the two live formats: RFC1123-ish and ISO 8601.
// Unknown zones fall back to UTC (the Market Ticker EDT incident);
// anything unparseable becomes now, like the Perl.
func parsePubDate(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		"Mon, 02 Jan 2006 15:04:05 -0700",
		"Mon, 2 Jan 2006 15:04:05 -0700",
		"02 Jan 2006 15:04:05 -0700",
		"Mon, 02 Jan 2006 15:04:05 MST",
		"Mon, 2 Jan 2006 15:04:05 MST",
		time.RFC3339,
		"2006-01-02T15:04:05Z0700",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Now()
}

var (
	brRe        = regexp.MustCompile(`(?i)<br\s*/?>`)
	wsRe        = regexp.MustCompile(`\s+`)
	aRe         = regexp.MustCompile(`(?is)<a[^>]*>(.*?)</a>`)
	blockRe     = regexp.MustCompile(`(?is)<(blockquote|iframe|div|img|p|h\d)[^>]*>(.*?)</(?:blockquote|iframe|div|img|p|h\d)>`)
	selfCloseRe = regexp.MustCompile(`(?i)<(blockquote|img|iframe|div|p|h\d)[^>]*/?>`)
	anyTagRe    = regexp.MustCompile(`</?[^>]+>`)
	quoteRe     = regexp.MustCompile(`"([^"]+)"`)
	ticksRe     = regexp.MustCompile("``(.+)''")
)

// decode converts html-ish feed text into one clean line: entities
// resolved, tags stripped (content kept), whitespace collapsed, quotes
// colored.
func decode(s string) string {
	// the Perl's shortcut replacements first (some produce text that
	// colorquotes then picks up, like the backtick quotes)
	for _, r := range [][2]string{
		{"&#187;", ">>"}, {"&#8211;", "-"}, {"&#8212;", "--"},
		{"&#8217;", "'"}, {"&#8220;", "``"}, {"&#8221;", "''"},
		{"&#8230;", "..."}, {"&#8482;", "(tm)"}, {"&mdash;", "--"},
	} {
		s = strings.ReplaceAll(s, r[0], r[1])
	}
	s = html.UnescapeString(s)

	s = strings.ReplaceAll(s, "\n", " ")
	s = brRe.ReplaceAllString(s, " ")
	s = wsRe.ReplaceAllString(s, " ")

	for {
		before := s
		s = aRe.ReplaceAllString(s, "$1")
		s = blockRe.ReplaceAllString(s, "$2")
		if s == before {
			break
		}
	}
	s = selfCloseRe.ReplaceAllString(s, "")
	s = anyTagRe.ReplaceAllString(s, "")

	return colorquotes(s)
}

// colorquotes colors "quoted" and “quoted” spans, cycling g c r y
// starting at y.
func colorquotes(s string) string {
	colors := []string{"g", "c", "r", "y"}
	color := "y"
	replace := func(re *regexp.Regexp) {
		for {
			loc := re.FindStringSubmatchIndex(s)
			if loc == nil {
				return
			}
			s = s[:loc[0]] + "{" + color + "}" + s[loc[2]:loc[3]] + "{/}" + s[loc[1]:]
			colors = append(colors, color)
			color = colors[0]
			colors = colors[1:]
		}
	}
	replace(quoteRe)
	replace(ticksRe)
	return s
}
