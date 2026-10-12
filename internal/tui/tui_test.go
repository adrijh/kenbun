package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/adrijh/kenbun/internal/store"
)

func TestProcsFilter(t *testing.T) {
	m := fixture(t, "profiles.jsonl")
	if len(m.procs) < 2 {
		t.Fatalf("fixture has %d processes, want several", len(m.procs))
	}
	m = press(m, "/", "P", "y", "t", "h")
	rows := m.procRows()
	for _, r := range rows {
		if !strings.Contains(strings.ToLower(r.name), "pyth") {
			t.Errorf("%q shown for filter %q", r.name, m.procFilter)
		}
	}
	if len(rows) == 0 || len(rows) == len(m.procs) {
		t.Fatalf("filter %q shows %d of %d processes", m.procFilter, len(rows), len(m.procs))
	}
	if !strings.Contains(m.View().Content, fmt.Sprintf("of %d processes", len(m.procs))) {
		t.Error("view lacks the filter line")
	}

	m = press(m, "enter") // keeps the filter, stops typing
	if m.searching || m.procFilter != "Pyth" {
		t.Fatalf("after enter: searching %v, filter %q", m.searching, m.procFilter)
	}
	m = press(m, "enter") // opens the selected match
	if m.screen != screenTop || !strings.Contains(m.proc, "python") {
		t.Fatalf("opened %q on screen %d", m.proc, m.screen)
	}

	m = press(m, "esc", "esc") // back to the list, then clear the filter
	if m.screen != screenProcs || m.procFilter != "" || len(m.procRows()) != len(m.procs) {
		t.Fatalf("after esc: screen %d, filter %q", m.screen, m.procFilter)
	}
}

func TestFuncsFilter(t *testing.T) {
	m := fixture(t, "profiles.jsonl").open(screenTop, "python3.14")
	m = press(m, "/", "f", "i", "b", "enter")
	funcs := m.funcs()
	if len(funcs) == 0 || len(funcs) == len(m.top) {
		t.Fatalf("filter fib shows %d of %d functions", len(funcs), len(m.top))
	}
	m = press(m, "enter") // the first match, in the flame graph
	if m.screen != screenFlame || !strings.Contains(m.search, "fib") {
		t.Fatalf("enter on a filtered row: screen %d, search %q", m.screen, m.search)
	}
	m = press(m, "esc", "esc") // flame → functions: clears the filter, then back
	if m.funcFilter != "" || m.screen != screenTop {
		t.Fatalf("esc kept filter %q on screen %d", m.funcFilter, m.screen)
	}
}

func TestPIDRows(t *testing.T) {
	m := model{expanded: map[string]bool{}, w: 100, h: 20, procs: []store.ProcStat{
		{Name: "web", Weight: 4, PIDs: []store.PIDStat{{PID: 2, Weight: 3}, {PID: 1, Weight: 1}}},
		{Name: "db", Weight: 2, PIDs: []store.PIDStat{{PID: 3, Weight: 2}}},
	}}
	if n := len(m.procRows()); n != 2 {
		t.Fatalf("collapsed: %d rows, want 2", n)
	}
	m = press(m, "right", "down")
	if r := m.procRows()[m.pcur]; r.name != "web" || r.pid != 2 {
		t.Fatalf("expanded, second row is %+v, want web pid 2", r)
	}
	if v := m.View().Content; !strings.Contains(v, "pid 1") || !strings.Contains(v, "2 pids") {
		t.Errorf("view lacks the PID rows:\n%s", v)
	}
	if next, _ := m.Update(key("enter")); next.(model).pid != 2 || next.(model).proc != "web" {
		t.Errorf("enter opened %q pid %d, want web pid 2", next.(model).proc, next.(model).pid)
	}
	m = press(m, "left") // collapses, back on the process row
	if len(m.procRows()) != 2 || m.pcur != 0 {
		t.Errorf("after left: %d rows, cursor %d", len(m.procRows()), m.pcur)
	}
	m = press(m, "G")
	if m.pcur != 1 {
		t.Errorf("G: cursor %d, want the last row", m.pcur)
	}
	m = press(m, "right") // db has one PID: nothing to expand
	if len(m.procRows()) != 2 {
		t.Errorf("expanded a single-PID process")
	}
}

func TestStep(t *testing.T) {
	end := time.Unix(10_000, 0)
	m := model{win: 0, paused: true, at: end, asOf: end, last: end, oldest: end.Add(-75 * time.Second)}
	m = press(m, "[")
	if want := end.Add(-30 * time.Second); !m.at.Equal(want) {
		t.Fatalf("[: window ends %v, want %v", m.at, want)
	}
	m = press(m, "[", "[", "[") // the third would hold nothing
	if want := end.Add(-60 * time.Second); !m.at.Equal(want) {
		t.Fatalf("[ past the data: window ends %v, want %v", m.at, want)
	}
	m = press(m, "]", "]", "]", "]")
	if !m.at.Equal(end) || !m.paused {
		t.Fatalf("] past a capture's end: %v, paused %v; want %v", m.at, m.paused, end)
	}

	live := model{win: 0, asOf: time.Now(), oldest: time.Now().Add(-time.Hour)}
	live = press(live, "[")
	if !live.paused || live.at.After(time.Now().Add(-29*time.Second)) {
		t.Fatalf("[ when live: paused %v at %v", live.paused, live.at)
	}
	live = press(live, "]", "]")
	if live.paused {
		t.Fatal("] up to now: still paused, want live")
	}
}

func TestCPU(t *testing.T) {
	end := time.Unix(10_000, 0)
	m := model{win: 1, asOf: end} // 1m, data for all of it
	if got := m.cpu(97 * 60); got != "1000m" {
		t.Errorf("a core for a minute = %s, want 1000m", got)
	}
	m.oldest = end.Add(-29500 * time.Millisecond) // kenbun started 30s ago
	if got := m.cpu(97 * 30); got != "1000m" {
		t.Errorf("a core for the 30s there is = %s, want 1000m", got)
	}
}

func TestHelp(t *testing.T) {
	m := press(model{w: 60, h: 20}, "?")
	if v := m.View().Content; !m.help || !strings.Contains(v, "first/last") || !strings.Contains(v, "earlier/later") {
		t.Fatalf("help: %v\n%s", m.help, v)
	}
	if m = press(m, "x"); m.help {
		t.Fatal("a key didn't close help")
	}
	if !strings.Contains(m.footer(), "help") {
		t.Errorf("narrow footer dropped ? help: %q", m.footer())
	}
}

func press(m model, keys ...string) model {
	for _, k := range keys {
		m = step(m, key(k))
	}
	return m
}

func key(k string) tea.KeyPressMsg {
	switch k {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	}
	return tea.KeyPressMsg{Code: []rune(k)[0], Text: k}
}

type fixable struct{ fixed *bool }

func (fixable) Error() string { return "no profiler" }
func (f fixable) Fix() (string, func(context.Context) error) {
	return "install it", func(context.Context) error { *f.fixed = true; return nil }
}

// An error the source can fix is offered on i, and fixed while the TUI says so.
func TestFix(t *testing.T) {
	fixed := false
	m := step(model{w: 100, h: 20}, errMsg{fmt.Errorf("pid 1: %w", fixable{&fixed})})
	if v := ansi.Strip(m.View().Content); !strings.Contains(v, "i install it") {
		t.Errorf("footer doesn't offer the fix:\n%s", v)
	}
	next, cmd := m.Update(key("i"))
	m = next.(model)
	if m.fixing != "install it" || cmd == nil || !strings.Contains(ansi.Strip(m.View().Content), "… install it") {
		t.Fatalf("after i: fixing %q", m.fixing)
	}
	msg := cmd()
	if !fixed {
		t.Fatal("fix didn't run")
	}
	if m = step(m, msg); m.fixing != "" || m.err != nil {
		t.Errorf("after the fix: fixing %q, err %v", m.fixing, m.err)
	}
}
