// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/probe"
	"github.com/angryang/ai-monitor/internal/provider"
	"github.com/angryang/ai-monitor/internal/request"
)

// stubProber stands in for the probe runner so the handler tests never
// touch the network.
type stubProber struct {
	mu sync.Mutex
	n  int
}

func (s *stubProber) RunManual(_ context.Context, t config.TaskConfig, opts probe.RunOptions) probe.Result {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	return probe.Result{
		TaskID:    t.ID,
		TaskName:  t.Name,
		Protocol:  string(t.Protocol),
		Model:     t.Model,
		Status:    probe.StatusSuccess,
		StartedAt: time.Now().UTC(),
		EndedAt:   time.Now().UTC(),
	}
}

// requestTestEnv is the in-memory wiring used by every handler test.
type requestTestEnv struct {
	h         *AdminRequestHandlers
	requests  *request.SqliteStore
	providers *provider.SqliteStore
	svc       *request.Service
}

// newRequestHandlers wires the whole AI request manager over one in-memory
// SQLite file, with two providers already registered. That makes "a
// request can be pointed at any provider" testable without a network.
func newRequestHandlers(t *testing.T) *requestTestEnv {
	t.Helper()
	return newRequestHandlersWithProber(t, &stubProber{})
}

func newRequestHandlersWithProber(t *testing.T, p request.Prober) *requestTestEnv {
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
	providerStore, err := provider.NewSqliteStore(taskStore.DB())
	if err != nil {
		t.Fatalf("provider store: %v", err)
	}
	providerStore.SetCipher(cipher)
	requestStore, err := request.NewSqliteStore(taskStore.DB())
	if err != nil {
		t.Fatalf("request store: %v", err)
	}
	ctx := context.Background()
	for _, name := range []string{"alpha", "beta"} {
		if err := providerStore.CreateProvider(ctx, &provider.Provider{
			ID:        "prv-" + name,
			Name:      name,
			Protocol:  config.ProtocolOpenAI,
			BaseURL:   "https://" + name + ".test",
			APIKey:    "sk-" + name,
			Models:    []string{"model-" + name},
			TimeoutMS: 30000,
		}); err != nil {
			t.Fatalf("seed provider: %v", err)
		}
	}
	svc := request.NewService(requestStore, p, request.ServiceOptions{
		Providers: providerStore,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(svc.CancelAll)
	return &requestTestEnv{
		h: NewAdminRequestHandlers(&AdminRequestHandlersConfig{
			Store:     requestStore,
			Service:   svc,
			Providers: providerStore,
			Logger:    func(string, ...any) {},
		}),
		requests:  requestStore,
		providers: providerStore,
		svc:       svc,
	}
}

// mustJSON decodes a response body into a map.
func mustJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body, _ := io.ReadAll(rec.Body)
	out := map[string]any{}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v — body=%s", err, string(body))
	}
	return out
}

// do invokes a handler with a JSON body and the path variables set.
func do(h http.HandlerFunc, method, path string, body string, vars map[string]string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range vars {
		r.SetPathValue(k, v)
	}
	h(rec, r)
	return rec
}

// requestBody builds a valid create payload.
func requestBody(overrides map[string]any) map[string]any {
	body := map[string]any{
		"name":        "greeting",
		"provider_id": "prv-alpha",
		"model":       "model-alpha",
		"user_prompt": "hello ${who}",
		"prompt_vars": map[string]string{"who": "world"},
	}
	for k, v := range overrides {
		if v == nil {
			delete(body, k)
			continue
		}
		body[k] = v
	}
	raw, _ := json.Marshal(body)
	return map[string]any{"__raw": string(raw)}
}

func postRequest(t *testing.T, env *requestTestEnv, overrides map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw := requestBody(overrides)["__raw"].(string)
	return do(env.h.create, "POST", "/admin/requests", raw, nil)
}

// TestRequestHandlers_CreateAndList pins the flat CRUD round trip: the
// created request nests its provider, and the plaintext api key never
// leaves the server.
func TestRequestHandlers_CreateAndList(t *testing.T) {
	env := newRequestHandlers(t)

	rec := postRequest(t, env, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201 — body=%s", rec.Code, rec.Body.String())
	}
	out := mustJSON(t, rec)
	if out["name"] != "greeting" {
		t.Fatalf("name = %v, want greeting", out["name"])
	}
	if out["provider_id"] != "prv-alpha" {
		t.Fatalf("provider_id = %v, want prv-alpha", out["provider_id"])
	}
	pv, ok := out["provider"].(map[string]any)
	if !ok {
		t.Fatalf("provider summary missing: %v", out)
	}
	if pv["name"] != "alpha" {
		t.Fatalf("provider summary = %v, want alpha", pv)
	}
	models, _ := pv["models"].([]any)
	if len(models) != 1 || models[0] != "model-alpha" {
		t.Fatalf("provider models = %v, want [model-alpha]", pv["models"])
	}
	if out["model"] != "model-alpha" {
		t.Fatalf("request model = %v, want model-alpha", out["model"])
	}
	if _, leaked := pv["api_key"]; leaked {
		t.Fatalf("provider summary leaked the plaintext api key")
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("no id in create response")
	}

	// List is flat: one array of requests, no group wrapper.
	list := do(env.h.list, "GET", "/admin/requests", "", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d", list.Code)
	}
	lout := mustJSON(t, list)
	items, ok := lout["requests"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("requests = %v, want one row", lout["requests"])
	}

	// Get returns the same shape.
	got := do(env.h.get, "GET", "/admin/requests/"+id, "", map[string]string{"id": id})
	if got.Code != http.StatusOK || mustJSON(t, got)["id"] != id {
		t.Fatalf("get status=%d body=%s", got.Code, got.Body.String())
	}
}

// TestRequestHandlers_DifferentProviders pins the whole point of the
// refactor: two requests in the same flat list may target different
// providers, and each response nests the right one.
func TestRequestHandlers_DifferentProviders(t *testing.T) {
	env := newRequestHandlers(t)

	if rec := postRequest(t, env, nil); rec.Code != http.StatusCreated {
		t.Fatalf("first create: %d %s", rec.Code, rec.Body.String())
	}
	if rec := postRequest(t, env, map[string]any{
		"name": "second", "provider_id": "prv-beta", "model": "model-beta",
	}); rec.Code != http.StatusCreated {
		t.Fatalf("second create: %d %s", rec.Code, rec.Body.String())
	}

	list := mustJSON(t, do(env.h.list, "GET", "/admin/requests", "", nil))
	items, _ := list["requests"].([]any)
	if len(items) != 2 {
		t.Fatalf("requests = %d, want 2", len(items))
	}
	seen := map[string]string{}
	for _, raw := range items {
		row, _ := raw.(map[string]any)
		pv, _ := row["provider"].(map[string]any)
		seen[row["name"].(string)] = pv["name"].(string)
	}
	if seen["greeting"] != "alpha" || seen["second"] != "beta" {
		t.Fatalf("provider mapping = %v, want greeting→alpha, second→beta", seen)
	}
}

// TestRequestHandlers_RejectsUnknownProvider pins that a request cannot
// be saved pointing at a provider that does not exist.
func TestRequestHandlers_RejectsUnknownProvider(t *testing.T) {
	env := newRequestHandlers(t)

	rec := postRequest(t, env, map[string]any{"provider_id": "prv-gone"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — body=%s", rec.Code, rec.Body.String())
	}
	errObj := mustJSON(t, rec)["error"].(map[string]any)
	if errObj["code"] != "unknown_provider" {
		t.Fatalf("error code = %v, want unknown_provider", errObj["code"])
	}
	if n, _ := env.requests.ListRequests(context.Background()); len(n) != 0 {
		t.Fatalf("invalid request was persisted: %v", n)
	}
}

// TestRequestHandlers_RejectsUnknownModel pins that a request may only
// select one of the models its provider actually offers.
func TestRequestHandlers_RejectsUnknownModel(t *testing.T) {
	env := newRequestHandlers(t)

	rec := postRequest(t, env, map[string]any{"model": "model-beta"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — body=%s", rec.Code, rec.Body.String())
	}
	errObj := mustJSON(t, rec)["error"].(map[string]any)
	if errObj["code"] != "unknown_model" {
		t.Fatalf("error code = %v, want unknown_model", errObj["code"])
	}
	if n, _ := env.requests.ListRequests(context.Background()); len(n) != 0 {
		t.Fatalf("invalid request was persisted: %v", n)
	}
}

// TestRequestHandlers_PatchRePointsProvider pins that a request can be
// moved to another provider without re-entering its prompts.
func TestRequestHandlers_PatchRePointsProvider(t *testing.T) {
	env := newRequestHandlers(t)
	created := mustJSON(t, postRequest(t, env, nil))
	id := created["id"].(string)

	rec := do(env.h.patch, "PATCH", "/admin/requests/"+id,
		`{"provider_id":"prv-beta","name":"renamed","model":"model-beta"}`, map[string]string{"id": id})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	out := mustJSON(t, rec)
	if out["provider_id"] != "prv-beta" || out["name"] != "renamed" || out["model"] != "model-beta" {
		t.Fatalf("patched = %v", out)
	}
	if out["user_prompt"] != "hello ${who}" {
		t.Fatalf("user_prompt lost on a partial patch: %v", out["user_prompt"])
	}
	pv := out["provider"].(map[string]any)
	if pv["name"] != "beta" {
		t.Fatalf("nested provider was not refreshed: %v", pv)
	}
}

// TestRequestHandlers_Delete pins removal and the not-found contract.
func TestRequestHandlers_Delete(t *testing.T) {
	env := newRequestHandlers(t)
	created := mustJSON(t, postRequest(t, env, nil))
	id := created["id"].(string)

	rec := do(env.h.delete, "DELETE", "/admin/requests/"+id, "", map[string]string{"id": id})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d", rec.Code)
	}
	again := do(env.h.delete, "DELETE", "/admin/requests/"+id, "", map[string]string{"id": id})
	if again.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", again.Code)
	}
}

// TestRequestHandlers_RunBatch pins the async run endpoint: 202 + batch
// id, then poll to completion.
func TestRequestHandlers_RunBatch(t *testing.T) {
	env := newRequestHandlers(t)
	created := mustJSON(t, postRequest(t, env, nil))
	id := created["id"].(string)

	rec := do(env.h.run, "POST", "/admin/requests/run",
		`{"request_ids":["`+id+`"]}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run status = %d, want 202 — body=%s", rec.Code, rec.Body.String())
	}
	batch := mustJSON(t, rec)
	batchID, _ := batch["batch_id"].(string)
	if batchID == "" {
		t.Fatalf("no batch id in response: %v", batch)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		poll := do(env.h.getBatch, "GET", "/admin/request-batches/"+batchID, "",
			map[string]string{"id": batchID})
		if poll.Code != http.StatusOK {
			t.Fatalf("poll status = %d", poll.Code)
		}
		if mustJSON(t, poll)["state"] == string(request.BatchDone) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("batch %s did not finish", batchID)
}

// TestRequestHandlers_RunRejectsEmptySelection pins the validation.
func TestRequestHandlers_RunRejectsEmptySelection(t *testing.T) {
	env := newRequestHandlers(t)
	rec := do(env.h.run, "POST", "/admin/requests/run", `{"request_ids":[]}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestRequestHandlers_DisabledStore pins that a build without the store
// answers 503 rather than silently accepting writes.
func TestRequestHandlers_DisabledStore(t *testing.T) {
	h := NewAdminRequestHandlers(&AdminRequestHandlersConfig{})
	rec := do(h.list, "GET", "/admin/requests", "", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
