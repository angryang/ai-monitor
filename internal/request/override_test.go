// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package request

import (
	"context"
	"errors"
	"testing"
)

func strPtr(s string) *string { return &s }

// TestContentOverride_ReachesTaskConfig pins the core requirement: the
// content the operator typed for this run is what the probe actually
// sends, not the saved template.
func TestContentOverride_ReachesTaskConfig(t *testing.T) {
	svc, _, prober, _, ids := seedService(t, 1)

	batch, err := svc.RunRequests(context.Background(), ids[:1], RunOptions{
		Overrides: map[string]ContentOverride{
			ids[0]: {
				SystemPrompt: strPtr("you are a pirate"),
				UserPrompt:   strPtr("say ${greeting}"),
				PromptVars:   map[string]string{"greeting": "ahoy"},
			},
		},
	})
	if err != nil {
		t.Fatalf("run requests: %v", err)
	}
	waitBatch(t, svc, batch.ID)

	calls, _ := prober.snapshot()
	if len(calls) != 1 {
		t.Fatalf("prober calls = %d, want 1", len(calls))
	}
	tc := calls[0].task
	if tc.UserPrompt != "say ${greeting}" {
		t.Fatalf("user prompt = %q, want the custom one", tc.UserPrompt)
	}
	if tc.SystemPrompt != "you are a pirate" {
		t.Fatalf("system prompt = %q, want the custom one", tc.SystemPrompt)
	}
	if tc.PromptVars["greeting"] != "ahoy" {
		t.Fatalf("prompt vars = %v, want the custom ones", tc.PromptVars)
	}
}

// TestContentOverride_NotPersisted pins that an override is strictly
// one-shot: the saved request is untouched, and the next run without an
// override goes back to the template.
func TestContentOverride_NotPersisted(t *testing.T) {
	svc, store, prober, _, ids := seedService(t, 1)

	batch, err := svc.RunRequests(context.Background(), ids[:1], RunOptions{
		Overrides: map[string]ContentOverride{
			ids[0]: {UserPrompt: strPtr("one-off question")},
		},
	})
	if err != nil {
		t.Fatalf("run requests: %v", err)
	}
	waitBatch(t, svc, batch.ID)

	stored, err := store.GetRequest(context.Background(), ids[0])
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	if stored.UserPrompt != "hello" {
		t.Fatalf("stored user prompt = %q, want the untouched template %q", stored.UserPrompt, "hello")
	}

	second, err := svc.RunRequests(context.Background(), ids[:1], RunOptions{})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	waitBatch(t, svc, second.ID)
	calls, _ := prober.snapshot()
	if len(calls) != 2 {
		t.Fatalf("prober calls = %d, want 2", len(calls))
	}
	if calls[0].task.UserPrompt != "one-off question" {
		t.Fatalf("first run prompt = %q, want the custom one", calls[0].task.UserPrompt)
	}
	if calls[1].task.UserPrompt != "hello" {
		t.Fatalf("second run prompt = %q, want the stored template", calls[1].task.UserPrompt)
	}
}

// TestContentOverride_PerRequest pins that a batch can carry a different
// custom content for every selected request — including requests that
// point at different providers.
func TestContentOverride_PerRequest(t *testing.T) {
	svc, _, prober, _, ids := seedService(t, 3)

	batch, err := svc.RunRequests(context.Background(), ids,
		RunOptions{Overrides: map[string]ContentOverride{
			ids[0]:  {UserPrompt: strPtr("question A")},
			ids[2]:  {UserPrompt: strPtr("question C")},
			"ghost": {UserPrompt: strPtr("should be ignored")},
		}})
	if err != nil {
		t.Fatalf("run requests: %v", err)
	}
	waitBatch(t, svc, batch.ID)

	calls, _ := prober.snapshot()
	if len(calls) != 3 {
		t.Fatalf("prober calls = %d, want 3", len(calls))
	}
	byRequest := map[string]string{}
	for _, c := range calls {
		byRequest[c.opts.AIRequestID] = c.task.UserPrompt
	}
	if byRequest[ids[0]] != "question A" || byRequest[ids[2]] != "question C" {
		t.Fatalf("custom prompts not applied per request: %v", byRequest)
	}
	if byRequest[ids[1]] != "hello" {
		t.Fatalf("request without an override = %q, want the template", byRequest[ids[1]])
	}
}

// TestContentOverride_EmptyVarsClears pins that an explicit empty map
// clears the template's variables for this run only.
func TestContentOverride_EmptyVarsClears(t *testing.T) {
	svc, store, prober, _, ids := seedService(t, 1)
	stored, err := store.GetRequest(context.Background(), ids[0])
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	stored.PromptVars = map[string]string{"who": "world"}
	if err := store.UpdateRequest(context.Background(), stored); err != nil {
		t.Fatalf("update request: %v", err)
	}

	batch, err := svc.RunRequests(context.Background(), ids[:1], RunOptions{
		Overrides: map[string]ContentOverride{
			ids[0]: {UserPrompt: strPtr("hi ${who}"), PromptVars: map[string]string{}},
		},
	})
	if err != nil {
		t.Fatalf("run requests: %v", err)
	}
	waitBatch(t, svc, batch.ID)

	calls, _ := prober.snapshot()
	if len(calls) != 1 {
		t.Fatalf("prober calls = %d, want 1", len(calls))
	}
	if len(calls[0].task.PromptVars) != 0 {
		t.Fatalf("prompt vars = %v, want cleared for this run", calls[0].task.PromptVars)
	}
	// The template still has its variable.
	again, err := store.GetRequest(context.Background(), ids[0])
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	if again.PromptVars["who"] != "world" {
		t.Fatalf("stored prompt vars = %v, want untouched", again.PromptVars)
	}
}

// TestContentOverride_BlankUserPromptRejected pins the input validation:
// an empty custom prompt must fail the whole batch before anything is
// fired, rather than sending a blank request upstream.
func TestContentOverride_BlankUserPromptRejected(t *testing.T) {
	svc, _, prober, _, ids := seedService(t, 1)

	_, err := svc.RunRequests(context.Background(), ids[:1], RunOptions{
		Overrides: map[string]ContentOverride{
			ids[0]: {UserPrompt: strPtr("   ")},
		},
	})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
	calls, _ := prober.snapshot()
	if len(calls) != 0 {
		t.Fatalf("prober was called %d times for an invalid override; want 0", len(calls))
	}
}

// TestContentOverride_SystemPromptOnly pins that a partial override keeps
// the stored user prompt — pointers, not zero values, decide what changes.
func TestContentOverride_SystemPromptOnly(t *testing.T) {
	svc, _, prober, _, ids := seedService(t, 1)

	batch, err := svc.RunRequests(context.Background(), ids[:1], RunOptions{
		Overrides: map[string]ContentOverride{
			ids[0]: {SystemPrompt: strPtr("")},
		},
	})
	if err != nil {
		t.Fatalf("run requests: %v", err)
	}
	waitBatch(t, svc, batch.ID)

	calls, _ := prober.snapshot()
	if len(calls) != 1 {
		t.Fatalf("prober calls = %d, want 1", len(calls))
	}
	if calls[0].task.UserPrompt != "hello" {
		t.Fatalf("user prompt = %q, want the stored one", calls[0].task.UserPrompt)
	}
}
