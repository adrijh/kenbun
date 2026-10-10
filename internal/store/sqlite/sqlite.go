// Package sqlite opens a store backed by SQLite, using the pure-Go driver
// (no cgo). It's the only package that imports the driver.
package sqlite

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"

	"github.com/adrijh/kenbun/internal/store"
)

var schema = append(append([]string{}, store.BaseSchema...),
	`CREATE INDEX IF NOT EXISTS samples_process_ts ON samples (process, ts)`, // Stacks
	`CREATE INDEX IF NOT EXISTS samples_ts ON samples (ts)`,                  // Processes, Prune
)

// Without statistics SQLite answers Processes by walking the (process, ts)
// index over every row (~100x slower). A sampled ANALYZE costs <1ms.
var maintenance = []string{
	`PRAGMA analysis_limit = 1000`,
	`ANALYZE`,
}

// Open opens or creates a SQLite database. An empty path means in-memory.
func Open(path string) (*store.Store, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" + // readers don't block the writer
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)"
	if path == "" {
		dsn = ":memory:"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if path == "" {
		// Every connection to :memory: is its own empty database, so use exactly one.
		db.SetMaxOpenConns(1)
	}

	return store.New(db, store.Dialect{
		Schema:      schema,
		Maintenance: maintenance,
	})
}
