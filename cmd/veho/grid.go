package main

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// A grid is a table whose cells hold text or grids of their own. A nested
// grid has no frame: the borders of its cell are its outer borders, and its
// lines reach them (.specs/veho-cli.md §3).
type grid struct {
	rows [][]gridCell
}

type gridCell struct {
	text   string
	colour string // an escape sequence, applied after the layout
	sub    *grid
}

// line is a line of a drawn grid; left and right when a horizontal line
// reaches that edge, where the enclosing grid's vertical line joins it.
type line struct {
	s           string
	left, right bool
}

// drawn is a grid drawn to a width, without frame, with the offsets of its
// vertical lines where they reach its top and its bottom.
type drawn struct {
	lines       []line
	top, bottom []int
}

const reset = "\x1b[0m"

// width is a text's width on a terminal.
// shortcut: counts runes, which suits the API's ASCII; wide (CJK) runes
// would misalign, use a width table (golang.org/x/text/width) if they come.
func width(s string) int { return utf8.RuneCountInString(s) }

// natural is the width a cell needs: its text and a space on each side, or
// its grid's.
func (c gridCell) natural() int {
	if c.sub != nil {
		return c.sub.natural()
	}
	return width(c.text) + 2
}

func (g *grid) columns() int {
	n := 0
	for _, r := range g.rows {
		n = max(n, len(r))
	}
	return n
}

func (g *grid) widths() []int {
	w := make([]int, g.columns())
	for _, r := range g.rows {
		for i, c := range r {
			w[i] = max(w[i], c.natural())
		}
	}
	return w
}

// natural is the grid's width: its columns and the lines between them.
func (g *grid) natural() int {
	w := g.widths()
	n := len(w) - 1
	for _, x := range w {
		n += x
	}
	return n
}

// drawCell draws a cell to w columns.
func drawCell(c gridCell, w int, on bool) drawn {
	if c.sub != nil {
		return c.sub.draw(w, on)
	}
	t := c.text
	if on && c.colour != "" {
		t = c.colour + t + reset
	}
	return drawn{lines: []line{{s: " " + t + strings.Repeat(" ", max(0, w-width(c.text)-1))}}}
}

// lineAt is a cell's line i, below its end a blank line that carries on its
// vertical lines.
func (d drawn) lineAt(i, w int) line {
	if i < len(d.lines) {
		return d.lines[i]
	}
	b := []rune(strings.Repeat(" ", w))
	for _, x := range d.bottom {
		b[x] = '│'
	}
	return line{s: string(b)}
}

// join is the vertical line between two cells' lines: a rule on either side
// meets it.
func join(left, right bool) string {
	switch {
	case left && right:
		return "┼"
	case left:
		return "┤"
	case right:
		return "├"
	}
	return "│"
}

// rule is the horizontal line across columns of widths, crossing their
// lines, with a joint where a vertical line comes from above (up) or below
// (down).
func rule(widths []int, up, down [][]int) string {
	var b strings.Builder
	for c, w := range widths {
		if c > 0 {
			b.WriteRune('┼')
		}
		for x := range w {
			u, d := c < len(up) && slices.Contains(up[c], x), c < len(down) && slices.Contains(down[c], x)
			switch {
			case u && d:
				b.WriteRune('┼')
			case u:
				b.WriteRune('┴')
			case d:
				b.WriteRune('┬')
			default:
				b.WriteRune('─')
			}
		}
	}
	return b.String()
}

// draw draws the grid to total columns, the extra in its last column.
func (g *grid) draw(total int, on bool) drawn {
	widths := g.widths()
	if extra := total - g.natural(); extra > 0 && len(widths) > 0 {
		widths[len(widths)-1] += extra
	}
	starts := make([]int, len(widths)) // where each column begins
	for i := 1; i < len(widths); i++ {
		starts[i] = starts[i-1] + widths[i-1] + 1
	}
	var out drawn
	offsets := func(cells []drawn, top bool) []int {
		var xs []int
		for c, d := range cells {
			if c > 0 {
				xs = append(xs, starts[c]-1)
			}
			v := d.bottom
			if top {
				v = d.top
			}
			for _, x := range v {
				xs = append(xs, starts[c]+x)
			}
		}
		return xs
	}
	var above []drawn
	for r, row := range g.rows {
		cells := make([]drawn, len(widths))
		height := 1
		for c := range widths {
			cell := gridCell{}
			if c < len(row) {
				cell = row[c]
			}
			cells[c] = drawCell(cell, widths[c], on)
			height = max(height, len(cells[c].lines))
		}
		if r == 0 {
			out.top = offsets(cells, true)
		} else {
			up, down := make([][]int, len(widths)), make([][]int, len(widths))
			for c := range widths {
				up[c], down[c] = above[c].bottom, cells[c].top
			}
			out.lines = append(out.lines, line{s: rule(widths, up, down), left: true, right: true})
		}
		for i := range height {
			var b strings.Builder
			var l line
			composed := line{}
			for c := range widths {
				prev := l.right
				l = cells[c].lineAt(i, widths[c])
				if c == 0 {
					composed.left = l.left
				} else {
					b.WriteString(join(prev, l.left))
				}
				b.WriteString(l.s)
			}
			composed.s, composed.right = b.String(), l.right
			out.lines = append(out.lines, composed)
		}
		above = cells
	}
	out.bottom = offsets(above, false)
	return out
}

// render draws the grid in its frame.
func (g *grid) render(on bool) string {
	total := g.natural()
	d := g.draw(total, on)
	edge := func(xs []int, l, joint, r rune) string {
		b := []rune(strings.Repeat("─", total))
		for _, x := range xs {
			b[x] = joint
		}
		return string(l) + string(b) + string(r)
	}
	out := []string{edge(d.top, '┌', '┬', '┐')}
	for _, l := range d.lines {
		out = append(out, join(false, l.left)+l.s+join(l.right, false))
	}
	return strings.Join(append(out, edge(d.bottom, '└', '┴', '┘')), "\n")
}
