// Package tui is kenbun's terminal UI: process list → top functions ⇄
// flamegraph (or the asyncio loop view), over a sliding time window that can
// be paused, stepped back and forth, and diffed.
// It polls its Source every refresh interval.
package tui

import (
	"context"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/adrijh/kenbun/internal/store"
)

// Source is what the TUI reads: *store.Store locally, *api.Client remotely.
type Source interface {
	Processes(ctx context.Context, from, to time.Time) ([]store.ProcStat, error)
	Stacks(ctx context.Context, process string, pid int64, from, to time.Time) ([]store.StackWeight, error)
	ThreadStacks(ctx context.Context, process string, pid int64, from, to time.Time) ([]store.ThreadStack, error)
	Oldest(ctx context.Context) (time.Time, error)
}

type Options struct {
	At     time.Time     // non-zero: start paused with the window ending here (offline files)
	Window time.Duration // initial window, rounded up to a preset; 0 = 1m
	OTLP   string        // where this process receives OTLP, if it does
}

var windows = []time.Duration{30 * time.Second, time.Minute, 5 * time.Minute, 15 * time.Minute}

const refresh = 2 * time.Second

type screen int

const (
	screenProcs screen = iota
	screenTop
	screenFlame
	screenLoop
)

type (
	tickMsg  struct{}
	procsMsg struct {
		gen   int
		procs []store.ProcStat
	}
	stacksMsg struct {
		gen   int
		top   []store.FuncStat
		tree  *store.Node
		prev  map[string]store.FuncStat // previous window, only in diff mode
		prevN int64
	}
	loopMsg struct {
		gen   int
		loops []store.LoopStat
	}
	oldestMsg struct{ t time.Time }
	errMsg    struct{ err error }
)

type model struct {
	src  Source
	otlp string
	w, h int
	err  error
	gen  int // bumped whenever query parameters change; older results are dropped

	win    int // index into windows
	paused bool
	at     time.Time // window end while paused
	asOf   time.Time // window end of the latest query, shown in the header
	last   time.Time // a capture's end, the latest window there is; zero when live
	oldest time.Time // the oldest sample held, the earliest window there is
	diff   bool
	help   bool

	screen     screen
	procs      []store.ProcStat
	procFilter string          // / on the process list: only names containing it
	expanded   map[string]bool // processes whose PIDs are listed under them
	pcur       int             // index into procRows()
	proc       string
	pid        int64 // the one PID opened, 0 for all of proc's

	loops []store.LoopStat

	top        []store.FuncStat
	funcFilter string // / on the functions table
	tree       *store.Node
	prev       map[string]store.FuncStat
	prevN      int64
	tcur       int // index into funcs()

	// Flamegraph state is kept as frame names from the root so it survives
	// refreshes, which rebuild the tree.
	focus     []string   // zoomed-in node
	zooms     [][]string // earlier focuses, undone by esc
	sel       []string   // selected node (always inside focus)
	search    string
	searching bool   // typing the search query
	exact     bool   // search is a function picked in the table, matched exactly
	jump      string // function to select once the tree arrives (from the top table)
}

func New(src Source, o Options) tea.Model {
	m := model{src: src, win: 1, otlp: o.OTLP, last: o.At, expanded: map[string]bool{}}
	for o.Window > 0 && m.win < len(windows)-1 && windows[m.win] < o.Window {
		m.win++
	}
	if !o.At.IsZero() {
		m.paused, m.at = true, o.At
	}
	m.asOf = m.end()
	return m
}

func (m model) Init() tea.Cmd { return tea.Batch(m.load(), tick()) }

// step returns the window end one window earlier (dir -1) or later (+1),
// kept where there is data; ok false when it can't move. Stepping later than
// now goes live again.
func (m model) step(dir int) (at time.Time, live, ok bool) {
	at = m.asOf.Add(time.Duration(dir) * m.window())
	switch {
	case dir < 0 && !m.oldest.IsZero() && !at.After(m.oldest):
		return m.at, false, false // the window would hold nothing
	case dir > 0 && !m.paused:
		return m.at, false, false
	case dir > 0 && !m.last.IsZero() && at.After(m.last):
		return m.last, false, !m.asOf.Equal(m.last)
	case dir > 0 && m.last.IsZero() && !at.Before(time.Now()):
		return time.Time{}, true, true
	}
	return at, false, true
}

func tick() tea.Cmd {
	return tea.Tick(refresh, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m model) window() time.Duration { return windows[m.win] }

func (m model) end() time.Time {
	if m.paused {
		return m.at
	}
	return time.Now()
}

// reload invalidates in-flight results and queries the current screen.
func (m model) reload() (model, tea.Cmd) {
	m.gen++
	m.asOf = m.end()
	return m, m.load()
}

// load queries the source for the current screen off the update loop, and how
// far back its data goes.
func (m model) load() tea.Cmd {
	src := m.src
	return tea.Batch(m.query(), func() tea.Msg {
		t, _ := src.Oldest(context.Background()) // unknown (an older kenbun): no limit
		return oldestMsg{t}
	})
}

func (m model) query() tea.Cmd {
	src, gen, proc, pid := m.src, m.gen, m.proc, m.pid
	to := m.asOf
	from := to.Add(-m.window())
	if m.screen == screenProcs {
		return func() tea.Msg {
			ps, err := src.Processes(context.Background(), from, to)
			if err != nil {
				return errMsg{err}
			}
			return procsMsg{gen, ps}
		}
	}
	if m.screen == screenLoop {
		return func() tea.Msg {
			ts, err := src.ThreadStacks(context.Background(), proc, pid, from, to)
			if err != nil {
				return errMsg{err}
			}
			return loopMsg{gen, store.Loop(ts)}
		}
	}
	diff := m.diff && m.screen == screenTop
	prevFrom := from.Add(-m.window())
	return func() tea.Msg {
		ctx := context.Background()
		stacks, err := src.Stacks(ctx, proc, pid, from, to)
		if err != nil {
			return errMsg{err}
		}
		msg := stacksMsg{gen: gen, top: store.Top(stacks, 0), tree: store.Tree(stacks)}
		if diff {
			prev, err := src.Stacks(ctx, proc, pid, prevFrom, from)
			if err != nil {
				return errMsg{err}
			}
			msg.prev = map[string]store.FuncStat{}
			for _, f := range store.Top(prev, 0) {
				msg.prev[f.Name] = f
			}
			for _, s := range prev {
				msg.prevN += s.Weight
			}
		}
		return msg
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height

	case tickMsg:
		if m.paused {
			return m, tick()
		}
		m.asOf = m.end()
		return m, tea.Batch(m.load(), tick())

	case procsMsg:
		if msg.gen != m.gen {
			break
		}
		// Keep the cursor on the same row when the order changes.
		var sel procRow
		if rs := m.procRows(); m.pcur < len(rs) {
			sel = rs[m.pcur]
		}
		m.procs, m.err = msg.procs, nil
		rs := m.procRows()
		for i, r := range rs {
			if r.name == sel.name && r.pid == sel.pid {
				m.pcur = i
			}
		}
		m.pcur = clamp(m.pcur, len(rs))

	case stacksMsg:
		if msg.gen != m.gen {
			break
		}
		m.top, m.tree, m.prev, m.prevN, m.err = msg.top, msg.tree, msg.prev, msg.prevN, nil
		m.tcur = clamp(m.tcur, len(m.funcs()))
		if m.jump != "" {
			name := m.jump
			m.selectFirst(func(n string) bool { return n == name })
			m.jump = ""
		}

	case loopMsg:
		if msg.gen != m.gen {
			break
		}
		m.loops, m.err = msg.loops, nil

	case oldestMsg:
		m.oldest = msg.t

	case errMsg:
		m.err = msg.err

	case tea.KeyPressMsg:
		if m.help { // any key closes it
			m.help = false
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			return m, nil
		}
		if m.searching {
			return m.searchKey(msg)
		}
		return m.key(msg)
	}
	return m, nil
}

func (m model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch k := msg.String(); k {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "?":
		m.help = true
		return m, nil
	case "w":
		m.win = (m.win + 1) % len(windows)
		return m.reload()
	case "p", "space":
		m.paused = !m.paused
		m.at = m.asOf // freeze the window on screen; unpausing jumps back to now
		return m.reload()
	case "[", "]":
		dir := 1
		if k == "[" {
			dir = -1
		}
		at, live, ok := m.step(dir)
		if !ok {
			return m, nil
		}
		m.paused, m.at = !live, at
		return m.reload()
	case "tab":
		switch m.screen {
		case screenTop:
			m.screen = screenFlame
		case screenFlame, screenLoop:
			m.screen = screenTop
		}
		return m.reload()
	case "a":
		switch m.screen {
		case screenTop, screenFlame:
			if m.hasLoop() {
				m.screen, m.loops = screenLoop, nil
				return m.reload()
			}
		case screenLoop:
			m.screen = screenTop
			return m.reload()
		}
	}

	switch m.screen {
	case screenProcs:
		rows := m.procRows()
		if i, ok := m.move(msg.String(), m.pcur, len(rows)); ok {
			m.pcur = i
			return m, nil
		}
		switch msg.String() {
		case "/":
			m.searching, m.procFilter, m.pcur = true, "", 0
			return m, nil
		case "esc":
			m.procFilter = ""
			return m, nil
		}
		if m.pcur >= len(rows) {
			break
		}
		r := rows[m.pcur]
		switch msg.String() {
		case "right", "l":
			if r.pids > 1 {
				m.expanded[r.name] = true
			}
		case "left", "h":
			delete(m.expanded, r.name)
			if r.pid != 0 { // back up to the process row
				m.pcur = slices.IndexFunc(m.procRows(), func(p procRow) bool { return p.name == r.name })
			}
		case "enter":
			m.screen, m.proc, m.pid = screenTop, r.name, r.pid
			m.top, m.tree, m.tcur, m.funcFilter = nil, nil, 0, ""
			m.focus, m.zooms, m.sel, m.search = nil, nil, nil, ""
			return m.reload()
		}

	case screenTop:
		funcs := m.funcs()
		if i, ok := m.move(msg.String(), m.tcur, len(funcs)); ok {
			m.tcur = i
			return m, nil
		}
		switch msg.String() {
		case "/":
			m.searching, m.funcFilter, m.tcur = true, "", 0
		case "d":
			m.diff = !m.diff
			return m.reload()
		case "enter":
			if m.tcur < len(funcs) {
				// Highlight every occurrence: the table row adds up all of them.
				name := funcs[m.tcur].Name
				m.screen, m.search, m.exact, m.focus, m.zooms, m.jump = screenFlame, name, true, nil, nil, name
				return m.reload()
			}
		case "esc", "backspace":
			if m.funcFilter != "" {
				m.funcFilter = ""
				break
			}
			m.screen = screenProcs
			return m.reload()
		}

	case screenFlame:
		return m.flameKey(msg)

	case screenLoop:
		if k := msg.String(); k == "esc" || k == "backspace" {
			m.screen = screenTop
			return m.reload()
		}
	}
	return m, nil
}

// move applies a list key to cursor i over n rows: one row, a page, or an
// end. ok is false when k isn't one.
func (m model) move(k string, i, n int) (int, bool) {
	page := max(1, m.h-6)
	switch k {
	case "up", "k":
		i--
	case "down", "j":
		i++
	case "pgup", "ctrl+b":
		i -= page
	case "pgdown", "ctrl+f":
		i += page
	case "home", "g":
		i = 0
	case "end", "G":
		i = n - 1
	default:
		return i, false
	}
	return clamp(i, n), true
}

// searchKey edits the / query: the flame graph's search, or the process
// list's or functions table's filter, which narrow them as you type.
func (m model) searchKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	q, cur := &m.search, new(int)
	switch m.screen {
	case screenProcs:
		q, cur = &m.procFilter, &m.pcur
	case screenTop:
		q, cur = &m.funcFilter, &m.tcur
	}
	switch k := msg.String(); k {
	case "enter":
		m.searching = false
		if m.screen == screenFlame {
			m.selectFirst(m.highlighted)
		}
	case "esc":
		m.searching, *q = false, ""
	case "up", "down", "pgup", "pgdown":
		if m.screen != screenFlame {
			return m.key(msg)
		}
	case "backspace":
		if r := []rune(*q); len(r) > 0 {
			*q, *cur = string(r[:len(r)-1]), 0
		}
	default:
		*q += msg.Text
		*cur = 0 // the best match: lists are sorted by CPU
	}
	return m, nil
}

// procRow is a row of the process list: a process, or one of its PIDs under
// it when it's expanded.
type procRow struct {
	name   string
	pid    int64 // 0: the process, all its PIDs
	pids   int   // on a process row, how many PIDs it has
	weight int64
}

// procRows is the process list narrowed by the filter, with expanded
// processes' PIDs under them.
func (m model) procRows() []procRow {
	var rows []procRow
	for _, p := range m.procs {
		if !contains(p.Name, m.procFilter) {
			continue
		}
		rows = append(rows, procRow{p.Name, 0, len(p.PIDs), p.Weight})
		if m.expanded[p.Name] && len(p.PIDs) > 1 {
			for _, ps := range p.PIDs {
				rows = append(rows, procRow{p.Name, ps.PID, 0, ps.Weight})
			}
		}
	}
	return rows
}

// funcs is the functions table narrowed by the filter.
func (m model) funcs() []store.FuncStat {
	if m.funcFilter == "" {
		return m.top
	}
	var out []store.FuncStat
	for _, f := range m.top {
		if contains(f.Name, m.funcFilter) {
			out = append(out, f)
		}
	}
	return out
}

func contains(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// hasLoop: the process runs an asyncio event loop, judging by on-CPU samples.
func (m model) hasLoop() bool {
	for _, f := range m.top {
		if f.Name == store.LoopFrame {
			return true
		}
	}
	return false
}

func clamp(i, n int) int { return max(0, min(i, n-1)) }
