// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
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
//
// Prefer NewSqliteStoreFromDB when the process opens the database itself:
// the legacy monitoring-task migration has to rewrite the `tasks` table
// *before* AutoMigrate runs, and that is only possible when the caller owns
// the GORM handle.
func NewSqliteStore(opts SqliteOptions) (*SqliteStore, error) {
	if opts.Path == "" {
		return nil, fmt.Errorf("config: sqlite path required")
	}
	db, err := OpenDB(opts.Path, opts.BusyTimeout)
	if err != nil {
		return nil, err
	}
	return NewSqliteStoreFromDB(db, opts)
}

// NewSqliteStoreFromDB builds a store on top of an already-open GORM
// handle and runs the task-table migration.
//
// Sharing the handle with the provider / request / run stores keeps one
// SQLite file, one WAL and one busy-timeout for the whole process.
func NewSqliteStoreFromDB(db *gorm.DB, opts SqliteOptions) (*SqliteStore, error) {
	if db == nil {
		return nil, fmt.Errorf("config: nil db handle")
	}
	if opts.BusyTimeout == 0 {
		opts.BusyTimeout = DefaultBusyTimeout
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = 5 * time.Second
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
//
// Strict: the first row whose secrets cannot be decrypted aborts the whole
// call. Use ListTasksLenient on the startup path, where one broken row must
// not take the service down.
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

// ListTasksLenient loads every task, keeping rows whose encrypted secrets
// cannot be opened instead of aborting the whole listing.
//
// Why: a master key mismatch (rotated key, DB copied from another instance)
// used to make the service die at startup with a bare "cipher: message
// authentication failed", leaving no way to reach the admin UI and repair
// the task. Here the affected row is still returned — with SecretError set
// and the secrets left empty — so the operator can re-enter the api key.
//
// The second return value carries one entry per task whose secrets failed
// to decrypt; the caller is expected to log them loudly.
//
// The third return value is reserved for failures of the listing itself
// (unreachable database, a row whose persisted JSON cannot be decoded).
// It is deliberately distinct from the per-row errors: a caller that
// conflates the two ends up treating "the query failed" as "there are no
// tasks", which makes every task disappear from the admin UI without any
// error being surfaced.
func (s *SqliteStore) ListTasksLenient(ctx context.Context) ([]TaskConfig, []error, error) {
	var rows []TaskConfig
	if err := s.db.WithContext(ctx).Order("created_at ASC").Find(&rows).Error; err != nil {
		return nil, nil, fmt.Errorf("config: list: %w", err)
	}
	out := make([]TaskConfig, 0, len(rows))
	var secretErrs []error
	for i := range rows {
		row := rows[i]
		if err := decodeJSONColumns(&row); err != nil {
			return nil, nil, fmt.Errorf("config: task %q: %w", row.Name, err)
		}
		if err := s.decodeSecrets(&row); err != nil {
			// Keep the row: its non-secret fields (base_url, model,
			// schedule, …) are still valid and re-entering the api key
			// makes the task work again.
			row.APIKey = ""
			row.Notify.WeComWebhook = ""
			row.SecretError = err.Error()
			secretErrs = append(secretErrs, fmt.Errorf("config: task %q: %w", row.Name, err))
		}
		out = append(out, row)
	}
	return out, secretErrs, nil
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

// GetTaskLenient fetches a single task by id, tolerating secrets that cannot
// be decrypted.
//
// A master key mismatch on one row must not make the task unreachable —
// otherwise the operator cannot even open the edit form to re-enter the api
// key. The returned row carries SecretError and empty secrets.
func (s *SqliteStore) GetTaskLenient(ctx context.Context, id string) (*TaskConfig, error) {
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
		row.APIKey = ""
		row.Notify.WeComWebhook = ""
		row.SecretError = err.Error()
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

// ListTasksByProvider returns the tasks that point at one provider.
//
// The provider delete route uses it to refuse removing a provider that is
// still in use: without the guard, a task would keep a dangling provider
// id and every probe would fail with "provider not found".
func (s *SqliteStore) ListTasksByProvider(ctx context.Context, providerID string) ([]TaskConfig, error) {
	var rows []TaskConfig
	if err := s.db.WithContext(ctx).Where("provider_id = ?", providerID).Order("created_at ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("config: list tasks by provider: %w", err)
	}
	for i := range rows {
		if err := decodeJSONColumns(&rows[i]); err != nil {
			return nil, err
		}
		if err := s.decodeSecrets(&rows[i]); err != nil {
			rows[i].APIKey = ""
			rows[i].Notify.WeComWebhook = ""
			rows[i].SecretError = err.Error()
		}
	}
	return rows, nil
}

// preWrite moves the runtime webhook string into its encrypted byte column
// and JSON-encodes the text protocol blocks. It returns ErrInvalidConfig
// when the data itself is malformed (validation done elsewhere).
//
// The upstream credentials are not touched here any more: they belong to
// the provider, and the provider store seals them with the same master key.
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
	if t.ProviderID == "" {
		return fmt.Errorf("config: missing provider_id: %w", ErrInvalidConfig)
	}
	if strings.TrimSpace(t.Model) == "" {
		return fmt.Errorf("config: missing model: %w", ErrInvalidConfig)
	}
	if t.CanaryGroup == "" {
		t.CanaryGroup = CanaryGroupLabel
	}
	if err := t.Normalise(); err != nil {
		return err
	}
	// The protocol blocks are runtime-only now (the provider owns them),
	// but a caller may still hand one in — validate its JSON so a broken
	// header map is caught at write time rather than at probe time.
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
	// Encrypt the notification webhook (if cipher attached) — silently keep
	// ciphertext as-is when the input is empty and ciphertext exists.
	c := s.cipherOrNil()
	if c != nil && t.Notify.WeComWebhook != "" {
		enc, err := c.Encrypt([]byte(t.Notify.WeComWebhook))
		if err != nil {
			return fmt.Errorf("config: encrypt wecom webhook: %w", err)
		}
		t.Notify.WeComWebhookEnc = enc
	}
	// Always zero plaintext secrets after the write.
	t.Notify.WeComWebhook = ""
	return nil
}

// decodeSecrets reverses preWrite's encryption step. Decryption failures
// surface as errors so callers (probe runner) can decide policy. Missing
// ciphertext results in empty strings.
//
// Only the notification webhook is sealed on the task itself: the upstream
// api key belongs to the provider and is resolved at bind time.
func (s *SqliteStore) decodeSecrets(t *TaskConfig) error {
	c := s.cipherOrNil()
	if c == nil {
		return nil
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
//
// The upstream coordinates are not part of the fingerprint: they live on the
// provider and are resolved at bind time, so editing a provider takes
// effect on the next probe without a scheduler reload. The selected model
// IS part of it — it lives on the task row and changing it changes what the
// probe sends.
func (s *SqliteStore) fingerprintAll(ctx context.Context) (map[string]string, error) {
	type fpRow struct {
		ID          string
		Name        string
		Enabled     bool
		ProviderID  string
		Model       string
		PromptVars  string
		Schedule    string
		CanaryGroup string
		NotifyEnabled bool
		Channel     string
		WebhookEnc  []byte
	}
	var rows []fpRow
	if err := s.db.WithContext(ctx).
		Table(TaskConfigTableName).
		Select("id, name, enabled, provider_id, model, prompt_vars, schedule, canary_group, notify_enabled, notify_channel, notify_wecom_webhook_enc as webhook_enc").
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("config: fingerprint scan: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%t|%s|%s|%s|%s|%s|%t|%s|%x",
			r.ID, r.Name, r.Enabled, r.ProviderID, r.Model, r.PromptVars, r.Schedule,
			r.CanaryGroup, r.NotifyEnabled, r.Channel, r.WebhookEnc)))
		out[r.ID] = hex.EncodeToString(h[:])
	}
	return out, nil
}

// decodeJSONColumns fills in the PromptVars map and the *Protocol structs
// from the JSON-serialised text columns. Empty/blank columns are left nil
// so callers don't have to distinguish "absent" from "empty".
//
// The protocol blocks are runtime-only since the provider registry took
// over the upstream coordinates, so a row read from SQLite never carries
// them — the collapse below keeps the function correct for callers that
// hand in a struct that was filled in memory.
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
