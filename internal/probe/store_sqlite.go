// SQLite-backed implementation of probe.RunStore. We piggy-back on the same
// ai-monitor.db file the config store uses — the schema lives in the same
// migration epoch so `db.AutoMigrate` picks up `probe_runs` automatically
// the first time the binary starts with the new code.
package probe

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// SqliteRunStore is the production implementation of RunStore. It holds a
// single GORM handle so Save / ListByTask / Prune share the same connection
// pool and locking story as the config store.
type SqliteRunStore struct {
	db *gorm.DB
}

// NewSqliteRunStore wraps an already-opened GORM handle. We don't open the
// file here because the binary's main() opens one DB connection and shares
// it across the config + user + run stores — keeping the file path in a
// single place avoids "file is not a database" lock errors.
func NewSqliteRunStore(db *gorm.DB) *SqliteRunStore {
	return &SqliteRunStore{db: db}
}

// AutoMigrate creates / updates the probe_runs table. Safe to call on
// every boot — GORM only writes a CREATE TABLE / CREATE INDEX if the
// schema differs from what's on disk.
func (s *SqliteRunStore) AutoMigrate() error {
	if err := s.db.AutoMigrate(&ProbeRun{}); err != nil {
		return fmt.Errorf("probe_runs: migrate: %w", err)
	}
	return nil
}

// Save inserts a new run row. Returns the assigned id via the *ProbeRun
// pointer so the caller can log / link it.
func (s *SqliteRunStore) Save(ctx context.Context, run *ProbeRun) error {
	// GORM doesn't honour ctx for the create statement by default; we wrap
	// the call in a WithContext so future DB drivers (Postgres, MySQL) can
	// honour cancellation without an API change.
	if err := s.db.WithContext(ctx).Create(run).Error; err != nil {
		return fmt.Errorf("probe_runs: insert: %w", err)
	}
	return nil
}

// ListByTask returns the newest `limit` runs for taskID. SQL: ORDER BY
// started_at DESC, then id DESC (id is the tiebreaker for runs that share
// a millisecond timestamp, which happens at high cron frequencies).
func (s *SqliteRunStore) ListByTask(ctx context.Context, taskID string, limit int) ([]ProbeRun, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("probe_runs: limit must be > 0 (got %d)", limit)
	}
	// Clamp to a sane upper bound so a malicious or buggy client can't ask
	// for a million rows.
	if limit > 1000 {
		limit = 1000
	}
	var runs []ProbeRun
	if err := s.db.WithContext(ctx).
		Where("task_id = ?", taskID).
		Order("started_at DESC, id DESC").
		Limit(limit).
		Find(&runs).Error; err != nil {
		return nil, fmt.Errorf("probe_runs: list: %w", err)
	}
	return runs, nil
}

// Prune enforces the per-task retention policy. We keep the `keep` most
// recent rows per task and delete the rest. Implemented as one DELETE with
// a subquery — O(N) once per Save, not a per-task hot loop.
//
//   DELETE FROM probe_runs
//   WHERE id NOT IN (
//     SELECT id FROM probe_runs pr
//     WHERE pr.task_id = probe_runs.task_id
//     ORDER BY pr.started_at DESC, pr.id DESC
//     LIMIT :keep
//   )
//
// SQLite evaluates the subquery per outer row, which is fine at our scale
// (hundreds of tasks × hundreds of rows). If the table grows to millions
// we should switch to a per-task loop.
func (s *SqliteRunStore) Prune(ctx context.Context, keep int) error {
	if keep <= 0 {
		return fmt.Errorf("probe_runs: keep must be > 0 (got %d)", keep)
	}
	sql := `
DELETE FROM probe_runs
WHERE id NOT IN (
  SELECT id FROM (
    SELECT id, ROW_NUMBER() OVER (
      PARTITION BY task_id
      ORDER BY started_at DESC, id DESC
    ) AS rn
    FROM probe_runs
  ) WHERE rn <= ?
)`
	if err := s.db.WithContext(ctx).Exec(sql, keep).Error; err != nil {
		return fmt.Errorf("probe_runs: prune: %w", err)
	}
	return nil
}

// Close is a no-op for the shared-handle case. We don't close the underlying
// *sql.DB because the config / user stores own it; closing it here would
// break their queries. Returns nil to satisfy the RunStore contract.
func (s *SqliteRunStore) Close() error { return nil }
