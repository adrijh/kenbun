package tui

import (
	"fmt"
	"math"
	"path"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/adrijh/kenbun/internal/store"
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
	waitS  = lipgloss.NewStyle().Foreground(carp) // loop blocked waiting (vs upS: on CPU)
	upS    = lipgloss.NewStyle().Foreground(waveRed)
	downS  = greenS
)

var screenNames = map[screen][2]string{ // english, japanese
	screenProcs: {"PROCS", "プロセス"},
	screenTop:   {"FUNCTIONS", "関数"},
	screenFlame: {"FLAME", "炎"},
	screenLoop:  {"LOOP", "ループ"},
}

// keys are each screen's own, in the footer as far as they fit and all in the
// ? help, followed by common.
var (
	keys = map[screen][][2]string{
		screenProcs: {{"↑↓", "move"}, {"enter", "open"}, {"→←", "PIDs"}, {"/", "filter"}},
		screenTop:   {{"↑↓", "move"}, {"enter", "show in flame"}, {"/", "filter"}, {"tab", "flame"}, {"a", "asyncio loop"}, {"d", "diff"}, {"esc", "back"}},
		screenFlame: {{"←↑↓→", "move"}, {"enter", "zoom"}, {"esc", "zoom out/back"}, {"/", "search"}, {"tab", "functions"}, {"a", "asyncio loop"}},
		screenLoop:  {{"esc", "functions"}},
	}
	common   = [][2]string{{"[ ]", "earlier/later"}, {"w", "window"}, {"p", "pause"}, {"q", "quit"}}
	listKeys = [][2]string{{"pgup pgdn", "page"}, {"g G", "first/last"}, {"esc", "clear the filter"}}
)

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

	switch {
	case m.help:
		m.viewHelp(&b)
	case m.screen == screenProcs:
		m.viewProcs(&b, rows)
	case m.screen == screenTop:
		m.viewTop(&b, rows)
	case m.screen == screenFlame:
		m.viewFlame(&b, rows)
	case m.screen == screenLoop:
		m.viewLoop(&b, rows)
	}

	// Pin error and footer to the bottom.
	if pad := m.h - 2 - strings.Count(b.String(), "\n"); pad > 0 {
		b.WriteString(strings.Repeat("\n", pad))
	}
	switch {
	case m.fixing != "":
		b.WriteString(pauseS.Render(m.clip("… "+m.fixing, 0)))
	case m.err != nil:
		b.WriteString(upS.Render(m.clip("✗ "+m.err.Error(), 0)))
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
		left += dimS.Render(" ❯ ") + boldS.Render(m.proc)
		if m.pid != 0 {
			left += dimS.Render(" pid ") + boldS.Render(fmt.Sprint(m.pid))
		}
		left += dimS.Render(" ❯ ") + crumb(m.screen)
	}

	var chips []string
	// The window's end, as of the latest query: green and moving with each
	// refresh while live, frozen in yellow when paused.
	clockS := greenS.Bold(true)
	if m.paused {
		clockS = pauseS
	}
	chips = append(chips, clockS.Render(m.asOf.Local().Format("15:04:05")))
	chips = append(chips, dimS.Render("window ")+fmtDur(m.window()))
	if m.diff && m.screen == screenTop {
		chips = append(chips, redS.Render("Δ vs previous "+fmtDur(m.window())))
	}
	if m.otlp != "" {
		chips = append(chips, dimS.Render("otlp ")+m.otlp)
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

// footer: the keys that fit, and always how to see them all.
func (m model) footer() string {
	key := func(k [2]string) string { return redS.Render(k[0]) + " " + dimS.Render(k[1]) }
	if m.help {
		return key([2]string{"any key", "close"})
	}
	last := key([2]string{"?", "help"})
	var parts []string
	used := lipgloss.Width(last)
	var fix [][2]string
	if f := m.fixer(); f != nil && m.fixing == "" {
		what, _ := f.Fix()
		fix = [][2]string{{"i", what}}
	}
	for _, k := range slices.Concat(fix, keys[m.screen], common) {
		if k[0] == "a" && !m.hasLoop() {
			continue
		}
		w := lipgloss.Width(k[0]) + 1 + lipgloss.Width(k[1]) + 3
		if used+w > m.width() {
			break
		}
		used += w
		parts = append(parts, key(k))
	}
	return strings.Join(append(parts, last), "   ")
}

func (m model) viewHelp(b *strings.Builder) {
	b.WriteString("  " + accS.Bold(true).Render(screenNames[m.screen][0]) + dimS.Render(" keys") + "\n\n")
	all := keys[m.screen]
	if m.screen == screenProcs || m.screen == screenTop {
		all = slices.Concat(all, listKeys)
	}
	for _, k := range slices.Concat(all, common) {
		b.WriteString(fmt.Sprintf("  %s  %s\n", redS.Render(fmt.Sprintf("%10s", k[0])), k[1]))
	}
	b.WriteString("\n  " + dimS.Render("[ ] step the window back and forth by its size; ] past now goes live again") + "\n")
}

// filterLine shows a list's / filter while it's typed or applied.
func (m model) filterLine(b *strings.Builder, q string, n, of int, what string) bool {
	if q == "" && !m.searching {
		return false
	}
	cursor := ""
	if m.searching {
		cursor = redS.Render("▌")
	}
	b.WriteString(dimS.Render("  filter ") + hit.Render(" /"+q+" ") + cursor +
		dimS.Render(fmt.Sprintf("  %d of %d %s", n, of, what)) + "\n")
	return true
}

// cpu is weight in the shown window as millicores, as 'kenbun top' gives it:
// averaged over the part of the window there is data for, in whole seconds.
func (m model) cpu(w int64) string {
	span := m.window()
	if !m.oldest.IsZero() {
		span = min(span, m.asOf.Sub(m.oldest).Truncate(time.Second)+time.Second)
	}
	return fmt.Sprintf("%dm", store.Millicores(w, span))
}

func fmtDur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

func (m model) viewProcs(b *strings.Builder, rows int) {
	if len(m.procs) == 0 && m.python {
		b.WriteString("  no Python processes running" + redS.Render(" ▌") + "\n")
		return
	}
	if len(m.procs) == 0 {
		wait := "  awaiting profiles"
		if m.otlp != "" {
			wait += dimS.Render(" on OTLP ") + m.otlp
		}
		b.WriteString(wait + redS.Render(" ▌") + "\n")
		return
	}
	var all int64
	for _, p := range m.procs {
		all += p.Weight // shares stay of all processes, filtered or not
	}
	procs := m.procRows()
	if m.filterLine(b, m.procFilter, len(procs), len(m.procs), "processes") {
		rows--
	}
	// A process with several PIDs says so; expanded, they're listed under it.
	labels := make([][2]string, len(procs)) // name, dim suffix
	nameW := 12
	for i, r := range procs {
		switch {
		case r.pid != 0:
			branch := "├"
			if i+1 == len(procs) || procs[i+1].pid == 0 {
				branch = "└"
			}
			labels[i] = [2]string{"", fmt.Sprintf("  %s pid %d", branch, r.pid)}
		case r.pids > 1:
			arrow := "▸"
			if m.expanded[r.name] {
				arrow = "▾"
			}
			labels[i] = [2]string{r.name, fmt.Sprintf(" %s %d pids", arrow, r.pids)}
		default:
			labels[i] = [2]string{r.name, ""}
		}
		nameW = max(nameW, len([]rune(labels[i][0]+labels[i][1])))
	}
	nameW = min(nameW, 48)
	barW := max(0, min(32, m.width()-nameW-22))

	b.WriteString(dimS.Render(fmt.Sprintf("  %-*s %7s %7s  %s", nameW, "PROCESS", "CPU", "SHARE", "")) + "\n")
	rows--
	start := max(0, m.pcur-rows+1)
	for i := start; i < min(len(procs), start+rows); i++ {
		r, l := procs[i], labels[i]
		share := pct(r.weight, all)
		suffix := []rune(l[1])
		if len(suffix) > nameW {
			suffix = suffix[:nameW]
		}
		name := strings.TrimRight(fit(l[0], nameW-len(suffix)), " ")
		pad := strings.Repeat(" ", max(0, nameW-len([]rune(name))-len(suffix)))
		cols := fmt.Sprintf(" %7s %6.1f%%  ", m.cpu(r.weight), share)
		bar := bar(share/100, barW)
		if i == m.pcur {
			b.WriteString(selS.Render("▶ "+name+string(suffix)+pad+cols+bar) + "\n")
			continue
		}
		b.WriteString("  " + name + dimS.Render(string(suffix)) + pad + cols + heat(share).Render(bar) + "\n")
	}
}

func (m model) viewTop(b *strings.Builder, rows int) {
	if len(m.top) == 0 {
		b.WriteString(dimS.Render("  no samples in window") + "\n")
		return
	}
	funcs := m.funcs()
	if m.filterLine(b, m.funcFilter, len(funcs), len(m.top), "functions") {
		rows--
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
	// Locations line up right after the longest name on screen, not at the
	// far edge of a wide terminal.
	longest := 0
	for _, f := range funcs[start:min(len(funcs), start+rows)] {
		longest = max(longest, len([]rune(f.Name))+2+len([]rune(shortLoc(f.Loc))))
	}
	nameW = min(nameW, longest)
	for i := start; i < min(len(funcs), start+rows); i++ {
		f := funcs[i]
		self, tot := pct(f.Self, total), pct(f.Total, total)
		isSel := i == m.tcur

		var deltas string
		if diff {
			p := m.prev[f.Name]
			deltas = m.delta(self-pct(p.Self, m.prevN), isSel) + " " + m.delta(tot-pct(p.Total, m.prevN), isSel) + " "
		}
		solid, shade := splitBar(self/100, tot/100, barW)
		name, loc := nameLoc(f.Name, f.Loc, nameW)
		cols := fmt.Sprintf("%5.1f%% %6.1f%% ", self, tot)

		if isSel {
			b.WriteString(selS.Render("▶ "+cols+deltas+" "+solid+shade+"  "+name+loc) + "\n")
			continue
		}
		nameS := lipgloss.NewStyle()
		if strings.HasPrefix(f.Name, "[") { // native frames: context, rarely the culprit
			nameS = dimS
		}
		b.WriteString("  " + cols + deltas + " " + heat(self).Render(solid) + ruleS.Render(shade) + "  " + nameS.Render(name) + dimS.Render(loc) + "\n")
	}
}

// viewLoop shows each asyncio loop thread: how much of its time callbacks held
// the loop, and which callbacks, on CPU or waiting (sleep, blocking I/O).
func (m model) viewLoop(b *strings.Builder, rows int) {
	if len(m.loops) == 0 {
		b.WriteString(dimS.Render("  no asyncio event loop in window (needs Python frames; off-CPU needs --profile)") + "\n")
		return
	}
	for _, l := range m.loops {
		if rows < 4 {
			break
		}
		share := func(d time.Duration) string { return fmt.Sprintf("%.1f%%", pct(int64(d), int64(l.Total))) }
		blocked := pct(int64(l.Blocked()), int64(l.Total))
		b.WriteString("  " + dimS.Render("thread ") + boldS.Render(l.Thread) + dimS.Render("  loop blocked ") +
			heat(blocked).Bold(true).Render(share(l.Blocked())) + "  " + heat(blocked).Render(bar(blocked/100, 20)) + "  " +
			upS.Render("cpu "+share(l.CPU)) + "  " + waitS.Render("waiting "+share(l.Wait)) + "  " +
			dimS.Render("idle "+share(l.Idle)) + "\n")
		head := fmt.Sprintf("  %7s %7s %7s  %-4s  ", "BLOCKED", "CPU", "WAIT", "")
		b.WriteString(dimS.Render(head+"CALLBACK") + "\n")
		rows -= 2
		nameW := max(8, m.width()-lipgloss.Width(head))
		for _, c := range l.Callbacks {
			if rows < 2 {
				break
			}
			kind, kindS := "cpu ", upS
			if c.Wait > c.CPU {
				kind, kindS = "wait", waitS
			}
			b.WriteString(fmt.Sprintf("  %7s %7s %7s  ", share(c.Blocked()), share(c.CPU), share(c.Wait)) +
				kindS.Render(kind) + "  " + fit(c.Name, nameW) + "\n")
			rows--
		}
		b.WriteString("\n")
		rows--
	}
}

// nameLoc fits a function name into w columns with its location at the right
// edge. The location goes first when the name would be cut below 16 columns.
func nameLoc(name, loc string, w int) (string, string) {
	l := shortLoc(loc)
	room := w - len([]rune(l)) - 2
	if l == "" || (room < len([]rune(name)) && room < 16) {
		return fit(name, w), ""
	}
	return fit(name, room), "  " + l
}

// shortLoc is file:line with the file's directories left out.
func shortLoc(loc string) string {
	i := strings.LastIndex(loc, ":")
	if i < 0 {
		return loc
	}
	return path.Base(loc[:i]) + loc[i:]
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
