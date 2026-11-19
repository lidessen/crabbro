// Package ledger persists gateway accounting rows in ~/.crabbro/gateway/usage.db
// and calibrates plan quota visibility from upstream rate-limit headers and AIMD feedback.
package ledger

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"

	"github.com/lidessen/crabbro/internal/shared"
)

// UsageEntry records one completed or attempted gateway exchange in usage.db.
type UsageEntry struct {
	Timestamp        time.Time
	PlanID           string
	Model            string
	Protocol         string
	RouteID          string
	SessionKey       string
	Status           int
	ErrorKind        string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	TotalTokens      int64
	TTFTMs           int64
	DurationMs       int64
}

// Store is the dedicated SQLite quota & usage ledger (~/.crabbro/gateway/usage.db).
type Store struct {
	db         *sql.DB
	Path       string
	Retention  time.Duration
	Calibrator *Calibrator
}

// Open opens or initializes usage.db at path.
func Open(path string, retention time.Duration) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("usage ledger path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if retention <= 0 {
		retention = shared.DefaultGatewayUsageRetention
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{
		db:         db,
		Path:       path,
		Retention:  retention,
		Calibrator: NewCalibrator(),
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	_, _ = s.Prune()
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS gateway_usage (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT,
  ts                 INTEGER NOT NULL,
  plan_id            TEXT NOT NULL,
  model              TEXT NOT NULL,
  protocol           TEXT NOT NULL,
  route_id           TEXT NOT NULL,
  session_key        TEXT NOT NULL,
  status             INTEGER NOT NULL,
  error_kind         TEXT NOT NULL DEFAULT '',
  input_tokens       INTEGER NOT NULL DEFAULT 0,
  output_tokens      INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
  cache_write_tokens INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens   INTEGER NOT NULL DEFAULT 0,
  total_tokens       INTEGER NOT NULL DEFAULT 0,
  ttft_ms            INTEGER NOT NULL DEFAULT 0,
  duration_ms        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_gw_usage_plan_ts ON gateway_usage(plan_id, ts);
CREATE INDEX IF NOT EXISTS idx_gw_usage_ts ON gateway_usage(ts);
`)
	return err
}

// Close closes the SQLite database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Record inserts one accounting row into gateway_usage.
func (s *Store) Record(e UsageEntry) error {
	if s == nil || s.db == nil {
		return nil
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	if e.TotalTokens == 0 {
		e.TotalTokens = e.InputTokens + e.OutputTokens + e.CacheReadTokens + e.CacheWriteTokens + e.ReasoningTokens
	}
	ts := e.Timestamp.UTC().UnixMilli()
	_, err := s.db.Exec(`
INSERT INTO gateway_usage (
  ts, plan_id, model, protocol, route_id, session_key, status, error_kind,
  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, total_tokens,
  ttft_ms, duration_ms
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ts, e.PlanID, e.Model, e.Protocol, e.RouteID, e.SessionKey, e.Status, e.ErrorKind,
		e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens, e.ReasoningTokens, e.TotalTokens,
		e.TTFTMs, e.DurationMs,
	)
	return err
}

// WindowTotals returns total requests and total tokens recorded for planID since the given timestamp.
func (s *Store) WindowTotals(planID string, since time.Time) (requests int64, tokens int64, err error) {
	if s == nil || s.db == nil {
		return 0, 0, nil
	}
	err = s.db.QueryRow(`
SELECT COUNT(*), COALESCE(SUM(total_tokens), 0)
FROM gateway_usage
WHERE plan_id = ? AND ts >= ?`,
		planID, since.UTC().UnixMilli(),
	).Scan(&requests, &tokens)
	return requests, tokens, err
}

// Aggregate summarizes gateway accounting over [since, until] optionally filtered by plan.
func (s *Store) Aggregate(window string, since, until time.Time, planFilter string) (shared.GatewayUsageResponse, error) {
	resp := shared.GatewayUsageResponse{
		Window:  window,
		Since:   since.UTC(),
		Until:   until.UTC(),
		Buckets: []shared.GatewayUsageBucket{},
	}
	if s == nil || s.db == nil {
		return resp, nil
	}
	q := `
SELECT
  plan_id,
  model,
  COUNT(*),
  SUM(CASE WHEN status >= 400 OR error_kind != '' THEN 1 ELSE 0 END),
  COALESCE(SUM(input_tokens), 0),
  COALESCE(SUM(output_tokens), 0),
  COALESCE(SUM(cache_read_tokens), 0),
  COALESCE(SUM(cache_write_tokens), 0),
  COALESCE(SUM(reasoning_tokens), 0),
  COALESCE(SUM(total_tokens), 0),
  COALESCE(AVG(NULLIF(ttft_ms, 0)), 0),
  COALESCE(AVG(NULLIF(duration_ms, 0)), 0)
FROM gateway_usage
WHERE ts >= ? AND ts <= ?`
	args := []any{since.UTC().UnixMilli(), until.UTC().UnixMilli()}
	if planFilter != "" {
		q += ` AND plan_id = ?`
		args = append(args, planFilter)
	}
	q += ` GROUP BY plan_id, model`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return resp, err
	}
	defer rows.Close()

	var sumTTFTWeight, sumDurWeight float64
	for rows.Next() {
		var b shared.GatewayUsageBucket
		if err := rows.Scan(
			&b.Plan, &b.Model, &b.Requests, &b.Errors,
			&b.InputTokens, &b.OutputTokens, &b.CacheReadTokens, &b.CacheWriteTokens,
			&b.ReasoningTokens, &b.TotalTokens, &b.AvgTTFTMs, &b.AvgDurationMs,
		); err != nil {
			return resp, err
		}
		if denom := b.InputTokens + b.CacheReadTokens; denom > 0 {
			b.CacheHitRatio = float64(b.CacheReadTokens) / float64(denom)
		}
		resp.Buckets = append(resp.Buckets, b)
		resp.Totals.Requests += b.Requests
		resp.Totals.Errors += b.Errors
		resp.Totals.InputTokens += b.InputTokens
		resp.Totals.OutputTokens += b.OutputTokens
		resp.Totals.CacheReadTokens += b.CacheReadTokens
		resp.Totals.CacheWriteTokens += b.CacheWriteTokens
		resp.Totals.ReasoningTokens += b.ReasoningTokens
		resp.Totals.TotalTokens += b.TotalTokens
		sumTTFTWeight += b.AvgTTFTMs * float64(b.Requests)
		sumDurWeight += b.AvgDurationMs * float64(b.Requests)
	}
	if err := rows.Err(); err != nil {
		return resp, err
	}
	sort.Slice(resp.Buckets, func(i, j int) bool {
		if resp.Buckets[i].TotalTokens != resp.Buckets[j].TotalTokens {
			return resp.Buckets[i].TotalTokens > resp.Buckets[j].TotalTokens
		}
		if resp.Buckets[i].Plan != resp.Buckets[j].Plan {
			return resp.Buckets[i].Plan < resp.Buckets[j].Plan
		}
		return resp.Buckets[i].Model < resp.Buckets[j].Model
	})
	if resp.Totals.Requests > 0 {
		resp.Totals.AvgTTFTMs = sumTTFTWeight / float64(resp.Totals.Requests)
		resp.Totals.AvgDurationMs = sumDurWeight / float64(resp.Totals.Requests)
	}
	if denom := resp.Totals.InputTokens + resp.Totals.CacheReadTokens; denom > 0 {
		resp.Totals.CacheHitRatio = float64(resp.Totals.CacheReadTokens) / float64(denom)
	}
	return resp, nil
}

// Prune removes accounting rows older than Retention.
func (s *Store) Prune() (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-s.Retention).UnixMilli()
	res, err := s.db.Exec(`DELETE FROM gateway_usage WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
