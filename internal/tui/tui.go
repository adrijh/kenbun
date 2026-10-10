// Package tui is kenbun's terminal UI: process list → top functions ⇄
// flamegraph (or the asyncio loop view), over a sliding time window that can
// be paused and diffed.
// It polls its Source every refresh interval.
package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/adrijh/kenbun/internal/store"
)

// Source is what the TUI reads: *store.Store locally, *api.Client remotely.
type Source interface {
	Processes(ctx context.Context, from, to time.Time) ([]store.ProcStat, error)
	Stacks(ctx context.Context, process string, from, to time.Time) ([]store.StackWeight, error)
	ThreadStacks(ctx context.Context, process string, from, to time.Time) ([]store.ThreadStack, error)
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
	clockMsg struct{} // redraws the header clock
	procsMsg struct {
		gen   int
		procs []store.ProcStat
	}
	stacksMsg struct {
		gen   int
		loops []store.LoopStat

		top   []store.FuncStat
		tree  *store.Node
		prev  map[string]store.FuncStat // previous window, only in diff mode
		prevN int64
	}
	loopMsg struct {
		gen   int
		loops []store.LoopStat
	}
	errMsg struct{ err error }
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
	diff   bool

	screen screen
	procs  []store.ProcStat
	pcur   int
	proc   string

	loops []store.LoopStat

	top   []store.FuncStat
	tree  *store.Node
	prev  map[string]store.FuncStat
	prevN int64
	tcur  int

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
	m := model{src: src, win: 1, otlp: o.OTLP}
	for o.Window > 0 && m.win < len(windows)-1 && windows[m.win] < o.Window {
		m.win++
	}
	if !o.At.IsZero() {
		m.paused, m.at = true, o.At
	}
	return m
}

func (m model) Init() tea.Cmd { return tea.Batch(m.load(), tick(), clock()) }

func tick() tea.Cmd {
	return tea.Tick(refresh, func(time.Time) tea.Msg { return tickMsg{} })
}

// clock fires on each wall-clock second.
func clock() tea.Cmd {
	return tea.Every(time.Second, func(time.Time) tea.Msg { return clockMsg{} })
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
	return m, m.load()
}

// load queries the source for the current screen off the update loop.
func (m model) load() tea.Cmd {
	src, gen, proc := m.src, m.gen, m.proc
	to := m.end()
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
			ts, err := src.ThreadStacks(context.Background(), proc, from, to)
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
		stacks, err := src.Stacks(ctx, proc, from, to)
		if err != nil {
			return errMsg{err}
		}
		msg := stacksMsg{gen: gen, top: store.Top(stacks, 0), tree: store.Tree(stacks)}
		if diff {
			prev, err := src.Stacks(ctx, proc, prevFrom, from)
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

	case clockMsg:
		return m, clock()

	case tickMsg:
		if m.paused {
			return m, tick()
		}
		return m, tea.Batch(m.load(), tick())

	case procsMsg:
		if msg.gen != m.gen {
			break
		}
		// Keep the cursor on the same process when the order changes.
		sel := ""
		if m.pcur < len(m.procs) {
			sel = m.procs[m.pcur].Name
		}
		m.procs, m.err = msg.procs, nil
		for i, p := range m.procs {
			if p.Name == sel {
				m.pcur = i
			}
		}
		m.pcur = clamp(m.pcur, len(m.procs))

	case stacksMsg:
		if msg.gen != m.gen {
			break
		}
		m.top, m.tree, m.prev, m.prevN, m.err = msg.top, msg.tree, msg.prev, msg.prevN, nil
		m.tcur = clamp(m.tcur, len(m.top))
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

	case errMsg:
		m.err = msg.err

	case tea.KeyPressMsg:
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
	case "w":
		m.win = (m.win + 1) % len(windows)
		return m.reload()
	case "p", "space":
		m.paused = !m.paused
		m.at = time.Now()
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
		switch msg.String() {
		case "up", "k":
			m.pcur = clamp(m.pcur-1, len(m.procs))
		case "down", "j":
			m.pcur = clamp(m.pcur+1, len(m.procs))
		case "enter":
			if m.pcur < len(m.procs) {
				m.screen, m.proc = screenTop, m.procs[m.pcur].Name
				m.top, m.tree, m.tcur = nil, nil, 0
				m.focus, m.zooms, m.sel, m.search = nil, nil, nil, ""
				return m.reload()
			}
		}

	case screenTop:
		switch msg.String() {
		case "up", "k":
			m.tcur = clamp(m.tcur-1, len(m.top))
		case "down", "j":
			m.tcur = clamp(m.tcur+1, len(m.top))
		case "d":
			m.diff = !m.diff
			return m.reload()
		case "enter":
			if m.tcur < len(m.top) {
				// Highlight every occurrence: the table row adds up all of them.
				name := m.top[m.tcur].Name
				m.screen, m.search, m.exact, m.focus, m.zooms, m.jump = screenFlame, name, true, nil, nil, name
				return m.reload()
			}
		case "esc", "backspace":
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

func (m model) searchKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.searching = false
		m.selectFirst(m.highlighted)
	case "esc":
		m.searching, m.search = false, ""
	case "backspace":
		if r := []rune(m.search); len(r) > 0 {
			m.search = string(r[:len(r)-1])
		}
	default:
		m.search += msg.Text
	}
	return m, nil
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
