package tui

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/adrijh/kenbun/internal/store"
)

// Rounded cell edges must never overlap neighbours or overflow the screen.
// Wide, shallow trees with many siblings make float edges drift across .5
// boundaries; this seed panicked at width 141 (iteration 280) before the fix.
func TestFlameLayoutFits(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 9))
	for i := range 400 {
		checkFlameFits(t, i, randomStacks(r), 141)
	}
	for i := range 100 {
		checkFlameFits(t, i, randomStacks(r), 20+r.IntN(200))
	}
}

func randomStacks(r *rand.Rand) []store.StackWeight {
	var stacks []store.StackWeight
	for range 1 + r.IntN(2000) {
		frames := make([]string, 1+r.IntN(12))
		for j := range frames {
			frames[j] = fmt.Sprintf("f%d", r.IntN(40))
		}
		stacks = append(stacks, store.StackWeight{Frames: frames, Weight: 1 + r.Int64N(3)})
	}
	return stacks
}

func checkFlameFits(t *testing.T, i int, stacks []store.StackWeight, width int) {
	t.Helper()
	m := model{w: width, h: 72, tree: store.Tree(stacks)}
	var b strings.Builder
	m.viewFlame(&b, 68)
	for _, line := range strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")[1:] { // skip status
		if w := lipgloss.Width(line); w > width {
			t.Fatalf("run %d, width %d: line is %d wide", i, width, w)
		}
	}
}

func TestLabel(t *testing.T) {
	for in, want := range map[string]string{
		"github.com/k3s-io/k3s/pkg/server.(*Server).Start": "server.(*Server).Start",
		"runtime.mcall":      "runtime.mcall",
		"[libc.so.6]":        "[libc.so.6]",
		"<module>":           "<module>",
		"path/filepath.Walk": "filepath.Walk",
	} {
		if got := label(in); got != want {
			t.Errorf("label(%q) = %q, want %q", in, got, want)
		}
	}
}
