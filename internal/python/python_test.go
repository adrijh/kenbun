package python

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/adrijh/kenbun/internal/store"
)

func TestParse(t *testing.T) {
	ts := time.Unix(100, 0)
	pyspy := "<module> (app.py:20);handler (app.py:16);<genexpr> (app.py:7) 13\n" +
		"_bootstrap (threading.py:1044);bg (app.py:12) 2\n" +
		"garbage\n"
	want := []store.Sample{
		{TS: ts, Process: "app.py", PID: 7, Frames: []string{"<genexpr>", "handler", "<module>"}, Locs: []string{"app.py:7", "app.py:16", "app.py:20"}, Weight: 13},
		{TS: ts, Process: "app.py", PID: 7, Frames: []string{"bg", "_bootstrap"}, Locs: []string{"app.py:12", "threading.py:1044"}, Weight: 2},
	}
	if got := parse(pyspy, "app.py", 7, ts, pySpyFrame); !reflect.DeepEqual(got, want) {
		t.Errorf("py-spy:\n%+v\nwant\n%+v", got, want)
	}

	tachyon := "tid:42;app.py:<module>:20;base_events.py:BaseEventLoop._run_once:2071;app.py:Work.crunch.<locals>.<genexpr>:7 9\n"
	want = []store.Sample{{TS: ts, Process: "app.py", PID: 7, Thread: "42",
		Frames: []string{"Work.crunch.<locals>.<genexpr>", "BaseEventLoop._run_once", "<module>"},
		Locs:   []string{"app.py:7", "base_events.py:2071", "app.py:20"}, Weight: 9}}
	if got := parse(tachyon, "app.py", 7, ts, tachyonFrame); !reflect.DeepEqual(got, want) {
		t.Errorf("tachyon:\n%+v\nwant\n%+v", got, want)
	}
}

// kenbun runs profilers as root: not ones someone else could swap.
func TestTrusted(t *testing.T) {
	f := filepath.Join(t.TempDir(), "py-spy")
	if err := os.WriteFile(f, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := trusted(f); err != nil {
		t.Errorf("own 0755 file: %v", err)
	}
	if err := os.Chmod(f, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := trusted(f); err == nil {
		t.Error("world-writable file is trusted")
	}
}

func TestName(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"/usr/bin/python3.14", "app.py"}, "app.py"},
		{[]string{"/app/.venv/bin/python3", "/app/.venv/bin/uvicorn", "main:app"}, "uvicorn"},
		{[]string{"python3", "-u", "-X", "dev", "-m", "http.server"}, "http.server"},
		{[]string{"python3", "-c", "import time"}, "python -c"},
		{[]string{"python3"}, "python"},
		{[]string{"gunicorn: worker [app:app]"}, "gunicorn"},
	} {
		if got := name(tc.args); got != tc.want {
			t.Errorf("name(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestIsPython(t *testing.T) {
	for path, want := range map[string]bool{
		"/usr/bin/python3.14": true,
		"python3.13t":         true,
		"python":              true,
		"/opt/homebrew/Cellar/python@3.12/3.12.4/Frameworks/Python.framework/Versions/3.12/Resources/Python.app/Contents/MacOS/Python": true,
		"/usr/bin/python-argcomplete-check-easy-install-script":                                                                        false,
		"/usr/bin/bash": false,
	} {
		if got := isPython(path); got != want {
			t.Errorf("isPython(%q) = %v", path, got)
		}
	}
}

// The CPU column is what each process used inside the window, from the
// counters at its ends.
func TestProcesses(t *testing.T) {
	t0 := time.Unix(1000, 0)
	snaps := []snapshot{
		{t0, map[int64]proc{1: {"api", 10 * time.Second}, 2: {"api", time.Second}}},
		{t0.Add(10 * time.Second), map[int64]proc{1: {"api", 12 * time.Second}, 2: {"api", 6 * time.Second}}},
		{t0.Add(20 * time.Second), map[int64]proc{1: {"api", 13 * time.Second}, 3: {"worker", time.Second}}}, // 2 exited, 3 started
	}
	s := func(d time.Duration) int64 { return int64(d.Seconds() * store.SamplesPerSecond) }

	got := processes(snaps, t0.Add(5*time.Second), t0.Add(25*time.Second))
	want := []store.ProcStat{
		{Name: "api", Weight: s(3 * time.Second), PIDs: []store.PIDStat{{PID: 1, Weight: s(3 * time.Second)}}},
		{Name: "worker", Weight: s(time.Second), PIDs: []store.PIDStat{{PID: 3, Weight: s(time.Second)}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("processes =\n%+v\nwant\n%+v", got, want)
	}

	// A window starting before the first snapshot counts from it; one ending
	// before it has nothing.
	got = processes(snaps, t0.Add(-time.Minute), t0.Add(15*time.Second))
	if len(got) != 1 || got[0].Weight != s(7*time.Second) || len(got[0].PIDs) != 2 {
		t.Errorf("processes from before the first snapshot = %+v", got)
	}
	if got := processes(snaps, t0.Add(-time.Minute), t0); got != nil {
		t.Errorf("processes before the first snapshot = %+v", got)
	}
}
