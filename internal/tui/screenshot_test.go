package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/adrijh/kenbun/internal/ingest"
	"github.com/adrijh/kenbun/internal/store/sqlite"
)

// TestScreenshots writes the real View of each screen, ANSI and all, for the
// README. `make screenshots` runs it and turns the files into images.
func TestScreenshots(t *testing.T) {
	dir := os.Getenv("KENBUN_SCREENSHOTS")
	if dir == "" {
		t.Skip("set KENBUN_SCREENSHOTS to an output directory")
	}
	shot := func(name string, m model, rows int) {
		t.Helper()
		m = step(m, tea.WindowSizeMsg{Width: 110, Height: rows})
		if err := os.WriteFile(filepath.Join(dir, name+".ansi"), []byte(m.View().Content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	m := fixture(t, "profiles.jsonl")
	shot("procs", m, 15)
	m = m.open(screenTop, "python3.14")
	shot("functions", m, 18)
	m.search, m.exact, m.jump = "fib", true, "fib" // enter on fib: highlighted in the flame graph
	shot("flame", m.open(screenFlame, "python3.14"), 28)
	shot("loop", fixture(t, "asyncio.jsonl.gz").open(screenLoop, "python3.14"), 9)
}

// fixture is the TUI as `kenbun view` opens a capture, shown live.
func fixture(t *testing.T, file string) model {
	t.Helper()
	samples, err := ingest.ReadJSONL("../ingest/testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Ingest(context.Background(), samples); err != nil {
		t.Fatal(err)
	}
	last := samples[0].TS
	for _, s := range samples {
		last = maxTime(last, s.TS)
	}
	m := New(st, Options{At: last.Add(time.Nanosecond), Window: 5 * time.Minute, OTLP: "localhost:4317"}).(model)
	m = settle(m, m.load())
	m.paused = false // a capture opens paused; screenshots show the live header
	return m
}

func (m model) open(s screen, proc string) model {
	m.screen, m.proc = s, proc
	return settle(m, m.load())
}

// settle runs cmd and its batched commands, updating m with each result.
func settle(m model, cmd tea.Cmd) model {
	if cmd == nil {
		return m
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			m = settle(m, c)
		}
		return m
	}
	return step(m, msg)
}

func step(m model, msg tea.Msg) model {
	next, _ := m.Update(msg)
	return next.(model)
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
