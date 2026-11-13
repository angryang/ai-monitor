// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package server

// Monitoring tasks no longer have a system prompt. These tests pin both
// halves of that contract on the /admin/tasks API:
//
//   - a legacy client that still sends system_prompt is not rejected
//     (readJSON rejects unknown fields), but the value is discarded;
//   - the field is never echoed back, and editing a legacy row clears the
//     value that used to sit in the SQLite column.
//
// AI request groups are unaffected: they use /admin/request-* endpoints.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/angryang/ai-monitor/internal/config"
)

func newTaskHandlersForPromptTest(t *testing.T) (*AdminTaskHandlers, *config.SqliteStore) {
	t.Helper()
	store, err := config.NewSqliteStore(config.SqliteOptions{Path: ":memory:"})
	if err != nil {
		t.Fatalf("task store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cipher, err := config.NewAESCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	store.SetCipher(cipher)
	// A monitoring task names the provider it probes, so the handler needs
	// a registry to validate that reference.
	providers := newMemProviderStore(alphaProvider())
	return NewAdminTaskHandlers(&AdminTaskHandlersConfig{
		Store:     store,
		Cipher:    cipher,
		Providers: providers,
		Logger:    func(string, ...any) {},
	}), store
}

func decodeTaskObject(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v — body=%s", err, rec.Body.String())
	}
	return out
}

func TestTaskHandlers_CreateIgnoresSystemPrompt(t *testing.T) {
	h, store := newTaskHandlersForPromptTest(t)

	// system_prompt is deliberately present: old clients must still parse,
	// while the stored task and the wire response must not carry it.
	body := `{"name":"legacy","provider_id":"prv-alpha","model":"gpt-test","user_prompt":"hi",` +
		`"system_prompt":"you are helpful","schedule":"@every 1m",` +
		`"notify":{"enabled":false}}`
	rec := httptest.NewRecorder()
	h.create(rec, httptest.NewRequest("POST", "/admin/tasks", strings.NewReader(body)))
	if rec.Code != 201 {
		t.Fatalf("create status = %d, want 201 — body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := decodeTaskObject(t, rec)["system_prompt"]; ok {
		t.Fatal("create response must not echo system_prompt")
	}

	stored, err := store.GetTask(context.Background(), "legacy")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.SystemPrompt != "" {
		t.Fatalf("stored SystemPrompt = %q, want empty", stored.SystemPrompt)
	}

	getReq := httptest.NewRequest("GET", "/admin/tasks/legacy", nil)
	getReq.SetPathValue("id", "legacy")
	getRec := httptest.NewRecorder()
	h.get(getRec, getReq)
	if getRec.Code != 200 {
		t.Fatalf("get status = %d — body=%s", getRec.Code, getRec.Body.String())
	}
	if _, ok := decodeTaskObject(t, getRec)["system_prompt"]; ok {
		t.Fatal("get response must not echo system_prompt")
	}
}

func TestTaskHandlers_PatchClearsLegacySystemPrompt(t *testing.T) {
	h, store := newTaskHandlersForPromptTest(t)

	legacy := seedTask("legacy", false)
	legacy.SystemPrompt = "you are helpful"
	if err := store.CreateTask(context.Background(), &legacy); err != nil {
		t.Fatalf("seed legacy task: %v", err)
	}

	// A partial update (name only) must not resurrect the old value, and an
	// explicit system_prompt from an old client must not set a new one.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PATCH", "/admin/tasks/legacy",
		strings.NewReader(`{"name":"legacy","system_prompt":"new system"}`))
	req.SetPathValue("id", "legacy")
	h.patch(rec, req)
	if rec.Code != 200 {
		t.Fatalf("patch status = %d — body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := decodeTaskObject(t, rec)["system_prompt"]; ok {
		t.Fatal("patch response must not echo system_prompt")
	}
	stored, err := store.GetTask(context.Background(), "legacy")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.SystemPrompt != "" {
		t.Fatalf("stored SystemPrompt after patch = %q, want empty", stored.SystemPrompt)
	}
}
