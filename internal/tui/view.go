package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

var (
	bold     = lipgloss.NewStyle().Bold(true)
	faint    = lipgloss.NewStyle().Faint(true)
	selected = lipgloss.NewStyle().Reverse(true)
	worse    = lipgloss.NewStyle().Foreground(lipgloss.Color("#e05d5d"))
	better   = lipgloss.NewStyle().Foreground(lipgloss.Color("#5dbb63"))
)

var help = map[screen]string{
	screenProcs: "↑↓ move · enter open · w window · p pause · q quit",
	screenTop:   "↑↓ move · enter flamegraph at function · tab flamegraph · d diff · w window · p pause · esc back",
	screenFlame: "←↑↓→ move · enter zoom · / search · tab top · w window · p pause · esc back",
}

func (m model) View() tea.View {
	var b strings.Builder
	b.WriteString(bold.Render(m.clip(m.header(), 0)) + "\n\n")
	rows := 20
	if m.h > 0 {
		rows = max(1, m.h-4) // header, blank, error/blank, help
	}

	switch m.screen {
	case screenProcs:
		m.viewProcs(&b, rows)
	case screenTop:
		m.viewTop(&b, rows)
	case screenFlame:
		m.viewFlame(&b, rows)
	}

	// Pin the help line to the bottom.
	if pad := m.h - 2 - strings.Count(b.String(), "\n"); pad > 0 {
		b.WriteString(strings.Repeat("\n", pad))
	}
	if m.err != nil {
		b.WriteString(worse.Render(m.clip("error: "+m.err.Error(), 0)))
	}
	b.WriteString("\n" + faint.Render(m.clip(help[m.screen], 0)))

	v := tea.NewView(b.String())
	v.AltScreen = true
	return v
}

func (m model) header() string {
	parts := []string{"kenbun"}
	if m.screen != screenProcs {
		parts = append(parts, m.proc)
	}
	parts = append(parts, "last "+fmtDur(m.window()))
	if m.paused {
		parts = append(parts, "PAUSED at "+m.at.Local().Format("15:04:05"))
	} else {
		parts = append(parts, "live")
	}
	if m.diff && m.screen == screenTop {
		parts = append(parts, "diff vs previous "+fmtDur(m.window()))
	}
	return strings.Join(parts, " · ")
}

func fmtDur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

func (m model) viewProcs(b *strings.Builder, rows int) {
	if len(m.procs) == 0 {
		b.WriteString("no profiles in window (OTLP on :4317)\n")
		return
	}
	var all int64
	for _, p := range m.procs {
		all += p.Weight
	}
	fmt.Fprintf(b, "%8s %6s  %s\n", "SAMPLES", "%", "PROCESS")
	rows--
	start := max(0, m.pcur-rows+1)
	for i := start; i < min(len(m.procs), start+rows); i++ {
		p := m.procs[i]
		line := m.clip(fmt.Sprintf("%8d %6.1f  %s", p.Weight, pct(p.Weight, all), p.Name), 0)
		if i == m.pcur {
			line = selected.Render(line)
		}
		b.WriteString(line + "\n")
	}
}

func (m model) viewTop(b *strings.Builder, rows int) {
	var total int64
	if m.tree != nil {
		total = m.tree.Total
	}
	if len(m.top) == 0 {
		b.WriteString("no samples in window\n")
		return
	}
	diff := m.diff && m.prev != nil
	if diff {
		fmt.Fprintf(b, "%6s %7s %7s %7s  %s\n", "SELF%", "TOTAL%", "ΔSELF", "ΔTOTAL", "FUNCTION")
	} else {
		fmt.Fprintf(b, "%6s %7s  %s\n", "SELF%", "TOTAL%", "FUNCTION")
	}
	rows--
	start := max(0, m.tcur-rows+1)
	for i := start; i < min(len(m.top), start+rows); i++ {
		f := m.top[i]
		self, tot := pct(f.Self, total), pct(f.Total, total)
		isSel := i == m.tcur

		var line string
		if diff {
			p := m.prev[f.Name]
			ds := m.delta(self-pct(p.Self, m.prevN), isSel)
			dt := m.delta(tot-pct(p.Total, m.prevN), isSel)
			line = fmt.Sprintf("%6.1f %7.1f %s %s  %s", self, tot, ds, dt, m.clip(f.Name, 32))
		} else {
			line = fmt.Sprintf("%6.1f %7.1f  %s", self, tot, m.clip(f.Name, 16))
		}
		if isSel {
			line = selected.Render(line)
		}
		b.WriteString(line + "\n")
	}
}

// delta formats a change in percentage points; red = more CPU than before.
func (m model) delta(d float64, plain bool) string {
	s := fmt.Sprintf("%+7.1f", d)
	switch {
	case m.prevN == 0:
		return fmt.Sprintf("%7s", "-")
	case plain || (d > -0.05 && d < 0.05):
		return s
	case d > 0:
		return worse.Render(s)
	default:
		return better.Render(s)
	}
}

// clip keeps a line within the terminal width (indent = columns before s).
func (m model) clip(s string, indent int) string {
	r := []rune(s)
	if w := m.w - indent; m.w > 0 && len(r) > w {
		return string(r[:max(0, w-1)]) + "…"
	}
	return s
}

func pct(n, of int64) float64 {
	if of == 0 {
		return 0
	}
	return 100 * float64(n) / float64(of)
}
