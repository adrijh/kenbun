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
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
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
	Thread  string   // thread.name (the kernel comm), "" if unknown
	OffCPU  bool     // Weight is nanoseconds blocked (off-CPU probe), not on-CPU samples
	Frames  []string // leaf first
	Locs    []string // file:line of each frame, "" if unknown
	Weight  int64
}

// ProcStat is one row of the process list.
type ProcStat struct {
	Name   string    `json:"name"`
	Weight int64     `json:"weight"`
	PIDs   []PIDStat `json:"pids,omitempty"` // heaviest first; one {0, Weight} when the sender gave none
}

// PIDStat is one process instance under a ProcStat's name.
type PIDStat struct {
	PID    int64 `json:"pid"`
	Weight int64 `json:"weight"`
}

// StackWeight is the aggregated weight of one distinct stack.
type StackWeight struct {
	Frames []string `json:"frames"`         // leaf first
	Locs   []string `json:"locs,omitempty"` // file:line per frame, "" if unknown
	Weight int64    `json:"weight"`
}

// ThreadStack is the aggregated weight of one stack in one thread, on or off CPU.
type ThreadStack struct {
	Thread string   `json:"thread"`
	OffCPU bool     `json:"off_cpu"`
	Frames []string `json:"frames"`         // leaf first
	Locs   []string `json:"locs,omitempty"` // file:line per frame, "" if unknown
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
		folded   VARCHAR NOT NULL, -- frames joined by \x1f, leaf first
		locs     VARCHAR NOT NULL  -- their file:line, joined the same way
	)`,
	`CREATE TABLE IF NOT EXISTS samples (
		ts       BIGINT NOT NULL, -- unix nanoseconds: no timezone handling differences between backends
		process  VARCHAR NOT NULL,
		pid      BIGINT,
		thread   VARCHAR NOT NULL,
		off_cpu  INTEGER NOT NULL, -- 1: weight is nanoseconds off-CPU, 0: on-CPU sample count
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
	newStacks := map[int64][2]string{}
	for i, smp := range samples {
		folded, locs := strings.Join(smp.Frames, sep), strings.Join(smp.Locs, sep)
		id := stackID(folded + "\x1e" + locs)
		ids[i] = id
		if _, ok := s.seen[id]; !ok {
			newStacks[id] = [2]string{folded, locs}
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit

	stackRows := make([][]any, 0, len(newStacks))
	for id, st := range newStacks {
		stackRows = append(stackRows, []any{id, st[0], st[1]})
	}
	if err := insertRows(ctx, tx, "INSERT OR IGNORE INTO stacks (stack_id, folded, locs) VALUES ", stackRows); err != nil {
		return fmt.Errorf("insert stacks: %w", err)
	}

	sampleRows := make([][]any, len(samples))
	for i, smp := range samples {
		off := 0
		if smp.OffCPU {
			off = 1
		}
		sampleRows[i] = []any{smp.TS.UnixNano(), smp.Process, smp.PID, smp.Thread, off, ids[i], smp.Weight}
	}
	if err := insertRows(ctx, tx, "INSERT INTO samples (ts, process, pid, thread, off_cpu, stack_id, weight) VALUES ", sampleRows); err != nil {
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

func (s *Store) prune(ctx context.Context, before time.Time) error {
	s.mu.Lock() // no Ingest may trust seen while stacks are deleted
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM samples WHERE ts < ?`, before.UnixNano()); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM stacks WHERE stack_id NOT IN (SELECT stack_id FROM samples)`); err != nil {
		return err
	}
	// Forget them all rather than track which went: the next batches re-insert
	// their stacks (INSERT OR IGNORE) and refill it.
	s.seen = map[int64]struct{}{}
	return nil
}

// stackID must be stable across runs (stacks persist in a file DB),
// so no maphash/random seeds.
func stackID(folded string) int64 {
	h := fnv.New64a()
	h.Write([]byte(folded))
	return int64(h.Sum64())
}

// Processes returns processes by total on-CPU weight in [from, to), each with
// its PIDs.
func (s *Store) Processes(ctx context.Context, from, to time.Time) ([]ProcStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT process, coalesce(pid, 0), sum(weight)
		FROM samples
		WHERE ts >= ? AND ts < ? AND off_cpu = 0
		GROUP BY process, pid`, from.UnixNano(), to.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ProcStat
	at := map[string]int{}
	for rows.Next() {
		var name string
		var p PIDStat
		if err := rows.Scan(&name, &p.PID, &p.Weight); err != nil {
			return nil, err
		}
		i, ok := at[name]
		if !ok {
			i, at[name] = len(out), len(out)
			out = append(out, ProcStat{Name: name})
		}
		out[i].Weight += p.Weight
		out[i].PIDs = append(out[i].PIDs, p)
	}
	SortProcesses(out)
	return out, rows.Err()
}

// SortProcesses orders processes, and each one's PIDs, heaviest first.
func SortProcesses(ps []ProcStat) {
	heaviest := func(a, b int64) int { return cmp.Compare(b, a) }
	slices.SortFunc(ps, func(a, b ProcStat) int { return cmp.Or(heaviest(a.Weight, b.Weight), cmp.Compare(a.Name, b.Name)) })
	for _, p := range ps {
		slices.SortFunc(p.PIDs, func(a, b PIDStat) int { return cmp.Or(heaviest(a.Weight, b.Weight), cmp.Compare(a.PID, b.PID)) })
	}
}

// Stacks returns the on-CPU weight of each distinct stack of one process in
// [from, to), of all its PIDs or only pid if it isn't 0. This is the input for
// both Top and the flamegraph.
func (s *Store) Stacks(ctx context.Context, process string, pid int64, from, to time.Time) ([]StackWeight, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT st.folded, st.locs, a.w
		FROM (
			SELECT stack_id, sum(weight) AS w
			FROM samples
			WHERE process = ? AND ts >= ? AND ts < ? AND off_cpu = 0 AND (? = 0 OR pid = ?)
			GROUP BY stack_id
		) a
		JOIN stacks st ON st.stack_id = a.stack_id`,
		process, from.UnixNano(), to.UnixNano(), pid, pid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StackWeight
	for rows.Next() {
		var folded, locs string
		var w int64
		if err := rows.Scan(&folded, &locs, &w); err != nil {
			return nil, err
		}
		out = append(out, StackWeight{Frames: split(folded), Locs: splitLocs(locs), Weight: w})
	}
	return out, rows.Err()
}

// Oldest is when the oldest sample held was taken; zero when there are none.
// Right after a start (or in a short capture) it's later than a window's start,
// and an average over the whole window would undercount.
func (s *Store) Oldest(ctx context.Context) (time.Time, error) {
	var ns sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT min(ts) FROM samples`).Scan(&ns); err != nil || !ns.Valid {
		return time.Time{}, err
	}
	return time.Unix(0, ns.Int64), nil
}

// ThreadStacks is Stacks per thread, on and off CPU: the input for Loop.
func (s *Store) ThreadStacks(ctx context.Context, process string, pid int64, from, to time.Time) ([]ThreadStack, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.thread, a.off_cpu, st.folded, st.locs, a.w
		FROM (
			SELECT thread, off_cpu, stack_id, sum(weight) AS w
			FROM samples
			WHERE process = ? AND ts >= ? AND ts < ? AND (? = 0 OR pid = ?)
			GROUP BY thread, off_cpu, stack_id
		) a
		JOIN stacks st ON st.stack_id = a.stack_id`,
		process, from.UnixNano(), to.UnixNano(), pid, pid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ThreadStack
	for rows.Next() {
		var t ThreadStack
		var folded, locs string
		if err := rows.Scan(&t.Thread, &t.OffCPU, &folded, &locs, &t.Weight); err != nil {
			return nil, err
		}
		t.Frames, t.Locs = split(folded), splitLocs(locs)
		out = append(out, t)
	}
	return out, rows.Err()
}

func split(folded string) []string {
	if folded == "" {
		return nil
	}
	return strings.Split(folded, sep)
}

// splitLocs is nil when no frame has a location, so JSON leaves it out.
func splitLocs(locs string) []string {
	if strings.Trim(locs, sep) == "" {
		return nil
	}
	return strings.Split(locs, sep)
}

// loc is frame i's file:line, "" if unknown.
func loc(locs []string, i int) string {
	if i < len(locs) {
		return locs[i]
	}
	return ""
}

// hottest is the key with the most weight, "" for none.
func hottest[W int64 | time.Duration](m map[string]W) string {
	best, bw := "", W(0)
	for k, w := range m {
		if w > bw || (w == bw && k < best) {
			best, bw = k, w
		}
	}
	return best
}

// Prune deletes samples older than the cutoff and the stacks no sample uses
// any more, then runs the dialect's maintenance. Call it periodically (every ~30s).
func (s *Store) Prune(ctx context.Context, before time.Time) error {
	if err := s.prune(ctx, before); err != nil {
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
