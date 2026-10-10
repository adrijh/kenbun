package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Theme: Kanagawa (Hokusai's Great Wave), the common dark terminal theme, so
// kenbun sits in it naturally. The terminal's own background is kept; contrast
// is checked against Kanagawa's (contrast_test.go).
const (
	bgHex     = "#1f1f28" // sumiInk1: the background contrast is checked against
	inkHex    = "#16161d" // sumiInk0: text on light cells
	fujiHex   = "#dcd7ba" // fujiWhite: body text, selected flame frame
	violetHex = "#938aa9" // springViolet1: secondary text
	greenHex  = "#98bb6c" // springGreen: live, highlighted frames
	sealHex   = "#c34043" // autumnRed: the 見聞 seal
	selRowHex = "#2d4f67" // waveBlue2: selected table row (Kanagawa's selection)
)

var (
	ink     = lipgloss.Color(inkHex)
	fuji    = lipgloss.Color(fujiHex)
	crystal = lipgloss.Color("#7e9cd8") // crystalBlue: accents
	waveRed = lipgloss.Color("#e46876") // keys, hot spots, more CPU
	carp    = lipgloss.Color("#e6c384") // carpYellow: paused, warm
	aqua    = lipgloss.Color("#7aa89f") // waveAqua2: cool
	rule    = lipgloss.Color("#54546d") // sumiInk4

	logoS  = lipgloss.NewStyle().Background(lipgloss.Color(sealHex)).Foreground(fuji).Bold(true)
	redS   = lipgloss.NewStyle().Foreground(waveRed).Bold(true)
	accS   = lipgloss.NewStyle().Foreground(crystal)
	greenS = lipgloss.NewStyle().Foreground(lipgloss.Color(greenHex))
	boldS  = lipgloss.NewStyle().Bold(true)
	dimS   = lipgloss.NewStyle().Foreground(lipgloss.Color(violetHex))
	ruleS  = lipgloss.NewStyle().Foreground(rule)
	selS   = lipgloss.NewStyle().Background(lipgloss.Color(selRowHex)).Foreground(fuji).Bold(true)
	pauseS = lipgloss.NewStyle().Foreground(carp).Bold(true)
	upS    = lipgloss.NewStyle().Foreground(waveRed)
	downS  = greenS
)

var screenNames = map[screen][2]string{ // english, japanese
	screenProcs: {"PROCS", "プロセス"},
	screenTop:   {"FUNCTIONS", "関数"},
	screenFlame: {"FLAME", "炎"},
	screenLoop:  {"LOOP", "ループ"},
}

var keys = map[screen][][2]string{
	screenProcs: {{"↑↓", "move"}, {"enter", "open"}, {"w", "window"}, {"p", "pause"}, {"q", "quit"}},
	screenTop:   {{"↑↓", "move"}, {"enter", "show in flame"}, {"tab", "flame"}, {"a", "asyncio loop"}, {"d", "diff"}, {"w", "window"}, {"p", "pause"}, {"esc", "back"}},
	screenFlame: {{"←↑↓→", "move"}, {"enter", "zoom"}, {"esc", "zoom out/back"}, {"/", "search"}, {"tab", "functions"}, {"a", "asyncio loop"}, {"w", "window"}, {"p", "pause"}},
	screenLoop:  {{"esc", "functions"}, {"w", "window"}, {"p", "pause"}, {"q", "quit"}},
}

func (m model) width() int {
	if m.w == 0 {
		return 100
	}
	return m.w
}

func (m model) View() tea.View {
	var b strings.Builder
	b.WriteString(m.header() + "\n")
	b.WriteString(ruleS.Render(strings.Repeat("━", m.width())) + "\n")
	rows := 20
	if m.h > 0 {
		rows = max(1, m.h-4) // header, rule, error, footer
	}

	switch m.screen {
	case screenProcs:
		m.viewProcs(&b, rows)
	case screenTop:
		m.viewTop(&b, rows)
	case screenFlame:
		m.viewFlame(&b, rows)
	case screenLoop:
		m.viewLoop(&b, rows)
	}

	// Pin error and footer to the bottom.
	if pad := m.h - 2 - strings.Count(b.String(), "\n"); pad > 0 {
		b.WriteString(strings.Repeat("\n", pad))
	}
	if m.err != nil {
		b.WriteString(upS.Render(m.clip("✗ エラー "+m.err.Error(), 0)))
	}
	b.WriteString("\n" + m.footer())

	v := tea.NewView(b.String())
	v.AltScreen = true
	v.WindowTitle = "見聞 kenbun"
	return v
}

// header: logo and breadcrumbs on the left, status chips on the right.
func (m model) header() string {
	left := logoS.Render(" 見聞 ") + redS.Render(" KENBUN ") + dimS.Render("// ")
	crumb := func(s screen) string {
		n := screenNames[s]
		if s == m.screen {
			return accS.Bold(true).Render(n[0]) + " " + dimS.Render(n[1])
		}
		return dimS.Render(n[0])
	}
	left += crumb(screenProcs)
	if m.screen != screenProcs {
		left += dimS.Render(" ❯ ") + boldS.Render(m.proc) + dimS.Render(" ❯ ") + crumb(m.screen)
	}

	var chips []string
	// The window's end, as of the latest query: green and moving with each
	// refresh while live, frozen in yellow when paused.
	clockS := greenS.Bold(true)
	if m.paused {
		clockS = pauseS
	}
	chips = append(chips, clockS.Render(m.asOf.Local().Format("15:04:05")))
	chips = append(chips, dimS.Render("窓 ")+fmtDur(m.window()))
	if m.diff && m.screen == screenTop {
		chips = append(chips, redS.Render("差分 Δ "+fmtDur(m.window())))
	}
	if m.otlp != "" {
		chips = append(chips, dimS.Render("受信 ")+m.otlp)
	}

	// Drop chips from the right until everything fits.
	for len(chips) > 0 {
		right := strings.Join(chips, ruleS.Render(" ┃ "))
		if gap := m.width() - lipgloss.Width(left) - lipgloss.Width(right); gap >= 2 {
			return left + strings.Repeat(" ", gap) + right
		}
		chips = chips[:len(chips)-1]
	}
	return left
}

func (m model) footer() string {
	var parts []string
	used := 0
	for _, k := range keys[m.screen] {
		if k[0] == "a" && !m.hasLoop() {
			continue
		}
		w := lipgloss.Width(k[0]) + 1 + lipgloss.Width(k[1]) + 3
		if used+w > m.width() {
			break
		}
		used += w
		parts = append(parts, redS.Render(k[0])+" "+dimS.Render(k[1]))
	}
	return strings.Join(parts, "   ")
}

func fmtDur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

func (m model) viewProcs(b *strings.Builder, rows int) {
	if len(m.procs) == 0 {
		b.WriteString(dimS.Render("  受信待機中 ") + "awaiting profiles" + redS.Render(" ▌") + "\n")
		return
	}
	var all int64
	nameW := 12
	for _, p := range m.procs {
		all += p.Weight
		nameW = max(nameW, len([]rune(p.Name)))
	}
	nameW = min(nameW, 40)
	barW := max(0, min(32, m.width()-nameW-24))

	b.WriteString(dimS.Render(fmt.Sprintf("  %-*s %8s %7s  %s", nameW, "PROCESS", "SAMPLES", "SHARE", "")) + "\n")
	rows--
	start := max(0, m.pcur-rows+1)
	for i := start; i < min(len(m.procs), start+rows); i++ {
		p := m.procs[i]
		share := pct(p.Weight, all)
		cols := fmt.Sprintf("%-*s %8d %6.1f%%  ", nameW, fit(p.Name, nameW), p.Weight, share)
		bar := bar(share/100, barW)
		if i == m.pcur {
			b.WriteString(selS.Render("▶ "+cols+bar) + "\n")
			continue
		}
		b.WriteString("  " + cols + heat(share).Render(bar) + "\n")
	}
}

func (m model) viewTop(b *strings.Builder, rows int) {
	if len(m.top) == 0 {
		b.WriteString(dimS.Render("  no samples in window") + "\n")
		return
	}
	var total int64
	if m.tree != nil {
		total = m.tree.Total
	}
	diff := m.diff && m.prev != nil
	const barW = 24

	head := fmt.Sprintf("  %6s %7s ", "SELF", "TOTAL")
	if diff {
		head += fmt.Sprintf("%8s %8s ", "ΔSELF", "ΔTOTAL")
	}
	head += fmt.Sprintf(" %-*s  FUNCTION", barW, "█ self ░ total")
	b.WriteString(dimS.Render(head) + "\n")
	rows--

	nameW := max(8, m.width()-lipgloss.Width(head)+len("FUNCTION"))
	start := max(0, m.tcur-rows+1)
	for i := start; i < min(len(m.top), start+rows); i++ {
		f := m.top[i]
		self, tot := pct(f.Self, total), pct(f.Total, total)
		isSel := i == m.tcur

		var deltas string
		if diff {
			p := m.prev[f.Name]
			deltas = m.delta(self-pct(p.Self, m.prevN), isSel) + " " + m.delta(tot-pct(p.Total, m.prevN), isSel) + " "
		}
		solid, shade := splitBar(self/100, tot/100, barW)
		name := fit(f.Name, nameW)
		cols := fmt.Sprintf("%5.1f%% %6.1f%% ", self, tot)

		if isSel {
			b.WriteString(selS.Render("▶ "+cols+deltas+" "+solid+shade+"  "+name) + "\n")
			continue
		}
		nameS := lipgloss.NewStyle()
		if strings.HasPrefix(f.Name, "[") { // native frames: context, rarely the culprit
			nameS = dimS
		}
		b.WriteString("  " + cols + deltas + " " + heat(self).Render(solid) + ruleS.Render(shade) + "  " + nameS.Render(name) + "\n")
	}
}

// viewLoop shows each asyncio loop thread: how much of its time callbacks held
// the loop, and which callbacks, on CPU or waiting (sleep, blocking I/O).
func (m model) viewLoop(b *strings.Builder, rows int) {
	if len(m.loops) == 0 {
		b.WriteString(dimS.Render("  no asyncio event loop in window (needs Python frames; off-CPU needs --profile)") + "\n")
		return
	}
	waitS := lipgloss.NewStyle().Foreground(carp)
	for _, l := range m.loops {
		if rows < 4 {
			break
		}
		blocked := pct(int64(l.Blocked()), int64(l.Total))
		b.WriteString("  " + dimS.Render("thread ") + boldS.Render(l.Thread) + dimS.Render("  loop blocked ") +
			heat(blocked).Bold(true).Render(fmt.Sprintf("%.1f%%", blocked)) + "  " +
			heat(blocked).Render(bar(blocked/100, 20)) + "  " +
			upS.Render(fmt.Sprintf("cpu %.1f%%", pct(int64(l.CPU), int64(l.Total)))) + "  " +
			waitS.Render(fmt.Sprintf("waiting %.1f%%", pct(int64(l.Wait), int64(l.Total)))) + "  " +
			dimS.Render(fmt.Sprintf("idle %.1f%%", pct(int64(l.Idle), int64(l.Total)))) + "\n")
		head := fmt.Sprintf("  %7s %7s %7s  %-4s  ", "BLOCKED", "CPU", "WAIT", "")
		b.WriteString(dimS.Render(head+"CALLBACK") + "\n")
		rows -= 2
		nameW := max(8, m.width()-lipgloss.Width(head))
		for _, c := range l.Callbacks {
			if rows < 2 {
				break
			}
			kind, kindS := "cpu", upS
			if c.Wait > c.CPU {
				kind, kindS = "wait", waitS
			}
			b.WriteString(fmt.Sprintf("  %6.1f%% %6.1f%% %6.1f%%  ", pct(int64(c.Blocked()), int64(l.Total)),
				pct(int64(c.CPU), int64(l.Total)), pct(int64(c.Wait), int64(l.Total))) +
				kindS.Render(fmt.Sprintf("%-4s", kind)) + "  " + fit(c.Name, nameW) + "\n")
			rows--
		}
		b.WriteString("\n")
		rows--
	}
}

// delta formats a change in percentage points: ▲ pink = more CPU than before.
func (m model) delta(d float64, plain bool) string {
	if m.prevN == 0 { // nothing in the previous window to compare with
		return dimS.Render(fmt.Sprintf("%8s", "– "))
	}
	arrow, st := "▲", upS
	switch {
	case math.Abs(d) < 0.05:
		arrow, st = "·", dimS
		d = 0
	case d < 0:
		arrow, st = "▼", downS
	}
	s := fmt.Sprintf("%s %+5.1f ", arrow, d)
	if plain {
		return s
	}
	return st.Render(s)
}

// heat colors by share: aqua when small, yellow, then red when it dominates.
func heat(p float64) lipgloss.Style {
	switch {
	case p >= 40:
		return upS
	case p >= 10:
		return lipgloss.NewStyle().Foreground(carp)
	default:
		return lipgloss.NewStyle().Foreground(aqua)
	}
}

var eighths = []string{"", "▏", "▎", "▍", "▌", "▋", "▊", "▉"}

// bar draws frac (0..1) of w columns with eighth-block precision, padded to w.
func bar(frac float64, w int) string {
	n := int(math.Round(max(0, min(1, frac)) * float64(w*8)))
	s := strings.Repeat("█", n/8) + eighths[n%8]
	return s + strings.Repeat(" ", w-len([]rune(s)))
}

// splitBar draws total as a bar whose first (self) part is solid, padded to w.
func splitBar(self, total float64, w int) (solid, shade string) {
	s := int(math.Round(max(0, min(1, self)) * float64(w)))
	t := max(s, int(math.Round(max(0, min(1, total))*float64(w))))
	if s == 0 && self > 0 {
		s, t = 1, max(t, 1) // keep tiny self visible
	}
	return strings.Repeat("█", s), strings.Repeat("░", t-s) + strings.Repeat(" ", w-t)
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
