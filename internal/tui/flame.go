package tui

import (
	"fmt"
	"hash/fnv"
	"math"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/adrijh/kenbun/internal/store"
)

// Icicle layout: the zoomed-in node spans the full width on the first row,
// children below it, each as wide as its share of the parent.

// Cells use Kanagawa's autumn tones with dark text, native frames a muted ink
// with light text; the selection is fujiWhite, highlights springGreen
// (contrast_test.go keeps every pairing readable). Hue says what a frame is.
var (
	warmHex = []string{"#ffa066", "#dca561", "#e6c384", "#e46876", "#d27e99"} // symbolized: surimiOrange, autumnYellow, carpYellow, waveRed, sakuraPink
	coolHex = []string{"#54546d", "#4a4a60"}                                  // [native]: sumiInk4 shades, recede
	warmFg  = inkHex
	coolFg  = fujiHex

	warm, cool = palette(warmHex, warmFg), palette(coolHex, coolFg)
	hit        = cellStyle(greenHex, inkHex)
	picked     = cellStyle(fujiHex, inkHex).Bold(true)
)

func cellStyle(bg, fg string) lipgloss.Style {
	return lipgloss.NewStyle().Background(lipgloss.Color(bg)).Foreground(lipgloss.Color(fg))
}

func palette(bgs []string, fg string) []lipgloss.Style {
	out := make([]lipgloss.Style, len(bgs))
	for i, bg := range bgs {
		out[i] = cellStyle(bg, fg)
	}
	return out
}

// colorOf is stable per name, so a function keeps its color across refreshes.
func colorOf(name string) lipgloss.Style {
	p := warm
	if strings.HasPrefix(name, "[") {
		p = cool
	}
	h := fnv.New32a()
	h.Write([]byte(name))
	return p[h.Sum32()%uint32(len(p))]
}

// resolve maps frame names (from the root) to nodes, stopping at the first
// name that no longer exists. The result starts with root.
func resolve(root *store.Node, names []string) []*store.Node {
	chain := []*store.Node{root}
	for _, name := range names {
		i := slices.IndexFunc(chain[len(chain)-1].Children, func(c *store.Node) bool { return c.Name == name })
		if i < 0 {
			break
		}
		chain = append(chain, chain[len(chain)-1].Children[i])
	}
	return chain
}

// highlighted reports whether a frame matches the highlight: exactly the
// function picked in the functions table, or containing the / search text.
func (m model) highlighted(name string) bool {
	switch {
	case m.search == "":
		return false
	case m.exact:
		return name == m.search
	default:
		return strings.Contains(strings.ToLower(name), strings.ToLower(m.search))
	}
}

// selectFirst selects the first node (heaviest-first DFS) whose name matches,
// and zooms out if it lies outside the current zoom.
func (m *model) selectFirst(match func(name string) bool) {
	if m.tree == nil {
		return
	}
	var path []string
	var walk func(n *store.Node) bool
	walk = func(n *store.Node) bool {
		for _, c := range n.Children {
			path = append(path, c.Name)
			if match(c.Name) || walk(c) {
				return true
			}
			path = path[:len(path)-1]
		}
		return false
	}
	if !walk(m.tree) {
		return
	}
	m.sel = path
	if len(m.focus) > len(path) || !slices.Equal(m.focus, path[:len(m.focus)]) {
		m.focus, m.zooms = nil, nil
	}
}

// matchTotal is the weight under matching nodes, counting nested matches once.
func matchTotal(n *store.Node, match func(string) bool) int64 {
	if match(n.Name) {
		return n.Total
	}
	var sum int64
	for _, c := range n.Children {
		sum += matchTotal(c, match)
	}
	return sum
}

func (m model) flameKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.tree == nil {
		if msg.String() == "esc" {
			m.screen = screenTop
			return m.reload()
		}
		return m, nil
	}
	// Normalize: drop names that vanished in the last refresh, keep sel inside focus.
	focus := resolve(m.tree, m.focus)
	m.focus = names(focus)
	chain := resolve(m.tree, m.sel)
	if len(chain) < len(focus) || chain[len(focus)-1] != focus[len(focus)-1] {
		chain = focus
	}
	m.sel = names(chain)
	cur := chain[len(chain)-1]

	switch msg.String() {
	case "up", "k":
		if len(m.sel) > len(m.focus) {
			m.sel = m.sel[:len(m.sel)-1]
		}
	case "down", "j":
		if len(cur.Children) > 0 {
			m.sel = append(slices.Clip(m.sel), cur.Children[0].Name)
		}
	case "left", "h", "right", "l":
		if len(m.sel) == len(m.focus) {
			break // the zoomed node has no visible siblings
		}
		sibs := chain[len(chain)-2].Children
		i := slices.Index(sibs, cur)
		if s := msg.String(); s == "left" || s == "h" {
			i = max(0, i-1)
		} else {
			i = min(len(sibs)-1, i+1)
		}
		m.sel = append(slices.Clip(m.sel[:len(m.sel)-1]), sibs[i].Name)
	case "enter":
		if !slices.Equal(m.sel, m.focus) {
			m.zooms = append(m.zooms, m.focus)
			m.focus = slices.Clone(m.sel)
		}
	case "/":
		m.searching, m.search, m.exact = true, "", false
	case "esc", "backspace": // undo the last zoom; with none left, back to the table
		if n := len(m.zooms); n > 0 {
			m.focus, m.zooms = m.zooms[n-1], m.zooms[:n-1]
			break
		}
		m.screen = screenTop
		return m.reload()
	}
	return m, nil
}

func names(chain []*store.Node) []string {
	out := make([]string, 0, len(chain)-1)
	for _, n := range chain[1:] {
		out = append(out, n.Name)
	}
	return out
}

// label shortens a frame name for a cell: Go's import path prefix
// (github.com/org/repo/pkg.Func) says little in a few columns. The status line
// shows the full name.
func label(name string) string {
	if strings.HasPrefix(name, "[") {
		return name
	}
	if i := strings.LastIndex(name, "/"); i >= 0 && i < len(name)-1 {
		return name[i+1:]
	}
	return name
}

type cell struct {
	n    *store.Node
	x, w int
}

func (m model) viewFlame(b *strings.Builder, rows int) {
	if m.tree == nil || m.tree.Total == 0 {
		b.WriteString("no samples in window\n")
		return
	}
	width := m.w
	if width == 0 {
		width = 100
	}
	focus := resolve(m.tree, m.focus)
	sel := resolve(m.tree, m.sel)
	if len(sel) < len(focus) || sel[len(focus)-1] != focus[len(focus)-1] {
		sel = focus
	}
	selNode := sel[len(sel)-1]

	// Status line first, so the graph gets the rest.
	all := m.tree.Total
	status := accS.Bold(true).Render("◆ "+m.clip(selNode.Name, 60)) +
		dimS.Render("  total ") + fmt.Sprintf("%.1f%%", pct(selNode.Total, all)) +
		dimS.Render("  self ") + fmt.Sprintf("%.1f%%", pct(selNode.Self, all))
	if len(focus) > 1 {
		status += dimS.Render("  zoom ") + redS.Render(focus[len(focus)-1].Name)
	}
	if m.search != "" || m.searching {
		cursor := ""
		if m.searching {
			cursor = redS.Render("▌")
		}
		chip := dimS.Render("  search ") + hit.Render(" /"+m.search+" ")
		if m.exact {
			chip = dimS.Render("  highlight ") + hit.Render(" "+m.search+" ")
		}
		status += chip + cursor +
			dimS.Render(fmt.Sprintf("  %.1f%% highlighted", pct(matchTotal(focus[len(focus)-1], m.highlighted), all)))
	}
	b.WriteString(status + "\n")
	rows--

	// Scroll so the selection stays visible.
	offset := max(0, len(sel)-len(focus)-rows+1)
	grid := make([][]cell, rows)
	var place func(n *store.Node, x, w float64, depth int)
	place = func(n *store.Node, x, w float64, depth int) {
		x0, x1 := int(math.Round(x)), int(math.Round(x+w))
		d := depth - offset
		if x1-x0 < 1 || d >= rows {
			return
		}
		if d >= 0 {
			grid[d] = append(grid[d], cell{n, x0, x1 - x0})
		}
		for _, c := range n.Children {
			cw := w * float64(c.Total) / float64(n.Total)
			place(c, x, cw, depth+1)
			x += cw
		}
	}
	place(focus[len(focus)-1], 0, float64(width), 0)

	for _, row := range grid {
		col := 0
		for _, c := range row {
			// Rounding can make a cell start a column inside its left neighbour
			// or end past the screen edge: trim it.
			if c.x < col {
				c.w -= col - c.x
				c.x = col
			}
			c.w = min(c.w, width-c.x)
			if c.w <= 0 {
				continue
			}
			b.WriteString(strings.Repeat(" ", c.x-col))
			st := colorOf(c.n.Name)
			switch {
			case c.n == selNode:
				st = picked
			case m.highlighted(c.n.Name):
				st = hit
			}
			switch {
			case c.w == 1:
				b.WriteString(st.Render(" "))
			case c.w <= 4: // too narrow for a useful label: a plain block reads better
				b.WriteString(st.Render(strings.Repeat(" ", c.w-1)) + " ")
			default: // one blank column separates neighbours
				b.WriteString(st.Render(fit(label(c.n.Name), c.w-1)) + " ")
			}
			col = c.x + c.w
		}
		b.WriteString("\n")
	}
}

// fit truncates or pads s to exactly w columns.
func fit(s string, w int) string {
	r := []rune(s)
	if len(r) > w {
		return string(r[:w-1]) + "…"
	}
	return s + strings.Repeat(" ", w-len(r))
}
