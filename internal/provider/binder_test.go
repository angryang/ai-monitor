// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/angryang/ai-monitor/internal/config"
)

// taskWithProvider builds a monitoring task that names a provider.
func taskWithProvider(id, providerID string) config.TaskConfig {
	return config.TaskConfig{
		ID:         id,
		Name:       id,
		Enabled:    true,
		ProviderID: providerID,
		UserPrompt: "hi",
		Schedule:   "@every 1m",
	}
}

// TestBindTask_FillsUpstreamCoordinates is the core contract: a task only
// names a provider, and the binder hands the probe layer everything it
// needs.
func TestBindTask_FillsUpstreamCoordinates(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	p := seedProvider(t, store, "alpha")
	p.TimeoutMS = 45000
	p.InsecureSkipVerify = true
	p.ThinkingEnabled = true
	// The provider offers several models; the task picks its own.
	p.Models = []string{"gpt-test", "gpt-4o"}
	p.OpenAI = &config.OpenAIConfig{StreamPath: "/v1/chat/completions"}
	if err := store.UpdateProvider(ctx, p); err != nil {
		t.Fatalf("update provider: %v", err)
	}

	binder := NewBinder(store)
	task := taskWithProvider("t-1", p.ID)
	task.Model = "gpt-4o" // the task's own selection, not the provider's primary
	bound, err := binder.BindTask(ctx, task)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if bound.Protocol != config.ProtocolOpenAI {
		t.Fatalf("protocol = %q, want openai", bound.Protocol)
	}
	if bound.BaseURL != p.BaseURL {
		t.Fatalf("base_url = %q, want %q", bound.BaseURL, p.BaseURL)
	}
	if bound.Model != task.Model {
		t.Fatalf("model = %q, want the task's own selection %q", bound.Model, task.Model)
	}
	if bound.APIKey != "sk-secret" {
		t.Fatalf("api key = %q, want the provider's key", bound.APIKey)
	}
	if bound.EffectiveTimeout() != 45000*1000*1000 {
		t.Fatalf("timeout = %v, want 45s", bound.EffectiveTimeout())
	}
	if !bound.InsecureSkipVerify || !bound.ThinkingEnabled {
		t.Fatalf("switches not inherited: %+v", bound)
	}
	if bound.OpenAI == nil || bound.OpenAI.StreamPath != "/v1/chat/completions" {
		t.Fatalf("protocol block not inherited: %+v", bound.OpenAI)
	}
	// Only the matching block may be carried over: the validator rejects a
	// task that sets more than one.
	if bound.Anthropic != nil || bound.Gemini != nil {
		t.Fatalf("foreign protocol blocks were copied: %+v", bound)
	}
	if err := bound.ValidateProtocolSegments(); err != nil {
		t.Fatalf("the bound task must pass validation: %v", err)
	}
}

// TestBindTask_RejectsModelNotOffered pins the new multi-model rule: a
// task may only run against a model the provider still offers. Dropping a
// model from the provider must make the task fail to bind with ErrModel
// rather than silently probing a model that no longer exists.
func TestBindTask_RejectsModelNotOffered(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	p := seedProvider(t, store, "alpha")
	p.Models = []string{"gpt-test"} // only one candidate
	if err := store.UpdateProvider(ctx, p); err != nil {
		t.Fatalf("update provider: %v", err)
	}

	task := taskWithProvider("t-1", p.ID)
	task.Model = "gpt-4o" // no longer offered
	_, err := NewBinder(store).BindTask(ctx, task)
	if !errors.Is(err, ErrModel) {
		t.Fatalf("err = %v, want ErrModel", err)
	}
}

// TestBindTask_DoesNotMutateInput makes sure the persisted row is left
// alone: the binder works on a copy.
func TestBindTask_DoesNotMutateInput(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	p := seedProvider(t, store, "alpha")

	task := taskWithProvider("t-1", p.ID)
	binder := NewBinder(store)
	if _, err := binder.BindTask(ctx, task); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if task.BaseURL != "" || task.Model != "" || task.APIKey != "" {
		t.Fatalf("the input task was mutated: %+v", task)
	}
}

// TestBindTask_MissingProvider covers the dangling reference: the task
// cannot be probed and the caller must be told why.
func TestBindTask_MissingProvider(t *testing.T) {
	store, _ := newTestStore(t)
	binder := NewBinder(store)
	_, err := binder.BindTask(context.Background(), taskWithProvider("t-1", "prv-gone"))
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("err = %v, want ErrMissing", err)
	}
}

// TestBindTask_NoProviderID covers a task that references nothing at all.
func TestBindTask_NoProviderID(t *testing.T) {
	store, _ := newTestStore(t)
	binder := NewBinder(store)
	_, err := binder.BindTask(context.Background(), taskWithProvider("t-1", ""))
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("err = %v, want ErrMissing", err)
	}
}

// TestBindTask_SecretError covers the master-key mismatch: the provider
// exists but its key cannot be opened.
func TestBindTask_SecretError(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	p := seedProvider(t, store, "alpha")
	// Simulate a key that was sealed with a different master key.
	p.APIKeyEnc = []byte("not-a-valid-ciphertext")
	if err := store.UpdateProvider(ctx, p); err != nil {
		t.Fatalf("update provider: %v", err)
	}
	binder := NewBinder(store)
	_, err := binder.BindTask(ctx, taskWithProvider("t-1", p.ID))
	if !errors.Is(err, ErrSecret) {
		t.Fatalf("err = %v, want ErrSecret", err)
	}
}

// TestBindTask_NoKey covers a provider without any credential.
func TestBindTask_NoKey(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	p := &Provider{
		ID:        "prv-nokey",
		Name:      "nokey",
		Protocol:  config.ProtocolOpenAI,
		BaseURL:   "https://e.test",
		Model:     "gpt-test",
		TimeoutMS: 30000,
	}
	if err := store.CreateProvider(ctx, p); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	binder := NewBinder(store)
	_, err := binder.BindTask(ctx, taskWithProvider("t-1", p.ID))
	if !errors.Is(err, ErrNoKey) {
		t.Fatalf("err = %v, want ErrNoKey", err)
	}
}

// TestBindTasks_SkipsBrokenTasks verifies that one dangling provider does
// not stop the other tasks from being scheduled.
func TestBindTasks_SkipsBrokenTasks(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	p := seedProvider(t, store, "alpha")

	binder := NewBinder(store)
	tasks := []config.TaskConfig{
		taskWithProvider("good", p.ID),
		taskWithProvider("bad", "prv-gone"),
	}
	bound, errs := binder.BindTasks(ctx, tasks)
	if len(bound) != 1 || bound[0].ID != "good" {
		t.Fatalf("bound = %+v, want only the healthy task", bound)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want one error", errs)
	}
	if !errors.Is(errs[0], ErrMissing) {
		t.Fatalf("err = %v, want ErrMissing", errs[0])
	}
}
