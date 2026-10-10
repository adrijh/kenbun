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
	samples, err := ingest.ReadJSONL("../ingest/testdata/profiles.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Ingest(ctx, samples); err != nil {
		t.Fatal(err)
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

	wantSt, _ := st.Stacks(ctx, "python3.14", from, to)
	gotSt, err := c.Stacks(ctx, "python3.14", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.Top(gotSt, 0), store.Top(wantSt, 0)) { // row order is unspecified
		t.Error("Stacks via client differ from store")
	}

	wantTS, _ := st.ThreadStacks(ctx, "python3.14", from, to)
	gotTS, err := c.ThreadStacks(ctx, "python3.14", from, to)
	if err != nil || len(gotTS) == 0 || len(gotTS) != len(wantTS) || !reflect.DeepEqual(store.Loop(gotTS), store.Loop(wantTS)) {
		t.Errorf("ThreadStacks via client: %d rows, %v; want %d rows", len(gotTS), err, len(wantTS))
	}
}
