package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	db     *gorm.DB
	cipher Cipher // optional; encrypts API keys + wecom webhooks on disk
	mu     sync.RWMutex
	opts   SqliteOptions
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

// SetCipher attaches an encryption backend. Pass nil to disable encryption.
func (s *SqliteStore) SetCipher(c Cipher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cipher = c
}

// DB returns the underlying *gorm.DB. It exists so peer stores (e.g. the
// probe run history store) can share the same SQLite file handle / WAL
// without re-opening it. Callers should treat the returned handle as
// read-only; writes should still go through the appropriate Store method
// so encryption / watch / poll bookkeeping stays correct.
func (s *SqliteStore) DB() *gorm.DB {
	return s.db
}

// cipherOrNil returns the configured cipher under a read-lock.
func (s *SqliteStore) cipherOrNil() Cipher {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cipher
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
		if err := s.decodeSecrets(&rows[i]); err != nil {
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
	if err := s.decodeSecrets(&row); err != nil {
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
	// Encrypt API key (if cipher attached) — silently keep ciphertext as-is
	// when the input is empty and ciphertext exists.
	c := s.cipherOrNil()
	if c != nil {
		if t.APIKey != "" {
			enc, err := c.Encrypt([]byte(t.APIKey))
			if err != nil {
				return fmt.Errorf("config: encrypt api_key: %w", err)
			}
			t.APIKeyEnc = enc
		}
		if t.Notify.WeComWebhook != "" {
			enc, err := c.Encrypt([]byte(t.Notify.WeComWebhook))
			if err != nil {
				return fmt.Errorf("config: encrypt wecom webhook: %w", err)
			}
			t.Notify.WeComWebhookEnc = enc
		}
	}
	// Always zero plaintext secrets after the write.
	t.APIKey = ""
	t.Notify.WeComWebhook = ""
	return nil
}

// decodeSecrets reverses preWrite's encryption step. Decryption failures
// surface as errors so callers (probe runner) can decide policy. Missing
// ciphertext results in empty strings.
func (s *SqliteStore) decodeSecrets(t *TaskConfig) error {
	c := s.cipherOrNil()
	if c == nil {
		return nil
	}
	if len(t.APIKeyEnc) > 0 && t.APIKey == "" {
		raw, err := c.Decrypt(t.APIKeyEnc)
		if err != nil {
			return fmt.Errorf("config: decrypt api_key: %w", err)
		}
		t.APIKey = string(raw)
	}
	if len(t.Notify.WeComWebhookEnc) > 0 && t.Notify.WeComWebhook == "" {
		raw, err := c.Decrypt(t.Notify.WeComWebhookEnc)
		if err != nil {
			return fmt.Errorf("config: decrypt wecom webhook: %w", err)
		}
		t.Notify.WeComWebhook = string(raw)
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

// Watch starts a background poller that compares row fingerprints on every
// PollInterval tick. When a row's fingerprint changes (update or new row)
// it pushes an Event with Type=EventUpsert; when a row disappears, EventDelete
// is pushed. Callers must drain the channel or the buffered queue will fill
// and the poller will block.
func (s *SqliteStore) Watch(ctx context.Context) (<-chan Event, error) {
	if s.opts.PollInterval <= 0 {
		return nil, fmt.Errorf("config: poll interval must be > 0")
	}
	// Snapshot the initial state so we only emit diffs.
	snapshot, err := s.fingerprintAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("config: snapshot: %w", err)
	}
	out := make(chan Event, 64)
	ticker := time.NewTicker(s.opts.PollInterval)
	go func() {
		defer ticker.Stop()
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				current, err := s.fingerprintAll(ctx)
				if err != nil {
					continue // transient DB error; try again next tick
				}
				for id, fp := range current {
					if old, ok := snapshot[id]; !ok || old != fp {
						select {
						case out <- Event{Type: EventUpsert, TaskID: id, At: time.Now().UTC()}:
						case <-ctx.Done():
							return
						}
					}
				}
				for id := range snapshot {
					if _, ok := current[id]; !ok {
						select {
						case out <- Event{Type: EventDelete, TaskID: id, At: time.Now().UTC()}:
						case <-ctx.Done():
							return
						}
					}
				}
				snapshot = current
			}
		}
	}()
	return out, nil
}

// fingerprintAll returns a map of task id -> fingerprint. The fingerprint is
// the SHA-256 of the row + secret column. Using hash avoids recomputing the
// full ciphertext on every tick.
func (s *SqliteStore) fingerprintAll(ctx context.Context) (map[string]string, error) {
	type fpRow struct {
		ID            string
		Name          string
		Enabled       bool
		Protocol      string
		BaseURL       string
		APIKeyEnc     []byte
		Model         string
		PromptVars    string
		Schedule      string
		TimeoutMS     int64
		CanaryGroup   string
		NotifyEnabled bool
		WebhookEnc    []byte
		OpenAIEnc     string
		AnthropicEnc  string
		GeminiEnc     string
	}
	var rows []fpRow
	if err := s.db.WithContext(ctx).
		Table(TaskConfigTableName).
		Select("id, name, enabled, protocol, base_url, api_key_enc, model, prompt_vars, schedule, timeout_ms, canary_group, enabled as notify_enabled, wecom_webhook_enc as webhook_enc, openai_extra_headers as openai_enc, anthropic_extra_headers as anthropic_enc, gemini_extra_headers as gemini_enc").
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("config: fingerprint scan: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%t|%s|%s|%x|%s|%s|%s|%d|%s|%t|%x|%s|%s|%s",
			r.ID, r.Name, r.Enabled, r.Protocol, r.BaseURL, r.APIKeyEnc, r.Model,
			r.PromptVars, r.Schedule, r.TimeoutMS, r.CanaryGroup, r.NotifyEnabled,
			r.WebhookEnc, r.OpenAIEnc, r.AnthropicEnc, r.GeminiEnc)))
		out[r.ID] = hex.EncodeToString(h[:])
	}
	return out, nil
}

// decodeJSONColumns fills in the PromptVars map and the *Protocol structs
// from the JSON-serialised text columns. Empty/blank columns are left nil
// so callers don't have to distinguish "absent" from "empty".
func decodeJSONColumns(t *TaskConfig) error {
	if err := t.Hydrate(); err != nil {
		return err
	}
	// Collapse "embedded" fields back to nil if their customisable columns
	// are empty (i.e. GORM reconstructed them only because the column had a
	// default value). This keeps the protocol-segment validator accurate.
	// MaxTokens == 0 means the column was NULL on read (NOT NULL DEFAULT 1024
	// would otherwise make every Anthropic struct non-zero).
	if t.OpenAI != nil && t.OpenAI.StreamPath == "" && t.OpenAI.ExtraHeadersJSON == "" {
		t.OpenAI = nil
	}
	if t.Anthropic != nil && t.Anthropic.ExtraHeadersJSON == "" && (t.Anthropic.MaxTokens == 0 || t.Anthropic.MaxTokens == 1024) {
		t.Anthropic = nil
	}
	if t.Gemini != nil && t.Gemini.ExtraHeadersJSON == "" && (t.Gemini.Alt == "" || t.Gemini.Alt == "sse") {
		t.Gemini = nil
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
