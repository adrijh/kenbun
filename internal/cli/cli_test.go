package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/adrijh/kenbun/internal/ingest"
	"github.com/adrijh/kenbun/internal/store"
	"github.com/adrijh/kenbun/internal/store/sqlite"
)

// fixture loads a capture and queries all of it.
func fixture(t *testing.T, file string) (Query, *bytes.Buffer) {
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
	first, last := samples[0].TS, samples[0].TS
	for _, s := range samples {
		if s.TS.Before(first) {
			first = s.TS
		}
		if s.TS.After(last) {
			last = s.TS
		}
	}
	var out bytes.Buffer
	return Query{Src: st, Since: last.Sub(first) + time.Second, End: last.Add(time.Nanosecond), Limit: 5, Out: &out}, &out
}

func TestSince(t *testing.T) {
	for d, want := range map[time.Duration]string{5 * time.Minute: "5m", 90 * time.Second: "1m30s", time.Hour: "1h", 30 * time.Second: "30s", 1500 * time.Millisecond: "2s"} {
		if got := since(d); got != want {
			t.Errorf("since(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestCommands(t *testing.T) {
	ctx := context.Background()
	q, out := fixture(t, "profiles.jsonl")
	run := func(name string, f func() error) string {
		t.Helper()
		out.Reset()
		if err := f(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Logf("%s:\n%s", name, out)
		return out.String()
	}

	s := run("top", func() error { return Top(ctx, q) })
	if !hasRow(s, "74.1%", "python3.14") || !hasRow(s, "3", "chrome") {
		t.Errorf("top lacks python3.14 at 74.1%%, or chrome's 3 PIDs")
	}
	if s := run("functions --pid", func() error {
		q := q
		q.PID = 2736
		return Functions(ctx, q, "nvim")
	}); !strings.HasPrefix(s, "nvim pid 2736 ·") || !strings.Contains(s, "8 samples") {
		t.Errorf("functions --pid isn't nvim's PID 2736 alone (8 of 11 samples)")
	}
	if s := run("functions", func() error { return Functions(ctx, q, "python3.14") }); !strings.Contains(s, "37.8%  fib (/app/src/myapp/main.py:18)") {
		t.Errorf("functions lacks fib at 37.8%% total, at main.py:18")
	}
	// The capture is shorter than two windows: diff compares its halves.
	if s := run("diff", func() error { return Diff(ctx, q, "python3.14") }); !strings.Contains(s, "(all there is) vs the 6s before") ||
		!strings.Contains(s, "ΔSELF") {
		t.Errorf("diff on a short capture should split it in halves")
	}

	// An hour of --since on 13s of data averages CPU over the 13s.
	long := q
	long.Since = time.Hour
	want := fmt.Sprintf("last 13s (all there is) · %dm CPU", store.Millicores(887, 13*time.Second))
	if s := run("top --since 1h", func() error { return Top(ctx, long) }); !strings.Contains(s, want) {
		t.Errorf("top should fit the window to the data")
	}

	q, out = fixture(t, "asyncio.jsonl.gz")
	if s := run("loop", func() error { return Loop(ctx, q, "python3.14") }); !strings.Contains(s, "wait  sleepy_handler (") || !strings.Contains(s, "asyncio_demo.py:30)") ||
		!strings.Contains(s, "cpu  cpu_handler") {
		t.Errorf("loop: want sleepy_handler waiting and cpu_handler on CPU")
	}

	half := q
	half.Since /= 2
	if s := run("diff", func() error { return Diff(ctx, half, "python3.14") }); !strings.Contains(s, "ΔSELF") {
		t.Errorf("diff of two halves of a capture should list changes")
	}

	q.JSON = true
	run("loop --json", func() error { return Loop(ctx, q, "python3.14") })
	var got struct {
		Loops []struct {
			Blocked   float64
			Callbacks []struct{ Callback, Kind string }
		}
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got.Loops) != 1 || got.Loops[0].Callbacks[0].Kind != "wait" {
		t.Errorf("loop --json = %+v, %v", got, err)
	}
}

// hasRow: a line of s holds these columns, next to each other.
func hasRow(s string, cols ...string) bool {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(" "+strings.Join(strings.Fields(line), " ")+" ", " "+strings.Join(cols, " ")+" ") {
			return true
		}
	}
	return false
}
