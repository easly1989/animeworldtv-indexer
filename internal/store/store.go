// Package store is a small SQLite-backed cache (pure-Go driver, no CGO).
// Later features add their own tables next to kv.
package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

// Open creates the database file (and its directory) if needed.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite has one writer anyway; this avoids "database is locked"
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS kv (
		key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at INTEGER NOT NULL);
		CREATE TABLE IF NOT EXISTS downloads (hash TEXT PRIMARY KEY, data TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS download_categories (name TEXT PRIMARY KEY)`); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Get decodes the value at key into v if it is younger than maxAge.
func (s *Store) Get(key string, maxAge time.Duration, v any) bool {
	var raw string
	var at int64
	if err := s.db.QueryRow(`SELECT value, updated_at FROM kv WHERE key = ?`, key).Scan(&raw, &at); err != nil {
		return false
	}
	if time.Since(time.Unix(at, 0)) > maxAge {
		return false
	}
	return json.Unmarshal([]byte(raw), v) == nil
}

// Put stores v as JSON under key, replacing any previous value.
func (s *Store) Put(key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, string(raw), time.Now().Unix())
	return err
}

// The download queue (see package downloader): one JSON row per job.

func (s *Store) PutDownload(hash string, data []byte) error {
	_, err := s.db.Exec(`INSERT INTO downloads (hash, data) VALUES (?, ?)
		ON CONFLICT(hash) DO UPDATE SET data = excluded.data`, hash, string(data))
	return err
}

func (s *Store) DeleteDownload(hash string) error {
	_, err := s.db.Exec(`DELETE FROM downloads WHERE hash = ?`, hash)
	return err
}

func (s *Store) Downloads() ([][]byte, error) {
	rows, err := s.db.Query(`SELECT data FROM downloads`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		out = append(out, []byte(raw))
	}
	return out, rows.Err()
}

func (s *Store) PutCategory(name string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO download_categories (name) VALUES (?)`, name)
	return err
}

func (s *Store) Categories() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM download_categories`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
