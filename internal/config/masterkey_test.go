// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package config

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestMasterKeyMismatch_ListTasksLenientKeepsTask is the regression test for
// the startup crash "config: decrypt api_key: cipher: message authentication
// failed".
//
// The scenario: a task was encrypted with master key A, the process later
// starts with master key B (rotated key, or a DB copied from another
// instance). The strict ListTasks aborts, which used to take the whole
// service down at startup. The lenient variant must keep the task so the
// operator can repair it from the admin UI.
func TestMasterKeyMismatch_ListTasksLenientKeepsTask(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	keyA := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	cipherA, err := NewAESCipher(keyA)
	if err != nil {
		t.Fatalf("cipher A: %v", err)
	}
	store.SetCipher(cipherA)

	tc := sampleOpenAI("t-key", "prod")
	if err := store.CreateTask(ctx, tc); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Restart with a different master key.
	keyB := []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	cipherB, err := NewAESCipher(keyB)
	if err != nil {
		t.Fatalf("cipher B: %v", err)
	}
	store.SetCipher(cipherB)

	// Strict listing fails — this is the original bug.
	if _, err := store.ListTasks(ctx); err == nil {
		t.Fatal("ListTasks should fail when the master key does not match")
	} else if !errors.Is(err, ErrMasterKeyMismatch) {
		t.Fatalf("ListTasks error should wrap ErrMasterKeyMismatch, got %v", err)
	}

	// Lenient listing keeps the row and flags it.
	tasks, secretErrs, err := store.ListTasksLenient(ctx)
	if err != nil {
		t.Fatalf("ListTasksLenient should not fail for a per-row secret problem: %v", err)
	}
	if len(secretErrs) != 1 {
		t.Fatalf("expected 1 secret error, got %d (%v)", len(secretErrs), secretErrs)
	}
	if !strings.Contains(secretErrs[0].Error(), "prod") {
		t.Errorf("secret error should name the task, got %v", secretErrs[0])
	}
	if len(tasks) != 1 {
		t.Fatalf("expected the task to be kept, got %d", len(tasks))
	}
	got := tasks[0]
	if got.SecretError == "" {
		t.Error("SecretError should be set on the returned task")
	}
	if got.APIKey != "" {
		t.Errorf("APIKey should be empty when it could not be decrypted, got %q", got.APIKey)
	}
	if got.Runnable() {
		t.Error("a task with undecryptable secrets must not be runnable")
	}
	// The non-secret fields survive, so the task can be repaired in place.
	if got.BaseURL != tc.BaseURL || got.Model != tc.Model || got.Schedule != tc.Schedule {
		t.Errorf("non-secret fields were lost: %+v", got)
	}
}

// TestMasterKeyMismatch_GetTaskLenientKeepsTask covers the single-task
// read path: the operator must be able to open the edit form to re-enter the
// api key.
func TestMasterKeyMismatch_GetTaskLenientKeepsTask(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	cipherA, err := NewAESCipher([]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatalf("cipher A: %v", err)
	}
	store.SetCipher(cipherA)
	if err := store.CreateTask(ctx, sampleOpenAI("t-key2", "prod")); err != nil {
		t.Fatalf("create: %v", err)
	}

	cipherB, err := NewAESCipher([]byte("cccccccccccccccccccccccccccccccc"))
	if err != nil {
		t.Fatalf("cipher B: %v", err)
	}
	store.SetCipher(cipherB)

	if _, err := store.GetTask(ctx, "t-key2"); err == nil {
		t.Fatal("GetTask should fail when the master key does not match")
	} else if !errors.Is(err, ErrMasterKeyMismatch) {
		t.Fatalf("GetTask error should wrap ErrMasterKeyMismatch, got %v", err)
	}

	got, err := store.GetTaskLenient(ctx, "t-key2")
	if err != nil {
		t.Fatalf("GetTaskLenient: %v", err)
	}
	if got.SecretError == "" {
		t.Error("SecretError should be set")
	}
	if got.APIKey != "" {
		t.Errorf("APIKey should be empty, got %q", got.APIKey)
	}
	if got.BaseURL == "" {
		t.Error("non-secret fields should survive")
	}
}

// TestMasterKeyMismatch_ReEnterAPIKeyRecoversTask is the happy path: once
// the operator supplies the secret again it is re-sealed with the current
// master key and the task becomes runnable.
func TestMasterKeyMismatch_ReEnterAPIKeyRecoversTask(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	cipherA, err := NewAESCipher([]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatalf("cipher A: %v", err)
	}
	store.SetCipher(cipherA)
	if err := store.CreateTask(ctx, sampleOpenAI("t-key3", "prod")); err != nil {
		t.Fatalf("create: %v", err)
	}

	cipherB, err := NewAESCipher([]byte("dddddddddddddddddddddddddddddddd"))
	if err != nil {
		t.Fatalf("cipher B: %v", err)
	}
	store.SetCipher(cipherB)

	// Operator re-enters the api key via the normal update path.
	row, err := store.GetTaskLenient(ctx, "t-key3")
	if err != nil {
		t.Fatalf("GetTaskLenient: %v", err)
	}
	row.APIKey = "sk-recovered"
	if err := store.UpdateTask(ctx, row); err != nil {
		t.Fatalf("update: %v", err)
	}

	// Now the task decrypts cleanly with the current key.
	tasks, err := store.ListTasks(ctx)
	if err != nil {
		t.Fatalf("ListTasks after recovery: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].APIKey != "sk-recovered" {
		t.Errorf("APIKey = %q, want %q", tasks[0].APIKey, "sk-recovered")
	}
	if !tasks[0].Runnable() {
		t.Error("task should be runnable again after the secret was re-entered")
	}
}

// TestRunnableTasks_FiltersUndecryptable guards the scheduler input: tasks
// without usable credentials must never reach the cron scheduler, otherwise
// every tick would fail and notify.
func TestRunnableTasks_FiltersUndecryptable(t *testing.T) {
	good := TaskConfig{ID: "ok", Name: "ok"}
	bad := TaskConfig{ID: "bad", Name: "bad", SecretError: "boom"}
	got := RunnableTasks([]TaskConfig{good, bad})
	if len(got) != 1 || got[0].ID != "ok" {
		t.Fatalf("RunnableTasks = %+v, want only the healthy task", got)
	}
}

// TestListTasksLenient_ReportsListFailure is the store-side half of the
// "前端查询不到已经创建的任务" regression.
//
// A failure of the listing itself must be reported through the error
// return, not as "zero tasks". Callers that conflate the two turn a
// database hiccup into "every task disappeared".
func TestListTasksLenient_ReportsListFailure(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.CreateTask(ctx, sampleOpenAI("t-listfail", "prod")); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Closing the store makes every subsequent query fail.
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	tasks, secretErrs, err := store.ListTasksLenient(ctx)
	if err == nil {
		t.Fatalf("ListTasksLenient must report the failure; got tasks=%d secretErrs=%v", len(tasks), secretErrs)
	}
	if len(tasks) != 0 {
		t.Errorf("tasks should be empty when the listing failed, got %d", len(tasks))
	}
}
