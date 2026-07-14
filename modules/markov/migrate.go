package markov

import (
	"fmt"
	"strings"
)

// MigrateStats reports what a Perl import contained.
type MigrateStats struct {
	Dictionaries []string
	TopWords     int
	TotalCount   int64
}

// MigrateFromPerl maps the Perl chains (word -> {__count, childword ->
// ...}) onto Node tries, one per dictionary_* key in the dump. The
// caller stores each trie under its dictionary key in namespace
// "markov" (the module splits legacy blobs into per-word rows at Load).
func MigrateFromPerl(dump map[string]any) (map[string]map[string]*Node, MigrateStats, error) {
	out := make(map[string]map[string]*Node)
	var stats MigrateStats
	for key, v := range dump {
		if !strings.HasPrefix(key, "dictionary_") {
			continue
		}
		words, ok := v.(map[string]any)
		if !ok {
			return nil, stats, fmt.Errorf("markov: %s is %T", key, v)
		}
		chains := make(map[string]*Node, len(words))
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

func nodeFromPerl(v any) (*Node, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("node is %T", v)
	}
	nd := &Node{}
	for k, cv := range m {
		if k == "__count" {
			if c, ok := migInt(cv); ok {
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
			nd.Children = make(map[string]*Node)
		}
		nd.Children[k] = child
	}
	return nd, nil
}

// migInt handles json numbers and the strings Perl sometimes stores
// numbers as.
func migInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case string:
		var i int
		if _, err := fmt.Sscanf(n, "%d", &i); err == nil {
			return i, true
		}
	}
	return 0, false
}
