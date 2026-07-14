// Command bvsimport bootstraps per-nick markov dictionaries from
// BenV's irssi and weechat #bvs logs (2010-2026). Usage:
//
//	go run ./tools/bvsimport -dsn postgres://... logs/2014/#bvs.log logs/2020/irc.junerules.#bvs ...
//
// Without -dsn it dry-runs: parse everything, print the per-nick line
// counts so obvious spammers can be vetoed with -skip before anything
// is written. Only chat lines count: joins/parts/actions, bot commands
// (!...) and the bots themselves are filtered.
//
// Lines are learned through markov.LearnLine, the exact live-learning
// path, into nick_<nick> dictionaries only; the global dictionary is
// NOT touched (it already carries the same years via the Perl
// migration, importing would double-count). Rows overwrite any
// existing nick dict rows, so run this BEFORE deploying the per-nick
// learning build (or accept losing what it live-learned since boot).
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"go-botje/internal/storage"
	"go-botje/modules/markov"
)

var (
	// irssi: "00:29 < Bram> text" / "00:29 <@hoer> text"
	irssiRe = regexp.MustCompile(`^\d{2}:\d{2} <[@+%~ ]?([^>]+)> (.*)$`)
	// weechat: "2020-01-01 01:05:24\tBenV\ttext"; joins/parts/actions
	// carry -->/<--/--/* in the nick column
	weechatRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\t([^\t]+)\t(.*)$`)
	// the bots the live module refuses to learn from, plus our own
	defaultSkip = "hoer,meretrix,x,the_baby,dromertje,calvin,lippy"
)

// canonical merges nick variants: explicit -alias pairs first, then
// the reconnect-ghost convention (trailing underscores stripped).
// Digit variants (willow1, mussie2) are NOT auto-merged: those can be
// different people, -alias decides.
func canonical(lower string, alias map[string]string) string {
	if to, ok := alias[lower]; ok {
		lower = to
	}
	return strings.TrimRight(lower, "_")
}

// parseLine extracts (nick, text) from one log line of either format;
// ok is false for meta lines (joins, parts, actions, log markers).
func parseLine(line string) (nick, text string, ok bool) {
	if g := irssiRe.FindStringSubmatch(line); g != nil {
		return strings.TrimSpace(g[1]), g[2], true
	}
	if g := weechatRe.FindStringSubmatch(line); g != nil {
		nick = strings.TrimSpace(strings.TrimLeft(g[1], "@+%~"))
		switch nick {
		case "-->", "<--", "--", "*", "":
			return "", "", false
		}
		return nick, g[2], true
	}
	return "", "", false
}

func main() {
	var (
		dsn     = flag.String("dsn", "", "postgres dsn; empty for a dry run")
		skip    = flag.String("skip", "", "extra nicks to skip, comma-separated (bots are always skipped)")
		aliases = flag.String("alias", "", "nick merges, comma-separated variant=canonical (e.g. wi11ow=willow)")
		order   = flag.Int("order", 3, "markov order (must match markov_order)")
	)
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: bvsimport [-dsn ...] [-skip nick,nick] [-alias a=b,...] <logfile>...")
		os.Exit(2)
	}
	skipSet := map[string]bool{}
	for _, n := range strings.Split(defaultSkip+","+*skip, ",") {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			skipSet[n] = true
		}
	}
	alias := map[string]string{}
	for _, pair := range strings.Split(*aliases, ",") {
		if from, to, ok := strings.Cut(strings.ToLower(strings.TrimSpace(pair)), "="); ok {
			alias[from] = to
		}
	}

	dicts := map[string]map[string]*markov.Node{} // lower nick -> chains
	lines := map[string]int{}
	var total, skipped int
	for _, path := range flag.Args() {
		file, err := os.Open(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bvsimport:", err)
			os.Exit(1)
		}
		sc := bufio.NewScanner(file)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			nick, text, ok := parseLine(sc.Text())
			if !ok {
				continue
			}
			lower := canonical(strings.ToLower(nick), alias)
			if lower == "" || skipSet[lower] || strings.HasPrefix(text, "!") || strings.TrimSpace(text) == "" {
				skipped++
				continue
			}
			if dicts[lower] == nil {
				dicts[lower] = make(map[string]*markov.Node)
			}
			markov.LearnLine(dicts[lower], *order, text)
			lines[lower]++
			total++
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "bvsimport: %s: %v\n", path, err)
			os.Exit(1)
		}
		file.Close()
	}

	nicks := make([]string, 0, len(lines))
	for n := range lines {
		nicks = append(nicks, n)
	}
	sort.Slice(nicks, func(i, j int) bool { return lines[nicks[i]] > lines[nicks[j]] })
	fmt.Printf("%d chat lines (%d skipped), %d nicks:\n", total, skipped, len(nicks))
	for _, n := range nicks {
		fmt.Printf("%8d  %s (%d words)\n", lines[n], n, len(dicts[n]))
	}

	if *dsn == "" {
		fmt.Println("dry run, nothing written (pass -dsn to import)")
		return
	}
	store, err := storage.OpenPostgres(context.Background(), *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bvsimport:", err)
		os.Exit(1)
	}
	defer store.Close()
	for nick, chains := range dicts {
		batch := make(map[string]any, len(chains))
		for w, nd := range chains {
			batch[fmt.Sprintf("dictionary_%d_nick_%s:%s", *order, nick, w)] = nd
		}
		if err := store.PutMany("markov", batch); err != nil {
			fmt.Fprintf(os.Stderr, "bvsimport: import %s: %v\n", nick, err)
			os.Exit(1)
		}
		fmt.Printf("imported nick_%s (%d words)\n", nick, len(chains))
	}
}
