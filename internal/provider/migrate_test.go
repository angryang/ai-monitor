// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/angryang/ai-monitor/internal/config"
	"gorm.io/gorm"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// legacyTaskSchema is the shape of the `tasks` table the pre-provider
// build left behind: every row carried its own copy of the upstream
// coordinates.
//
// The test creates it verbatim so the migration is exercised against the
// real column layout, not a simplified stand-in.
const legacyTaskSchema = `
CREATE TABLE tasks (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  protocol TEXT NOT NULL,
  base_url TEXT NOT NULL,
  api_key_enc BLOB,
  model TEXT NOT NULL,
  system_prompt TEXT,
  user_prompt TEXT,
  prompt_vars TEXT,
  schedule TEXT NOT NULL,
  timeout_ms INTEGER NOT NULL DEFAULT 30000,
  canary_group TEXT NOT NULL DEFAULT 'default',
  channel TEXT NOT NULL DEFAULT 'wecom',
  wecom_webhook_enc BLOB,
  stream_path TEXT,
  openai_extra_headers TEXT,
  max_tokens INTEGER NOT NULL DEFAULT 1024,
  anthropic_extra_headers TEXT,
  alt TEXT NOT NULL DEFAULT 'sse',
  gemini_extra_headers TEXT,
  created_at DATETIME,
  updated_at DATETIME,
  insecure_skip_verify INTEGER NOT NULL DEFAULT 0,
  thinking_enabled INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX idx_tasks_name ON tasks(name);`

type taskMigrateEnv struct {
	db     *gorm.DB
	prov   *SqliteStore
	cipher *config.AESCipher
}

// newTaskMigrateEnv builds a database that carries the retired task
// schema. The task store's AutoMigrate would create the *new* schema, so
// the table is replaced afterwards — exactly the situation an upgrading
// instance is in.
func newTaskMigrateEnv(t *testing.T) *taskMigrateEnv {
	t.Helper()
	taskStore, err := config.NewSqliteStore(config.SqliteOptions{Path: ":memory:"})
	if err != nil {
		t.Fatalf("task store: %v", err)
	}
	t.Cleanup(func() { _ = taskStore.Close() })
	cipher, err := config.NewAESCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	db := taskStore.DB()
	prov, err := NewSqliteStore(db)
	if err != nil {
		t.Fatalf("provider store: %v", err)
	}
	prov.SetCipher(cipher)
	if err := db.Exec("DROP TABLE `tasks`").Error; err != nil {
		t.Fatalf("drop tasks: %v", err)
	}
	if err := db.Exec(legacyTaskSchema).Error; err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	return &taskMigrateEnv{db: db, prov: prov, cipher: cipher}
}

// insertLegacyTask writes one row in the retired layout.
func (e *taskMigrateEnv) insertLegacyTask(t *testing.T, id, name, key, baseURL, model string, enabled bool) {
	t.Helper()
	enc, err := e.cipher.Encrypt([]byte(key))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := e.db.Exec(`INSERT INTO tasks
		(id, name, enabled, protocol, base_url, api_key_enc, model, system_prompt, user_prompt,
		 prompt_vars, schedule, timeout_ms, canary_group, channel, stream_path, created_at, updated_at)
		VALUES (?, ?, ?, 'openai', ?, ?, ?, 'legacy system', 'hello ${who}', '{"who":"world"}',
		        '@every 1m', 45000, 'default', 'wecom', '/v1/chat/completions', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		id, name, enabled, baseURL, enc, model).Error; err != nil {
		t.Fatalf("insert task: %v", err)
	}
}

// TestMigrateLegacyTasks is the upgrade path for an existing install: the
// upstream becomes a provider, the task keeps its own fields and points at
// it, and no credential is lost.
func TestMigrateLegacyTasks(t *testing.T) {
	e := newTaskMigrateEnv(t)
	e.insertLegacyTask(t, "t-1", "prod", "sk-legacy-secret", "https://e.test", "gpt-test", true)
	ctx := context.Background()

	res, err := MigrateLegacyTasks(ctx, e.db, e.prov, testLogger())
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if res.TasksMigrated != 1 || res.ProvidersCreated != 1 || res.ProvidersReused != 0 {
		t.Fatalf("result = %+v, want 1 task / 1 provider created", res)
	}

	// The table was rebuilt: the provider column exists, the upstream
	// columns are gone.
	if !columnExists(ctx, e.db, "tasks", "provider_id") {
		t.Fatal("provider_id column missing after the migration")
	}
	for _, gone := range []string{"protocol", "base_url", "api_key_enc"} {
		if columnExists(ctx, e.db, "tasks", gone) {
			t.Fatalf("column %q should have been dropped from the task table", gone)
		}
	}
	// The selected model now lives on the task itself: the legacy model
	// is carried over so an existing task keeps probing the same one.
	if !columnExists(ctx, e.db, "tasks", "model") {
		t.Fatal("model column missing after the migration")
	}
	var carried string
	if err := e.db.Raw("SELECT model FROM `tasks` WHERE id = ?", "t-1").Scan(&carried).Error; err != nil {
		t.Fatalf("read task model: %v", err)
	}
	if carried != "gpt-test" {
		t.Fatalf("task model = %q, want gpt-test", carried)
	}

	// The task kept its own fields.
	var row struct {
		ID          string
		Name        string
		Enabled     bool
		ProviderID  string
		UserPrompt  string
		PromptVars  string
		Schedule    string
		CanaryGroup string
	}
	if err := e.db.Raw("SELECT id, name, enabled, provider_id, user_prompt, prompt_vars, schedule, canary_group FROM `tasks`").Scan(&row).Error; err != nil {
		t.Fatalf("read task: %v", err)
	}
	if row.ID != "t-1" || row.Name != "prod" || !row.Enabled {
		t.Fatalf("task fields lost: %+v", row)
	}
	if row.UserPrompt != "hello ${who}" || row.PromptVars != `{"who":"world"}` {
		t.Fatalf("prompt lost: %+v", row)
	}
	if row.Schedule != "@every 1m" || row.CanaryGroup != "default" {
		t.Fatalf("schedule lost: %+v", row)
	}
	if row.ProviderID == "" {
		t.Fatal("the task has no provider reference")
	}

	// The provider carries the upstream coordinates, and the sealed key
	// still opens with the same master key.
	p, err := e.prov.GetProvider(ctx, row.ProviderID)
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if p.BaseURL != "https://e.test" || p.Model != "gpt-test" || p.Protocol != config.ProtocolOpenAI {
		t.Fatalf("provider coordinates lost: %+v", p)
	}
	if p.APIKey != "sk-legacy-secret" {
		t.Fatalf("api key = %q, want the legacy key (secret_error=%q)", p.APIKey, p.SecretError)
	}
	if p.TimeoutMS != 45000 {
		t.Fatalf("timeout = %d, want 45000", p.TimeoutMS)
	}
	if p.OpenAI == nil || p.OpenAI.StreamPath != "/v1/chat/completions" {
		t.Fatalf("protocol block lost: %+v", p.OpenAI)
	}

	// The migrated task can be bound to its provider — the probe path
	// works right after the upgrade.
	bound, err := NewBinder(e.prov).BindTask(ctx, config.TaskConfig{
		ID:         row.ID,
		Name:       row.Name,
		ProviderID: row.ProviderID,
		UserPrompt: row.UserPrompt,
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if bound.BaseURL != "https://e.test" || bound.APIKey != "sk-legacy-secret" {
		t.Fatalf("bound task lost the upstream: %+v", bound)
	}

	// The schema GORM expects has to be in place after the migration: the
	// AutoMigrate that runs right afterwards must be a no-op. If it isn't,
	// GORM rebuilds the table and drops columns on the way.
	if err := e.db.AutoMigrate(&config.TaskConfig{}); err != nil {
		t.Fatalf("auto-migrate after migration: %v", err)
	}
	var after2 struct {
		Count int64
	}
	if err := e.db.Raw("SELECT COUNT(*) as count FROM `tasks`").Scan(&after2).Error; err != nil {
		t.Fatalf("count after auto-migrate: %v", err)
	}
	if after2.Count != 1 {
		t.Fatalf("tasks = %d after auto-migrate, want 1", after2.Count)
	}
	if !columnExists(ctx, e.db, "tasks", "provider_id") {
		t.Fatal("provider_id column missing after auto-migrate")
	}

	// Rerunning the migration is a no-op.
	again, err := MigrateLegacyTasks(ctx, e.db, e.prov, testLogger())
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if again.TasksMigrated != 0 || again.ProvidersCreated != 0 {
		t.Fatalf("migration is not idempotent: %+v", again)
	}
	var count int64
	if err := e.db.Raw("SELECT COUNT(*) FROM `tasks`").Scan(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("tasks = %d, want 1", count)
	}
}

// TestMigrateLegacyTasks_ReusesExistingProvider covers the real-world case
// where the operator already registered the upstream by hand before the
// upgrade: the legacy task must map onto that provider instead of creating
// a duplicate.
func TestMigrateLegacyTasks_ReusesExistingProvider(t *testing.T) {
	e := newTaskMigrateEnv(t)
	ctx := context.Background()

	manual := &Provider{
		ID:        "prv-manual",
		Name:      "prod",
		Protocol:  config.ProtocolOpenAI,
		BaseURL:   "https://e.test",
		APIKey:    "sk-legacy-secret",
		Model:     "gpt-test",
		TimeoutMS: 45000,
	}
	if err := e.prov.CreateProvider(ctx, manual); err != nil {
		t.Fatalf("create manual provider: %v", err)
	}
	e.insertLegacyTask(t, "t-1", "prod", "sk-legacy-secret", "https://e.test", "gpt-test", true)

	res, err := MigrateLegacyTasks(ctx, e.db, e.prov, testLogger())
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if res.ProvidersReused != 1 || res.ProvidersCreated != 0 {
		t.Fatalf("result = %+v, want the existing provider to be reused", res)
	}
	var row struct {
		ProviderID string
	}
	if err := e.db.Raw("SELECT provider_id FROM `tasks`").Scan(&row).Error; err != nil {
		t.Fatalf("read task: %v", err)
	}
	if row.ProviderID != "prv-manual" {
		t.Fatalf("provider_id = %q, want prv-manual", row.ProviderID)
	}
}

// TestMigrateLegacyTasks_DistinctUpstreams verifies that two tasks with
// different upstreams end up on different providers.
func TestMigrateLegacyTasks_DistinctUpstreams(t *testing.T) {
	e := newTaskMigrateEnv(t)
	e.insertLegacyTask(t, "t-1", "a", "sk-a", "https://a.test", "gpt-a", true)
	e.insertLegacyTask(t, "t-2", "b", "sk-b", "https://b.test", "gpt-b", false)
	ctx := context.Background()

	res, err := MigrateLegacyTasks(ctx, e.db, e.prov, testLogger())
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if res.TasksMigrated != 2 || res.ProvidersCreated != 2 {
		t.Fatalf("result = %+v, want 2 tasks / 2 providers", res)
	}
	var rows []struct {
		ID         string
		ProviderID string
		Enabled    bool
	}
	if err := e.db.Raw("SELECT id, provider_id, enabled FROM `tasks` ORDER BY id").Scan(&rows).Error; err != nil {
		t.Fatalf("read tasks: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("tasks = %d, want 2", len(rows))
	}
	if rows[0].ProviderID == rows[1].ProviderID {
		t.Fatal("two distinct upstreams must not share one provider")
	}
	if !rows[0].Enabled || rows[1].Enabled {
		t.Fatalf("the enable flag was not carried over: %+v", rows)
	}
}
