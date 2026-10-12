package api

import (
	"context"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/adrijh/kenbun/internal/ingest"
	"github.com/adrijh/kenbun/internal/store"
	"github.com/adrijh/kenbun/internal/store/sqlite"
)

// The client must return exactly what the store returns.
func TestClientMatchesStore(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, f := range []string{"profiles.jsonl", "asyncio.jsonl.gz"} {
		samples, err := ingest.ReadJSONL("../ingest/testdata/" + f)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Ingest(ctx, samples); err != nil {
			t.Fatal(err)
		}
	}

	srv := httptest.NewServer(Handler(st))
	defer srv.Close()
	c := NewClient(srv.URL)
	from, to := time.Time{}, time.Now().Add(time.Hour)

	want, _ := st.Processes(ctx, from, to)
	got, err := c.Processes(ctx, from, to)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Processes = %v, %v; want %v", got, err, want)
	}

	wantSt, _ := st.Stacks(ctx, "python3.14", 0, from, to)
	gotSt, err := c.Stacks(ctx, "python3.14", 0, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.Top(gotSt, 0), store.Top(wantSt, 0)) { // row order is unspecified
		t.Error("Stacks via client differ from store")
	}

	wantOld, _ := st.Oldest(ctx)
	if gotOld, err := c.Oldest(ctx); err != nil || !gotOld.Equal(wantOld) || gotOld.IsZero() {
		t.Errorf("Oldest = %v, %v; want %v", gotOld, err, wantOld)
	}

	wantTS, _ := st.ThreadStacks(ctx, "python3.14", 0, from, to)
	gotTS, err := c.ThreadStacks(ctx, "python3.14", 0, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if want := store.Loop(wantTS); len(want) == 0 || !reflect.DeepEqual(store.Loop(gotTS), want) {
		t.Error("ThreadStacks via client differ from store")
	}
}
