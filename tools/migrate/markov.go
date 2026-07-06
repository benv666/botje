package main

import (
	"fmt"
	"strings"

	"go-botje/modules/markov"
)

// markovStats is the verification summary for a dictionary migration.
type markovStats struct {
	Dictionaries []string
	TopWords     int
	TotalCount   int64
}

// markovFromPerl maps the Perl chains (word -> {__count, childword ->
// ...}) onto markov.Node tries, one per dictionary_* key in the dump.
func markovFromPerl(dump map[string]any) (map[string]map[string]*markov.Node, markovStats, error) {
	out := make(map[string]map[string]*markov.Node)
	var stats markovStats
	for key, v := range dump {
		if !strings.HasPrefix(key, "dictionary_") {
			continue
		}
		words, ok := v.(map[string]any)
		if !ok {
			return nil, stats, fmt.Errorf("markov: %s is %T", key, v)
		}
		chains := make(map[string]*markov.Node, len(words))
		for word, wv := range words {
			if strings.HasPrefix(word, "__") {
				continue // stray bookkeeping keys at the top level
			}
			nd, err := nodeFromPerl(wv)
			if err != nil {
				return nil, stats, fmt.Errorf("markov: %s/%q: %w", key, word, err)
			}
			chains[word] = nd
			stats.TotalCount += int64(nd.Count)
		}
		out[key] = chains
		stats.Dictionaries = append(stats.Dictionaries, key)
		stats.TopWords += len(chains)
	}
	if len(out) == 0 {
		return nil, stats, fmt.Errorf("markov: no dictionary_* keys in dump")
	}
	return out, stats, nil
}

func nodeFromPerl(v any) (*markov.Node, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("node is %T", v)
	}
	nd := &markov.Node{}
	for k, cv := range m {
		if k == "__count" {
			if c, ok := toInt(cv); ok {
				nd.Count = c
			}
			continue
		}
		if strings.HasPrefix(k, "__") {
			continue
		}
		child, err := nodeFromPerl(cv)
		if err != nil {
			return nil, err
		}
		if nd.Children == nil {
			nd.Children = make(map[string]*markov.Node)
		}
		nd.Children[k] = child
	}
	return nd, nil
}
