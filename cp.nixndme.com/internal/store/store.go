// Package store keeps metadata in SQLite. File contents live in the vault, never here.
// All timestamps are unix seconds.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no CGO, cross-compiles to arm64
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("store: not found")

// Store wraps the SQLite database.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY,
  username TEXT NOT NULL UNIQUE,
  pass_hash TEXT NOT NULL,
  totp_secret TEXT NOT NULL DEFAULT '',
  totp_enabled INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
  token_hash TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  csrf TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  ip TEXT NOT NULL DEFAULT '',
  ua TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS shares (
  pk INTEGER PRIMARY KEY AUTOINCREMENT,
  slug TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL CHECK (kind IN ('text','file','link')),
  title TEXT NOT NULL DEFAULT '',
  language TEXT NOT NULL DEFAULT '',
  link_url TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  public INTEGER NOT NULL DEFAULT 0,
  pass_hash TEXT NOT NULL DEFAULT '',
  burn INTEGER NOT NULL DEFAULT 0,
  max_downloads INTEGER NOT NULL DEFAULT 0,
  downloads INTEGER NOT NULL DEFAULT 0,
  views INTEGER NOT NULL DEFAULT 0,
  consumed_until INTEGER NOT NULL DEFAULT 0,
  size INTEGER NOT NULL DEFAULT 0,
  ended_at INTEGER NOT NULL DEFAULT 0,
  end_reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS shares_expiry ON shares(ended_at, expires_at);
CREATE TABLE IF NOT EXISTS files (
  id INTEGER PRIMARY KEY,
  share_pk INTEGER NOT NULL REFERENCES shares(pk) ON DELETE CASCADE,
  blob_id TEXT NOT NULL,
  name TEXT NOT NULL,
  mime TEXT NOT NULL,
  size INTEGER NOT NULL,
  position INTEGER NOT NULL,
  role TEXT NOT NULL DEFAULT 'file' -- 'text' = the share's pasted text, 'file' = an attachment
);
CREATE INDEX IF NOT EXISTS files_share ON files(share_pk);
CREATE TABLE IF NOT EXISTS uploads (
  blob_id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  mime TEXT NOT NULL,
  declared INTEGER NOT NULL,
  received INTEGER NOT NULL DEFAULT 0,
  records INTEGER NOT NULL DEFAULT 0,
  next_chunk INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS grants (
  token_hash TEXT PRIMARY KEY,
  share_pk INTEGER NOT NULL REFERENCES shares(pk) ON DELETE CASCADE,
  expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS access_log (
  id INTEGER PRIMARY KEY,
  share_pk INTEGER NOT NULL REFERENCES shares(pk) ON DELETE CASCADE,
  at INTEGER NOT NULL,
  ip TEXT NOT NULL,
  ua TEXT NOT NULL,
  action TEXT NOT NULL,
  ok INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS access_share ON access_log(share_pk, at);
`

// Open opens (creating if needed) the database in dataDir. synchronous=FULL plus WAL keeps
// the database consistent across sudden power loss.
func Open(dataDir string) (*Store, error) {
	dsn := "file:" + filepath.Join(dataDir, "cp.db") +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite has one writer; a single connection avoids SQLITE_BUSY
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	if err := migrateShareNumbers(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate share numbers: %w", err)
	}
	if err := migrateFileRoles(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate file roles: %w", err)
	}
	return &Store{db: db, now: time.Now}, nil
}

// migrateShareNumbers rebuilds a shares table created before AUTOINCREMENT, so a share
// number ("Paste #21") is never handed out again after that share is deleted.
func migrateShareNumbers(db *sql.DB) error {
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'shares'`).Scan(&ddl); err != nil {
		return err
	}
	if strings.Contains(strings.ToUpper(ddl), "AUTOINCREMENT") {
		return nil
	}
	newDDL := strings.Replace(ddl, "pk INTEGER PRIMARY KEY,", "pk INTEGER PRIMARY KEY AUTOINCREMENT,", 1)
	if newDDL == ddl {
		return fmt.Errorf("unexpected shares schema")
	}
	newDDL = strings.Replace(newDDL, "CREATE TABLE shares", "CREATE TABLE shares_new", 1)
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer db.Exec(`PRAGMA foreign_keys = ON`)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		newDDL,
		`INSERT INTO shares_new SELECT * FROM shares`,
		`DROP TABLE shares`,
		`ALTER TABLE shares_new RENAME TO shares`,
		`CREATE INDEX IF NOT EXISTS shares_expiry ON shares(ended_at, expires_at)`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// migrateFileRoles adds files.role to databases from before mixed shares, and marks the
// stored text of existing text shares as role 'text'.
func migrateFileRoles(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('files') WHERE name = 'role'`).Scan(&n); err != nil || n > 0 {
		return err
	}
	if _, err := db.Exec(`ALTER TABLE files ADD COLUMN role TEXT NOT NULL DEFAULT 'file'`); err != nil {
		return err
	}
	_, err := db.Exec(`UPDATE files SET role = 'text' WHERE share_pk IN (SELECT pk FROM shares WHERE kind = 'text')`)
	return err
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// SetClock overrides the time source (tests only).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Now returns the store's current unix time.
func (s *Store) Now() int64 { return s.now().Unix() }

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
