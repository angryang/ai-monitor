// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/probe"
)

// recordingProber keeps the exact task config the service assembled, so
// the HTTP tests can prove that custom content reached the upstream
// boundary (and that the stored template did not change).
type recordingProber struct {
	mu    sync.Mutex
	tasks []config.TaskConfig
}

func (p *recordingProber) RunManual(_ context.Context, t config.TaskConfig, _ probe.RunOptions) probe.Result {
	p.mu.Lock()
	p.tasks = append(p.tasks, t)
	p.mu.Unlock()
	return probe.Result{
		TaskID:    t.ID,
		Status:    probe.StatusSuccess,
		StartedAt: time.Now().UTC(),
		EndedAt:   time.Now().UTC(),
	}
}

func (p *recordingProber) snapshot() []config.TaskConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]config.TaskConfig, len(p.tasks))
	copy(out, p.tasks)
	return out
}

// seedRunFixture creates flat requests with the given predefined prompts.
func seedRunFixture(t *testing.T, env *requestTestEnv, prompts ...string) []string {
	t.Helper()
	ids := make([]string, 0, len(prompts))
	for i, prompt := range prompts {
		providerID := "prv-alpha"
		if i%2 == 1 {
			providerID = "prv-beta"
		}
		model := "model-alpha"
		if i%2 == 1 {
			model = "model-beta"
		}
		rec := do(env.h.create, "POST", "/admin/requests",
			`{"name":"req-`+string(rune('a'+i))+`","provider_id":"`+providerID+`","model":"`+model+`","user_prompt":"`+prompt+`"}`, nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create request: %d — %s", rec.Code, rec.Body.String())
		}
		ids = append(ids, mustJSON(t, rec)["id"].(string))
	}
	return ids
}

func waitForBatchDone(t *testing.T, h *AdminRequestHandlers, batchID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec := do(h.getBatch, "GET", "/admin/request-batches/"+batchID, "",
			map[string]string{"id": batchID})
		if rec.Code == http.StatusOK && mustJSON(t, rec)["state"] == "done" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("batch %s did not finish", batchID)
}

// TestRequestHandlers_RunCustomContent pins the end-to-end wire contract:
// custom content sent with the run request is what the probe sees, while
// the predefined request stays untouched in the store.
func TestRequestHandlers_RunCustomContent(t *testing.T) {
	prober := &recordingProber{}
	env := newRequestHandlersWithProber(t, prober)
	ids := seedRunFixture(t, env, "hello")
	id := ids[0]

	body := `{"request_ids":["` + id + `"],"overrides":{"` + id + `":{` +
		`"system_prompt":"be brief","user_prompt":"say ${word}","prompt_vars":{"word":"ahoy"}}}}`
	rec := do(env.h.run, "POST", "/admin/requests/run", body, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status: got %d, want 202 — body=%s", rec.Code, rec.Body.String())
	}
	waitForBatchDone(t, env.h, mustJSON(t, rec)["batch_id"].(string))

	tasks := prober.snapshot()
	if len(tasks) != 1 {
		t.Fatalf("prober calls = %d, want 1", len(tasks))
	}
	if tasks[0].UserPrompt != "say ${word}" {
		t.Fatalf("user prompt = %q, want the custom one", tasks[0].UserPrompt)
	}
	if tasks[0].SystemPrompt != "be brief" {
		t.Fatalf("system prompt = %q, want the custom one", tasks[0].SystemPrompt)
	}
	if tasks[0].PromptVars["word"] != "ahoy" {
		t.Fatalf("prompt vars = %v, want the custom ones", tasks[0].PromptVars)
	}

	// The override must never be written back to the predefined request.
	stored, err := env.requests.GetRequest(context.Background(), id)
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	if stored.UserPrompt != "hello" {
		t.Fatalf("stored user prompt = %q, want the untouched template", stored.UserPrompt)
	}
	got := do(env.h.get, "GET", "/admin/requests/"+id, "", map[string]string{"id": id})
	if !strings.Contains(got.Body.String(), `"user_prompt":"hello"`) {
		t.Fatalf("request view does not show the original prompt: %s", got.Body.String())
	}
}

// TestRequestHandlers_RunMultiProviderCustomContent pins that a batch
// spanning several providers still applies a per-request override, and
// that an unmentioned request keeps its own predefined content.
func TestRequestHandlers_RunMultiProviderCustomContent(t *testing.T) {
	prober := &recordingProber{}
	env := newRequestHandlersWithProber(t, prober)
	ids := seedRunFixture(t, env, "hello", "world")

	body := `{"request_ids":["` + ids[0] + `","` + ids[1] + `"],` +
		`"overrides":{"` + ids[1] + `":{"user_prompt":"custom for the second"}}}`
	rec := do(env.h.run, "POST", "/admin/requests/run", body, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status: got %d, want 202 — body=%s", rec.Code, rec.Body.String())
	}
	waitForBatchDone(t, env.h, mustJSON(t, rec)["batch_id"].(string))

	tasks := prober.snapshot()
	if len(tasks) != 2 {
		t.Fatalf("prober calls = %d, want 2", len(tasks))
	}
	prompts := map[string]bool{}
	urls := map[string]bool{}
	for _, tc := range tasks {
		prompts[tc.UserPrompt] = true
		urls[tc.BaseURL] = true
	}
	if !prompts["custom for the second"] || !prompts["hello"] {
		t.Fatalf("batch did not apply the per-request override: %v", prompts)
	}
	if len(urls) != 2 {
		t.Fatalf("batch reached %d upstreams, want 2: %v", len(urls), urls)
	}
}

// TestRequestHandlers_RunRejectsBlankOverride pins that a blank custom
// prompt is rejected before anything is fired.
func TestRequestHandlers_RunRejectsBlankOverride(t *testing.T) {
	prober := &recordingProber{}
	env := newRequestHandlersWithProber(t, prober)
	ids := seedRunFixture(t, env, "hello")

	body := `{"request_ids":["` + ids[0] + `"],"overrides":{"` + ids[0] + `":{"user_prompt":"   "}}}`
	rec := do(env.h.run, "POST", "/admin/requests/run", body, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 — body=%s", rec.Code, rec.Body.String())
	}
	if len(prober.snapshot()) != 0 {
		t.Fatalf("a blank override still fired %d request(s)", len(prober.snapshot()))
	}
}

// TestRequestHandlers_RunUnknownField pins that the strict decoder still
// rejects typos in the run payload.
func TestRequestHandlers_RunUnknownField(t *testing.T) {
	env := newRequestHandlers(t)
	ids := seedRunFixture(t, env, "hello")

	body := `{"request_ids":["` + ids[0] + `"],"user_prompts":"typo"}`
	rec := do(env.h.run, "POST", "/admin/requests/run", body, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 — body=%s", rec.Code, rec.Body.String())
	}
}

// TestRequestHandlers_RunEmptyBodyIsRejected pins that calling run with
// no selection is a clear 400 rather than an obscure server error.
func TestRequestHandlers_RunEmptyBodyIsRejected(t *testing.T) {
	env := newRequestHandlers(t)
	rec := do(env.h.run, "POST", "/admin/requests/run", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 — body=%s", rec.Code, rec.Body.String())
	}
}
