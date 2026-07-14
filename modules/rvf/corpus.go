package rvf

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed corpus/nl.txt corpus/en.txt
var corpusFS embed.FS

// Puzzle is one board: a category and the text to guess.
type Puzzle struct {
	Category string `json:"category"`
	Text     string `json:"text"`
}

// parseCorpus reads "Category: puzzle" lines; blank lines and #
// comments are skipped.
func parseCorpus(data string) ([]Puzzle, error) {
	var out []Puzzle
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cat, text, ok := strings.Cut(line, ":")
		cat, text = strings.TrimSpace(cat), strings.TrimSpace(text)
		if !ok || cat == "" || text == "" {
			return nil, fmt.Errorf("rvf: bad corpus line %q", line)
		}
		out = append(out, Puzzle{Category: cat, Text: text})
	}
	return out, nil
}

// builtinPuzzles returns the embedded corpus for lang ("nl"/"en").
func builtinPuzzles(lang string) ([]Puzzle, error) {
	data, err := corpusFS.ReadFile("corpus/" + lang + ".txt")
	if err != nil {
		return nil, err
	}
	return parseCorpus(string(data))
}
