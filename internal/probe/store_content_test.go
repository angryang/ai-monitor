// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package probe

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/angryang/ai-monitor/internal/config"
)

// TestStore_ContentColumnsMigrateAndRoundTrip verifies §18.14's schema
// change: AutoMigrate adds the semantic-content columns (and is
// idempotent on a second call), and a row survives a save/list cycle
// with the content intact.
func TestStore_ContentColumnsMigrateAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.db")
	store, err := config.NewSqliteStore(config.SqliteOptions{Path: path})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	rs := NewSqliteRunStore(store.DB())
	if err := rs.AutoMigrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Running the migration twice must be a no-op, not an error.
	if err := rs.AutoMigrate(); err != nil {
		t.Fatalf("migrate again: %v", err)
	}

	m := store.DB().Migrator()
	for _, col := range []string{
		"input_content",
		"thinking_content",
		"output_content",
		"content_truncated",
		"response_error_body",
	} {
		if !m.HasColumn(&ProbeRun{}, col) {
			t.Fatalf("missing column %q after migration", col)
		}
	}

	run := &ProbeRun{
		TaskID:            "t1",
		TaskName:          "n",
		Protocol:          "openai",
		Model:             "m",
		Status:            "success",
		InputContent:      "[user]\n你好",
		ThinkingContent:   "先想一想",
		OutputContent:     "答案",
		ResponseErrorBody: `{"error":"nope"}`,
	}
	if err := rs.Save(context.Background(), run); err != nil {
		t.Fatalf("save: %v", err)
	}

	rows, err := rs.ListByTask(context.Background(), "t1", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	got := rows[0]
	if got.InputContent != run.InputContent {
		t.Fatalf("InputContent = %q, want %q", got.InputContent, run.InputContent)
	}
	if got.ThinkingContent != run.ThinkingContent {
		t.Fatalf("ThinkingContent = %q, want %q", got.ThinkingContent, run.ThinkingContent)
	}
	if got.OutputContent != run.OutputContent {
		t.Fatalf("OutputContent = %q, want %q", got.OutputContent, run.OutputContent)
	}
	if got.ResponseErrorBody != run.ResponseErrorBody {
		t.Fatalf("ResponseErrorBody = %q, want %q", got.ResponseErrorBody, run.ResponseErrorBody)
	}
}
