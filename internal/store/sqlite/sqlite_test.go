package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/adrijh/kenbun/internal/ingest"
	"github.com/adrijh/kenbun/internal/store"
)

// Fixture → Convert → SQLite → Top, with the numbers from inspect_profiles.py.
func TestFixtureEndToEnd(t *testing.T) {
	ctx := context.Background()
	st, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	samples, err := ingest.ReadJSONL("../../ingest/testdata/profiles.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Ingest(ctx, samples); err != nil {
		t.Fatal(err)
	}

	procs, err := st.Processes(ctx, time.Time{}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(procs) == 0 || procs[0].Name != "python3.14" || procs[0].Weight != 657 {
		t.Fatalf("Processes()[0] = %+v, want python3.14/657", procs)
	}

	stacks, err := st.Stacks(ctx, "python3.14", 0, time.Time{}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, fs := range store.Top(stacks, 0) {
		if fs.Name == "fib" {
			if pct := 100 * float64(fs.Total) / 657; pct < 37 || pct > 38.5 {
				t.Errorf("fib total = %.1f%%, want ~37.7%%", pct)
			}
			return
		}
	}
	t.Error("fib not in Top")
}

// testdata/asyncio.jsonl.gz: asyncio_demo.py under `kenbun --profile`, where
// per second sleepy_handler blocks the loop 0.2s in time.sleep and cpu_handler
// ~0.08s computing. Off-CPU is sampled, so waiting is the noisier estimate.
func TestAsyncioLoopFixture(t *testing.T) {
	ctx := context.Background()
	st, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	samples, err := ingest.ReadJSONL("../../ingest/testdata/asyncio.jsonl.gz")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Ingest(ctx, samples); err != nil {
		t.Fatal(err)
	}

	ts, err := st.ThreadStacks(ctx, "python3.14", 0, time.Time{}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	loops := store.Loop(ts)
	if len(loops) != 1 {
		t.Fatalf("got %d loops, want 1: %+v", len(loops), loops)
	}
	l := loops[0]
	share := func(d time.Duration) float64 { return 100 * float64(d) / float64(l.Total) }
	if p := share(l.Idle); p < 65 || p > 80 {
		t.Errorf("idle = %.1f%%, want ~72%%", p)
	}
	byName := map[string]store.CallbackStat{}
	for _, c := range l.Callbacks {
		byName[c.Name] = c
	}
	if c := byName["sleepy_handler"]; share(c.Wait) < 12 || share(c.Wait) > 28 || c.CPU > c.Wait/10 {
		t.Errorf("sleepy_handler = %+v (wait %.1f%%), want ~20%% waiting", c, share(c.Wait))
	}
	if c := byName["cpu_handler"]; share(c.CPU) < 6 || share(c.CPU) > 11 || c.Wait > c.CPU/10 {
		t.Errorf("cpu_handler = %+v (cpu %.1f%%), want ~8%% on CPU", c, share(c.CPU))
	}

	// Off-CPU must stay out of the on-CPU views.
	stacks, err := st.Stacks(ctx, "python3.14", 0, time.Time{}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range store.Top(stacks, 0) {
		if f.Name == "sleepy_handler" {
			t.Errorf("sleepy_handler in on-CPU Top: %+v", f)
		}
	}
}
