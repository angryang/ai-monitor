// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package server

// Tests for §18.12 start / stop handlers. We exercise the handler funcs
// directly (no mux / auth / httptest server plumbing) to keep the suite
// focused on the per-handler contract:
//
//   - start: persist Enabled=true, register cron entry, return taskResponse.
//   - stop : retract cron entry, persist Enabled=false, return taskResponse.
//   - both are idempotent on already-{enabled,disabled} tasks.
//   - DB / scheduler errors surface as 5xx with structured error bodies.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/scheduler"
)

// memStore is a tiny in-memory ConfigStore. It's enough for start / stop —
// all we need is GetTask and UpdateTask. ListTasks/CreateTask/DeleteTask/
// Watch/Close are stubbed because start / stop don't reach them.
type memStore struct {
	mu    sync.Mutex
	tasks map[string]config.TaskConfig
	err   error // optional error to force UpdateTask failures
}

func newMemStore(t *testing.T, seed ...config.TaskConfig) *memStore {
	t.Helper()
	s := &memStore{tasks: map[string]config.TaskConfig{}}
	for _, tc := range seed {
		s.tasks[tc.ID] = tc
	}
	return s
}

func (m *memStore) GetTask(_ context.Context, id string) (*config.TaskConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tc, ok := m.tasks[id]
	if !ok {
		return nil, config.ErrNotFound
	}
	clone := tc
	return &clone, nil
}

func (m *memStore) UpdateTask(_ context.Context, tc *config.TaskConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	if _, ok := m.tasks[tc.ID]; !ok {
		return config.ErrNotFound
	}
	m.tasks[tc.ID] = *tc
	return nil
}

func (m *memStore) ListTasks(_ context.Context) ([]config.TaskConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]config.TaskConfig, 0, len(m.tasks))
	for _, tc := range m.tasks {
		out = append(out, tc)
	}
	return out, nil
}
func (m *memStore) CreateTask(_ context.Context, _ *config.TaskConfig) error { return nil }
func (m *memStore) DeleteTask(_ context.Context, _ string) error             { return nil }
func (m *memStore) Watch(_ context.Context) (<-chan config.Event, error)     { return nil, nil }
func (m *memStore) Close() error                                             { return nil }

// memScheduler records Add / Remove calls. It satisfies scheduler.Scheduler
// and lets us assert "scheduler.Add was called once with this exact task".
type memScheduler struct {
	mu        sync.Mutex
	added     []config.TaskConfig
	removed   []string
	startErr  error // if non-nil, Add returns this
	removeErr error
}

func (m *memScheduler) Add(t config.TaskConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.startErr != nil {
		return m.startErr
	}
	m.added = append(m.added, t)
	return nil
}
func (m *memScheduler) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.removeErr != nil {
		return m.removeErr
	}
	m.removed = append(m.removed, id)
	return nil
}
func (m *memScheduler) Start(_ context.Context) error                         { return nil }
func (m *memScheduler) Stop(_ context.Context) error                          { return nil }
func (m *memScheduler) Reload(_ context.Context, _ []config.TaskConfig) error { return nil }

func seedTask(id string, enabled bool) config.TaskConfig {
	return config.TaskConfig{
		ID:         id,
		Name:       id,
		Enabled:    enabled,
		Protocol:   config.ProtocolOpenAI,
		BaseURL:    "https://example.test",
		Model:      "gpt-test",
		UserPrompt: "hi",
		Schedule:   "@every 1m",
	}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body, _ := io.ReadAll(rec.Body)
	out := map[string]any{}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v — body=%s", err, string(body))
	}
	return out
}

func TestStartTask_DisabledToEnabled(t *testing.T) {
	store := newMemStore(t, seedTask("alpha", false))
	sch := &memScheduler{}
	h := NewAdminTaskHandlers(&AdminTaskHandlersConfig{
		Store:     store,
		Scheduler: sch,
		Logger:    func(string, ...any) {},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/tasks/alpha/start", nil)
	req.SetPathValue("id", "alpha")
	h.start(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status: got %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	resp := decode(t, rec)
	if got, _ := resp["enabled"].(bool); !got {
		t.Fatalf("response enabled = false; want true — resp=%v", resp)
	}
	// DB was flipped.
	if got := store.tasks["alpha"].Enabled; !got {
		t.Fatalf("store.Enabled = false; want true")
	}
	// Scheduler was called exactly once with the enabled task.
	if len(sch.added) != 1 {
		t.Fatalf("scheduler.Add called %d times; want 1", len(sch.added))
	}
	if !sch.added[0].Enabled {
		t.Fatalf("scheduler.Add received disabled task: %+v", sch.added[0])
	}
	if len(sch.removed) != 0 {
		t.Fatalf("scheduler.Remove called %d times; want 0", len(sch.removed))
	}
}

func TestStartTask_AlreadyEnabledIsIdempotent(t *testing.T) {
	store := newMemStore(t, seedTask("alpha", true))
	sch := &memScheduler{}
	h := NewAdminTaskHandlers(&AdminTaskHandlersConfig{
		Store:     store,
		Scheduler: sch,
		Logger:    func(string, ...any) {},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/tasks/alpha/start", nil)
	req.SetPathValue("id", "alpha")
	h.start(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status: got %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	// No scheduler call on idempotent re-start.
	if len(sch.added) != 0 || len(sch.removed) != 0 {
		t.Fatalf("scheduler should be silent on idempotent start: added=%v removed=%v", sch.added, sch.removed)
	}
}

func TestStartTask_NotFound(t *testing.T) {
	store := newMemStore(t)
	sch := &memScheduler{}
	h := NewAdminTaskHandlers(&AdminTaskHandlersConfig{
		Store:     store,
		Scheduler: sch,
		Logger:    func(string, ...any) {},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/tasks/missing/start", nil)
	req.SetPathValue("id", "missing")
	h.start(rec, req)

	if rec.Code != 404 {
		t.Fatalf("status: got %d, want 404 — body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "task_not_found") {
		t.Fatalf("expected task_not_found error code; body=%s", rec.Body.String())
	}
}

func TestStartTask_SchedulerErrorRollsBackDB(t *testing.T) {
	store := newMemStore(t, seedTask("alpha", false))
	sch := &memScheduler{startErr: fmt.Errorf("cron expression invalid")}
	h := NewAdminTaskHandlers(&AdminTaskHandlersConfig{
		Store:     store,
		Scheduler: sch,
		Logger:    func(string, ...any) {},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/tasks/alpha/start", nil)
	req.SetPathValue("id", "alpha")
	h.start(rec, req)

	if rec.Code != 500 {
		t.Fatalf("status: got %d, want 500 — body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "start_schedule_failed") {
		t.Fatalf("expected start_schedule_failed error; body=%s", rec.Body.String())
	}
	// Roll-back must have flipped DB back to false.
	if got := store.tasks["alpha"].Enabled; got {
		t.Fatalf("DB.Enabled = true after scheduler failure; want false (rolled back)")
	}
}

func TestStartTask_NoScheduler(t *testing.T) {
	store := newMemStore(t, seedTask("alpha", false))
	h := NewAdminTaskHandlers(&AdminTaskHandlersConfig{
		Store:  store,
		Logger: func(string, ...any) {},
		// Scheduler intentionally nil.
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/tasks/alpha/start", nil)
	req.SetPathValue("id", "alpha")
	h.start(rec, req)

	if rec.Code != 503 {
		t.Fatalf("status: got %d, want 503 — body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "scheduler_disabled") {
		t.Fatalf("expected scheduler_disabled error; body=%s", rec.Body.String())
	}
}

func TestStopTask_EnabledToDisabled(t *testing.T) {
	store := newMemStore(t, seedTask("alpha", true))
	sch := &memScheduler{}
	h := NewAdminTaskHandlers(&AdminTaskHandlersConfig{
		Store:     store,
		Scheduler: sch,
		Logger:    func(string, ...any) {},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/tasks/alpha/stop", nil)
	req.SetPathValue("id", "alpha")
	h.stop(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status: got %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	if got := store.tasks["alpha"].Enabled; got {
		t.Fatalf("DB.Enabled = true; want false")
	}
	if len(sch.removed) != 1 || sch.removed[0] != "alpha" {
		t.Fatalf("scheduler.Remove wrong: %v", sch.removed)
	}
}

func TestStopTask_AlreadyDisabledIsIdempotent(t *testing.T) {
	store := newMemStore(t, seedTask("alpha", false))
	sch := &memScheduler{}
	h := NewAdminTaskHandlers(&AdminTaskHandlersConfig{
		Store:     store,
		Scheduler: sch,
		Logger:    func(string, ...any) {},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/tasks/alpha/stop", nil)
	req.SetPathValue("id", "alpha")
	h.stop(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status: got %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	// Remove is idempotent — should have fired once.
	if len(sch.removed) != 1 {
		t.Fatalf("Remove fired %d times; want 1", len(sch.removed))
	}
}

func TestStopTask_PersistenceFailureAfterRetract(t *testing.T) {
	// stop retracts first, then persists. If persist fails, the cron entry
	// is already gone — there is nothing to roll back.
	store := newMemStore(t, seedTask("alpha", true))
	store.err = errors.New("disk full")
	sch := &memScheduler{}
	h := NewAdminTaskHandlers(&AdminTaskHandlersConfig{
		Store:     store,
		Scheduler: sch,
		Logger:    func(string, ...any) {},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/tasks/alpha/stop", nil)
	req.SetPathValue("id", "alpha")
	h.stop(rec, req)

	if rec.Code != 500 {
		t.Fatalf("status: got %d, want 500 — body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "stop_persist_failed") {
		t.Fatalf("expected stop_persist_failed error; body=%s", rec.Body.String())
	}
	if len(sch.removed) != 1 {
		t.Fatalf("scheduler.Remove fired %d times; want 1", len(sch.removed))
	}
}

// TestPatchTask_InsecureSkipVerifyMerge pins the PATCH semantics of the
// TLS switch: an absent field keeps the stored value, an explicit false
// turns verification back on.
func TestPatchTask_InsecureSkipVerifyMerge(t *testing.T) {
	base := seedTask("alpha", true)
	base.InsecureSkipVerify = true
	store := newMemStore(t, base)
	h := NewAdminTaskHandlers(&AdminTaskHandlersConfig{
		Store:  store,
		Logger: func(string, ...any) {},
	})

	// 1. PATCH without the field keeps the stored true.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PATCH", "/admin/tasks/alpha", strings.NewReader(`{"name":"alpha"}`))
	req.SetPathValue("id", "alpha")
	h.patch(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status: got %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	if !store.tasks["alpha"].InsecureSkipVerify {
		t.Fatalf("an absent field must keep the stored value")
	}

	// 2. PATCH with an explicit false turns it off.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("PATCH", "/admin/tasks/alpha", strings.NewReader(`{"insecure_skip_verify":false}`))
	req.SetPathValue("id", "alpha")
	h.patch(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status: got %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	if store.tasks["alpha"].InsecureSkipVerify {
		t.Fatalf("an explicit false must turn the switch off")
	}
}

// TestListTasks_ReportsGlobalTLS verifies the response carries both the
// per-task flag and the instance-wide switch so the UI can explain why a
// task still skips verification.
func TestListTasks_ReportsGlobalTLS(t *testing.T) {
	task := seedTask("alpha", true)
	store := newMemStore(t, task)
	h := NewAdminTaskHandlers(&AdminTaskHandlersConfig{
		Store:              store,
		Logger:             func(string, ...any) {},
		InsecureSkipVerify: true,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/tasks", nil)
	h.list(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	var parsed struct {
		Tasks []struct {
			InsecureSkipVerify       bool `json:"insecure_skip_verify"`
			InsecureSkipVerifyGlobal bool `json:"insecure_skip_verify_global"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(parsed.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(parsed.Tasks))
	}
	if parsed.Tasks[0].InsecureSkipVerifyGlobal != true {
		t.Fatalf("the global switch must be reported to clients")
	}
}

// TestListTasks_ReportsSecretError verifies that a task whose secrets could
// not be decrypted is still listed, with secret_error set, so the operator
// can see it and re-enter the api key.
func TestListTasks_ReportsSecretError(t *testing.T) {
	task := seedTask("alpha", true)
	task.SecretError = "config: decrypt api_key: master key mismatch"
	store := newMemStore(t, task)
	h := NewAdminTaskHandlers(&AdminTaskHandlersConfig{
		Store:  store,
		Logger: func(string, ...any) {},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/tasks", nil)
	h.list(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	var parsed struct {
		Tasks []struct {
			ID          string `json:"id"`
			SecretError string `json:"secret_error"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(parsed.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(parsed.Tasks))
	}
	if parsed.Tasks[0].SecretError == "" {
		t.Fatal("secret_error must be reported so the UI can flag the task")
	}
}

// silence unused imports if the suite grows.
var _ scheduler.Scheduler = (*memScheduler)(nil)

// TestPatchTask_ThinkingEnabledMerge mirrors the TLS-switch merge test:
// an absent thinking_enabled keeps the stored value; an explicit false
// turns it off.
func TestPatchTask_ThinkingEnabledMerge(t *testing.T) {
	base := seedTask("alpha", true)
	base.ThinkingEnabled = true
	store := newMemStore(t, base)
	h := NewAdminTaskHandlers(&AdminTaskHandlersConfig{
		Store:  store,
		Logger: func(string, ...any) {},
	})

	// 1. PATCH without the field keeps the stored true.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PATCH", "/admin/tasks/alpha", strings.NewReader(`{"name":"alpha"}`))
	req.SetPathValue("id", "alpha")
	h.patch(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status: got %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	if !store.tasks["alpha"].ThinkingEnabled {
		t.Fatalf("an absent field must keep the stored value")
	}

	// 2. PATCH with an explicit false turns it off.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("PATCH", "/admin/tasks/alpha", strings.NewReader(`{"thinking_enabled":false}`))
	req.SetPathValue("id", "alpha")
	h.patch(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status: got %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	if store.tasks["alpha"].ThinkingEnabled {
		t.Fatalf("an explicit false must turn the switch off")
	}
}

// TestGetTask_ReportsThinkingEnabled verifies the response carries the
// per-task thinking flag so the edit form can hydrate the switch.
func TestGetTask_ReportsThinkingEnabled(t *testing.T) {
	task := seedTask("alpha", true)
	task.ThinkingEnabled = true
	store := newMemStore(t, task)
	h := NewAdminTaskHandlers(&AdminTaskHandlersConfig{
		Store:  store,
		Logger: func(string, ...any) {},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/tasks/alpha", nil)
	req.SetPathValue("id", "alpha")
	h.get(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status: got %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		ThinkingEnabled bool `json:"thinking_enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !body.ThinkingEnabled {
		t.Fatalf("thinking_enabled = false, want true")
	}
}
