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
	if len(procs) == 0 || procs[0] != (store.ProcStat{Name: "python3.14", Weight: 657}) {
		t.Fatalf("Processes()[0] = %+v, want python3.14/657", procs)
	}

	stacks, err := st.Stacks(ctx, "python3.14", time.Time{}, time.Now().Add(time.Hour))
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
