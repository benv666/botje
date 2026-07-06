// Package rss is the feed module: per-channel/query RSS and Atom
// subscriptions with polling, new-item broadcasts, and short-id recall
// with 3-line paging. Ported from IRC_RSS.pm. Fixed vs the Perl: the
// short-id allocator skips ids still attached to live items (the !LI6
// cats incident), the new-subscriber item cap no longer leaks to other
// subscribers in the same broadcast round, and feeds are saved after
// every successful fetch instead of only on add/del/unload. The Perl
// 30s fetch watchdog is replaced by the fetcher's own timeout; the
// retry cadence (3x 60s, then an hour) is the same.
package rss

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/fetch"
	"go-botje/internal/format"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
	"go-botje/internal/sched"
)

const (
	feedsKey       = "feeds"
	maxIDKey       = "maxItemId"
	defaultRefresh = 30             // minutes
	minRefresh     = 5              // minutes
	historyMinAge  = 24 * time.Hour // items younger than this survive pruning
	historyKeep    = 10             // never prune below this many items
	maxBroadcast   = 25
	newSubItems    = 5
	maxErrors      = 100
)

// item is one feed entry in history.
type item struct {
	ID          string `json:"_id"`
	Title       string `json:"title"`
	Link        string `json:"link"`
	Guid        string `json:"guid"`
	Description string `json:"description"`
	Time        int64  `json:"datetime"`
	Updated     int64  `json:"updatetime"`
}

// subscription is one (server, channel) delivery target.
type subscription struct {
	User     string           `json:"user"`
	Received map[string]int64 `json:"received"` // guid -> epoch marked read
}

// feed is one url with its history and subscriptions.
type feed struct {
	Tag           string                              `json:"tag"`
	Grep          string                              `json:"grep,omitempty"`
	Refresh       int                                 `json:"refresh"`
	Title         string                              `json:"title"`
	Description   string                              `json:"description"`
	Link          string                              `json:"link"`
	LastUpdate    int64                               `json:"lastupdate"`
	History       map[string]*item                    `json:"history"`
	Subscriptions map[string]map[string]*subscription `json:"subscriptions"`
	ErrorCount    int                                 `json:"errorCount,omitempty"`
	NoReschedule  bool                                `json:"noReschedule,omitempty"`

	timer  sched.Tag
	active bool
	tries  int
}

type stored struct {
	Feeds map[string]*feed `json:"feeds"`
	MaxID int              `json:"maxItemId"`
}

// shownState is the per-channel description paging state.
type shownState struct {
	id    string
	parts []string
	shown int
}

var (
	addRe     = regexp.MustCompile(`^add\s+(\S+)(?:\s+(.+))?\s*$`)
	delRe     = regexp.MustCompile(`^del\s+(\S+)$`)
	refreshRe = regexp.MustCompile(`^refresh\s+(\S+)$`)
	listRe    = regexp.MustCompile(`^list(?:\s+(.+))?$`)
	itemRe    = regexp.MustCompile(`^\s*(\S+)$`)
	lastRe    = regexp.MustCompile(`^last\s+(\S+)$`)

	optRefreshRe = regexp.MustCompile(`refresh=(\d+)`)
	optTargetRe  = regexp.MustCompile(`(?:\s|^)(query|#(\S+))(?:\s|$)`)
	optTagRe     = regexp.MustCompile(`tag=(\S+)`)
	optGrepRe    = regexp.MustCompile(`grep="((?:\\"|[^"])+)"`)
	sizeRe       = regexp.MustCompile(`(?i)size:?\s*(\d+(\.\d+)?)\s*(G|M|T)B\b`)
	colorJunkRe  = regexp.MustCompile(`\x1b\[.*?m|\{.\}`)
)

const rssHelp = "Usage  : !rss (add|del) <rss url|all> [options] -- !rss <rssId>: description (multiple time for more) -- !rss last -- !rss refresh url -- !rss list <url>-- !rss list url\n" +
	`Example: !rss add http://static.demonoid.me/rss/4.172.xml refresh=30 tag=Demonoid grep="^grep says \"what\?\"$" #ps3games`

// Module implements module.Module.
type Module struct {
	// Now and Rand are injectable for tests.
	Now  func() time.Time
	Rand func() float64

	// fetch is the fetcher seam, replaced in tests.
	fetch func(url string, opts fetch.Options, cb func(fetch.Result)) bool

	ctx       *module.Context
	feeds     map[string]*feed
	ids       *idAlloc
	lastShown map[string]*shownState // per channel
}

// New returns an unloaded rss module.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "rss" }

func (m *Module) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Module) rand() float64 {
	if m.Rand != nil {
		return m.Rand()
	}
	return rand.Float64()
}

func (m *Module) Load(ctx *module.Context) error {
	m.ctx = ctx
	if m.fetch == nil {
		m.fetch = ctx.Fetch.Fetch
	}
	m.feeds = make(map[string]*feed)
	m.lastShown = make(map[string]*shownState)

	var st stored
	if _, err := ctx.Store.Get(m.Name(), feedsKey, &st); err != nil {
		return fmt.Errorf("rss: load: %w", err)
	}
	if st.Feeds != nil {
		m.feeds = st.Feeds
	}
	m.ids = newIDAlloc(st.MaxID, m.idInUse)

	// restart pollers staggered so a boot with many feeds does not
	// stampede
	offset := time.Duration(m.rand()*60) * time.Second
	for url := range m.feeds {
		u := url
		m.feeds[url].timer = m.ctx.Sched.After(offset, func() { m.refreshFeed(u) })
		offset += 15 * time.Second
	}

	ctx.Cmd.Register(m.Name(), "rss", m.cbRSS)
	ctx.Cmd.RegisterDefault(m.Name(), 10, false, m.cbDefault)
	return nil
}

func (m *Module) Unload() error {
	for _, f := range m.feeds {
		m.ctx.Sched.Unschedule(f.timer)
	}
	m.ctx.Cmd.UnregisterModule(m.Name())
	m.ctx.Bus.UnregisterModule(m.Name())
	return m.save()
}

func (m *Module) save() error {
	return m.ctx.Store.Put(m.Name(), feedsKey, &stored{Feeds: m.feeds, MaxID: m.ids.counter()})
}

// idInUse reports whether a short id is still attached to a live item:
// the !LI6 fix.
func (m *Module) idInUse(id string) bool {
	for _, f := range m.feeds {
		for _, it := range f.History {
			if it.ID == id {
				return true
			}
		}
	}
	return false
}

// cbDefault recalls items for bare !<id> lines; unknown ids pass.
func (m *Module) cbDefault(d *cmd.Data) bool {
	if d.Event.SenderMe {
		return false
	}
	id := d.Data
	if _, _, ok := m.findItem(id); !ok {
		return false // let something else handle it
	}
	m.recall(id, d.Event.Channel)
	return true
}

func (m *Module) cbRSS(d *cmd.Data) bool {
	if d.Event.SenderMe {
		return false
	}
	server, channel, nick := d.Event.Server, d.Event.Channel, d.Event.Sender.Nick
	msg := d.Data

	switch {
	case msg == "":
		m.ctx.Privmsg(channel, rssHelp)
	case addRe.MatchString(msg):
		g := addRe.FindStringSubmatch(msg)
		m.addFeed(g[1], server, channel, nick, g[2])
	case delRe.MatchString(msg):
		m.delFeed(delRe.FindStringSubmatch(msg)[1], server, channel, nick)
	case refreshRe.MatchString(msg):
		m.refreshCmd(refreshRe.FindStringSubmatch(msg)[1], server, channel)
	case strings.HasPrefix(msg, "list url"):
		m.showFeeds("", server, channel, nick, true)
	case listRe.MatchString(msg):
		m.showFeeds(listRe.FindStringSubmatch(msg)[1], server, channel, nick, false)
	case itemRe.MatchString(msg):
		m.recall(itemRe.FindStringSubmatch(msg)[1], channel)
	case lastRe.MatchString(msg):
		m.showLastItems(lastRe.FindStringSubmatch(msg)[1], server, channel)
	default:
		m.ctx.Privmsg(channel, "Syntax error!")
		m.ctx.Privmsg(channel, rssHelp)
	}
	return true
}

func (m *Module) addFeed(url, server, channel, user, options string) {
	refresh := defaultRefresh
	target := user // query by default
	tag := "RSS"
	grep := ""
	if channel != user {
		target = channel
	}

	if options != "" {
		if g := optRefreshRe.FindStringSubmatch(options); g != nil {
			var n int
			fmt.Sscanf(g[1], "%d", &n)
			if n <= minRefresh {
				m.ctx.Privmsg(channel, user+": Error - Refresh period invalid, minimum is 5 minutes.")
				return
			}
			refresh = n
		}
		if g := optTargetRe.FindStringSubmatch(options); g != nil {
			if g[1] == "query" {
				target = user
			} else {
				target = g[1]
			}
		}
		if g := optTagRe.FindStringSubmatch(options); g != nil {
			tag = g[1]
		}
		if g := optGrepRe.FindStringSubmatch(options); g != nil {
			grep = strings.ReplaceAll(g[1], `\"`, `"`)
			if _, err := regexp.Compile("(?i)" + grep); err != nil {
				m.ctx.Privmsg(channel, fmt.Sprintf("%s: Error, go back to regex school, failkut: %v", user, err))
				return
			}
		}
	}

	f, exists := m.feeds[url]
	if exists {
		if f.Subscriptions[server] != nil && f.Subscriptions[server][target] != nil {
			m.ctx.Privmsg(channel, user+": Error - Subscription for that feed/channel combination already exists.")
			return
		}
	} else {
		f = &feed{
			Tag: tag, Grep: grep, Refresh: refresh,
			History:       make(map[string]*item),
			Subscriptions: make(map[string]map[string]*subscription),
		}
		m.feeds[url] = f
	}
	if f.Subscriptions[server] == nil {
		f.Subscriptions[server] = make(map[string]*subscription)
	}
	f.Subscriptions[server][target] = &subscription{User: user, Received: make(map[string]int64)}
	if !exists {
		m.refreshFeed(url)
	}
	m.ctx.Privmsg(channel, fmt.Sprintf(
		"%s: Subscription added. It will be sent to %s with a refresh rate of %d minutes.",
		user, target, refresh))
	m.save()
}

func (m *Module) delFeed(url, server, channel, user string) {
	f := m.feeds[url]
	if f != nil && f.Subscriptions[server] == nil {
		if len(f.Subscriptions) == 0 {
			m.dropFeed(url)
			m.ctx.Privmsg(channel, user+": Feed had no subscriptions on any server: deleted.")
		} else {
			m.ctx.Privmsg(channel, fmt.Sprintf("%s: %s not subscribed to feed %s", user, channel, url))
		}
		return
	}
	if f == nil || f.Subscriptions[server] == nil {
		m.ctx.Privmsg(channel, user+": Error: no such subscription found for this url")
		return
	}
	target := channel
	if f.Subscriptions[server][target] == nil {
		m.ctx.Privmsg(channel, user+": Error: no such subscription found for this url")
		return
	}
	delete(f.Subscriptions[server], target)
	if len(f.Subscriptions[server]) == 0 {
		delete(f.Subscriptions, server)
	}
	if len(f.Subscriptions) == 0 {
		m.dropFeed(url)
	}
	m.ctx.Privmsg(channel, user+": OK: subscription removed.")
	m.save()
}

func (m *Module) dropFeed(url string) {
	if f := m.feeds[url]; f != nil {
		m.ctx.Sched.Unschedule(f.timer)
	}
	delete(m.feeds, url)
}

func (m *Module) refreshCmd(key, server, channel string) {
	url := key
	if m.feeds[url] == nil {
		matches := m.matchFeeds(key, server, channel)
		if len(matches) == 0 {
			m.ctx.Privmsg(channel, "No such feed!")
			return
		}
		url = matches[0]
	}
	m.ctx.Sched.Unschedule(m.feeds[url].timer)
	m.refreshFeed(url)
	m.ctx.Privmsg(channel, "Refresh scheduled!")
}

// refreshFeed starts a fetch for url if it still exists.
func (m *Module) refreshFeed(url string) {
	f := m.feeds[url]
	if f == nil || f.NoReschedule {
		return
	}
	f.active = true
	ok := m.fetch(url, fetch.Options{Timeout: 30 * time.Second}, func(res fetch.Result) {
		m.fetched(url, res)
	})
	if !ok {
		// fetcher busy with this url; try again in a minute
		f.active = false
		f.timer = m.ctx.Sched.After(time.Minute, func() { m.refreshFeed(url) })
	}
}

// fetched handles a fetch result: failures retry 3x after a minute
// then hourly; parse errors count toward the 100-strikes abandon;
// success merges items and broadcasts.
func (m *Module) fetched(url string, res fetch.Result) {
	f := m.feeds[url]
	if f == nil {
		return // deleted while fetching, bye feed
	}
	f.active = false

	if res.Err != nil || res.Status >= 400 {
		f.tries++
		delay := time.Minute
		if f.tries > 3 {
			f.tries = 0
			delay = time.Hour
		}
		f.timer = m.ctx.Sched.After(delay, func() { m.refreshFeed(url) })
		return
	}
	f.tries = 0

	head, items, err := parseFeed(res.Body)
	if err != nil {
		f.ErrorCount++
		if f.ErrorCount > maxErrors {
			f.NoReschedule = true // fuck this
			return
		}
		f.timer = m.ctx.Sched.After(time.Duration(f.Refresh)*time.Minute, func() { m.refreshFeed(url) })
		return
	}
	f.ErrorCount = 0
	f.Title, f.Description, f.Link = head.Title, head.Description, head.Link

	newItems := m.mergeItems(f, items)
	if newItems > 0 {
		m.broadcast(url)
		m.removeOldItems(f)
	}
	f.LastUpdate = m.now().Unix()
	f.timer = m.ctx.Sched.After(time.Duration(f.Refresh)*time.Minute, func() { m.refreshFeed(url) })
	m.save()
}

// mergeItems folds fetched items into history, applying the grep and
// assigning short ids to new ones.
func (m *Module) mergeItems(f *feed, items []feedItem) int {
	var grepRe *regexp.Regexp
	if f.Grep != "" {
		grepRe, _ = regexp.Compile("(?i)" + f.Grep) // validated at add
	}
	nowT := m.now().Unix()
	added := 0
	for _, fi := range items {
		if grepRe != nil && !grepRe.MatchString(fi.Title) {
			continue
		}
		if old := f.History[fi.Guid]; old != nil {
			old.Updated = nowT
			continue
		}
		f.History[fi.Guid] = &item{
			ID: m.ids.next(), Title: fi.Title, Link: fi.Link, Guid: fi.Guid,
			Description: fi.Description, Time: fi.Time.Unix(), Updated: nowT,
		}
		added++
	}
	return added
}

// broadcast sends unseen items to every subscription, oldest first,
// capped at 25 (5 for a first delivery).
func (m *Module) broadcast(url string) {
	f := m.feeds[url]
	guids := m.feedGuids(f) // newest first
	for server, byChannel := range f.Subscriptions {
		for channel, sub := range byChannel {
			maxItems := maxBroadcast
			if len(sub.Received) == 0 {
				maxItems = newSubItems // new user, no big backlog dump
			}
			var fresh []string
			for _, guid := range guids {
				if _, seen := sub.Received[guid]; !seen {
					fresh = append(fresh, guid)
				}
			}
			send := fresh[:min(len(fresh), maxItems)]
			for _, guid := range fresh {
				sub.Received[guid] = m.now().Unix()
			}
			for guid := range sub.Received {
				if f.History[guid] == nil {
					delete(sub.Received, guid)
				}
			}
			_ = server
			for i := len(send) - 1; i >= 0; i-- { // oldest first
				m.ctx.Privmsg(channel, m.printItem(f, f.History[send[i]]))
			}
		}
	}
}

// feedGuids returns history guids newest first (ties on guid for
// determinism; the perl left those to hash order).
func (m *Module) feedGuids(f *feed) []string {
	guids := make([]string, 0, len(f.History))
	for guid := range f.History {
		guids = append(guids, guid)
	}
	slices.SortFunc(guids, func(a, b string) int {
		if f.History[a].Time != f.History[b].Time {
			return int(f.History[b].Time - f.History[a].Time)
		}
		return strings.Compare(a, b)
	})
	return guids
}

// removeOldItems prunes stale history once a feed holds more than 10
// items.
func (m *Module) removeOldItems(f *feed) {
	if len(f.History) <= historyKeep {
		return
	}
	nowT := m.now()
	for guid, it := range f.History {
		if nowT.Sub(time.Unix(it.Updated, 0)) > historyMinAge {
			delete(f.History, guid)
			for _, byChannel := range f.Subscriptions {
				for _, sub := range byChannel {
					delete(sub.Received, guid)
				}
			}
		}
	}
}

// printItem is the broadcast line: [id, tag(, size)(, age)] title link,
// capped at 250 characters.
func (m *Module) printItem(f *feed, it *item) string {
	r := fmt.Sprintf("[{r}%s{/}, {y}%s{/}", it.ID, f.Tag)
	if g := sizeRe.FindStringSubmatch(it.Description); g != nil {
		r += sizeGB(g)
	}
	age := m.now().Sub(time.Unix(it.Time, 0))
	switch {
	case age > 12*time.Hour:
		r += ", " + time.Unix(it.Time, 0).Format("Mon 2 Jan 15:04")
	case age >= 5*time.Minute:
		switch {
		case age < time.Hour:
			r += fmt.Sprintf(", -%dmin", int(age.Minutes()))
		case age < 24*time.Hour:
			r += fmt.Sprintf(", -%.1fh", age.Hours())
		default:
			r += fmt.Sprintf(", -%dd", int(age.Hours()/24))
		}
	}
	r += fmt.Sprintf("] {c}%s{/} {w}%s{/}", it.Title, it.Link)
	if len(r) > 250 {
		r = r[:250] + "..."
	}
	return r
}

// sizeGB renders a size match as ", {g}X.XX{/} GB".
func sizeGB(g []string) string {
	var amount float64
	fmt.Sscanf(g[1], "%f", &amount)
	switch strings.ToUpper(g[3]) {
	case "M":
		amount /= 1024
	case "T":
		amount *= 1024
	}
	return fmt.Sprintf(", {g}%2.02f{/} GB", amount)
}

// findItem locates an item by short id across all feeds, urls sorted.
func (m *Module) findItem(id string) (*feed, *item, bool) {
	urls := make([]string, 0, len(m.feeds))
	for url := range m.feeds {
		urls = append(urls, url)
	}
	slices.Sort(urls)
	for _, url := range urls {
		for _, it := range m.feeds[url].History {
			if it.ID == id {
				return m.feeds[url], it, true
			}
		}
	}
	return nil, nil, false
}

// recall shows an item description; the same id again pages on.
func (m *Module) recall(id, channel string) {
	if ls := m.lastShown[channel]; ls != nil && ls.id == id {
		m.showMore(channel)
		return
	}
	f, it, ok := m.findItem(id)
	if !ok {
		m.ctx.Privmsg(channel, fmt.Sprintf("{r}Error{/}: no such item '%s' (learn to read)", id))
		return
	}

	text := it.Description
	size := ""
	if g := sizeRe.FindStringSubmatch(text); g != nil {
		size = sizeGB(g)
		text = sizeRe.ReplaceAllString(text, "")
	}
	first := fmt.Sprintf("{r}%s{/}, %s%s: ", id, f.Tag, size)
	rest := fmt.Sprintf("{r}%s{/}: ", id)
	width := 450 - len(rest)
	var parts []string
	for i, line := range format.WrapText(text, width) {
		if i == 0 {
			parts = append(parts, first+line)
		} else {
			parts = append(parts, rest+line)
		}
	}
	m.lastShown[channel] = &shownState{id: id, parts: parts, shown: -1}
	m.showMore(channel)
}

// showMore pages out the next burst of up to 3 description lines.
func (m *Module) showMore(channel string) {
	ls := m.lastShown[channel]
	if ls == nil {
		m.ctx.Privmsg(channel, "More? You haven't even told me what you want yet! {c}Idiot{/}.")
		return
	}
	if ls.shown >= len(ls.parts)-1 {
		m.ctx.Privmsg(channel, "{y}No more!{/}")
		return
	}
	burst := min(len(ls.parts)-1-ls.shown, 3)
	for i := range burst {
		line := ls.parts[ls.shown+1+i]
		if i == burst-1 {
			if left := len(ls.parts) - 1 - (ls.shown + burst); left > 0 {
				line += fmt.Sprintf(" {W}+%d{/}", left)
			}
		}
		m.ctx.Privmsg(channel, line)
	}
	ls.shown += burst
}

// matchFeeds returns urls whose url or tag contains key, filtered to
// feeds with a subscription on (server, channel); shortest match first.
func (m *Module) matchFeeds(key, server, channel string) []string {
	var out []string
	for url, f := range m.feeds {
		if server != "" {
			if f.Subscriptions[server] == nil {
				continue
			}
			if channel != "" && f.Subscriptions[server][channel] == nil {
				continue
			}
		}
		if key != "" {
			tag := colorJunkRe.ReplaceAllString(f.Tag, "")
			if !strings.Contains(strings.ToLower(url), strings.ToLower(key)) &&
				!strings.Contains(strings.ToLower(tag), strings.ToLower(key)) {
				continue
			}
		}
		out = append(out, url)
	}
	slices.SortFunc(out, func(a, b string) int {
		if len(a) != len(b) {
			return len(a) - len(b)
		}
		return strings.Compare(a, b)
	})
	return out
}

func (m *Module) showFeeds(key, server, channel, user string, showUrls bool) {
	matches := m.matchFeeds(key, server, channel)
	if len(matches) == 0 {
		with := ""
		if key != "" {
			with = " with " + key
		}
		m.ctx.Privmsg(channel, fmt.Sprintf("%s: No feeds found matching you%s.", user, with))
		return
	}
	suffix := ":"
	if key != "" {
		suffix = " matching " + key + ":"
	}
	m.ctx.Privmsg(channel, fmt.Sprintf("%s: Listing 'your' feeds%s", user, suffix))
	if showUrls {
		for _, url := range matches {
			m.ctx.Privmsg(channel, m.printFeed(url))
		}
		m.ctx.Privmsg(channel, "End of RSS feed list.")
		return
	}
	tags := make([]string, len(matches))
	for i, url := range matches {
		tags[i] = m.feeds[url].Tag
	}
	m.ctx.Privmsg(channel, fmt.Sprintf("%s: Your feed tags: %s", user, strings.Join(tags, ", ")))
}

func (m *Module) printFeed(url string) string {
	f := m.feeds[url]
	title, desc := f.Title, f.Description
	if title == "" {
		title = "Untitled"
	}
	if desc == "" {
		desc = "Undescribed"
	}
	return fmt.Sprintf("[%s] {W}%s{/}: {g}%s{/} | {m}%s{/} | updated %s ago",
		f.Tag, url, title, desc,
		m.now().Sub(time.Unix(f.LastUpdate, 0)).Truncate(time.Minute))
}

func (m *Module) showLastItems(key, server, channel string) {
	matches := m.matchFeeds(key, server, channel)
	if len(matches) == 0 {
		m.ctx.Privmsg(channel, fmt.Sprintf("Nothing relevant (for this channel) matches %s.", key))
		return
	}
	url := matches[0]
	f := m.feeds[url]
	m.ctx.Privmsg(channel, "Last items for "+url+":")
	guids := m.feedGuids(f) // newest first
	n := min(len(guids), 5)
	for i := n - 1; i >= 0; i-- { // oldest of the newest five first
		m.ctx.Privmsg(channel, m.printItem(f, f.History[guids[i]]))
	}
}

var _ = bus.None
