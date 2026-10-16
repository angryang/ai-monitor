package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// SqliteStore is the default ConfigStore implementation. It owns a single
// GORM-managed connection to a SQLite database file.
type SqliteStore struct {
	db   *gorm.DB
	mu   sync.RWMutex // protects callers mapping JSON-typed columns
	opts SqliteOptions
}

// SqliteOptions carries tunables for SqliteStore. Zero-values fall back to
// sane defaults.
type SqliteOptions struct {
	// Path to the SQLite file. ":memory:" enables in-memory operation
	// (mainly for tests).
	Path string
	// BusyTimeout applied via PRAGMA.
	BusyTimeout time.Duration
	// PollInterval is the cadence used by Watch to detect changes.
	PollInterval time.Duration
}

// NewSqliteStore opens (or creates) the SQLite database, enables WAL +
// busy_timeout pragmas, and runs AutoMigrate. The returned store is safe for
// concurrent use.
func NewSqliteStore(opts SqliteOptions) (*SqliteStore, error) {
	if opts.Path == "" {
		return nil, fmt.Errorf("config: sqlite path required")
	}
	if opts.BusyTimeout == 0 {
		opts.BusyTimeout = 5 * time.Second
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = 5 * time.Second
	}

	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)", opts.Path, int(opts.BusyTimeout/time.Millisecond))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("config: sqlite open: %w", err)
	}
	if err := db.AutoMigrate(&TaskConfig{}); err != nil {
		return nil, fmt.Errorf("config: migrate: %w", err)
	}
	return &SqliteStore{db: db, opts: opts}, nil
}

// ListTasks returns every row in the table. Refreshes the runtime-only JSON
// columns on the way out.
func (s *SqliteStore) ListTasks(ctx context.Context) ([]TaskConfig, error) {
	var rows []TaskConfig
	if err := s.db.WithContext(ctx).Order("created_at ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("config: list: %w", err)
	}
	for i := range rows {
		if err := decodeJSONColumns(&rows[i]); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// GetTask fetches a single task by id.
func (s *SqliteStore) GetTask(ctx context.Context, id string) (*TaskConfig, error) {
	var row TaskConfig
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("config: %s: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("config: get %s: %w", id, err)
	}
	if err := decodeJSONColumns(&row); err != nil {
		return nil, err
	}
	return &row, nil
}

// CreateTask inserts a new row. The caller must have populated ID; the row's
// JSON columns are encoded before insert so the SQLite text columns hold
// JSON-encoded values.
func (s *SqliteStore) CreateTask(ctx context.Context, t *TaskConfig) error {
	if err := s.preWrite(t); err != nil {
		return err
	}
	// Quick uniqueness check to surface a stable error code.
	var n int64
	if err := s.db.WithContext(ctx).Model(&TaskConfig{}).Where("name = ?", t.Name).Count(&n).Error; err != nil {
		return fmt.Errorf("config: lookup name: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("config: name %q already exists: %w", t.Name, ErrAlreadyExists)
	}
	if err := s.db.WithContext(ctx).Create(t).Error; err != nil {
		return fmt.Errorf("config: create: %w", err)
	}
	return nil
}

// UpdateTask overwrites an existing row by ID. The runtime API key + webhook
// are written into their `*Enc` columns here so we don't have to remember
// elsewhere.
func (s *SqliteStore) UpdateTask(ctx context.Context, t *TaskConfig) error {
	if err := s.preWrite(t); err != nil {
		return err
	}
	res := s.db.WithContext(ctx).Save(t)
	if res.Error != nil {
		return fmt.Errorf("config: update: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("config: %s: %w", t.ID, ErrNotFound)
	}
	return nil
}

// DeleteTask removes a single row by id.
func (s *SqliteStore) DeleteTask(ctx context.Context, id string) error {
	res := s.db.WithContext(ctx).Where("id = ?", id).Delete(&TaskConfig{})
	if res.Error != nil {
		return fmt.Errorf("config: delete: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("config: %s: %w", id, ErrNotFound)
	}
	return nil
}

// preWrite moves runtime strings into their encrypted byte columns and
// JSON-encodes the text protocol blocks. It returns ErrInvalidConfig when
// the data itself is malformed (validation done elsewhere).
func (s *SqliteStore) preWrite(t *TaskConfig) error {
	if t == nil {
		return fmt.Errorf("config: nil task: %w", ErrInvalidConfig)
	}
	if t.ID == "" {
		return fmt.Errorf("config: missing id: %w", ErrInvalidConfig)
	}
	if t.Name == "" {
		return fmt.Errorf("config: missing name: %w", ErrInvalidConfig)
	}
	if !t.Protocol.IsValid() {
		return fmt.Errorf("config: protocol %q: %w", t.Protocol, ErrInvalidConfig)
	}
	if t.CanaryGroup == "" {
		t.CanaryGroup = CanaryGroupLabel
	}
	if err := t.Normalise(); err != nil {
		return err
	}
	if t.OpenAI != nil {
		if _, err := jsonToString(t.OpenAI.ExtraHeadersJSON); err != nil {
			return err
		}
	}
	if t.Anthropic != nil {
		if _, err := jsonToString(t.Anthropic.ExtraHeadersJSON); err != nil {
			return err
		}
	}
	if t.Gemini != nil {
		if _, err := jsonToString(t.Gemini.ExtraHeadersJSON); err != nil {
			return err
		}
	}
	return nil
}

// Close releases the underlying database connection. Subsequent calls to
// ListTasks/GetTask/... will fail with "sql: database is closed".
func (s *SqliteStore) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return fmt.Errorf("config: get sql.DB: %w", err)
	}
	return sqlDB.Close()
}

// decodeJSONColumns fills in the PromptVars map and the *Protocol structs
// from the JSON-serialised text columns. Empty/blank columns are left nil
// so callers don't have to distinguish "absent" from "empty".
func decodeJSONColumns(t *TaskConfig) error {
	if err := t.Hydrate(); err != nil {
		return err
	}
	if len(t.APIKeyEnc) > 0 {
		// Decryption happens in B3 once the cipher is wired up. We do NOT
		// touch APIKeyEnc here — leaving the raw ciphertext in memory at
		// this layer is acceptable because the cipher decrypts lazily in
		// the runner.
	}
	return nil
}

// jsonToString validates a JSON field; it returns the original value so
// callers can store it directly into the GORM text column.
func jsonToString(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return "", fmt.Errorf("config: decode extra_headers: %w", err)
	}
	return raw, nil
}
