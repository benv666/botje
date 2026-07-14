package rss

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/fetch"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
	"go-botje/internal/sched"
	"go-botje/internal/storage"
)

type fixture struct {
	m       *Module
	b       *bus.Bus
	cmds    *cmd.Registry
	sch     *sched.Sched
	saver   *storage.Saver
	clk     time.Time
	sent    []string
	fetched []string
	cbs     map[string]func(fetch.Result)
}

func newFixtureAt(t *testing.T, store storage.Store, clk time.Time) *fixture {
	t.Helper()
	f := &fixture{clk: clk, cbs: map[string]func(fetch.Result){}}
	f.b = bus.New()
	f.b.RegisterEvent("IRC_PRIVMSG")
	f.cmds = cmd.New()
	f.sch = sched.New(func() time.Time { return f.clk })
	f.m = New()
	f.m.Now = func() time.Time { return f.clk }
	f.m.Rand = func() float64 { return 0 } // restore stagger offset 0
	f.m.fetch = func(url string, opts fetch.Options, cb func(fetch.Result)) bool {
		if _, dup := f.cbs[url]; dup {
			return false
		}
		f.fetched = append(f.fetched, url)
		f.cbs[url] = cb
		return true
	}
	f.saver = storage.NewSaver(store,
		func(fn func()) { fn() },
		func(err error) { t.Errorf("saver: %v", err) })
	err := f.m.Load(&module.Context{
		Bus: f.b, Cmd: f.cmds, Store: store, Sched: f.sch, Saver: f.saver,
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

func newFixture(t *testing.T, store storage.Store) *fixture {
	return newFixtureAt(t, store, time.Date(2026, 7, 3, 12, 0, 0, 0, time.Local))
}

func (f *fixture) rss(nick, data string) {
	msg := "!rss"
	if data != "" {
		msg += " " + data
	}
	f.raw(nick, msg)
}

func (f *fixture) raw(nick, msg string) {
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: "#testing",
		Msg: msg, Extra: map[string]any{}}
	ev.Sender.Nick = nick
	f.b.Submit(ev)
	f.cmds.Handle(ev)
}

func (f *fixture) complete(t *testing.T, url, body string) {
	t.Helper()
	cb, ok := f.cbs[url]
	if !ok {
		t.Fatalf("no fetch in flight for %q (have %q)", url, f.fetched)
	}
	delete(f.cbs, url)
	cb(fetch.Result{URL: url, Status: 200, Body: []byte(body)})
}

func (f *fixture) fail(t *testing.T, url string) {
	t.Helper()
	cb, ok := f.cbs[url]
	if !ok {
		t.Fatalf("no fetch in flight for %q", url)
	}
	delete(f.cbs, url)
	cb(fetch.Result{URL: url, Err: fmt.Errorf("connection refused")})
}

func (f *fixture) advance(d time.Duration) {
	f.clk = f.clk.Add(d)
	f.sch.RunDue()
}

func (f *fixture) take() []string {
	s := f.sent
	f.sent = nil
	return s
}

const feedURL = "http://feeds.example/nieuws.xml"

// rssWith builds a feed doc with the given item titles, one hour apart
// ending now-ish.
func rssWith(titles ...string) string {
	var items strings.Builder
	for i, title := range titles {
		when := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Hour)
		fmt.Fprintf(&items, `<item><title>%s</title><link>http://x/%d</link><guid>g-%s</guid>
			<description>desc %s</description><pubDate>%s</pubDate></item>`,
			title, i, title, title, when.Format("Mon, 02 Jan 2006 15:04:05 -0700"))
	}
	return `<?xml version="1.0"?><rss version="2.0"><channel><title>Nieuws</title>
		<description>Testfeed</description><link>http://x</link>` + items.String() + `</channel></rss>`
}

func (f *fixture) subscribe(t *testing.T, titles ...string) {
	t.Helper()
	f.rss("BenV", "add "+feedURL)
	f.complete(t, feedURL, rssWith(titles...))
	f.take()
}

func TestAddSubscribesAndBroadcasts(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.rss("BenV", "add "+feedURL)
	if len(f.fetched) != 1 || f.fetched[0] != feedURL {
		t.Fatalf("fetched = %q", f.fetched)
	}
	got := f.take()
	want := "#testing|BenV: Subscription added. It will be sent to #testing with a refresh rate of 30 minutes."
	if len(got) != 1 || got[0] != want {
		t.Fatalf("reply = %q, want %q", got, want)
	}

	f.complete(t, feedURL, rssWith("een", "twee"))
	got = f.take()
	if len(got) != 2 {
		t.Fatalf("broadcast = %q, want 2 items", got)
	}
	// oldest first; format: [{r}ID{/}, {y}TAG{/}, agepart] {c}title{/} {w}link{/}
	if !strings.Contains(got[0], ", {y}RSS{/}") || !strings.Contains(got[0], "{c}een{/} {w}http://x/0{/}") {
		t.Fatalf("item line = %q", got[0])
	}
	if !strings.Contains(got[1], "{c}twee{/}") {
		t.Fatalf("second item = %q", got[1])
	}
}

func TestOnlyNewItemsBroadcastOnRefresh(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.subscribe(t, "een", "twee")

	f.advance(30 * time.Minute) // refresh timer
	if len(f.fetched) != 2 {
		t.Fatalf("no refetch after refresh period: %q", f.fetched)
	}
	f.complete(t, feedURL, rssWith("een", "twee", "drie"))
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "{c}drie{/}") {
		t.Fatalf("refresh broadcast = %q, want just drie", got)
	}
}

func TestNewSubscriberGetsAtMostFive(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.rss("BenV", "add "+feedURL)
	f.take()
	f.complete(t, feedURL, rssWith("a", "b", "c", "d", "e", "f", "g"))
	got := f.take()
	if len(got) != 5 {
		t.Fatalf("new subscriber got %d items, want 5", len(got))
	}
	// the five newest, oldest of those first
	if !strings.Contains(got[0], "{c}c{/}") || !strings.Contains(got[4], "{c}g{/}") {
		t.Fatalf("items = %q", got)
	}
}

func TestAddOptionValidation(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.rss("BenV", "add "+feedURL+" refresh=3")
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0], "minimum is 5 minutes") {
		t.Fatalf("refresh=3 reply = %q", got)
	}
	f.rss("BenV", "add "+feedURL+" refresh=60 tag=Nieuws")
	if got := f.take(); !strings.Contains(got[0], "refresh rate of 60 minutes") {
		t.Fatalf("refresh=60 reply = %q", got)
	}
	f.complete(t, feedURL, rssWith("een"))
	if got := f.take(); !strings.Contains(got[0], ", {y}Nieuws{/}") {
		t.Fatalf("tag not used: %q", got)
	}
	// duplicate
	f.rss("BenV", "add "+feedURL)
	if got := f.take(); !strings.Contains(got[0], "already exists") {
		t.Fatalf("duplicate reply = %q", got)
	}
	// broken grep
	f.rss("BenV", `add http://other.example/f.xml grep="(["`)
	if got := f.take(); !strings.Contains(got[0], "go back to regex school, failkut") {
		t.Fatalf("bad grep reply = %q", got)
	}
}

func TestGrepFiltersTitles(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.rss("BenV", `add `+feedURL+` grep="bier"`)
	f.take()
	f.complete(t, feedURL, rssWith("bier bericht", "wijn bericht", "meer BIER"))
	got := f.take()
	if len(got) != 2 {
		t.Fatalf("grep broadcast = %q, want the 2 bier items (case-insensitive)", got)
	}
}

func TestQueryTarget(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.rss("BenV", "add "+feedURL+" query")
	if got := f.take(); !strings.Contains(got[0], "sent to BenV") {
		t.Fatalf("query add reply = %q", got)
	}
	f.complete(t, feedURL, rssWith("prive"))
	got := f.take()
	if len(got) != 1 || !strings.HasPrefix(got[0], "BenV|") {
		t.Fatalf("query broadcast = %q, want to nick", got)
	}
}

func TestDescriptionRecallAndPaging(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.rss("BenV", "add "+feedURL)
	f.take()
	long := strings.Repeat("heel lang verhaal over van alles en nog wat ", 40)
	doc := strings.Replace(rssWith("item"), "desc item", "size: 4.7 GB "+long, 1)
	f.complete(t, feedURL, doc)
	items := f.take()
	id := strings.SplitN(strings.SplitN(items[0], "{r}", 2)[1], "{/}", 2)[0]

	// recall: first burst of 3 wrapped lines, {W}+N on the last
	f.rss("BenV", id)
	got := f.take()
	if len(got) != 3 {
		t.Fatalf("recall = %d lines, want 3", len(got))
	}
	if !strings.HasPrefix(got[0], "#testing|{r}"+id+"{/}, RSS, {g}4.70{/} GB: ") {
		t.Fatalf("first line = %q", got[0])
	}
	if !strings.Contains(got[2], "{W}+") {
		t.Fatalf("no +N suffix: %q", got[2])
	}
	// same id again pages on (also via the bare default command)
	f.raw("BenV", "!"+id)
	got = f.take()
	if len(got) == 0 || strings.Contains(got[0], "no such item") {
		t.Fatalf("paging = %q", got)
	}
	// drain to the end
	for range 20 {
		f.rss("BenV", id)
		out := f.take()
		if len(out) == 1 && strings.Contains(out[0], "{y}No more!{/}") {
			return
		}
	}
	t.Fatal("paging never reached No more!")
}

func TestUnknownItem(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.subscribe(t, "een")
	f.rss("BenV", "ZZ9")
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0], "no such item 'ZZ9' (learn to read)") {
		t.Fatalf("unknown item = %q", got)
	}
	// bare !ZZ9 must NOT reply (default handler passes when unknown)
	f.raw("BenV", "!ZZ9")
	if got := f.take(); len(got) != 0 {
		t.Fatalf("bare unknown id replied: %q", got)
	}
}

func TestListAndLast(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.subscribe(t, "een", "twee")
	f.rss("BenV", "list")
	got := f.take()
	if len(got) != 2 || !strings.Contains(got[1], "Your feed tags: RSS") {
		t.Fatalf("list = %q", got)
	}
	f.rss("BenV", "list url")
	got = f.take()
	if len(got) != 3 || !strings.Contains(got[1], feedURL) || got[2] != "#testing|End of RSS feed list." {
		t.Fatalf("list url = %q", got)
	}
	f.rss("BenV", "last nieuws")
	got = f.take()
	if len(got) != 3 || !strings.Contains(got[0], "Last items for "+feedURL) {
		t.Fatalf("last = %q", got)
	}
	if !strings.Contains(got[1], "{c}een{/}") || !strings.Contains(got[2], "{c}twee{/}") {
		t.Fatalf("last items order = %q", got)
	}
}

func TestDel(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.subscribe(t, "een")
	f.rss("BenV", "del "+feedURL)
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0], "OK: subscription removed.") {
		t.Fatalf("del = %q", got)
	}
	f.advance(2 * time.Hour)
	if len(f.fetched) != 1 {
		t.Fatalf("feed still polled after del: %q", f.fetched)
	}
	f.rss("BenV", "del "+feedURL)
	if got := f.take(); !strings.Contains(got[0], "no such subscription") {
		t.Fatalf("second del = %q", got)
	}
}

func TestRefreshCommand(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.subscribe(t, "een")
	f.rss("BenV", "refresh nieuws")
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|Refresh scheduled!" {
		t.Fatalf("refresh = %q", got)
	}
	if len(f.fetched) != 2 {
		t.Fatalf("refresh did not refetch: %q", f.fetched)
	}
	f.rss("BenV", "refresh spookfeed")
	if got := f.take(); !strings.Contains(got[0], "No such feed!") {
		t.Fatalf("refresh unknown = %q", got)
	}
}

func TestFetchErrorRetries(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.rss("BenV", "add "+feedURL)
	f.take()
	// three failures retry after a minute each, the fourth backs off an hour
	for i := range 3 {
		f.fail(t, feedURL)
		f.advance(time.Minute)
		if len(f.fetched) != i+2 {
			t.Fatalf("retry %d missing: %q", i+1, f.fetched)
		}
	}
	f.fail(t, feedURL)
	f.advance(time.Minute)
	if len(f.fetched) != 4 {
		t.Fatalf("fourth failure retried after a minute, want hour backoff: %q", f.fetched)
	}
	f.advance(time.Hour)
	if len(f.fetched) != 5 {
		t.Fatalf("hour backoff retry missing: %q", f.fetched)
	}
}

func TestHistoryPruned(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	titles := strings.Fields("a b c d e f g h i j k l")
	f.subscribe(t, titles...)

	// two days later everything is stale; a fresh fetch with new items
	// prunes the old ones (history > 10)
	f.advance(48 * time.Hour)
	f.rss("BenV", "refresh nieuws")
	f.take()
	f.complete(t, feedURL, rssWith("nieuw1", "nieuw2"))
	f.take()
	if got := len(f.m.feeds[feedURL].History); got != 2 {
		t.Fatalf("history = %d items after prune, want 2", got)
	}
}

func TestRestoreResumesPolling(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	f.subscribe(t, "een")
	f.m.Unload()

	f2 := newFixtureAt(t, store, f.clk.Add(5*time.Minute))
	f2.advance(1 * time.Second) // stagger offset 0 (rand stubbed)
	if len(f2.fetched) != 1 || f2.fetched[0] != feedURL {
		t.Fatalf("restored feed not polled: %q", f2.fetched)
	}
	// history and ids survived: recall still works
	f2.complete(t, feedURL, rssWith("een"))
	f2.take()
	f2.rss("BenV", "list")
	if got := f2.take(); !strings.Contains(got[1], "Your feed tags: RSS") {
		t.Fatalf("restored list = %q", got)
	}
}

func TestHelpAndSyntaxError(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.rss("BenV", "")
	if got := f.take(); len(got) != 2 || !strings.Contains(got[0], "Usage  : !rss") {
		t.Fatalf("help = %q", got)
	}
	f.rss("BenV", "del te veel argumenten hier")
	got := f.take()
	if len(got) != 3 || got[0] != "#testing|Syntax error!" {
		t.Fatalf("syntax error = %q", got)
	}
}

// received-guids marked during a broadcast survive a restart: without
// this every dev-cycle boot re-sent the same items (BenV 2026-07-14).
func TestReceivedSurvivesRestart(t *testing.T) {
	store := storage.NewMemory()
	f := newFixture(t, store)
	f.subscribe(t, "een", "twee")
	// a REFRESH delivers drie: this mark only exists in RAM unless the
	// broadcast path persists it (the add path always saved)
	f.advance(30 * time.Minute)
	f.complete(t, feedURL, rssWith("een", "twee", "drie"))
	if got := f.take(); len(got) != 1 {
		t.Fatalf("refresh broadcast = %q", got)
	}
	if err := f.saver.FlushSync(); err != nil {
		t.Fatal(err)
	}

	// "restart": fresh module on the same store; the restore poll finds
	// the same three items and must stay quiet
	f2 := newFixtureAt(t, store, f.clk.Add(time.Minute))
	f2.advance(16 * time.Second) // restore stagger (offset 0 + 15s base)
	f2.complete(t, feedURL, rssWith("een", "twee", "drie"))
	if got := f2.take(); len(got) != 0 {
		t.Fatalf("restart re-broadcast already-seen items: %q", got)
	}
}
