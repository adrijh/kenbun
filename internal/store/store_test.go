package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite" // a test-only driver: package store itself imports none
)

func TestPruneStacks(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	s, err := New(db, Dialect{Schema: BaseSchema})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	t0 := time.Unix(1000, 0)
	old := Sample{TS: t0, Process: "p", Frames: []string{"old"}, Weight: 1}
	kept := Sample{TS: t0.Add(time.Minute), Process: "p", Frames: []string{"kept"}, Weight: 1}
	if err := s.Ingest(ctx, []Sample{old, kept}); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(ctx, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM stacks`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("stacks after prune = %d, %v; want 1 (only kept's)", n, err)
	}

	// A pruned stack coming back must be stored again, not assumed present.
	old.TS = t0.Add(2 * time.Minute)
	if err := s.Ingest(ctx, []Sample{old}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Stacks(ctx, "p", old.TS, old.TS.Add(time.Second))
	if err != nil || len(got) != 1 || got[0].Frames[0] != "old" {
		t.Errorf("Stacks after re-ingest = %+v, %v; want the old stack", got, err)
	}
}
