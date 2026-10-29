// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package config

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *SqliteStore {
	t.Helper()
	store, err := NewSqliteStore(SqliteOptions{Path: ":memory:", PollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func sampleOpenAI(id, name string) *TaskConfig {
	return &TaskConfig{
		ID:           id,
		Name:         name,
		Enabled:      true,
		Protocol:     ProtocolOpenAI,
		BaseURL:      "https://api.openai.com",
		APIKey:       "sk-test",
		Model:        "gpt-4o",
		SystemPrompt: "you are helpful",
		UserPrompt:   "say hi ${today}",
		PromptVars:   map[string]string{"today": "2026-01-01"},
		Schedule:     "*/30 * * * * *",
		Timeout:      30 * time.Second,
		OpenAI: &OpenAIConfig{
			StreamPath:       "/v1/chat/completions",
			ExtraHeadersJSON: `{"X-Foo":"bar"}`,
		},
	}
}

func TestAutoMigrate_CreatesTasksTable(t *testing.T) {
	store := newTestStore(t)
	// AutoMigrate is idempotent; calling it again is harmless and proves the
	// schema landed in the file.
	if err := store.db.AutoMigrate(&TaskConfig{}); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	// Check table existence via COUNT(*) on sqlite_master.
	var n int64
	if err := store.db.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", TaskConfigTableName).Scan(&n).Error; err != nil {
		t.Fatalf("sqlite_master lookup: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected tasks table to exist, got %d row(s)", n)
	}
}

func TestTaskConfig_RoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tc := sampleOpenAI("t-1", "prod-openai-gpt4o")
	if err := store.CreateTask(ctx, tc); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.GetTask(ctx, "t-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != tc.Name {
		t.Fatalf("name: got %q, want %q", got.Name, tc.Name)
	}
	if got.Protocol != ProtocolOpenAI {
		t.Fatalf("protocol: got %q, want openai", got.Protocol)
	}
	if got.OpenAI == nil || got.OpenAI.StreamPath != "/v1/chat/completions" {
		t.Fatalf("openai block roundtrip lost: %#v", got.OpenAI)
	}
}

// TestTaskConfig_InsecureSkipVerifyRoundTrip pins the TLS switch to the
// DB — a task that opts out of certificate verification must survive a
// restart, otherwise the probe silently reverts to verifying and starts
// failing again with an x509 error.
func TestTaskConfig_InsecureSkipVerifyRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tc := sampleOpenAI("t-1", "self-signed-upstream")
	tc.InsecureSkipVerify = true
	if err := store.CreateTask(ctx, tc); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.GetTask(ctx, "t-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.InsecureSkipVerify {
		t.Fatalf("insecure_skip_verify was lost on read")
	}

	// Flipping it back off must persist too.
	got.InsecureSkipVerify = false
	if err := store.UpdateTask(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	after, err := store.GetTask(ctx, "t-1")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if after.InsecureSkipVerify {
		t.Fatalf("insecure_skip_verify should be false after the update")
	}
}

// TestTaskConfig_InsecureSkipVerifyDefaultsToFalse guards the safe
// default: verification stays on unless someone explicitly opts out.
func TestTaskConfig_InsecureSkipVerifyDefaultsToFalse(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tc := sampleOpenAI("t-1", "default-tls")
	if err := store.CreateTask(ctx, tc); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.GetTask(ctx, "t-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.InsecureSkipVerify {
		t.Fatalf("insecure_skip_verify must default to false")
	}
}

// TestFingerprint_DetectsInsecureSkipVerifyChange ensures the hot-reload
// watcher notices a flipped TLS switch. Without the column in the
// fingerprint, flipping the flag in the admin UI would silently not take
// effect until the process restarts.
func TestFingerprint_DetectsInsecureSkipVerifyChange(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tc := sampleOpenAI("t-1", "fingerprint-tls")
	if err := store.CreateTask(ctx, tc); err != nil {
		t.Fatalf("create: %v", err)
	}
	before, err := store.fingerprintAll(ctx)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}

	tc.InsecureSkipVerify = true
	if err := store.UpdateTask(ctx, tc); err != nil {
		t.Fatalf("update: %v", err)
	}
	after, err := store.fingerprintAll(ctx)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if before["t-1"] == after["t-1"] {
		t.Fatalf("the fingerprint must change when insecure_skip_verify flips")
	}
}

func TestCreateTask_UniqueName(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.CreateTask(ctx, sampleOpenAI("t-1", "duplicate")); err != nil {
		t.Fatalf("first create: %v", err)
	}
	err := store.CreateTask(ctx, sampleOpenAI("t-2", "duplicate"))
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("err: got %v, want ErrAlreadyExists", err)
	}
}

func TestGetTask_NotFound(t *testing.T) {
	store := newTestStore(t)
	_, err := store.GetTask(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err: got %v, want ErrNotFound", err)
	}
}

func TestUpdateTask(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	tc := sampleOpenAI("t-1", "task")
	if err := store.CreateTask(ctx, tc); err != nil {
		t.Fatalf("create: %v", err)
	}
	tc.Enabled = false
	if err := store.UpdateTask(ctx, tc); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := store.GetTask(ctx, "t-1")
	if got.Enabled {
		t.Fatalf("Enabled: got true, want false")
	}
}

func TestDeleteTask(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	tc := sampleOpenAI("t-1", "task")
	if err := store.CreateTask(ctx, tc); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.DeleteTask(ctx, "t-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.DeleteTask(ctx, "t-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: got %v, want ErrNotFound", err)
	}
}

func TestValidateProtocolSegments(t *testing.T) {
	// A task claiming protocol=openai but also carrying anthropic.max_tokens
	// must fail validation.
	tc := sampleOpenAI("t-1", "mixed")
	tc.Anthropic = &AnthropicConfig{MaxTokens: 256, ExtraHeadersJSON: `{"X":"y"}`}
	err := tc.ValidateProtocolSegments()
	if err == nil || !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
	if !strings.Contains(err.Error(), "anthropic") {
		t.Fatalf("err msg should mention offending block: %v", err)
	}
}

func TestConfigStore_EncryptionRoundTrip(t *testing.T) {
	store := newTestStore(t)
	key := bytes.Repeat([]byte("k"), 32)
	cipher, err := NewAESCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	store.SetCipher(cipher)
	ctx := context.Background()
	tc := sampleOpenAI("t-1", "encrypted-task")
	tc.APIKey = "sk-supersecret"
	if err := store.CreateTask(ctx, tc); err != nil {
		t.Fatalf("create: %v", err)
	}
	// The blob must contain ciphertext (not plaintext). We just check that
	// the original plaintext does not occur in the ciphertext returned via the
	// raw query — `bytes.Contains` over the []byte the driver emits.
	type rowOut struct {
		Enc []byte
	}
	var got rowOut
	if err := store.db.WithContext(ctx).
		Table(TaskConfigTableName).
		Select("api_key_enc as enc").
		Where("id = ?", "t-1").
		Scan(&got).Error; err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got.Enc) == 0 {
		t.Fatalf("api_key_enc column is empty")
	}
	if bytes.Contains(got.Enc, []byte("sk-supersecret")) {
		t.Fatalf("plaintext leaked to disk: %x", got.Enc)
	}
	// Reading via the API should yield the decrypted secret.
	loaded, err := store.GetTask(ctx, "t-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if loaded.APIKey != "sk-supersecret" {
		t.Fatalf("decrypted key: got %q", loaded.APIKey)
	}
}

func TestConfigStore_Watch(t *testing.T) {
	store := newTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := store.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Add a task after Watch is running; expect an Upsert event.
	if err := store.CreateTask(ctx, sampleOpenAI("t-late", "late")); err != nil {
		t.Fatalf("create: %v", err)
	}

	select {
	case ev := <-ch:
		if ev.Type != EventUpsert || ev.TaskID != "t-late" {
			t.Fatalf("event: got %+v, want upsert/t-late", ev)
		}
	case <-ctx.Done():
		t.Fatalf("timeout waiting for upsert event")
	}

	// Delete should emit a Delete event.
	if err := store.DeleteTask(ctx, "t-late"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Type != EventDelete || ev.TaskID != "t-late" {
			t.Fatalf("event: got %+v, want delete/t-late", ev)
		}
	case <-ctx.Done():
		t.Fatalf("timeout waiting for delete event")
	}
}

func TestConfigStore_Watch_NoInitialSnap(t *testing.T) {
	// Pre-create a task so the snapshot includes it; then update nothing
	// afterwards. The poller must NOT emit any events because no diff exists.
	store := newTestStore(t)
	if err := store.CreateTask(context.Background(), sampleOpenAI("t-1", "warm")); err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	ch, err := store.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	select {
	case ev := <-ch:
		t.Fatalf("unexpected event: %+v", ev)
	case <-ctx.Done():
		// success: no diffs were emitted
	}
}
