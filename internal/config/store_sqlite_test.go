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

// sampleTask builds a monitoring task in the new model: it names the AI
// service provider it probes and owns only the task-specific fields.
//
// The upstream coordinates (protocol, URL, credentials, model) live on the
// provider — see internal/provider.
func sampleTask(id, name string) *TaskConfig {
	return &TaskConfig{
		ID:           id,
		Name:         name,
		Enabled:      true,
		ProviderID:   "prv-1",
		Model:        "gpt-test",
		SystemPrompt: "you are helpful",
		UserPrompt:   "say hi ${today}",
		PromptVars:   map[string]string{"today": "2026-01-01"},
		Schedule:     "*/30 * * * * *",
		CanaryGroup:  CanaryGroupLabel,
		Notify: NotifyConfig{
			Enabled: true,
			Channel: NotifyWeCom,
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

	tc := sampleTask("t-1", "prod-openai-gpt4o")
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
	if got.ProviderID != tc.ProviderID {
		t.Fatalf("provider_id: got %q, want %q", got.ProviderID, tc.ProviderID)
	}
	if got.UserPrompt != tc.UserPrompt {
		t.Fatalf("user_prompt: got %q, want %q", got.UserPrompt, tc.UserPrompt)
	}
	if got.PromptVars["today"] != "2026-01-01" {
		t.Fatalf("prompt_vars roundtrip lost: %#v", got.PromptVars)
	}
	if !got.Notify.Enabled || got.Notify.Channel != NotifyWeCom {
		t.Fatalf("notify block roundtrip lost: %+v", got.Notify)
	}
}

// TestTaskConfig_ProviderReferenceRoundTrip pins the provider reference
// to the DB. A task that loses its provider would probe a dangling id and
// fail on every tick.
func TestTaskConfig_ProviderReferenceRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tc := sampleTask("t-1", "provider-ref")
	if err := store.CreateTask(ctx, tc); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.GetTask(ctx, "t-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ProviderID != "prv-1" {
		t.Fatalf("provider_id was lost on read: %q", got.ProviderID)
	}

	// Re-pointing the task must persist too.
	got.ProviderID = "prv-2"
	if err := store.UpdateTask(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	after, err := store.GetTask(ctx, "t-1")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if after.ProviderID != "prv-2" {
		t.Fatalf("provider_id = %q, want prv-2", after.ProviderID)
	}
}

// TestCreateTask_RequiresProvider makes sure a task without a provider is
// rejected: there would be nothing to probe.
func TestCreateTask_RequiresProvider(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	tc := sampleTask("t-1", "no-provider")
	tc.ProviderID = ""
	if err := store.CreateTask(ctx, tc); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err: got %v, want ErrInvalidConfig", err)
	}
}

// TestTaskConfig_NotifySwitchRoundTrip pins the notification switch to the
// DB. The switch used to share the `enabled` column with the task's own
// switch (the embedded struct flattened onto the same column), which
// silently dropped one of the two. It now has its own column.
func TestTaskConfig_NotifySwitchRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tc := sampleTask("t-1", "notify-switch")
	tc.Notify.Enabled = true
	if err := store.CreateTask(ctx, tc); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.GetTask(ctx, "t-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Notify.Enabled {
		t.Fatalf("the notification switch was lost on read")
	}
	if !got.Enabled {
		t.Fatalf("the task's own switch must not be affected by the notification switch")
	}

	// Flipping the notification switch off must not disable the task.
	got.Notify.Enabled = false
	if err := store.UpdateTask(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	after, err := store.GetTask(ctx, "t-1")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if after.Notify.Enabled {
		t.Fatalf("notify_enabled should be false after the update")
	}
	if !after.Enabled {
		t.Fatalf("the task's own switch must survive a notification switch change")
	}
}

// TestFingerprint_DetectsProviderChange ensures the hot-reload watcher
// notices a re-pointed task. Without the provider in the fingerprint,
// switching the upstream would silently not take effect until the process
// restarts.
func TestFingerprint_DetectsProviderChange(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tc := sampleTask("t-1", "fingerprint-provider")
	if err := store.CreateTask(ctx, tc); err != nil {
		t.Fatalf("create: %v", err)
	}
	before, err := store.fingerprintAll(ctx)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}

	tc.ProviderID = "prv-2"
	if err := store.UpdateTask(ctx, tc); err != nil {
		t.Fatalf("update: %v", err)
	}
	after, err := store.fingerprintAll(ctx)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if before["t-1"] == after["t-1"] {
		t.Fatalf("the fingerprint must change when the provider changes")
	}
}

func TestCreateTask_UniqueName(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.CreateTask(ctx, sampleTask("t-1", "duplicate")); err != nil {
		t.Fatalf("first create: %v", err)
	}
	err := store.CreateTask(ctx, sampleTask("t-2", "duplicate"))
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
	tc := sampleTask("t-1", "task")
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
	tc := sampleTask("t-1", "task")
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
	// A bound task claiming protocol=openai but also carrying
	// anthropic.max_tokens must fail validation. The protocol itself comes
	// from the provider at bind time, so the test sets it explicitly.
	tc := sampleTask("t-1", "mixed")
	tc.Protocol = ProtocolOpenAI
	tc.Anthropic = &AnthropicConfig{MaxTokens: 256, ExtraHeadersJSON: `{"X":"y"}`}
	err := tc.ValidateProtocolSegments()
	if err == nil || !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
	if !strings.Contains(err.Error(), "anthropic") {
		t.Fatalf("err msg should mention offending block: %v", err)
	}
}

// TestConfigStore_EncryptionRoundTrip verifies the one secret a task still
// owns — the notification webhook — is sealed with the master key on disk
// and opened again on read.
//
// The upstream api key is not tested here any more: it belongs to the AI
// service provider (internal/provider), which has its own store and its
// own encryption tests.
func TestConfigStore_EncryptionRoundTrip(t *testing.T) {
	store := newTestStore(t)
	key := bytes.Repeat([]byte("k"), 32)
	cipher, err := NewAESCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	store.SetCipher(cipher)
	ctx := context.Background()
	tc := sampleTask("t-1", "encrypted-task")
	tc.Notify.WeComWebhook = "https://wecom.test/super-secret"
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
		Select("notify_wecom_webhook_enc as enc").
		Where("id = ?", "t-1").
		Scan(&got).Error; err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got.Enc) == 0 {
		t.Fatalf("notify_wecom_webhook_enc column is empty")
	}
	if bytes.Contains(got.Enc, []byte("super-secret")) {
		t.Fatalf("plaintext leaked to disk: %x", got.Enc)
	}
	// Reading via the API should yield the decrypted secret.
	loaded, err := store.GetTask(ctx, "t-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if loaded.Notify.WeComWebhook != "https://wecom.test/super-secret" {
		t.Fatalf("decrypted webhook: got %q", loaded.Notify.WeComWebhook)
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
	if err := store.CreateTask(ctx, sampleTask("t-late", "late")); err != nil {
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
	if err := store.CreateTask(context.Background(), sampleTask("t-1", "warm")); err != nil {
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
