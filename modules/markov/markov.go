// Package markov learns channel chatter into word chains and talks
// back: !talk [seed], any unknown !command (the "!je moeder" feature,
// which also means no Levenshtein suggestions while markov is loaded,
// same as the Perl), "talk" in query, and an optional idle talker.
// Ported from IRC_Markov.pm.
//
// The dictionary lives in memory and persists as one row per top-level
// word under namespace "markov" (name "dictionary_<order>_<dict>:<word>"),
// loaded in bulk at boot and saved through the shared Saver: learning
// marks only the words a line touched, the core flushes every minute.
// This replaces the pre-2026-07-13 whole-dictionary blob whose put
// rewrote ~8 MB synchronously on the dispatcher every 51st learned
// line (the Perl saved that way too, but to a local Storable file). A
// legacy blob is split into rows once at load. Known trade-off: an
// admin unload+reload mid-run can lose marks that have not flushed yet
// (< 1 minute of chatter); the Perl lost everything since its last
// 51-line save on a crash.
//
// Divergence: sanitizeWord handles contractions deliberately (the Perl
// matched them against $_ by accident) and keeps the apostrophe.
package markov

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"go-botje/internal/bus"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
	"go-botje/internal/sched"
)

// node is one chain link: how often this word followed the path here,
// and what followed it.
type Node struct {
	Count    int              `json:"c"`
	Children map[string]*Node `json:"n,omitempty"`
}

var (
	botNickRe     = regexp.MustCompile(`(?i)(the_baby|hoer|dromertje|calvin|bot|lippy)`)
	nickRe        = regexp.MustCompile(`(?i)(benv|lotjuh)`)
	badEndRe      = regexp.MustCompile(`(?i)^(de|het|een|the|a|an|is|or|of|and|en|,|:)$`)
	eolRe         = regexp.MustCompile(`[?!.]+$`)
	smileyRe      = regexp.MustCompile(`[:;>^]-?[)(pdDPX^]`)
	wordPunctRe   = regexp.MustCompile(`^(\w+)(\W)$`)
	contractRe    = regexp.MustCompile(`^\w+'\w+$`)
	plainWordRe   = regexp.MustCompile(`^\w+$`)
	queryTalkRe   = regexp.MustCompile(`(?i)^talk\s*(.*)$`)
	joinPunctRe   = regexp.MustCompile(`\s+([,!?.;:%])`)
	cmdWordRe     = regexp.MustCompile(`^!(\w+)`)
	idleChanSepRe = regexp.MustCompile(`\s+`)
)

type idleState struct {
	lastAct time.Time
	timer   sched.Tag
	set     bool
}

// Module implements module.Module.
type Module struct {
	// Now and Rand are injectable for tests.
	Now  func() time.Time
	Rand func() float64

	ctx        *module.Context
	order      int
	dictionary string
	chains     map[string]*Node
	keys       []string              // top-level key cache for random picks
	idle       map[string]*idleState // per channel
}

// New returns an unloaded markov module.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "markov" }

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
	ctx.Conf.CreateString("markov_dictionary", "default")
	ctx.Conf.CreateInt("markov_order", 3)
	ctx.Conf.CreateBool("markov_idle_talk", false)
	ctx.Conf.CreateInt("markov_idle_talk_timeout", 240) // minutes
	ctx.Conf.CreateString("markov_idle_talk_channels", "")

	m.order = ctx.Conf.Int("markov_order")
	m.dictionary = strings.ToLower(strings.TrimSpace(ctx.Conf.String("markov_dictionary")))
	m.idle = make(map[string]*idleState)

	if err := m.loadDictionary(); err != nil {
		return err
	}
	m.keys = make([]string, 0, len(m.chains))
	for w := range m.chains {
		m.keys = append(m.keys, w)
	}
	slices.Sort(m.keys) // the perl cached hash order; sorted is deterministic

	ctx.Cmd.Register(m.Name(), "talk", m.cbTalk)
	// the perl registered cbTalk as default twice; the (priority 1,
	// continue false) one answers every unknown !command
	ctx.Cmd.RegisterDefault(m.Name(), 0, true, m.cbTalk)
	ctx.Cmd.RegisterDefault(m.Name(), 1, false, m.cbTalk)
	return ctx.Bus.RegisterHook(m.Name(), "IRC_PRIVMSG", m.onPrivmsg)
}

func (m *Module) Unload() error {
	for _, st := range m.idle {
		if st.set {
			m.ctx.Sched.Unschedule(st.timer)
		}
	}
	m.ctx.Bus.UnregisterModule(m.Name())
	m.ctx.Cmd.UnregisterModule(m.Name())
	// pending saver marks survive unload and flush on the core cadence
	return nil
}

func (m *Module) storeKey() string {
	return fmt.Sprintf("dictionary_%d_%s", m.order, m.dictionary)
}

// loadDictionary bulk-loads the per-word rows, migrating a legacy
// whole-dictionary blob into rows once (the blob wins over any rows
// from a previously interrupted migration; the delete only happens
// after all rows landed, so a crash mid-migration is retried).
func (m *Module) loadDictionary() error {
	m.chains = make(map[string]*Node)
	all, err := m.ctx.Store.GetAll(m.Name())
	if err != nil {
		return fmt.Errorf("markov: load dictionary: %w", err)
	}
	prefix := m.storeKey() + ":"
	for name, raw := range all {
		w, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}
		nd := &Node{}
		if err := json.Unmarshal(raw, nd); err != nil {
			return fmt.Errorf("markov: load word %q: %w", w, err)
		}
		m.chains[w] = nd
	}
	raw, isLegacy := all[m.storeKey()]
	if !isLegacy {
		return nil
	}
	legacy := make(map[string]*Node)
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return fmt.Errorf("markov: parse legacy dictionary: %w", err)
	}
	m.chains = legacy
	batch := make(map[string]any, len(legacy))
	for w, nd := range legacy {
		batch[prefix+w] = nd
	}
	if err := m.ctx.Store.PutMany(m.Name(), batch); err != nil {
		return fmt.Errorf("markov: migrate dictionary to rows: %w", err)
	}
	// keep the blob under a backup name instead of deleting it: a
	// rolled-back pre-rows build would otherwise boot with an empty
	// dictionary (it can be renamed back by hand)
	if err := m.ctx.Store.Put(m.Name(), m.storeKey()+"_blob_backup", json.RawMessage(raw)); err != nil {
		return fmt.Errorf("markov: back up legacy dictionary: %w", err)
	}
	if err := m.ctx.Store.Delete(m.Name(), m.storeKey()); err != nil {
		return fmt.Errorf("markov: drop legacy dictionary: %w", err)
	}
	slog.Info("markov: migrated dictionary blob to per-word rows", "words", len(legacy))
	return nil
}

// markDirty queues the word's subtree for the next saver flush. The
// node pointer is stable (words are never deleted), so the flush
// serializes whatever the counts are by then.
func (m *Module) markDirty(w string) {
	nd := m.chains[w]
	m.ctx.Saver.Mark(m.Name(), m.storeKey()+":"+w, func() any { return nd })
}

func (m *Module) cbTalk(d *cmd.Data) bool {
	if d.Event.SenderMe {
		return false
	}
	m.ctx.Privmsg(d.Event.Channel, m.randomMessage(d.Data))
	return true
}

func (m *Module) onPrivmsg(ev *bus.Event) (bus.Handled, any) {
	if ev.SenderMe || ev.Msg == "" {
		return bus.None, nil
	}
	if isBot(ev.Sender.Nick) {
		return bus.None, nil
	}
	if ev.Query {
		if g := queryTalkRe.FindStringSubmatch(ev.Msg); g != nil {
			m.ctx.Privmsg(ev.Channel, m.randomMessage(g[1]))
			return bus.Replied, nil
		}
		return bus.None, nil
	}
	if m.isIdleChannel(ev.Channel) {
		st := m.idleFor(ev.Channel)
		st.lastAct = m.now()
		m.scheduleIdleTalk(ev.Channel)
	}
	// never learn registered commands; could contain a password
	if g := cmdWordRe.FindStringSubmatch(ev.Msg); g != nil && m.ctx.Cmd.Has(g[1]) {
		return bus.None, nil
	}
	m.addLine(ev.Msg)
	return bus.None, nil
}

func isBot(nick string) bool {
	return nick == "X" || nick == "x" || botNickRe.MatchString(nick)
}

// --- learning

func (m *Module) addLine(msg string) {
	var words []string
	for w := range strings.FieldsSeq(msg) {
		words = append(words, sanitizeWord(w)...)
	}
	if len(words) == 0 {
		return
	}
	last := words[len(words)-1]
	if !isEol(last) && !isBadEnd(last) {
		words = append(words, ".")
	}

	var prev []string
	for len(prev) < m.order && len(words) > 0 {
		prev = append(prev, words[0])
		words = words[1:]
	}
	if len(words) == 0 {
		m.addWords(prev)
	} else {
		for len(words) > 0 {
			prev = append(prev, words[0])
			words = words[1:]
			m.addWords(prev)
			prev = prev[1:]
		}
	}

}

// addWords bumps the chain counts along one window of order+1 words.
func (m *Module) addWords(words []string) {
	var nd *Node
	for i := 0; i <= m.order && i < len(words); i++ {
		w := words[i]
		if i == 0 {
			if m.chains[w] == nil {
				m.chains[w] = &Node{}
				m.keys = append(m.keys, w)
				slices.Sort(m.keys)
			}
			nd = m.chains[w]
			m.markDirty(w)
		} else {
			if nd.Children == nil {
				nd.Children = make(map[string]*Node)
			}
			if nd.Children[w] == nil {
				nd.Children[w] = &Node{}
			}
			nd = nd.Children[w]
		}
		nd.Count++
	}
}

// sanitizeWord normalizes one token: lowercased, known nicks
// capitalized, trailing punctuation split off, contractions kept whole,
// non-words dropped.
func sanitizeWord(w string) []string {
	w = strings.TrimSpace(w)
	w = strings.NewReplacer("\r", "", "\n", "").Replace(w)
	w = strings.ToLower(w)
	if nickRe.MatchString(w) {
		w = capitalize(w)
	}
	if g := wordPunctRe.FindStringSubmatch(w); g != nil {
		return []string{g[1], g[2]}
	}
	if contractRe.MatchString(w) || plainWordRe.MatchString(w) {
		return []string{w}
	}
	return nil
}

func capitalize(w string) string {
	r := []rune(w)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

func isBadEnd(w string) bool { return badEndRe.MatchString(w) }

func isEol(w string) bool {
	if w == "" {
		return true
	}
	return eolRe.MatchString(w) || smileyRe.MatchString(w)
}

// --- generation

func (m *Module) randomMessage(seed string) string {
	var words []string
	for w := range strings.FieldsSeq(seed) {
		words = append(words, sanitizeWord(w)...)
	}
	looper := 0
	for {
		w := m.randomWord(words)
		if w == "" {
			if len(words) > 6 {
				words = append(words, ".") // just doesn't end; we help
			} else {
				words = append(words, "*BLLEUEURRURUHGHG*.")
			}
		} else {
			words = append(words, w)
		}
		looper++
		if looper > 40 {
			words = append(words, "....") // this is getting rediculous
		}
		if isEol(words[len(words)-1]) {
			break
		}
	}
	r := strings.Join(words, " ")
	r = joinPunctRe.ReplaceAllString(r, "$1")
	return capitalize(r)
}

// randomWord picks the next word after words, using the longest chain
// that exists (with a small random chance to drop an order for
// variety), falling back to a uniform random dictionary word.
func (m *Module) randomWord(words []string) string {
	last := words
	if len(last) > m.order {
		last = last[len(last)-m.order:]
	}
	if len(last) == 0 || m.chains[last[len(last)-1]] == nil {
		return m.randomChainWord()
	}
	if nd := m.findMaxOrderChain(last); nd != nil {
		if w := m.weightedPick(nd); w != "" {
			return w
		}
	}
	return m.randomChainWord()
}

// randomChainWord is a uniform pick over all top-level words; empty
// when the dictionary is empty.
func (m *Module) randomChainWord() string {
	if len(m.keys) == 0 {
		return ""
	}
	return m.keys[int(m.rand()*float64(len(m.keys)))%len(m.keys)]
}

// findMaxOrderChain walks the longest existing chain for the last
// words, dropping an order on a miss or on the perl's variety roll
// (p = (1/(children+1))^2 / 4, the +1 being its __count key).
func (m *Module) findMaxOrderChain(last []string) *Node {
	for order := min(m.order, len(last)); order > 0; order-- {
		var nd *Node
		ok := true
		for i, w := range last[len(last)-order:] {
			if i == 0 {
				nd = m.chains[w]
			} else {
				nd = nd.Children[w]
			}
			if nd == nil {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		nkeys := len(nd.Children) + 1
		p := 0.25 / float64(nkeys) / float64(nkeys)
		if m.rand() < p {
			continue // random fail, retry with lower order
		}
		return nd
	}
	return nil
}

// weightedPick draws a child weighted by its count.
func (m *Module) weightedPick(nd *Node) string {
	if len(nd.Children) == 0 {
		return ""
	}
	keys := make([]string, 0, len(nd.Children))
	for w := range nd.Children {
		keys = append(keys, w)
	}
	slices.Sort(keys)
	total := 0
	cums := make([]int, len(keys))
	for i, w := range keys {
		total += nd.Children[w].Count
		cums[i] = total
	}
	r := int(m.rand() * float64(total))
	for i, c := range cums {
		if r <= c {
			return keys[i]
		}
	}
	return ""
}

// --- idle talker

func (m *Module) idleFor(channel string) *idleState {
	st := m.idle[channel]
	if st == nil {
		st = &idleState{}
		m.idle[channel] = st
	}
	return st
}

func (m *Module) isIdleChannel(channel string) bool {
	list := m.ctx.Conf.String("markov_idle_talk_channels")
	if list == "" {
		return false
	}
	return slices.Contains(idleChanSepRe.Split(list, -1), channel)
}

func (m *Module) scheduleIdleTalk(channel string) {
	st := m.idleFor(channel)
	if st.set {
		m.ctx.Sched.Unschedule(st.timer)
		st.set = false
	}
	if !m.ctx.Conf.Bool("markov_idle_talk") || !m.isIdleChannel(channel) {
		return
	}
	timeout := time.Duration(m.ctx.Conf.Int("markov_idle_talk_timeout")) * time.Minute
	st.timer = m.ctx.Sched.Schedule(st.lastAct.Add(timeout), func() { m.idleTalk(channel) })
	st.set = true
}

func (m *Module) idleTalk(channel string) {
	st := m.idleFor(channel)
	st.set = false
	message := m.randomMessage("")
	if int(m.rand()*16) == 0 {
		message = "Lala..." // once every while simply pick a preset
	}
	m.ctx.Privmsg(channel, message)
	st.lastAct = m.now()
	m.scheduleIdleTalk(channel)
}
