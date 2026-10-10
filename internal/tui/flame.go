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

var (
	warm   = palette("#e8a33d", "#d9822b", "#e07a5f", "#e0c341", "#d4a373", "#cc7a00") // symbolized
	cool   = palette("#7f9cb3", "#6c8ebf", "#8fa8c8", "#91a7a0")                       // [native] frames
	hit    = lipgloss.NewStyle().Background(lipgloss.Color("#d33682")).Foreground(lipgloss.Color("#ffffff"))
	picked = lipgloss.NewStyle().Background(lipgloss.Color("#ffffff")).Foreground(lipgloss.Color("#000000")).Bold(true)
)

func palette(colors ...string) []lipgloss.Style {
	out := make([]lipgloss.Style, len(colors))
	for i, c := range colors {
		out[i] = lipgloss.NewStyle().Background(lipgloss.Color(c)).Foreground(lipgloss.Color("#000000"))
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

func matches(name, lowerQuery string) bool {
	return lowerQuery != "" && strings.Contains(strings.ToLower(name), lowerQuery)
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
		m.focus = nil
	}
}

// matchTotal is the weight under nodes matching q, counting nested matches once.
func matchTotal(n *store.Node, q string) int64 {
	if matches(n.Name, q) {
		return n.Total
	}
	var sum int64
	for _, c := range n.Children {
		sum += matchTotal(c, q)
	}
	return sum
}

func (m model) flameKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.tree == nil {
		if k := msg.String(); k == "esc" || k == "backspace" {
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
		m.focus = slices.Clone(m.sel)
	case "/":
		m.searching, m.search = true, ""
	case "esc", "backspace":
		switch {
		case m.search != "":
			m.search = ""
		case len(m.focus) > 0:
			m.focus = m.focus[:len(m.focus)-1]
		default:
			m.screen = screenTop
			return m.reload()
		}
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
	q := strings.ToLower(m.search)

	// Status line first, so the graph gets the rest.
	all := m.tree.Total
	fmt.Fprintf(b, "%s  total %.1f%% · self %.1f%%", m.clip(selNode.Name, 40), pct(selNode.Total, all), pct(selNode.Self, all))
	if len(focus) > 1 {
		fmt.Fprintf(b, " · zoom: %s", focus[len(focus)-1].Name)
	}
	if m.search != "" || m.searching {
		fmt.Fprintf(b, " · /%s", m.search)
		if m.searching {
			b.WriteString("▏")
		}
		fmt.Fprintf(b, " (%.1f%%)", pct(matchTotal(focus[len(focus)-1], q), all))
	}
	b.WriteString("\n")
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
			b.WriteString(strings.Repeat(" ", c.x-col))
			st := colorOf(c.n.Name)
			switch {
			case c.n == selNode:
				st = picked
			case matches(c.n.Name, q):
				st = hit
			}
			if c.w == 1 {
				b.WriteString(st.Render(" "))
			} else { // one blank column separates neighbours
				b.WriteString(st.Render(fit(c.n.Name, c.w-1)) + " ")
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
