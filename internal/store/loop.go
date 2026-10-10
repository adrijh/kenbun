package store

import (
	"slices"
	"sort"
	"strings"
	"time"
)

// The eBPF profiler settings kenbun runs with (main.go, k8s/profiler.yaml).
// Loop needs them to put on-CPU sample counts and sampled off-CPU nanoseconds
// on one time scale.
const (
	SamplesPerSecond = 97  // prime: avoids lockstep with periodic work
	OffCPUThreshold  = 0.2 // share of context switches the off-CPU probe records
)

// LoopFrame runs every iteration of an asyncio event loop.
const LoopFrame = "BaseEventLoop._run_once"

// LoopStat is one thread's asyncio event loop. Loop time is everything sampled
// inside BaseEventLoop._run_once: running callbacks (CPU or Wait, which block the
// loop), waiting in the selector (Idle), and the loop's own overhead.
type LoopStat struct {
	Thread    string
	Total     time.Duration
	CPU       time.Duration // callbacks on CPU
	Wait      time.Duration // callbacks off CPU: sleep, blocking I/O, locks
	Idle      time.Duration // off CPU in the selector: nothing to do
	Callbacks []CallbackStat
}

// CallbackStat is the time one callback (usually a task's coroutine) held the loop.
type CallbackStat struct {
	Name      string
	CPU, Wait time.Duration
}

func (l LoopStat) Blocked() time.Duration     { return l.CPU + l.Wait }
func (c CallbackStat) Blocked() time.Duration { return c.CPU + c.Wait }

// Frames between Handle._run and the callback. The C Task (the default) leaves
// only native frames; these are the pure-Python Task's.
var asyncioInternal = map[string]bool{
	"Task.__step":                       true,
	"Task.__step_run_and_handle_result": true,
	"Task.__wakeup":                     true,
}

// Loop finds the threads running an asyncio event loop and splits their time
// into blocked (per callback, CPU vs waiting) and idle. Busiest loop first.
func Loop(stacks []ThreadStack) []LoopStat {
	loops := map[string]*LoopStat{}
	cbs := map[string]map[string]*CallbackStat{}
	for _, s := range stacks {
		if !slices.Contains(s.Frames, LoopFrame) {
			continue
		}
		l := loops[s.Thread]
		if l == nil {
			l = &LoopStat{Thread: s.Thread}
			loops[s.Thread], cbs[s.Thread] = l, map[string]*CallbackStat{}
		}
		d := s.duration()
		l.Total += d

		name, ok := callback(s.Frames)
		switch {
		case ok:
			c := cbs[s.Thread][name]
			if c == nil {
				c = &CallbackStat{Name: name}
				cbs[s.Thread][name] = c
			}
			if s.OffCPU {
				l.Wait, c.Wait = l.Wait+d, c.Wait+d
			} else {
				l.CPU, c.CPU = l.CPU+d, c.CPU+d
			}
		case s.OffCPU && slices.ContainsFunc(s.Frames, isSelect):
			l.Idle += d
		}
	}

	out := make([]LoopStat, 0, len(loops))
	for t, l := range loops {
		for _, c := range cbs[t] {
			l.Callbacks = append(l.Callbacks, *c)
		}
		sort.Slice(l.Callbacks, func(i, j int) bool {
			a, b := l.Callbacks[i], l.Callbacks[j]
			if a.Blocked() != b.Blocked() {
				return a.Blocked() > b.Blocked()
			}
			return a.Name < b.Name
		})
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Blocked() != out[j].Blocked() {
			return out[i].Blocked() > out[j].Blocked()
		}
		return out[i].Thread < out[j].Thread
	})
	return out
}

// callback names the callback a stack runs: the first frame below the
// outermost Handle._run that isn't native or asyncio plumbing.
func callback(frames []string) (string, bool) {
	i := len(frames) - 1
	for i >= 0 && frames[i] != "Handle._run" {
		i--
	}
	if i < 0 {
		return "", false
	}
	for j := i - 1; j >= 0; j-- {
		if f := frames[j]; !strings.HasPrefix(f, "[") && !asyncioInternal[f] {
			return f, true
		}
	}
	return "Handle._run", true // a native callback (e.g. Future.set_result)
}

// isSelect matches selectors' select (_PollLikeSelector.select for epoll,
// KqueueSelector.select, SelectSelector.select).
func isSelect(f string) bool { return strings.HasSuffix(f, "Selector.select") }

func (s ThreadStack) duration() time.Duration {
	if s.OffCPU {
		return time.Duration(float64(s.Weight) / OffCPUThreshold)
	}
	return time.Duration(s.Weight) * time.Second / SamplesPerSecond
}
