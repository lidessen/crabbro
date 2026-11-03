// Package history persists completed exchanges in SQLite so a restart does
// not drop the traffic an agent already recorded.
package history

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lidessen/chiling/internal/shared"
	_ "modernc.org/sqlite"
)

// Store is a SQLite history of captured exchanges.
type Store struct {
	db        *sql.DB
	Path      string
	Retention time.Duration
	MaxRows   int
}

// Open creates or opens the database at path.
func Open(path string, retention time.Duration, maxRows int) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("history path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if retention <= 0 {
		retention = shared.DefaultHistoryRetention
	}
	if maxRows <= 0 {
		maxRows = shared.DefaultHistoryMaxRows
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, Path: path, Retention: retention, MaxRows: maxRows}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	_, _ = s.Prune()
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS requests (
  id TEXT PRIMARY KEY,
  started_at TEXT NOT NULL,
  payload TEXT NOT NULL,
  request_body BLOB,
  response_body BLOB
);
CREATE INDEX IF NOT EXISTS idx_requests_started ON requests(started_at);
`)
	return err
}

// Close closes the database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

type storedRequest struct {
	Request shared.Request `json:"request"`
}

// Save upserts a completed exchange. Bodies are copies.
func (s *Store) Save(req *shared.Request, reqBody, respBody []byte) error {
	if s == nil || req == nil || req.ID == "" {
		return nil
	}
	blob, err := json.Marshal(storedRequest{Request: *req})
	if err != nil {
		return err
	}
	started := req.StartedAt.UTC().Format(time.RFC3339Nano)
	_, err = s.db.Exec(`
INSERT INTO requests (id, started_at, payload, request_body, response_body)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  started_at = excluded.started_at,
  payload = excluded.payload,
  request_body = excluded.request_body,
  response_body = excluded.response_body
`, string(req.ID), started, string(blob), reqBody, respBody)
	if err != nil {
		return err
	}
	_, _ = s.Prune()
	return nil
}

// Get loads one exchange and its bodies.
func (s *Store) Get(id shared.RequestID) (*shared.Request, []byte, []byte, error) {
	var payload string
	var reqBody, respBody []byte
	err := s.db.QueryRow(`SELECT payload, request_body, response_body FROM requests WHERE id = ?`, string(id)).
		Scan(&payload, &reqBody, &respBody)
	if err == sql.ErrNoRows {
		return nil, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}
	var stored storedRequest
	if err := json.Unmarshal([]byte(payload), &stored); err != nil {
		return nil, nil, nil, err
	}
	return &stored.Request, reqBody, respBody, nil
}

// Bodies returns stored bodies for an id.
func (s *Store) Bodies(id shared.RequestID) ([]byte, []byte, error) {
	_, rb, sb, err := s.Get(id)
	return rb, sb, err
}

// List returns up to limit exchanges, oldest first. limit <= 0 means all.
func (s *Store) List(limit int) ([]*shared.Request, error) {
	q := `SELECT payload FROM requests ORDER BY started_at ASC`
	var rows *sql.Rows
	var err error
	if limit > 0 {
		rows, err = s.db.Query(q+` LIMIT ?`, limit)
	} else {
		rows, err = s.db.Query(q)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*shared.Request
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var stored storedRequest
		if err := json.Unmarshal([]byte(payload), &stored); err != nil {
			continue
		}
		cp := stored.Request
		out = append(out, &cp)
	}
	return out, rows.Err()
}

// Count returns the number of stored exchanges.
func (s *Store) Count() int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n)
	return n
}

// Prune drops rows older than the retention window and rows beyond MaxRows.
func (s *Store) Prune() (int, error) {
	cutoff := time.Now().Add(-s.Retention).UTC().Format(time.RFC3339Nano)
	res, err := s.db.Exec(`DELETE FROM requests WHERE started_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	res, err = s.db.Exec(`
DELETE FROM requests WHERE id NOT IN (
  SELECT id FROM requests ORDER BY started_at DESC LIMIT ?
)`, s.MaxRows)
	if err != nil {
		return int(n), err
	}
	n2, _ := res.RowsAffected()
	return int(n + n2), nil
}

// Status is the agent-facing summary.
type Status struct {
	Enabled   bool   `json:"enabled"`
	Path      string `json:"path,omitempty"`
	Requests  int    `json:"requests"`
	Retention string `json:"retention,omitempty"`
	MaxRows   int    `json:"maxRows,omitempty"`
}

// Status reports configuration and the current row count.
func (s *Store) Status() Status {
	if s == nil {
		return Status{}
	}
	return Status{
		Enabled:   true,
		Path:      s.Path,
		Requests:  s.Count(),
		Retention: s.Retention.String(),
		MaxRows:   s.MaxRows,
	}
}
