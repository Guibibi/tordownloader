// Package store is the SQLite persistence layer for tordownloader. It owns the
// database connection and schema migrations for torrents, files, and categories.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)
)

// schema is applied on every Open. It is idempotent (IF NOT EXISTS) so opening
// an existing database is a no-op. Mirrors docs/ARCHITECTURE.md §5.
const schema = `
CREATE TABLE IF NOT EXISTS torrents (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  infohash        TEXT    NOT NULL UNIQUE,      -- lowercase 40-hex; the key Sonarr tracks
  torbox_id       INTEGER,                      -- TorBox's torrent id (operational handle)
  name            TEXT    NOT NULL DEFAULT '',
  category        TEXT    NOT NULL DEFAULT '',
  save_path       TEXT    NOT NULL DEFAULT '',
  content_path    TEXT    NOT NULL DEFAULT '',
  size            INTEGER NOT NULL DEFAULT 0,
  magnet          TEXT    NOT NULL DEFAULT '',      -- original magnet URI, for (re)submission
  source_blob     BLOB,                             -- .torrent bytes when added by file
  state           TEXT    NOT NULL DEFAULT 'QUEUED',
  torbox_state    TEXT    NOT NULL DEFAULT '',
  torbox_progress REAL    NOT NULL DEFAULT 0,
  local_progress  REAL    NOT NULL DEFAULT 0,
  dlspeed         INTEGER NOT NULL DEFAULT 0,
  seeds           INTEGER NOT NULL DEFAULT 0,   -- TorBox-reported seeders (fetching phase)
  peers           INTEGER NOT NULL DEFAULT 0,   -- TorBox-reported leechers (fetching phase)
  eta             INTEGER NOT NULL DEFAULT 0,   -- TorBox-side seconds remaining (fetching phase)
  cached          INTEGER NOT NULL DEFAULT 0,   -- 1 = already on TorBox at submit (instant hit)
  error           TEXT    NOT NULL DEFAULT '',
  added_on        INTEGER NOT NULL DEFAULT 0,
  active_since    INTEGER NOT NULL DEFAULT 0,   -- when it entered TORBOX_ACTIVE
  progress_at     INTEGER NOT NULL DEFAULT 0,   -- last time TorBox progress advanced (stall clock)
  completed_on    INTEGER NOT NULL DEFAULT 0,
  created_at      INTEGER NOT NULL DEFAULT 0,
  updated_at      INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_torrents_state ON torrents(state);

CREATE TABLE IF NOT EXISTS files (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  torrent_id     INTEGER NOT NULL REFERENCES torrents(id) ON DELETE CASCADE,
  torbox_file_id INTEGER,
  rel_path       TEXT    NOT NULL DEFAULT '',   -- path within the torrent (preserves folders)
  short_name     TEXT    NOT NULL DEFAULT '',
  size           INTEGER NOT NULL DEFAULT 0,
  downloaded     INTEGER NOT NULL DEFAULT 0,
  done           INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_files_torrent ON files(torrent_id);

CREATE TABLE IF NOT EXISTS categories (
  name      TEXT PRIMARY KEY,
  save_path TEXT NOT NULL DEFAULT ''
);
`

// Store wraps the database connection.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path, verifies the
// connection, and applies the schema. The parent directory is created if absent.
func Open(ctx context.Context, path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir %s: %w", dir, err)
		}
	}

	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// DB exposes the underlying *sql.DB for query layers built in later milestones.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the database connection.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	// Additive column migrations for databases created before these columns
	// existed. CREATE TABLE IF NOT EXISTS won't add columns to an existing table.
	cols := []struct{ table, column, ddl string }{
		{"torrents", "magnet", "TEXT NOT NULL DEFAULT ''"},
		{"torrents", "source_blob", "BLOB"},
		{"torrents", "progress_at", "INTEGER NOT NULL DEFAULT 0"},
		{"torrents", "seeds", "INTEGER NOT NULL DEFAULT 0"},
		{"torrents", "peers", "INTEGER NOT NULL DEFAULT 0"},
		{"torrents", "eta", "INTEGER NOT NULL DEFAULT 0"},
		{"torrents", "cached", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, c := range cols {
		if err := s.ensureColumn(ctx, c.table, c.column, c.ddl); err != nil {
			return err
		}
	}
	return nil
}

// ensureColumn adds a column to a table if it is not already present. SQLite
// has no "ADD COLUMN IF NOT EXISTS", so we inspect the schema first.
func (s *Store) ensureColumn(ctx context.Context, table, column, ddl string) error {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("inspect %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return fmt.Errorf("scan %s column: %w", table, err)
		}
		if name == column {
			return rows.Close()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, ddl))
	if err != nil {
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}
