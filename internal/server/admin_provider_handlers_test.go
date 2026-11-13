// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/provider"
	"github.com/angryang/ai-monitor/internal/request"
)

// stubChecker stands in for the connectivity checker so the handler tests
// never touch the network. It records the providers and the single model
// each on-demand check was asked to probe.
type stubChecker struct {
	mu sync.Mutex
	// result is the verdict returned for every check.
	result provider.CheckResult
	// seen records the providers passed to CheckModel.
	seen []provider.Provider
	// models records the model name handed to CheckModel.
	models []string
	calls  int
}

func newStubChecker(ok bool) *stubChecker {
	c := &stubChecker{}
	if ok {
		c.result = provider.CheckResult{OK: true, Kind: provider.CheckOK, Message: "upstream accepted the request"}
	} else {
		c.result = provider.CheckResult{
			Kind:       provider.CheckUnauthorized,
			StatusCode: 401,
			Message:    "Incorrect API key provided",
		}
	}
	return c
}

func (s *stubChecker) CheckModel(_ context.Context, p *provider.Provider, model string) provider.CheckResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if p != nil {
		s.seen = append(s.seen, *p)
	}
	s.models = append(s.models, model)
	res := s.result
	if p != nil {
		res.Protocol = string(p.Protocol)
	}
	res.Model = model
	res.CheckedAt = time.Now().UTC()
	return res
}

// Check keeps the old single-argument shape for tests that call the
// checker directly; the handler interface only uses CheckModel.
func (s *stubChecker) Check(ctx context.Context, p *provider.Provider) provider.CheckResult {
	return s.CheckModel(ctx, p, p.PrimaryModel())
}

func (s *stubChecker) lastSeen() (provider.Provider, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.seen) == 0 {
		return provider.Provider{}, false
	}
	return s.seen[len(s.seen)-1], true
}

func (s *stubChecker) lastModel() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.models) == 0 {
		return "", false
	}
	return s.models[len(s.models)-1], true
}

func (s *stubChecker) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// newProviderHandlers wires the handler bundle over an in-memory store.
func newProviderHandlers(t *testing.T, checker ProviderChecker) (*AdminProviderHandlers, *provider.SqliteStore) {
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
	store, err := provider.NewSqliteStore(taskStore.DB())
	if err != nil {
		t.Fatalf("provider store: %v", err)
	}
	store.SetCipher(cipher)
	return NewAdminProviderHandlers(&AdminProviderHandlersConfig{
		Store:   store,
		Checker: checker,
		Logger:  func(string, ...any) {},
	}), store
}

// providerBody builds a valid create payload.
func providerBody(overrides map[string]any) map[string]any {
	body := map[string]any{
		"name":     "alpha",
		"protocol": "openai",
		"base_url": "https://e.test",
		"api_key":  "sk-secret",
		"models":   []any{"gpt-test", "gpt-alt"},
	}
	for k, v := range overrides {
		if v == nil {
			delete(body, k)
			continue
		}
		body[k] = v
	}
	return body
}

// postJSON sends a JSON body to a handler.
func postJSON(t *testing.T, fn http.HandlerFunc, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	fn(rec, req)
	return rec
}

// patchJSON sends a JSON body to a PATCH handler.
func patchJSON(t *testing.T, fn http.HandlerFunc, path, id string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PATCH", path, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", id)
	fn(rec, req)
	return rec
}

// TestProviderCreate_SavesWithoutChecking pins the new rule: saving is a
// local operation, connectivity is an on-demand per-model check.
func TestProviderCreate_SavesWithoutChecking(t *testing.T) {
	h, store := newProviderHandlers(t, newStubChecker(true))

	rec := postJSON(t, h.create, "/admin/providers", providerBody(nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — body=%s", rec.Code, rec.Body.String())
	}
	out := mustJSON(t, rec)
	if out["id"] == "" || out["id"] == nil {
		t.Fatalf("response has no id: %v", out)
	}
	if out["name"] != "alpha" {
		t.Fatalf("name = %v, want alpha", out["name"])
	}
	if out["has_api_key"] != true {
		t.Fatalf("has_api_key = %v, want true", out["has_api_key"])
	}
	models, _ := out["models"].([]any)
	if len(models) != 2 || models[0] != "gpt-test" || models[1] != "gpt-alt" {
		t.Fatalf("models = %v, want [gpt-test gpt-alt]", out["models"])
	}
	// Saving never contacts the upstream and never claims a verdict.
	if out["last_verified_ok"] != false {
		t.Fatalf("last_verified_ok = %v, want false", out["last_verified_ok"])
	}
	if _, exists := out["check"]; exists {
		t.Fatalf("create must not return a check verdict: %v", out)
	}
	if n := h.cfg.Checker.(*stubChecker).callCount(); n != 0 {
		t.Fatalf("checker called %d times on create, want 0", n)
	}

	// The provider really is in the store.
	list, err := store.ListProviders(context.Background())
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("providers = %d, want 1", len(list))
	}
	if list[0].APIKey != "sk-secret" {
		t.Fatalf("api key = %q, want the decrypted value", list[0].APIKey)
	}
}

// TestProviderCreate_SavesEvenWhenUpstreamIsDown pins that an unreachable
// upstream no longer blocks a save: the operator checks models on demand.
func TestProviderCreate_SavesEvenWhenUpstreamIsDown(t *testing.T) {
	checker := newStubChecker(false)
	h, store := newProviderHandlers(t, checker)

	rec := postJSON(t, h.create, "/admin/providers", providerBody(nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — body=%s", rec.Code, rec.Body.String())
	}
	if checker.callCount() != 0 {
		t.Fatalf("checker was called on create: %d", checker.callCount())
	}
	list, err := store.ListProviders(context.Background())
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	if len(list) != 1 || len(list[0].Models) != 2 {
		t.Fatalf("providers = %+v, want one with two models", list)
	}
}

// TestProviderCheck_ProbesSelectedModel pins that the ad-hoc check route
// probes exactly the model named in the payload, not the whole list.
func TestProviderCheck_ProbesSelectedModel(t *testing.T) {
	checker := newStubChecker(true)
	h, _ := newProviderHandlers(t, checker)

	rec := postJSON(t, h.check, "/admin/providers/check",
		providerBody(map[string]any{"model": "gpt-alt"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	if got := mustJSON(t, rec)["model"]; got != "gpt-alt" {
		t.Fatalf("checked model = %v, want gpt-alt", got)
	}
	model, ok := checker.lastModel()
	if !ok || model != "gpt-alt" {
		t.Fatalf("checker received model %q, want gpt-alt", model)
	}
	if checker.callCount() != 1 {
		t.Fatalf("checker called %d times, want 1", checker.callCount())
	}
}

// TestProviderCreate_ForceIsIgnored pins that the legacy force flag is
// accepted (older clients) but has no effect on a save.
func TestProviderCreate_ForceIsIgnored(t *testing.T) {
	checker := newStubChecker(false)
	h, _ := newProviderHandlers(t, checker)

	rec := postJSON(t, h.create, "/admin/providers",
		providerBody(map[string]any{"force": true}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — body=%s", rec.Code, rec.Body.String())
	}
	if checker.callCount() != 0 {
		t.Fatalf("force must not trigger a check: %d calls", checker.callCount())
	}
}

// TestProviderCreate_RejectsInvalidPayload pins the validation contract.
func TestProviderCreate_RejectsInvalidPayload(t *testing.T) {
	h, _ := newProviderHandlers(t, newStubChecker(true))

	cases := map[string]map[string]any{
		"no name":     providerBody(map[string]any{"name": nil}),
		"no base url": providerBody(map[string]any{"base_url": nil}),
		"no models":   providerBody(map[string]any{"models": nil}),
		"blank model": providerBody(map[string]any{"models": []any{""}}),
		"bad proto":   providerBody(map[string]any{"protocol": "nope"}),
		"bad timeout": providerBody(map[string]any{"timeout": "abc"}),
	}
	for name, body := range cases {
		rec := postJSON(t, h.create, "/admin/providers", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 — body=%s", name, rec.Code, rec.Body.String())
		}
	}
}

// TestProviderCreate_RejectsUnknownFields pins that the strict decoder is
// in force on this route too.
func TestProviderCreate_RejectsUnknownFields(t *testing.T) {
	h, _ := newProviderHandlers(t, newStubChecker(true))

	rec := postJSON(t, h.create, "/admin/providers", providerBody(map[string]any{"nope": 1}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — body=%s", rec.Code, rec.Body.String())
	}
}

// TestProviderCreate_NeverLeaksAPIKey pins that the api key stays out of
// every response.
func TestProviderCreate_NeverLeaksAPIKey(t *testing.T) {
	h, _ := newProviderHandlers(t, newStubChecker(true))

	rec := postJSON(t, h.create, "/admin/providers", providerBody(nil))
	if strings.Contains(rec.Body.String(), "sk-secret") {
		t.Fatalf("api key leaked: %s", rec.Body.String())
	}
}

// TestProviderPatch_KeepsStoredKey pins that a PATCH without an api key
// keeps the stored one, and that the merged result — not the partial
// payload — is what gets saved.
func TestProviderPatch_KeepsStoredKey(t *testing.T) {
	checker := newStubChecker(true)
	h, store := newProviderHandlers(t, checker)

	rec := postJSON(t, h.create, "/admin/providers", providerBody(nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d — body=%s", rec.Code, rec.Body.String())
	}
	id := mustJSON(t, rec)["id"].(string)

	// Patch only the model list.
	rec = patchJSON(t, h.patch, "/admin/providers/"+id, id, map[string]any{
		"models": []any{"gpt-4o", "gpt-4o-mini"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: status = %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	out := mustJSON(t, rec)
	models, _ := out["models"].([]any)
	if len(models) != 2 || models[0] != "gpt-4o" || models[1] != "gpt-4o-mini" {
		t.Fatalf("models = %v, want [gpt-4o gpt-4o-mini]", out["models"])
	}
	if out["name"] != "alpha" {
		t.Fatalf("name = %v, want alpha (the stored value must survive)", out["name"])
	}
	if out["has_api_key"] != true {
		t.Fatalf("has_api_key = %v, want true", out["has_api_key"])
	}

	got, err := store.GetProvider(context.Background(), id)
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if got.APIKey != "sk-secret" {
		t.Fatalf("api key = %q, want the stored key to survive", got.APIKey)
	}

	// Patching is local too: the checker must not have been called.
	if checker.callCount() != 0 {
		t.Fatalf("checker called %d times on patch, want 0", checker.callCount())
	}
	stored, err := store.GetProvider(context.Background(), id)
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if len(stored.Models) != 2 || stored.PrimaryModel() != "gpt-4o" {
		t.Fatalf("stored models = %v, want [gpt-4o gpt-4o-mini]", stored.Models)
	}
}

// TestProviderPatch_RejectsRemovalOfModelInUse pins the referential guard:
// a model that a request (or task) still selects cannot be dropped from
// the provider's candidate list.
func TestProviderPatch_RejectsRemovalOfModelInUse(t *testing.T) {
	h, store := newProviderHandlers(t, newStubChecker(true))
	rec := postJSON(t, h.create, "/admin/providers", providerBody(nil))
	id := mustJSON(t, rec)["id"].(string)

	h.cfg.Requests = &stubRequestStore{inUse: []request.Request{
		{ID: "req-1", Name: "greeting", ProviderID: id, Model: "gpt-alt"},
	}}
	rec = patchJSON(t, h.patch, "/admin/providers/"+id, id,
		map[string]any{"models": []any{"gpt-test"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 — body=%s", rec.Code, rec.Body.String())
	}
	errObj := mustJSON(t, rec)["error"].(map[string]any)
	if errObj["code"] != "model_in_use" {
		t.Fatalf("code = %v, want model_in_use", errObj["code"])
	}
	details, _ := errObj["details"].(map[string]any)
	names, _ := details["requests"].([]any)
	if len(names) != 1 || names[0] != "greeting" {
		t.Fatalf("details = %v, want the request that uses gpt-alt", details)
	}
	got, err := store.GetProvider(context.Background(), id)
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if len(got.Models) != 2 {
		t.Fatalf("models = %v, the rejected patch must not be persisted", got.Models)
	}

	// Once nothing selects it, the same edit goes through.
	h.cfg.Requests = &stubRequestStore{}
	rec = patchJSON(t, h.patch, "/admin/providers/"+id, id,
		map[string]any{"models": []any{"gpt-test"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status after freeing the model = %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
}

// TestProviderPatch_NotFound pins the 404 contract.
func TestProviderPatch_NotFound(t *testing.T) {
	h, _ := newProviderHandlers(t, newStubChecker(true))
	rec := patchJSON(t, h.patch, "/admin/providers/nope", "nope", map[string]any{"models": []any{"x"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — body=%s", rec.Code, rec.Body.String())
	}
}

// TestProviderDelete pins the delete contract.
func TestProviderDelete(t *testing.T) {
	h, store := newProviderHandlers(t, newStubChecker(true))

	rec := postJSON(t, h.create, "/admin/providers", providerBody(nil))
	id := mustJSON(t, rec)["id"].(string)

	rec = httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/admin/providers/"+id, nil)
	req.SetPathValue("id", id)
	h.delete(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	if _, err := store.GetProvider(context.Background(), id); err == nil {
		t.Fatalf("provider survived the deletion")
	}
}

// stubRequestStore is the smallest request.Store that lets the provider
// delete route check whether an upstream is still referenced.
type stubRequestStore struct {
	inUse []request.Request
}

func (s *stubRequestStore) ListRequestsByProvider(_ context.Context, _ string) ([]request.Request, error) {
	return s.inUse, nil
}
func (s *stubRequestStore) ListRequests(context.Context) ([]request.Request, error) {
	return s.inUse, nil
}
func (s *stubRequestStore) GetRequest(context.Context, string) (*request.Request, error) {
	return nil, request.ErrNotFound
}
func (s *stubRequestStore) CreateRequest(context.Context, *request.Request) error { return nil }
func (s *stubRequestStore) UpdateRequest(context.Context, *request.Request) error { return nil }
func (s *stubRequestStore) DeleteRequest(context.Context, string) error           { return nil }

// TestProviderDelete_RefusesWhenInUse pins the referential guard: requests
// point at a provider by id, so removing one that is still in use would
// silently break them.
func TestProviderDelete_RefusesWhenInUse(t *testing.T) {
	h, store := newProviderHandlers(t, newStubChecker(true))
	rec := postJSON(t, h.create, "/admin/providers", providerBody(nil))
	id := mustJSON(t, rec)["id"].(string)

	h.cfg.Requests = &stubRequestStore{inUse: []request.Request{
		{ID: "req-1", Name: "greeting", ProviderID: id, Model: "gpt-test"},
		{ID: "req-2", Name: "farewell", ProviderID: id, Model: "gpt-alt"},
	}}

	del := httptest.NewRecorder()
	dreq := httptest.NewRequest("DELETE", "/admin/providers/"+id, nil)
	dreq.SetPathValue("id", id)
	h.delete(del, dreq)
	if del.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 — body=%s", del.Code, del.Body.String())
	}
	errObj := mustJSON(t, del)["error"].(map[string]any)
	if errObj["code"] != "provider_in_use" {
		t.Fatalf("error code = %v, want provider_in_use", errObj["code"])
	}
	details, _ := errObj["details"].(map[string]any)
	names, _ := details["requests"].([]any)
	if len(names) != 2 {
		t.Fatalf("details = %v, want both request names", errObj["details"])
	}
	if _, err := store.GetProvider(context.Background(), id); err != nil {
		t.Fatalf("provider was deleted despite being in use: %v", err)
	}

	// Once the requests are gone, the same delete succeeds.
	h.cfg.Requests = &stubRequestStore{}
	del = httptest.NewRecorder()
	dreq = httptest.NewRequest("DELETE", "/admin/providers/"+id, nil)
	dreq.SetPathValue("id", id)
	h.delete(del, dreq)
	if del.Code != http.StatusOK {
		t.Fatalf("status after freeing the provider = %d, want 200", del.Code)
	}
}

// TestProviderDelete_NotFound pins the 404 contract.
func TestProviderDelete_NotFound(t *testing.T) {
	h, _ := newProviderHandlers(t, newStubChecker(true))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/admin/providers/nope", nil)
	req.SetPathValue("id", "nope")
	h.delete(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — body=%s", rec.Code, rec.Body.String())
	}
}

// TestProviderList pins the listing contract.
func TestProviderList(t *testing.T) {
	h, _ := newProviderHandlers(t, newStubChecker(true))
	postJSON(t, h.create, "/admin/providers", providerBody(nil))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/providers", nil)
	h.list(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	out := mustJSON(t, rec)
	providers, ok := out["providers"].([]any)
	if !ok {
		t.Fatalf("no providers array: %v", out)
	}
	if len(providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(providers))
	}
	first := providers[0].(map[string]any)
	if first["name"] != "alpha" {
		t.Fatalf("name = %v", first["name"])
	}
}

// TestProviderCheck_ReportsVerdict pins the standalone check endpoint:
// it answers 200 with the verdict whether the upstream is healthy or not.
func TestProviderCheck_ReportsVerdict(t *testing.T) {
	h, _ := newProviderHandlers(t, newStubChecker(false))

	rec := postJSON(t, h.check, "/admin/providers/check", providerBody(nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	out := mustJSON(t, rec)
	if out["ok"] != false {
		t.Fatalf("ok = %v, want false", out["ok"])
	}
	if out["kind"] != string(provider.CheckUnauthorized) {
		t.Fatalf("kind = %v", out["kind"])
	}
	// No model in the payload means the primary one is probed.
	if out["model"] != "gpt-test" {
		t.Fatalf("model = %v, want gpt-test (the primary)", out["model"])
	}
}

// TestProviderCheckStored_UsesStoredKey pins that re-verifying a saved
// provider works without the operator re-typing the key.
func TestProviderCheckStored_UsesStoredKey(t *testing.T) {
	checker := newStubChecker(true)
	h, _ := newProviderHandlers(t, checker)

	rec := postJSON(t, h.create, "/admin/providers", providerBody(nil))
	id := mustJSON(t, rec)["id"].(string)

	rec = httptest.NewRecorder()
	raw := `{"model":"gpt-alt"}`
	req := httptest.NewRequest("POST", "/admin/providers/"+id+"/check", strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", id)
	h.checkStored(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	if got := mustJSON(t, rec)["model"]; got != "gpt-alt" {
		t.Fatalf("checked model = %v, want gpt-alt", got)
	}
	seen, ok := checker.lastSeen()
	if !ok {
		t.Fatalf("the checker was never called")
	}
	if seen.APIKey != "sk-secret" {
		t.Fatalf("the stored key was not used: %q", seen.APIKey)
	}
	model, _ := checker.lastModel()
	if model != "gpt-alt" {
		t.Fatalf("checker got model %q, want gpt-alt", model)
	}

	// An unknown model is rejected instead of silently probing the primary.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/admin/providers/"+id+"/check",
		strings.NewReader(`{"model":"does-not-exist"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", id)
	h.checkStored(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown model: status = %d, want 400 — body=%s", rec.Code, rec.Body.String())
	}
}

// TestProviderDuplicateName pins the 409 contract.
func TestProviderDuplicateName(t *testing.T) {
	h, _ := newProviderHandlers(t, newStubChecker(true))
	postJSON(t, h.create, "/admin/providers", providerBody(nil))

	rec := postJSON(t, h.create, "/admin/providers", providerBody(nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 — body=%s", rec.Code, rec.Body.String())
	}
}

// TestProviderStoreDisabled pins that a build without the registry
// answers 503 instead of pretending the write succeeded.
func TestProviderStoreDisabled(t *testing.T) {
	h := NewAdminProviderHandlers(&AdminProviderHandlersConfig{})
	rec := postJSON(t, h.create, "/admin/providers", providerBody(nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — body=%s", rec.Code, rec.Body.String())
	}
}

// TestProviderCheckDisabled pins that a build without the checker can
// still save providers (saving is local) but the check routes answer 503.
func TestProviderCheckDisabled(t *testing.T) {
	taskStore, err := config.NewSqliteStore(config.SqliteOptions{Path: ":memory:"})
	if err != nil {
		t.Fatalf("task store: %v", err)
	}
	t.Cleanup(func() { _ = taskStore.Close() })
	store, err := provider.NewSqliteStore(taskStore.DB())
	if err != nil {
		t.Fatalf("provider store: %v", err)
	}
	h := NewAdminProviderHandlers(&AdminProviderHandlersConfig{Store: store})
	rec := postJSON(t, h.create, "/admin/providers", providerBody(nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 — body=%s", rec.Code, rec.Body.String())
	}
	rec = postJSON(t, h.check, "/admin/providers/check", providerBody(nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("check: status = %d, want 503 — body=%s", rec.Code, rec.Body.String())
	}
}
