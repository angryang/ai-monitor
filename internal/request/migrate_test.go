// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package request

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/provider"
	"gorm.io/gorm"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// legacySchema is the shape of the two tables the group-era build left
// behind. The test creates them verbatim so the migration is exercised
// against the real column layout, not a simplified stand-in.
const legacySchema = `
CREATE TABLE request_groups (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  description TEXT,
  protocol TEXT NOT NULL,
  base_url TEXT NOT NULL,
  api_key_enc BLOB,
  model TEXT NOT NULL,
  timeout_ms INTEGER NOT NULL DEFAULT 30000,
  insecure_skip_verify INTEGER NOT NULL DEFAULT 0,
  thinking_enabled INTEGER NOT NULL DEFAULT 0,
  stream_path TEXT,
  openai_extra_headers TEXT,
  max_tokens INTEGER,
  anthropic_extra_headers TEXT,
  alt TEXT,
  gemini_extra_headers TEXT,
  created_at DATETIME,
  updated_at DATETIME
);
CREATE TABLE request_items (
  id TEXT PRIMARY KEY,
  group_id TEXT NOT NULL,
  name TEXT NOT NULL,
  description TEXT,
  system_prompt TEXT,
  user_prompt TEXT NOT NULL,
  prompt_vars TEXT,
  sort_order INTEGER NOT NULL DEFAULT 0,
  thinking_override INTEGER,
  created_at DATETIME,
  updated_at DATETIME
);`

type migrateEnv struct {
	db      *gorm.DB
	store   *SqliteStore
	prov    *provider.SqliteStore
	cipher  *config.AESCipher
	cleanup func()
}

func newMigrateEnv(t *testing.T) *migrateEnv {
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
	prov, err := provider.NewSqliteStore(db)
	if err != nil {
		t.Fatalf("provider store: %v", err)
	}
	prov.SetCipher(cipher)
	if err := db.Exec(legacySchema).Error; err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	store, err := NewSqliteStore(db)
	if err != nil {
		t.Fatalf("request store: %v", err)
	}
	return &migrateEnv{db: db, store: store, prov: prov, cipher: cipher}
}

func (e *migrateEnv) insertLegacy(t *testing.T, groupID, name, key string, items int) {
	t.Helper()
	enc, err := e.cipher.Encrypt([]byte(key))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := e.db.Exec(`INSERT INTO request_groups
		(id, name, description, protocol, base_url, api_key_enc, model, timeout_ms, stream_path, created_at, updated_at)
		VALUES (?, ?, ?, 'openai', 'https://e.test', ?, 'gpt-test', 30000, '/v1/chat/completions', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		groupID, name, "legacy upstream", enc).Error; err != nil {
		t.Fatalf("insert group: %v", err)
	}
	for i := 0; i < items; i++ {
		id := groupID + "-item-" + string(rune('a'+i))
		if err := e.db.Exec(`INSERT INTO request_items
			(id, group_id, name, description, system_prompt, user_prompt, prompt_vars, sort_order, created_at, updated_at)
			VALUES (?, ?, ?, '', 'be brief', 'hello ${who}', '{"who":"world"}', ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
			id, groupID, "req-"+string(rune('a'+i)), i).Error; err != nil {
			t.Fatalf("insert item: %v", err)
		}
	}
}

// TestMigrateLegacyGroups is the upgrade path for an existing install:
// the old group becomes a provider, its requests point at that provider,
// and no prompt or credential is lost.
func TestMigrateLegacyGroups(t *testing.T) {
	e := newMigrateEnv(t)
	e.insertLegacy(t, "grp-1", "legacy", "sk-legacy-secret", 3)
	ctx := context.Background()

	res, err := MigrateLegacyGroups(ctx, e.db, e.prov, testLogger())
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if res.GroupsMigrated != 1 || res.RequestsMigrated != 3 || res.Orphaned != 0 {
		t.Fatalf("result = %+v, want 1 group / 3 requests / 0 orphaned", res)
	}

	requests, err := e.store.ListRequests(ctx)
	if err != nil {
		t.Fatalf("list requests: %v", err)
	}
	if len(requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(requests))
	}
	pid := requests[0].ProviderID
	if pid == "" {
		t.Fatalf("request has no provider id")
	}
	for i := range requests {
		if requests[i].ProviderID != pid {
			t.Fatalf("request %d points at %q, want %q", i, requests[i].ProviderID, pid)
		}
		if requests[i].UserPrompt != "hello ${who}" || requests[i].SystemPrompt != "be brief" {
			t.Fatalf("prompts lost on request %d: %+v", i, requests[i])
		}
		if requests[i].PromptVars["who"] != "world" {
			t.Fatalf("prompt vars lost on request %d: %v", i, requests[i].PromptVars)
		}
	}

	// The sealed key must survive the move verbatim, i.e. the provider
	// can still decrypt it with the same master key.
	p, err := e.prov.GetProvider(ctx, pid)
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if p.Name != "legacy" || p.BaseURL != "https://e.test" || p.Model != "gpt-test" {
		t.Fatalf("provider coordinates lost: %+v", p)
	}
	if p.APIKey != "sk-legacy-secret" {
		t.Fatalf("api key = %q, want the legacy key (secret_error=%q)", p.APIKey, p.SecretError)
	}
	if p.OpenAI == nil || p.OpenAI.StreamPath != "/v1/chat/completions" {
		t.Fatalf("protocol block lost: %+v", p.OpenAI)
	}

	// The old tables are gone and rerunning the migration is a no-op.
	if tableExists(ctx, e.db, "request_groups") || tableExists(ctx, e.db, "request_items") {
		t.Fatalf("legacy tables were not dropped")
	}
	again, err := MigrateLegacyGroups(ctx, e.db, e.prov, testLogger())
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if again.GroupsMigrated != 0 || again.RequestsMigrated != 0 {
		t.Fatalf("migration is not idempotent: %+v", again)
	}
	if all, _ := e.store.ListRequests(ctx); len(all) != 3 {
		t.Fatalf("second migration duplicated requests: %d", len(all))
	}
}

// TestMigrateReusesExistingProvider covers the real-world case where the
// operator already registered the upstream by hand before the upgrade:
// the legacy group must map onto that provider, not create a duplicate.
func TestMigrateReusesExistingProvider(t *testing.T) {
	e := newMigrateEnv(t)
	e.insertLegacy(t, "grp-1", "legacy", "sk-old", 2)
	ctx := context.Background()

	manual := &provider.Provider{
		ID:        "prv-manual",
		Name:      "legacy",
		Protocol:  config.ProtocolOpenAI,
		BaseURL:   "https://manual.test",
		APIKey:    "sk-current",
		Model:     "gpt-current",
		TimeoutMS: 30000,
	}
	if err := e.prov.CreateProvider(ctx, manual); err != nil {
		t.Fatalf("create provider: %v", err)
	}

	res, err := MigrateLegacyGroups(ctx, e.db, e.prov, testLogger())
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if res.ProvidersReused != 1 || res.GroupsMigrated != 0 || res.RequestsMigrated != 2 {
		t.Fatalf("result = %+v, want 1 reused / 0 created / 2 requests", res)
	}
	requests, _ := e.store.ListRequests(ctx)
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	for i := range requests {
		if requests[i].ProviderID != "prv-manual" {
			t.Fatalf("request %d points at %q, want prv-manual", i, requests[i].ProviderID)
		}
	}
	all, _ := e.prov.ListProviders(ctx)
	if len(all) != 1 {
		t.Fatalf("providers = %d, want 1 (no duplicate created)", len(all))
	}
}

// TestMigrateNoLegacyTables pins the fresh-install path.
func TestMigrateNoLegacyTables(t *testing.T) {
	e := newMigrateEnv(t)
	if err := e.db.Exec("DROP TABLE request_items; DROP TABLE request_groups;").Error; err != nil {
		t.Fatalf("drop: %v", err)
	}
	res, err := MigrateLegacyGroups(context.Background(), e.db, e.prov, testLogger())
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if res.GroupsMigrated != 0 || res.RequestsMigrated != 0 {
		t.Fatalf("fresh install produced %+v", res)
	}
}
