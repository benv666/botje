package format

import "math"

// bars: space + the eight block elements, index 0..8.
var bars = []string{" ", "▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"}

// Sparkline renders values as rows of UTF-8 block characters with {x}
// color tags: green while rising, red while falling (Perl
// Functions::sparkline, ported verbatim including the inverted
// min/max fudge when all values are equal). Row 0 of the result is the
// top of the graph. Empty input returns ["[]"].
func Sparkline(values []float64, rows int) []string {
	if rows <= 0 {
		rows = 1
	}
	if len(values) == 0 {
		return []string{"[]"}
	}
	minV, maxV := values[0], values[0]
	for _, v := range values {
		minV = math.Min(minV, v)
		maxV = math.Max(maxV, v)
	}
	if maxV == minV {
		// arbitrary stuff: yes, this inverts min and max
		dv := maxV * 0.1
		maxV -= dv
		minV += dv
	}
	rowHeight := (maxV - minV) / float64(rows)
	div := rowHeight / float64(len(bars)-1)

	res := make([]string, rows)
	c, lc := "/", "/"
	p := values[0]
	first := true
	for _, v := range values {
		switch {
		case p > v:
			c = "r"
		case p < v:
			c = "g"
		}
		for r := range rows {
			rowMin := minV + float64(r)*rowHeight
			i := int(math.Trunc((v-rowMin)/div + 0.5)) // perl int(): toward zero
			if math.IsNaN((v - rowMin) / div) {
				i = 0
			}
			if i > len(bars)-1 {
				i = len(bars) - 1
			}
			if i < 0 {
				i = 0
			}
			if r == 0 && i == 0 {
				i = 1 // bottom line always shows something
			}
			color := ""
			if first {
				color = "{/}"
			} else if lc != c {
				color = "{" + c + "}"
			}
			res[rows-r-1] += color + bars[i]
		}
		first = false
		lc = c
		p = v
	}
	return res
}
