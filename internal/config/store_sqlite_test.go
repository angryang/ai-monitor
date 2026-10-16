package config

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *SqliteStore {
	t.Helper()
	store, err := NewSqliteStore(SqliteOptions{Path: ":memory:"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func sampleOpenAI(id, name string) *TaskConfig {
	return &TaskConfig{
		ID:          id,
		Name:        name,
		Enabled:     true,
		Protocol:    ProtocolOpenAI,
		BaseURL:     "https://api.openai.com",
		APIKey:      "sk-test",
		APIKeyEnc:   []byte("encrypted"),
		Model:       "gpt-4o",
		SystemPrompt: "you are helpful",
		UserPrompt:  "say hi ${today}",
		PromptVars:  map[string]string{"today": "2026-01-01"},
		Schedule:    "*/30 * * * * *",
		Timeout:     30 * time.Second,
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
	if err := store.db.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='tasks'").Scan(&n).Error; err != nil {
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
