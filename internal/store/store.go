// Package store keeps profiling samples in a SQL database through database/sql.
//
// It never imports a driver: a backend package (store/sqlite today, store/duckdb
// later if needed) opens the *sql.DB and passes it to New with its Dialect. main
// imports only the backend it uses, so the default build stays pure Go.
//
// The SQL is deliberately minimal and portable (runs unchanged on DuckDB and
// SQLite): the database filters by process and time and sums weight per stack.
// Everything else (self/total, flamegraph trees) is computed in Go.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"
)

// Sample is kenbun's own representation of a profiling sample.
// No pdata types live past the ingest layer.
type Sample struct {
	TS      time.Time
	Process string // grouping key: service.name or process.executable.name
	PID     int64
	Frames  []string // leaf first
	Weight  int64
}

// ProcStat is one row of the process list.
type ProcStat struct {
	Name   string `json:"name"`
	Weight int64  `json:"weight"`
}

// StackWeight is the aggregated weight of one distinct stack.
type StackWeight struct {
	Frames []string `json:"frames"` // leaf first
	Weight int64    `json:"weight"`
}

// Dialect holds what differs between backends.
type Dialect struct {
	Schema      []string     // CREATE statements, run in order on New
	Maintenance []string     // run on one connection after each Prune (e.g. refresh planner stats)
	OnClose     func() error // extra cleanup after the *sql.DB is closed (e.g. a DuckDB connector)
}

// sep joins frames into one string. It can't appear in symbol names.
const sep = "\x1f"

// batchRows caps rows per multi-row INSERT (SQLite allows 32766 bound parameters).
const batchRows = 500

// BaseSchema works on both DuckDB and SQLite. Backends append their own indexes.
var BaseSchema = []string{
	`CREATE TABLE IF NOT EXISTS stacks (
		stack_id BIGINT PRIMARY KEY,
		folded   VARCHAR NOT NULL -- frames joined by \x1f, leaf first
	)`,
	`CREATE TABLE IF NOT EXISTS samples (
		ts       BIGINT NOT NULL, -- unix nanoseconds: no timezone handling differences between backends
		process  VARCHAR NOT NULL,
		pid      BIGINT,
		stack_id BIGINT NOT NULL,
		weight   BIGINT NOT NULL
	)`,
}

type Store struct {
	db          *sql.DB
	maintenance []string
	onClose     func() error

	mu   sync.Mutex // serializes Ingest (SQLite allows one writer anyway)
	seen map[int64]struct{}
}

// New creates the schema and returns a ready store. It takes ownership of db.
func New(db *sql.DB, d Dialect) (*Store, error) {
	s := &Store{db: db, maintenance: d.Maintenance, onClose: d.OnClose, seen: map[int64]struct{}{}}
	for _, stmt := range d.Schema {
		if _, err := db.Exec(stmt); err != nil {
			s.Close()
			return nil, fmt.Errorf("create schema: %w", err)
		}
	}
	if err := s.loadSeen(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	err := s.db.Close()
	if s.onClose != nil {
		err = errors.Join(err, s.onClose())
	}
	return err
}

// Ingest writes one batch in a single transaction: new stacks, then samples.
func (s *Store) Ingest(ctx context.Context, samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]int64, len(samples))
	newStacks := map[int64]string{}
	for i, smp := range samples {
		folded := strings.Join(smp.Frames, sep)
		id := stackID(folded)
		ids[i] = id
		if _, ok := s.seen[id]; !ok {
			newStacks[id] = folded
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit

	stackRows := make([][]any, 0, len(newStacks))
	for id, folded := range newStacks {
		stackRows = append(stackRows, []any{id, folded})
	}
	if err := insertRows(ctx, tx, "INSERT OR IGNORE INTO stacks (stack_id, folded) VALUES ", stackRows); err != nil {
		return fmt.Errorf("insert stacks: %w", err)
	}

	sampleRows := make([][]any, len(samples))
	for i, smp := range samples {
		sampleRows[i] = []any{smp.TS.UnixNano(), smp.Process, smp.PID, ids[i], smp.Weight}
	}
	if err := insertRows(ctx, tx, "INSERT INTO samples (ts, process, pid, stack_id, weight) VALUES ", sampleRows); err != nil {
		return fmt.Errorf("insert samples: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	for id := range newStacks { // only after the commit succeeded
		s.seen[id] = struct{}{}
	}
	return nil
}

// insertRows runs multi-row INSERTs in chunks. Portable across backends and far
// faster than one statement per row (especially on DuckDB).
func insertRows(ctx context.Context, tx *sql.Tx, prefix string, rows [][]any) error {
	for start := 0; start < len(rows); start += batchRows {
		chunk := rows[start:min(start+batchRows, len(rows))]

		var b strings.Builder
		b.WriteString(prefix)
		placeholder := "(" + strings.TrimSuffix(strings.Repeat("?,", len(chunk[0])), ",") + ")"
		args := make([]any, 0, len(chunk)*len(chunk[0]))
		for i, row := range chunk {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(placeholder)
			args = append(args, row...)
		}
		if _, err := tx.ExecContext(ctx, b.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) loadSeen() error {
	rows, err := s.db.Query(`SELECT stack_id FROM stacks`)
	if err != nil {
		return fmt.Errorf("load stacks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		s.seen[id] = struct{}{}
	}
	return rows.Err()
}

// stackID must be stable across runs (stacks persist in a file DB),
// so no maphash/random seeds.
func stackID(folded string) int64 {
	h := fnv.New64a()
	h.Write([]byte(folded))
	return int64(h.Sum64())
}

// Processes returns processes by total weight in [from, to).
func (s *Store) Processes(ctx context.Context, from, to time.Time) ([]ProcStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT process, sum(weight) AS w
		FROM samples
		WHERE ts >= ? AND ts < ?
		GROUP BY process
		ORDER BY w DESC`, from.UnixNano(), to.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ProcStat
	for rows.Next() {
		var p ProcStat
		if err := rows.Scan(&p.Name, &p.Weight); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Stacks returns the weight of each distinct stack of one process in [from, to).
// This is the input for both Top and the flamegraph.
func (s *Store) Stacks(ctx context.Context, process string, from, to time.Time) ([]StackWeight, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT st.folded, a.w
		FROM (
			SELECT stack_id, sum(weight) AS w
			FROM samples
			WHERE process = ? AND ts >= ? AND ts < ?
			GROUP BY stack_id
		) a
		JOIN stacks st ON st.stack_id = a.stack_id`,
		process, from.UnixNano(), to.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StackWeight
	for rows.Next() {
		var folded string
		var w int64
		if err := rows.Scan(&folded, &w); err != nil {
			return nil, err
		}
		var frames []string
		if folded != "" {
			frames = strings.Split(folded, sep)
		}
		out = append(out, StackWeight{Frames: frames, Weight: w})
	}
	return out, rows.Err()
}

// Prune deletes samples older than the cutoff, then runs the dialect's
// maintenance. Call it periodically (every ~30s).
// Unreferenced stacks are kept: they're small and likely to come back.
func (s *Store) Prune(ctx context.Context, before time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM samples WHERE ts < ?`, before.UnixNano()); err != nil {
		return err
	}
	if len(s.maintenance) == 0 {
		return nil
	}
	// One connection: pragmas like analysis_limit are per-connection.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	for _, stmt := range s.maintenance {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("maintenance %q: %w", stmt, err)
		}
	}
	return nil
}
