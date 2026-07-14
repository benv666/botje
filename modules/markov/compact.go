package markov

import (
	"slices"
	"sort"
)

// The compact in-RAM trie (2026-07-14, backlog 2e): the Node/JSON shape
// stayed the storage and interchange format, but keeping it in RAM cost
// ~100x its JSON size (2.2GB live): every word string stored again at
// every node level, plus map[string]*Node overhead per node. In RAM the
// chains are cnodes instead: words interned once into a global table,
// children a slice sorted by word id. Conversion happens at the
// boundaries (load, saver flush, derive).

// interner maps words to dense ids. One table for all dictionaries:
// the same words fill the global, reverse and nick dicts, so each
// string lands in memory exactly once. Dispatcher-only, like the
// chains themselves.
type interner struct {
	ids  map[string]uint32
	strs []string
}

// wordTab is the process-wide table. It only ever grows, but the word
// set is bounded (~50k after 19 years of hoer). Named to dodge the
// generation functions' local "words" slices.
var wordTab = &interner{ids: make(map[string]uint32)}

// interned sentinel ids, for the hot comparisons.
var (
	tokStartID = wordTab.id(tokStart)
	tokEndID   = wordTab.id(tokEnd)
)

// id returns the word's id, interning it first if new.
func (in *interner) id(w string) uint32 {
	if id, ok := in.ids[w]; ok {
		return id
	}
	id := uint32(len(in.strs))
	in.ids[w] = id
	in.strs = append(in.strs, w)
	return id
}

// lookup is the read-only id: ok=false means the word was never seen,
// so it cannot be in any chain either.
func (in *interner) lookup(w string) (uint32, bool) {
	id, ok := in.ids[w]
	return id, ok
}

func (in *interner) str(id uint32) string { return in.strs[id] }

// kid is one child edge: 16 bytes instead of a map bucket.
type kid struct {
	word uint32
	node *cnode
}

// cnode is the compact chain node.
type cnode struct {
	count int32
	kids  []kid // sorted by word id
}

// child returns the child for a word id, or nil.
func (nd *cnode) child(id uint32) *cnode {
	i, ok := slices.BinarySearchFunc(nd.kids, id, func(k kid, id uint32) int {
		switch {
		case k.word < id:
			return -1
		case k.word > id:
			return 1
		}
		return 0
	})
	if !ok {
		return nil
	}
	return nd.kids[i].node
}

// ensureChild returns the child for a word id, inserting an empty one
// in id order if absent.
func (nd *cnode) ensureChild(id uint32) *cnode {
	i, ok := slices.BinarySearchFunc(nd.kids, id, func(k kid, id uint32) int {
		switch {
		case k.word < id:
			return -1
		case k.word > id:
			return 1
		}
		return 0
	})
	if ok {
		return nd.kids[i].node
	}
	ch := &cnode{}
	nd.kids = slices.Insert(nd.kids, i, kid{word: id, node: ch})
	return ch
}

// sortedKids returns the children as (word, node) pairs in ALPHABETICAL
// word order: the weighted pick iterated map keys sorted, and the
// pick-for-a-given-rand must stay identical (id order is arrival order,
// not alphabetical).
func (nd *cnode) sortedKids() []kid {
	out := slices.Clone(nd.kids)
	sort.Slice(out, func(i, j int) bool {
		return wordTab.str(out[i].word) < wordTab.str(out[j].word)
	})
	return out
}

// toCompact converts a storage-shaped subtree to the RAM shape.
func toCompact(n *Node) *cnode {
	nd := &cnode{count: int32(n.Count)}
	if len(n.Children) == 0 {
		return nd
	}
	nd.kids = make([]kid, 0, len(n.Children))
	for w, ch := range n.Children {
		nd.kids = append(nd.kids, kid{word: wordTab.id(w), node: toCompact(ch)})
	}
	slices.SortFunc(nd.kids, func(a, b kid) int {
		switch {
		case a.word < b.word:
			return -1
		case a.word > b.word:
			return 1
		}
		return 0
	})
	return nd
}

// fromCompact converts back to the storage shape (saver flush, derive
// persistence).
func fromCompact(nd *cnode) *Node {
	n := &Node{Count: int(nd.count)}
	if len(nd.kids) == 0 {
		return n
	}
	n.Children = make(map[string]*Node, len(nd.kids))
	for _, k := range nd.kids {
		n.Children[wordTab.str(k.word)] = fromCompact(k.node)
	}
	return n
}
