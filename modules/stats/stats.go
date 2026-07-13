// Package stats observes the humans: per (server, channel, nick)
// counters for chatter volume, link spam, sentiment, shouting,
// questions, /me actions, joins, kicks, and a time-of-day histogram.
// Not a port: the Perl never had this. Feeds three outputs: the
// !stats command (titles for the channel, numbers per nick), the
// prometheus registry (botje_user_* series, pushed from the dispatcher
// once a minute), and whatever future exposure BenV dreams up.
//
// Sentiment scoring is deliberately dumb and inspectable: word-boundary
// matches against conf-tweakable NL+EN wordlists plus a fixed smiley
// table. Own lines and queries are never counted, channel traffic only.
package stats

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"go-botje/internal/bus"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
)

// pushInterval is the metrics push cadence (dispatcher-side, so the
// scrape never touches module data).
const pushInterval = time.Minute

const (
	defaultHappyWords = "blij,leuk,mooi,lekker,super,geweldig,top,haha,hihi,lol,hehe,nice,great,awesome,happy,love,goed,prima,cool,hoera,bedankt,thanks,yay,gaaf,vet,heerlijk"
	defaultSadWords   = "jammer,helaas,slecht,kut,verdomme,shit,fuck,damn,sad,hate,haat,stom,klote,meh,argh,ugh,sucks,moe,ziek,depressief,huilen,balen,bah,rot,crap"
)

var (
	happySmileys = []string{":)", ":-)", ":D", ":-D", ":p", ":P", "xD", "^^", "😀", "😁", "😂", "🤣", "😊", "😍", "👍", "❤"}
	sadSmileys   = []string{":(", ":-(", ":'(", ";(", "😞", "😢", "😭", "💔"}
	linkRe       = regexp.MustCompile(`(?i)https?://|\bwww\.`)
)

// Tally is one (server, channel, nick)'s counters.
type Tally struct {
	Nick       string  `json:"nick"` // display casing, last seen
	Lines      int     `json:"lines"`
	Words      int     `json:"words"`
	Chars      int     `json:"chars"`
	Links      int     `json:"links"`
	Happy      int     `json:"happy"`
	Sad        int     `json:"sad"`
	Shouts     int     `json:"shouts"`
	Questions  int     `json:"questions"`
	Actions    int     `json:"actions"`
	Joins      int     `json:"joins"`
	KicksGiven int     `json:"kicks_given"`
	KicksGot   int     `json:"kicks_got"`
	Hours      [24]int `json:"hours"`
}

// Module implements module.Module.
type Module struct {
	Now func() time.Time // injectable for tests

	ctx     *module.Context
	tallies map[string]*Tally // "server channel nick" (lowercased key)

	// memoized wordlist sets, rebuilt when the conf string changes
	happyRaw, sadRaw string
	happySet, sadSet map[string]bool

	unloaded bool
}

// New returns an unloaded stats module.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "stats" }

func (m *Module) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Module) Load(ctx *module.Context) error {
	m.ctx = ctx
	ctx.Conf.CreateString("stats_happy_words", defaultHappyWords)
	ctx.Conf.CreateString("stats_sad_words", defaultSadWords)

	m.tallies = make(map[string]*Tally)
	all, err := ctx.Store.GetAll(m.Name())
	if err != nil {
		return fmt.Errorf("stats: load: %w", err)
	}
	for key, raw := range all {
		t := &Tally{}
		if err := json.Unmarshal(raw, t); err != nil {
			return fmt.Errorf("stats: load %q: %w", key, err)
		}
		m.tallies[key] = t
	}

	ctx.Cmd.Register(m.Name(), "stats", m.cbStats)
	for ev, fn := range map[string]bus.Handler{
		"IRC_PRIVMSG": m.onPrivmsg,
		"IRC_JOIN":    m.onJoin,
		"IRC_KICK":    m.onKick,
	} {
		if err := ctx.Bus.RegisterHook(m.Name(), ev, fn); err != nil {
			return err
		}
	}
	if ctx.Metrics != nil {
		var push func()
		push = func() {
			if m.unloaded {
				return
			}
			m.pushMetrics()
			m.ctx.Sched.After(pushInterval, push)
		}
		m.ctx.Sched.After(pushInterval, push)
		m.pushMetrics() // series appear right after boot
	}
	return nil
}

func (m *Module) Unload() error {
	m.unloaded = true
	m.ctx.Bus.UnregisterModule(m.Name())
	m.ctx.Cmd.UnregisterModule(m.Name())
	return nil
}

// tally returns (creating if needed) the counters for nick in channel.
func (m *Module) tally(server, channel, nick string) *Tally {
	key := server + " " + strings.ToLower(channel) + " " + strings.ToLower(nick)
	t := m.tallies[key]
	if t == nil {
		t = &Tally{}
		m.tallies[key] = t
	}
	t.Nick = nick
	m.ctx.Saver.Mark(m.Name(), key, func() any { return t })
	return t
}

func isChannel(target string) bool {
	return target != "" && (target[0] == '#' || target[0] == '&')
}

func (m *Module) onPrivmsg(ev *bus.Event) (bus.Handled, any) {
	if ev.SenderMe || ev.Query || !isChannel(ev.Channel) || ev.Sender.Nick == "" {
		return bus.None, nil
	}
	msg := ev.Msg
	action := false
	if rest, ok := strings.CutPrefix(msg, "\x01ACTION "); ok {
		action = true
		msg = strings.TrimSuffix(rest, "\x01")
	} else if strings.HasPrefix(msg, "\x01") {
		return bus.None, nil // other CTCP is not chat
	}

	t := m.tally(ev.Server, ev.Channel, ev.Sender.Nick)
	t.Lines++
	t.Chars += len(msg)
	t.Words += len(strings.Fields(msg))
	t.Hours[m.now().Hour()]++
	if action {
		t.Actions++
	}
	t.Links += len(linkRe.FindAllStringIndex(msg, -1))
	if strings.HasSuffix(strings.TrimSpace(msg), "?") {
		t.Questions++
	}
	if isShout(msg) {
		t.Shouts++
	}
	happy, sad := m.sentiment(msg)
	t.Happy += happy
	t.Sad += sad
	return bus.None, nil
}

func (m *Module) onJoin(ev *bus.Event) (bus.Handled, any) {
	if ev.SenderMe || !isChannel(ev.Channel) || ev.Sender.Nick == "" {
		return bus.None, nil
	}
	m.tally(ev.Server, ev.Channel, ev.Sender.Nick).Joins++
	return bus.None, nil
}

func (m *Module) onKick(ev *bus.Event) (bus.Handled, any) {
	channel, _ := ev.Extra["channel"].(string)
	target, _ := ev.Extra["target"].(string)
	if !isChannel(channel) || target == "" {
		return bus.None, nil
	}
	if !ev.SenderMe && ev.Sender.Nick != "" {
		m.tally(ev.Server, channel, ev.Sender.Nick).KicksGiven++
	}
	if target != ev.BotNick {
		m.tally(ev.Server, channel, target).KicksGot++
	}
	return bus.None, nil
}

// isShout: at least 8 uppercase letters and not a single lowercase one.
func isShout(msg string) bool {
	upper, lower := 0, 0
	for _, r := range msg {
		switch {
		case unicode.IsUpper(r):
			upper++
		case unicode.IsLower(r):
			lower++
		}
	}
	return upper >= 8 && lower == 0
}

// sentiment counts happy and sad hits: word-boundary matches against
// the conf wordlists plus smiley substrings.
func (m *Module) sentiment(msg string) (happy, sad int) {
	m.refreshWordSets()
	for _, w := range strings.Fields(strings.ToLower(msg)) {
		w = strings.TrimFunc(w, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
		if m.happySet[w] {
			happy++
		}
		if m.sadSet[w] {
			sad++
		}
	}
	for _, s := range happySmileys {
		happy += strings.Count(msg, s)
	}
	for _, s := range sadSmileys {
		sad += strings.Count(msg, s)
	}
	return happy, sad
}

func (m *Module) refreshWordSets() {
	if raw := m.ctx.Conf.String("stats_happy_words"); raw != m.happyRaw {
		m.happyRaw, m.happySet = raw, wordSet(raw)
	}
	if raw := m.ctx.Conf.String("stats_sad_words"); raw != m.sadRaw {
		m.sadRaw, m.sadSet = raw, wordSet(raw)
	}
}

func wordSet(raw string) map[string]bool {
	set := make(map[string]bool)
	for _, w := range strings.Split(raw, ",") {
		if w = strings.ToLower(strings.TrimSpace(w)); w != "" {
			set[w] = true
		}
	}
	return set
}

// channelTallies returns the tallies for one channel, display order by
// lines descending.
func (m *Module) channelTallies(server, channel string) []*Tally {
	prefix := server + " " + strings.ToLower(channel) + " "
	var out []*Tally
	for key, t := range m.tallies {
		if strings.HasPrefix(key, prefix) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Lines > out[j].Lines })
	return out
}

// cbStats: "!stats" = channel titles, "!stats <nick>" = one nick's
// numbers. Channel context required.
func (m *Module) cbStats(d *cmd.Data) bool {
	ev := d.Event
	if ev.Query || !isChannel(ev.Channel) {
		m.ctx.Privmsg(ev.Channel, "Stats werken alleen in een kanaal.")
		return true
	}
	arg := strings.TrimSpace(d.Data)
	if arg != "" {
		m.replyNick(ev.Server, ev.Channel, arg)
		return true
	}
	m.replyTitles(ev.Server, ev.Channel)
	return true
}

func (m *Module) replyNick(server, channel, nick string) {
	key := server + " " + strings.ToLower(channel) + " " + strings.ToLower(nick)
	t := m.tallies[key]
	if t == nil {
		m.ctx.Privmsg(channel, fmt.Sprintf("Niets geteld voor %s in %s.", nick, channel))
		return
	}
	night := t.Hours[0] + t.Hours[1] + t.Hours[2] + t.Hours[3] + t.Hours[4] + t.Hours[5]
	m.ctx.Privmsg(channel, fmt.Sprintf(
		"{b}%s{b} in %s: %d regels, %d woorden, %d links, %d vragen, %d geschreeuwd, %d acties, {g}%d blij{n}/{r}%d somber{n}, %d joins, kicks %d/%d, %d nachtregels",
		t.Nick, channel, t.Lines, t.Words, t.Links, t.Questions, t.Shouts, t.Actions,
		t.Happy, t.Sad, t.Joins, t.KicksGiven, t.KicksGot, night))
}

func (m *Module) replyTitles(server, channel string) {
	ts := m.channelTallies(server, channel)
	if len(ts) == 0 {
		m.ctx.Privmsg(channel, "Nog niets geteld hier.")
		return
	}
	type title struct {
		name  string
		score func(*Tally) int
		unit  string
	}
	titles := []title{
		{"Kletskous", func(t *Tally) int { return t.Lines }, "regels"},
		{"Linkslinger", func(t *Tally) int { return t.Links }, "links"},
		{"Vrolijkste", func(t *Tally) int { return t.Happy - t.Sad }, "blijheid"},
		{"Zwartkijker", func(t *Tally) int { return t.Sad - t.Happy }, "somberheid"},
		{"Schreeuwlelijk", func(t *Tally) int { return t.Shouts }, "keer geschreeuwd"},
		{"Vraagteken", func(t *Tally) int { return t.Questions }, "vragen"},
		{"Nachtbraker", func(t *Tally) int {
			return t.Hours[0] + t.Hours[1] + t.Hours[2] + t.Hours[3] + t.Hours[4] + t.Hours[5]
		}, "nachtregels"},
		{"Draaideur", func(t *Tally) int { return t.Joins }, "joins"},
	}
	var lines []string
	for _, ti := range titles {
		best, score := (*Tally)(nil), 0
		for _, t := range ts {
			if s := ti.score(t); s > score {
				best, score = t, s
			}
		}
		if best != nil {
			lines = append(lines, fmt.Sprintf("{b}%s{b}: %s (%d %s)", ti.name, best.Nick, score, ti.unit))
		}
	}
	m.ctx.Privmsg(channel, strings.Join(lines, "\n"))
}

// pushMetrics publishes the counters as prometheus series. Runs on the
// dispatcher; the registry does its own locking.
func (m *Module) pushMetrics() {
	reg := m.ctx.Metrics
	for key, t := range m.tallies {
		parts := strings.SplitN(key, " ", 3)
		if len(parts) != 3 {
			continue
		}
		labels := map[string]string{"server": parts[0], "channel": parts[1], "nick": parts[2]}
		for name, v := range map[string]int{
			"botje_user_lines_total":       t.Lines,
			"botje_user_words_total":       t.Words,
			"botje_user_links_total":       t.Links,
			"botje_user_happy_total":       t.Happy,
			"botje_user_sad_total":         t.Sad,
			"botje_user_shouts_total":      t.Shouts,
			"botje_user_questions_total":   t.Questions,
			"botje_user_actions_total":     t.Actions,
			"botje_user_joins_total":       t.Joins,
			"botje_user_kicks_given_total": t.KicksGiven,
			"botje_user_kicks_got_total":   t.KicksGot,
		} {
			reg.SetCounter(name, labels, float64(v))
		}
	}
}
