package store

import (
	"testing"
	"time"
)

func TestLoop(t *testing.T) {
	// Leaf first, as the eBPF profiler reports a Python 3.14 asyncio app (C Task).
	loop := []string{"BaseEventLoop._run_once", "BaseEventLoop.run_forever", "asyncio.run", "<module>"}
	stack := func(frames ...string) []string { return append(frames, loop...) }
	cb := func(frames ...string) []string {
		return stack(append(frames, "[libpython3.14.so.1.0]", "Handle._run")...)
	}
	ms := func(n int64) int64 { return n * 1e6 }

	stacks := []ThreadStack{
		// time.sleep in a handler: off CPU inside a callback.
		{Thread: "python3", OffCPU: true, Weight: ms(40), Frames: cb("finish_task_switch.isra.0", "do_nanosleep", "[libc.so.6]", "[libpython3.14.so.1.0]", "slow_handler")},
		// CPU-heavy handler: 97 samples = 1s; a nested call still counts for the handler.
		{Thread: "python3", Weight: 97, Frames: cb("fib", "fib", "cpu_handler")},
		{Thread: "python3", OffCPU: true, Weight: ms(2), Frames: cb("finish_task_switch.isra.0", "cpu_handler")}, // preempted
		// Waiting in epoll: idle.
		{Thread: "python3", OffCPU: true, Weight: ms(100), Frames: stack("finish_task_switch.isra.0", "do_epoll_wait", "[libc.so.6]", "_PollLikeSelector.select")},
		// The loop's own overhead: in neither.
		{Thread: "python3", Weight: 1, Frames: stack("heappop")},
		// Not in the loop: ignored.
		{Thread: "worker", Weight: 50, Frames: []string{"work", "Thread.run"}},
		{Thread: "python3", OffCPU: true, Weight: ms(10), Frames: []string{"[libc.so.6]", "<module>"}},
	}

	got := Loop(stacks)
	if len(got) != 1 {
		t.Fatalf("Loop found %d loops, want 1: %+v", len(got), got)
	}
	l := got[0]
	sec := time.Second
	want := LoopStat{
		Thread: "python3",
		Wait:   200*time.Millisecond + 10*time.Millisecond, // ÷ OffCPUThreshold
		CPU:    sec,
		Idle:   500 * time.Millisecond,
		Total:  sec + 210*time.Millisecond + 500*time.Millisecond + sec/97,
	}
	if l.Thread != want.Thread || l.Wait != want.Wait || l.CPU != want.CPU || l.Idle != want.Idle || l.Total != want.Total {
		t.Errorf("Loop = %+v, want %+v", l, want)
	}
	wantCB := []CallbackStat{
		{Name: "cpu_handler", CPU: sec, Wait: 10 * time.Millisecond},
		{Name: "slow_handler", Wait: 200 * time.Millisecond},
	}
	if len(l.Callbacks) != len(wantCB) || l.Callbacks[0] != wantCB[0] || l.Callbacks[1] != wantCB[1] {
		t.Errorf("Callbacks = %+v, want %+v", l.Callbacks, wantCB)
	}
}

func TestCallback(t *testing.T) {
	for _, tc := range []struct {
		frames []string
		want   string
		ok     bool
	}{
		{[]string{"sleep_handler", "[libpython]", "Handle._run", "BaseEventLoop._run_once"}, "sleep_handler", true},
		// pure-Python Task: skip its frames.
		{[]string{"inner", "handler", "Task.__step_run_and_handle_result", "Task.__step", "Handle._run"}, "handler", true},
		// nested loop (asyncio.run inside a callback): the outer callback blocks the outer loop.
		{[]string{"x", "Handle._run", "BaseEventLoop._run_once", "runner", "Handle._run"}, "runner", true},
		{[]string{"[libpython]", "Handle._run"}, "Handle._run", true},
		{[]string{"_PollLikeSelector.select", "BaseEventLoop._run_once"}, "", false},
	} {
		i := callback(tc.frames)
		got := ""
		if i >= 0 {
			got = tc.frames[i]
		}
		if got != tc.want || (i >= 0) != tc.ok {
			t.Errorf("callback(%v) = %d (%q); want %q, %v", tc.frames, i, got, tc.want, tc.ok)
		}
	}
}
