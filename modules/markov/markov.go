// Package markov learns channel chatter into word chains and talks
// back: !talk [seed], !talklike <nick> [seed] (per-nick dictionaries),
// any unknown !command (the "!je moeder" feature, which also means no
// Levenshtein suggestions while markov is loaded, same as the Perl),
// "talk" in query, and an optional idle talker. Ported from
// IRC_Markov.pm, then extended (2026-07-14): sentence sentinels wrap
// every learned line, and a reverse-trained copy of the global
// dictionary powers MegaHAL-style middle-out generation: a single-word
// !talk seed extends backwards to the sentence start first, then
// forward to the end, so the bot talks ABOUT the word instead of only
// FROM it.
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
	"go-botje/internal/format"
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

// Sentence sentinels (2026-07-14, BenV): every learned line is wrapped
// as [START] w0..wn [END], so generation knows where sentences begin
// (unseeded talk used to open at a uniformly random word) and end
// (stopword-ending lines used to leave dead-end chains, and a dead end
// meant a jump to a random word mid-sentence). Control characters:
// sanitizeWord strips those from real input, so no IRC message can
// forge them. Data learned before the sentinels keeps working, it just
// lacks the markers.
const (
	tokStart = "\x02"
	tokEnd   = "\x03"
)

func isSentinel(w string) bool { return w == tokStart || w == tokEnd }

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

// dict is one dictionary: the chains plus a sorted top-word cache for
// uniform random picks. The default dict learns all channel chatter;
// every talker also gets a nick_<nick> dict (!talklike, 2026-07-14).
type dict struct {
	name   string            // "default", "nick_benv", ...
	chains map[uint32]*cnode // top word id -> compact subtree
	keys   []string
}

func newDict(name string) *dict {
	return &dict{name: name, chains: make(map[uint32]*cnode)}
}

// rebuildKeys refreshes the sorted top-word cache (the perl cached
// hash order; sorted is deterministic). Sentinels are excluded: a
// uniform random pick must never open with one.
func (d *dict) rebuildKeys() {
	d.keys = make([]string, 0, len(d.chains))
	for id := range d.chains {
		if id != tokStartID && id != tokEndID {
			d.keys = append(d.keys, wordTab.str(id))
		}
	}
	slices.Sort(d.keys)
}

// Module implements module.Module.
type Module struct {
	// Now and Rand are injectable for tests.
	Now  func() time.Time
	Rand func() float64

	ctx        *module.Context
	order      int
	dictionary string
	main       *dict
	rev        *dict                 // main trained on reversed lines (middle-out)
	nicks      map[string]*dict      // dict name -> dict
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

	if err := m.loadDictionaries(); err != nil {
		return err
	}

	ctx.Cmd.Register(m.Name(), "talk", m.cbTalk)
	ctx.Cmd.Register(m.Name(), "talklike", m.cbTalkLike)
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

func (m *Module) storeKey(d *dict) string {
	return fmt.Sprintf("dictionary_%d_%s", m.order, d.name)
}

// nickDict returns (creating if needed) the per-nick dictionary for a
// talker. Nick dicts are named nick_<lowercased nick>: IRC nicks are
// case-insensitive and never contain a colon, so the storage key
// dictionary_<order>_nick_<nick>:<word> splits back unambiguously.
func (m *Module) nickDict(nick string) *dict {
	name := "nick_" + strings.ToLower(nick)
	d := m.nicks[name]
	if d == nil {
		d = newDict(name)
		m.nicks[name] = d
	}
	return d
}

// loadDictionaries bulk-loads the per-word rows of every dictionary
// (the default one plus the nick_* dicts), migrating a legacy
// whole-dictionary blob into rows once (the blob wins over any rows
// from a previously interrupted migration; the delete only happens
// after all rows landed, so a crash mid-migration is retried).
func (m *Module) loadDictionaries() error {
	m.main = newDict(m.dictionary)
	m.rev = newDict("reverse_" + m.dictionary)
	m.nicks = make(map[string]*dict)
	all, err := m.ctx.Store.GetAll(m.Name())
	if err != nil {
		return fmt.Errorf("markov: load dictionary: %w", err)
	}
	prefix := fmt.Sprintf("dictionary_%d_", m.order)
	for name, raw := range all {
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}
		// dict names never contain a colon; words can (":" is a valid
		// punctuation token), so cut at the first one
		dictName, w, ok := strings.Cut(rest, ":")
		if !ok {
			continue // the legacy blob, handled below
		}
		var d *dict
		switch {
		case dictName == m.dictionary:
			d = m.main
		case dictName == m.rev.name:
			d = m.rev
		case strings.HasPrefix(dictName, "nick_"):
			if m.nicks[dictName] == nil {
				m.nicks[dictName] = newDict(dictName)
			}
			d = m.nicks[dictName]
		default:
			continue // some other dictionary, not ours to load
		}
		nd := &Node{}
		if err := json.Unmarshal(raw, nd); err != nil {
			return fmt.Errorf("markov: load word %q: %w", w, err)
		}
		d.chains[wordTab.id(w)] = toCompact(nd)
	}
	if err := m.migrateLegacyBlob(all); err != nil {
		return err
	}
	if err := m.deriveReverse(); err != nil {
		return err
	}
	m.main.rebuildKeys()
	m.rev.rebuildKeys()
	for _, d := range m.nicks {
		d.rebuildKeys()
	}
	return nil
}

// deriveReverse builds the reverse dictionary from the forward one,
// once: the trie IS the multiset of learned windows (a node's count
// minus its children's sum = windows that ended at that depth), so
// inserting every window reversed reproduces exactly what learning
// the reversed lines would have stored. The global dictionary predates
// the logs we have, so this is the only way to bootstrap it.
func (m *Module) deriveReverse() error {
	if len(m.rev.chains) > 0 || len(m.main.chains) == 0 {
		return nil
	}
	path := make([]uint32, 0, m.order+1)
	var walk func(nd *cnode)
	walk = func(nd *cnode) {
		var childSum int32
		for _, k := range nd.kids {
			childSum += k.node.count
			path = append(path, k.word)
			walk(k.node)
			path = path[:len(path)-1]
		}
		if excess := nd.count - childSum; excess > 0 {
			insertReversed(m.rev.chains, path, excess)
		}
	}
	for id, nd := range m.main.chains {
		path = append(path[:0], id)
		walk(nd)
	}
	batch := make(map[string]any, len(m.rev.chains))
	for id, nd := range m.rev.chains {
		batch[m.storeKey(m.rev)+":"+wordTab.str(id)] = fromCompact(nd)
	}
	if err := m.ctx.Store.PutMany(m.Name(), batch); err != nil {
		return fmt.Errorf("markov: persist derived reverse dictionary: %w", err)
	}
	slog.Info("markov: derived reverse dictionary from forward chains", "words", len(m.rev.chains))
	return nil
}

// insertReversed adds one window, reversed, with the given count.
func insertReversed(chains map[uint32]*cnode, path []uint32, count int32) {
	var nd *cnode
	for i := len(path) - 1; i >= 0; i-- {
		id := path[i]
		if i == len(path)-1 {
			if chains[id] == nil {
				chains[id] = &cnode{}
			}
			nd = chains[id]
		} else {
			nd = nd.ensureChild(id)
		}
		nd.count += count
	}
}

// addReversed learns one line backwards into the reverse dictionary
// (global only: !talklike stays forward, the reverse of every nick
// dict would double another gigabyte).
func (m *Module) addReversed(msg string) {
	tokens := lineTokens(msg)
	slices.Reverse(tokens)
	m.learnTokens(m.rev, tokens)
}

func (m *Module) migrateLegacyBlob(all map[string]json.RawMessage) error {
	raw, isLegacy := all[m.storeKey(m.main)]
	if !isLegacy {
		return nil
	}
	legacy := make(map[string]*Node)
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return fmt.Errorf("markov: parse legacy dictionary: %w", err)
	}
	batch := make(map[string]any, len(legacy))
	for w, nd := range legacy {
		m.main.chains[wordTab.id(w)] = toCompact(nd)
		batch[m.storeKey(m.main)+":"+w] = nd
	}
	if err := m.ctx.Store.PutMany(m.Name(), batch); err != nil {
		return fmt.Errorf("markov: migrate dictionary to rows: %w", err)
	}
	// keep the blob under a backup name instead of deleting it: a
	// rolled-back pre-rows build would otherwise boot with an empty
	// dictionary (it can be renamed back by hand)
	if err := m.ctx.Store.Put(m.Name(), m.storeKey(m.main)+"_blob_backup", json.RawMessage(raw)); err != nil {
		return fmt.Errorf("markov: back up legacy dictionary: %w", err)
	}
	if err := m.ctx.Store.Delete(m.Name(), m.storeKey(m.main)); err != nil {
		return fmt.Errorf("markov: drop legacy dictionary: %w", err)
	}
	slog.Info("markov: migrated dictionary blob to per-word rows", "words", len(legacy))
	return nil
}

// markDirty queues the word's subtree for the next saver flush. The
// node pointer is stable (words are never deleted), so the flush
// serializes whatever the counts are by then.
func (m *Module) markDirty(d *dict, id uint32) {
	nd := d.chains[id]
	m.ctx.Saver.Mark(m.Name(), m.storeKey(d)+":"+wordTab.str(id), func() any { return fromCompact(nd) })
}

func (m *Module) cbTalk(d *cmd.Data) bool {
	if d.Event.SenderMe {
		return false
	}
	m.ctx.Privmsg(d.Event.Channel, m.talkMessage(d.Data))
	return true
}

// cbTalkLike is "!talklike <nick> [seed]": talk from one talker's own
// dictionary (bootstrapped from a decade of #bvs logs, then live).
func (m *Module) cbTalkLike(d *cmd.Data) bool {
	if d.Event.SenderMe {
		return false
	}
	nick, seed, _ := strings.Cut(strings.TrimSpace(d.Data), " ")
	if nick == "" {
		m.ctx.Privmsg(d.Event.Channel, "Gebruik: !talklike <nick> [seed]")
		return true
	}
	nd := m.nicks["nick_"+strings.ToLower(nick)]
	if nd == nil || len(nd.chains) == 0 {
		m.ctx.Privmsg(d.Event.Channel, fmt.Sprintf("Van %s heb ik nog niks geleerd.", nick))
		return true
	}
	m.ctx.Privmsg(d.Event.Channel, m.randomMessage(nd, seed))
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
			m.ctx.Privmsg(ev.Channel, m.talkMessage(g[1]))
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
	m.addLine(m.main, ev.Msg)
	m.addReversed(ev.Msg)
	if ev.Sender.Nick != "" {
		m.addLine(m.nickDict(ev.Sender.Nick), ev.Msg)
	}
	return bus.None, nil
}

func isBot(nick string) bool {
	return nick == "X" || nick == "x" || botNickRe.MatchString(nick)
}

// --- learning

// lineTokens sanitizes one chat line into the learned token sequence:
// [START] w0..wn [.]? [END]. The closing dot keeps the perl rule (dot
// unless the line already ends or ends on a stopword); END lands on
// every line regardless, which is what makes stopword endings
// terminate cleanly instead of dead-ending.
func lineTokens(msg string) []string {
	msg = format.Strip(msg) // mIRC/ANSI codes: no color junk in the dict
	words := []string{tokStart}
	for w := range strings.FieldsSeq(msg) {
		words = append(words, sanitizeWord(w)...)
	}
	if len(words) == 1 {
		return nil
	}
	last := words[len(words)-1]
	if !isEol(last) && !isBadEnd(last) {
		words = append(words, ".")
	}
	return append(words, tokEnd)
}

// LearnLine folds one chat line into storage-shaped chains: sanitized
// words wrapped in sentence sentinels, windows of order+1, per-level
// counts. Returns the top-level words touched. Exported so the
// bootstrap tool (tools/bvsimport) learns EXACTLY like the live module;
// the module itself learns into the compact representation through the
// same windows().
func LearnLine(chains map[string]*Node, order int, msg string) (touched []string) {
	windows(order, lineTokens(msg), func(win []string) {
		touched = append(touched, addWords(chains, order, win))
	})
	return touched
}

// windows slides the learn windows over a token sequence, handing each
// (up to) order+1-gram to insert. The slice is reused between calls:
// inserters must not retain it.
func windows(order int, tokens []string, insert func(window []string)) {
	if len(tokens) == 0 {
		return
	}
	var prev []string
	for len(prev) < order && len(tokens) > 0 {
		prev = append(prev, tokens[0])
		tokens = tokens[1:]
	}
	if len(tokens) == 0 {
		insert(prev)
		return
	}
	for len(tokens) > 0 {
		prev = append(prev, tokens[0])
		tokens = tokens[1:]
		insert(prev)
		prev = prev[1:]
	}
}

// addWords bumps the chain counts along one window of order+1 words
// and returns the window's top word.
func addWords(chains map[string]*Node, order int, words []string) string {
	var nd *Node
	for i := 0; i <= order && i < len(words); i++ {
		w := words[i]
		if i == 0 {
			if chains[w] == nil {
				chains[w] = &Node{}
			}
			nd = chains[w]
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
	return words[0]
}

// addLine learns one line into a dict, marking touched words dirty and
// keeping the top-word cache current.
func (m *Module) addLine(d *dict, msg string) {
	m.learnTokens(d, lineTokens(msg))
}

// learnTokens is the compact-side twin of LearnLine: same windows, the
// counts land in cnodes.
func (m *Module) learnTokens(d *dict, tokens []string) {
	before := len(d.chains)
	windows(m.order, tokens, func(win []string) {
		id := wordTab.id(win[0])
		nd := d.chains[id]
		if nd == nil {
			nd = &cnode{}
			d.chains[id] = nd
		}
		nd.count++
		for _, w := range win[1:] {
			nd = nd.ensureChild(wordTab.id(w))
			nd.count++
		}
		m.markDirty(d, id)
	})
	if len(d.chains) != before {
		d.rebuildKeys()
	}
}

// sanitizeWord normalizes one token: control characters stripped (mIRC
// color/bold codes, and nobody forges a sentence sentinel), lowercased,
// known nicks capitalized, trailing punctuation split off, contractions
// kept whole, non-words dropped.
func sanitizeWord(w string) []string {
	w = strings.Map(func(r rune) rune {
		if r < 0x20 {
			return -1
		}
		return r
	}, w)
	w = strings.TrimSpace(w)
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
	if len(r) == 0 {
		return w
	}
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

// talkMessage is the seed-aware entry point behind !talk, query talk
// and the unknown-!command default: a single-word seed known to the
// reverse dictionary gets the middle-out treatment (predict backwards
// to the sentence start, then forward to the end); everything else is
// the classic forward walk.
func (m *Module) talkMessage(seed string) string {
	var words []string
	for w := range strings.FieldsSeq(seed) {
		words = append(words, sanitizeWord(w)...)
	}
	if len(words) == 1 && m.rev != nil {
		if id, ok := wordTab.lookup(words[0]); ok && m.rev.chains[id] != nil {
			return m.middleOut(words[0])
		}
	}
	return m.randomMessageFrom(m.main, words)
}

func (m *Module) randomMessage(d *dict, seed string) string {
	var words []string
	for w := range strings.FieldsSeq(seed) {
		words = append(words, sanitizeWord(w)...)
	}
	return m.randomMessageFrom(d, words)
}

// randomMessageFrom walks forward from the given context until the
// sentence ends. Unseeded walks open at the START sentinel when the
// dictionary has one, so sentences begin like real sentences instead
// of at a uniformly random word.
func (m *Module) randomMessageFrom(d *dict, words []string) string {
	if len(words) == 0 && d.chains[tokStartID] != nil {
		words = []string{tokStart}
	}
	looper := 0
	for {
		w := m.randomWord(d, words)
		switch {
		case w == tokEnd:
			return render(words) // a learned sentence end
		case w == "" || w == tokStart:
			if len(words) > 6 {
				words = append(words, ".") // just doesn't end; we help
			} else {
				words = append(words, "*BLLEUEURRURUHGHG*.")
			}
		default:
			words = append(words, w)
		}
		looper++
		if looper > 40 {
			words = append(words, "....") // this is getting rediculous
		}
		if isEol(words[len(words)-1]) {
			return render(words)
		}
	}
}

// middleOut is the MegaHAL-style keyword walk: extend backwards from
// the seed through the reverse dictionary until the sentence start,
// then forward through the main one until the end.
func (m *Module) middleOut(seed string) string {
	back := []string{seed}
	for range 15 {
		w := m.pickNext(m.rev, back)
		if w == "" || w == tokEnd {
			break
		}
		back = append(back, w)
		if w == tokStart {
			break
		}
	}
	// reverse into sentence order; the START marker (when the walk got
	// that far) lands in front and sharpens the forward context
	words := make([]string, 0, len(back))
	for i := len(back) - 1; i >= 0; i-- {
		words = append(words, back[i])
	}
	return m.randomMessageFrom(m.main, words)
}

// pickNext extends a context by one word, or "" on a dead end. Unlike
// randomWord it never jumps to a random word: the backward walk must
// stop at dead ends, not teleport.
func (m *Module) pickNext(d *dict, words []string) string {
	last := words
	if len(last) > m.order {
		last = last[len(last)-m.order:]
	}
	if nd := m.findMaxOrderChain(d, last); nd != nil {
		return m.weightedPick(nd)
	}
	return ""
}

// render joins generated tokens into the outgoing line, dropping the
// sentinels (they are mIRC control codes and must never hit the wire).
func render(words []string) string {
	kept := words[:0:0]
	for _, w := range words {
		if !isSentinel(w) {
			kept = append(kept, w)
		}
	}
	r := strings.Join(kept, " ")
	r = joinPunctRe.ReplaceAllString(r, "$1")
	return capitalize(r)
}

// randomWord picks the next word after words, using the longest chain
// that exists (with a small random chance to drop an order for
// variety), falling back to a uniform random dictionary word.
func (m *Module) randomWord(d *dict, words []string) string {
	last := words
	if len(last) > m.order {
		last = last[len(last)-m.order:]
	}
	if len(last) == 0 {
		return m.randomChainWord(d)
	}
	if id, ok := wordTab.lookup(last[len(last)-1]); !ok || d.chains[id] == nil {
		return m.randomChainWord(d)
	}
	if nd := m.findMaxOrderChain(d, last); nd != nil {
		if w := m.weightedPick(nd); w != "" {
			return w
		}
	}
	return m.randomChainWord(d)
}

// randomChainWord is a uniform pick over all top-level words; empty
// when the dictionary is empty.
func (m *Module) randomChainWord(d *dict) string {
	if len(d.keys) == 0 {
		return ""
	}
	return d.keys[int(m.rand()*float64(len(d.keys)))%len(d.keys)]
}

// findMaxOrderChain walks the longest existing chain for the last
// words, dropping an order on a miss or on the perl's variety roll
// (p = (1/(children+1))^2 / 4, the +1 being its __count key).
func (m *Module) findMaxOrderChain(d *dict, last []string) *cnode {
	for order := min(m.order, len(last)); order > 0; order-- {
		var nd *cnode
		ok := true
		for i, w := range last[len(last)-order:] {
			id, known := wordTab.lookup(w)
			if !known {
				ok = false
				break
			}
			if i == 0 {
				nd = d.chains[id]
			} else {
				nd = nd.child(id)
			}
			if nd == nil {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		nkeys := len(nd.kids) + 1
		p := 0.25 / float64(nkeys) / float64(nkeys)
		if m.rand() < p {
			continue // random fail, retry with lower order
		}
		return nd
	}
	return nil
}

// weightedPick draws a child weighted by its count. Alphabetical word
// order, like the map-keys sort it replaces: the rand-to-child mapping
// must not depend on the representation.
func (m *Module) weightedPick(nd *cnode) string {
	if len(nd.kids) == 0 {
		return ""
	}
	kids := nd.sortedKids()
	total := 0
	cums := make([]int, len(kids))
	for i, k := range kids {
		total += int(k.node.count)
		cums[i] = total
	}
	r := int(m.rand() * float64(total))
	for i, c := range cums {
		if r <= c {
			return wordTab.str(kids[i].word)
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
	message := m.talkMessage("")
	if int(m.rand()*16) == 0 {
		message = "Lala..." // once every while simply pick a preset
	}
	m.ctx.Privmsg(channel, message)
	st.lastAct = m.now()
	m.scheduleIdleTalk(channel)
}
