// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

// Package server: /admin/tasks CRUD with RBAC. Viewers can read; only
// admins can create / update / delete.
//
// A monitoring task no longer carries the upstream coordinates. It names the
// AI service provider it probes (provider_id) and owns only what makes it a
// monitoring task: the prompt, the schedule, the canary group and the
// notification switch. The provider owns the protocol, the URL, the
// credentials, the model and the protocol-specific options — the same
// split the AI request manager uses.
//
// The provider is resolved when the task is listed (so the UI can render
// it) and again right before a probe runs (so an edit to the provider
// takes effect on the next tick).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/angryang/ai-monitor/internal/auth"
	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/probe"
	"github.com/angryang/ai-monitor/internal/provider"
	"github.com/angryang/ai-monitor/internal/scheduler"
)

// AdminTaskHandlersConfig carries the dependencies for admin/task routes.
type AdminTaskHandlersConfig struct {
	Auth     *auth.Middleware
	Users    auth.UserStore
	Store    config.ConfigStore
	Cipher   config.Cipher
	RunStore probe.RunStore // optional; nil disables /admin/tasks/{id}/runs
	// Scheduler backs POST /admin/tasks/{id}/{start,stop}. The handler
	// registers and retracts cron entries in lock-step with the persisted
	// Enabled flag. Optional; if nil, /start and /stop return 503 so admins
	// know the binary was built without one.
	Scheduler scheduler.Scheduler
	// Providers backs the provider reference of every task. It is used to
	// validate a task's provider and to nest the provider summary in the
	// response. Optional; when nil the provider summary is empty and a
	// task cannot be created or updated.
	Providers provider.Store
	// InsecureSkipVerify mirrors the instance-wide TLS switch. It is echoed
	// back to clients so the UI can explain why a task still skips
	// verification after the per-task flag was cleared.
	InsecureSkipVerify bool
	Logger             LoggerFunc
}

// AdminTaskHandlers is the bundle returned by NewAdminTaskHandlers.
type AdminTaskHandlers struct {
	cfg AdminTaskHandlersConfig
}

// NewAdminTaskHandlers wires the bundle.
func NewAdminTaskHandlers(cfg *AdminTaskHandlersConfig) *AdminTaskHandlers {
	if cfg == nil {
		cfg = &AdminTaskHandlersConfig{}
	}
	return &AdminTaskHandlers{cfg: *cfg}
}

// Mount registers every /admin/tasks route directly on parent. Returning
// the same instance keeps the call site from editing parent mux wiring.
//
// When Auth is nil the routes are mounted without authentication. That
// preserves the pre-§18 behaviour for builds/tests that run without auth —
// the production binary always wires a middleware, so this only affects
// the unauthenticated build.
func (h *AdminTaskHandlers) Mount(mux *http.ServeMux) *http.ServeMux {
	// Reads — any authenticated role.
	read := func(next http.HandlerFunc) http.Handler {
		if h.cfg.Auth == nil {
			return next
		}
		return h.cfg.Auth.Authenticate(next)
	}
	// Writes — admin only.
	adminAuth := func(next http.HandlerFunc) http.Handler {
		if h.cfg.Auth == nil {
			return next
		}
		return h.cfg.Auth.RequireRole(auth.RoleAdmin, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
		}))
	}
	mux.Handle("GET /admin/tasks", read(h.list))
	mux.Handle("GET /admin/tasks/{id}", read(h.get))
	mux.Handle("GET /admin/tasks/{id}/runs", read(h.listRuns))

	mux.Handle("POST /admin/tasks", adminAuth(h.create))
	mux.Handle("PATCH /admin/tasks/{id}", adminAuth(h.patch))
	mux.Handle("DELETE /admin/tasks/{id}", adminAuth(h.delete))
	mux.Handle("POST /admin/tasks/{id}/start", adminAuth(h.start))
	mux.Handle("POST /admin/tasks/{id}/stop", adminAuth(h.stop))
	return mux
}

// Routes kept for backward compatibility; returns a standalone mux that
// owns every method under /admin/tasks. Prefer Mount() in new code.
func (h *AdminTaskHandlers) Routes() http.Handler {
	return h.Mount(http.NewServeMux())
}

// ---- task payload ----

// log emits a structured line when a logger is configured. Safe on a nil
// Logger so tests don't have to wire one.
func (h *AdminTaskHandlers) log(msg string, args ...any) {
	if h.cfg.Logger != nil {
		h.cfg.Logger(msg, args...)
	}
}

// listTasks fetches every task, tolerating rows whose encrypted secrets
// cannot be opened with the current master key.
//
// Such rows are still returned (with SecretError set, secrets empty) so the
// UI can show the task and let an operator re-enter the webhook. Without
// this the whole listing would fail and the admin UI would be unusable
// exactly when it's needed most.
//
// A failure of the listing itself is returned as an error so the endpoint
// answers 500 instead of 200 with an empty list. An empty list is what the
// UI shows when the user genuinely has no tasks, so swallowing the error
// makes every task look like it silently vanished.
func (h *AdminTaskHandlers) listTasks(ctx context.Context) ([]config.TaskConfig, error) {
	if ll, ok := h.cfg.Store.(config.LenientTaskLister); ok {
		tasks, errs, err := ll.ListTasksLenient(ctx)
		if err != nil {
			return nil, err
		}
		for _, err := range errs {
			h.log("task_secret_decrypt_failed", "err", err.Error())
		}
		return tasks, nil
	}
	return h.cfg.Store.ListTasks(ctx)
}

// getTaskLenient fetches one task, tolerating secrets that cannot be
// decrypted with the current master key.
func (h *AdminTaskHandlers) getTaskLenient(ctx context.Context, id string) (*config.TaskConfig, error) {
	if g, ok := h.cfg.Store.(config.LenientTaskGetter); ok {
		return g.GetTaskLenient(ctx, id)
	}
	return h.cfg.Store.GetTask(ctx, id)
}

// list returns the full task list as JSON (no secrets).
func (h *AdminTaskHandlers) list(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Store == nil {
		WriteAPIError(w, http.StatusInternalServerError, "store_not_configured", "store missing")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	tasks, err := h.listTasks(ctx)
	if err != nil {
		WriteAPIError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	providers, err := h.providerIndex(ctx)
	if err != nil {
		WriteAPIError(w, http.StatusInternalServerError, "list_providers_failed", err.Error())
		return
	}
	// Convert to the wire-format taskResponse so we emit camelCase keys
	// (id/name/enabled/...) — the frontend's `Task` type expects those.
	// Going through `config.TaskConfig` directly would leak PascalCase.
	out := make([]taskResponse, len(tasks))
	for i := range tasks {
		out[i] = h.view(&tasks[i], providers)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"tasks": out})
}

// get returns a single task by id (no secrets).
func (h *AdminTaskHandlers) get(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Store == nil {
		WriteAPIError(w, http.StatusInternalServerError, "store_not_configured", "store missing")
		return
	}
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	t, err := h.cfg.Store.GetTask(ctx, id)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			WriteAPIError(w, http.StatusNotFound, "task_not_found", id)
			return
		}
		// A master key mismatch must not make the task unreachable: the
		// operator still needs to open the edit form to re-enter the
		// webhook.
		if errors.Is(err, config.ErrMasterKeyMismatch) {
			t, err = h.getTaskLenient(ctx, id)
		}
		if err != nil {
			WriteAPIError(w, http.StatusInternalServerError, "get_failed", err.Error())
			return
		}
	}
	providers, err := h.providerIndex(ctx)
	if err != nil {
		WriteAPIError(w, http.StatusInternalServerError, "list_providers_failed", err.Error())
		return
	}
	// Same camelCase conversion as list() — see comment there.
	writeJSON(w, http.StatusOK, h.view(t, providers))
}

// taskPayload is the request body for creating / updating a task.
//
// The pointer fields let PATCH tell "not mentioned" (keep the stored
// value) apart from an explicit false.
type taskPayload struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Enabled *bool  `json:"enabled,omitempty"`
	// ProviderID is the AI service provider this task probes.
	ProviderID string `json:"provider_id"`
	// Model is the ONE model selected from that provider's Models list.
	Model string `json:"model"`
	// UserPrompt is the probe body.
	UserPrompt string `json:"user_prompt"`
	PromptVars map[string]string `json:"prompt_vars,omitempty"`
	// SystemPrompt is deprecated for monitoring tasks. The field is kept on
	// the wire so older clients / scripts that still send it don't get a 400
	// from readJSON's DisallowUnknownFields, but its value is discarded:
	// scheduled probes never send a system prompt.
	SystemPrompt string `json:"system_prompt,omitempty"`
	Schedule     string `json:"schedule"`
	CanaryGroup  string `json:"canary_group,omitempty"`
	Notify       notifyPayload `json:"notify"`
}

type notifyPayload struct {
	Enabled      bool   `json:"enabled"`
	Channel      string `json:"channel,omitempty"`
	WeComWebhook string `json:"wecom_webhook,omitempty"`
}

// taskResponse is the JSON shape returned to clients — secrets are
// guaranteed absent regardless of what the store has on disk.
//
// The upstream coordinates are not here: they belong to the provider, which
// is nested in the response so the UI can render everything it needs from
// one round trip. The selected Model IS the task's own field.
type taskResponse struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// ProviderID is the provider this task probes.
	ProviderID string `json:"provider_id"`
	// Model is the one model this task selected from the provider.
	Model string `json:"model"`
	// Provider is the upstream this task probes. It is nil when the
	// provider was deleted in the meantime.
	Provider          *providerResponse `json:"provider,omitempty"`
	UserPrompt        string            `json:"user_prompt"`
	PromptVars        map[string]string `json:"prompt_vars"`
	Schedule          string            `json:"schedule"`
	CanaryGroup       string            `json:"canary_group"`
	Notify            notifyView        `json:"notify"`
	HasWebhook        bool              `json:"has_webhook"`
	SecretError       string            `json:"secret_error,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

type notifyView struct {
	Enabled    bool   `json:"enabled"`
	Channel    string `json:"channel"`
	HasWebhook bool   `json:"has_webhook"`
}

// ---- handlers ----

func (h *AdminTaskHandlers) create(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Store == nil {
		WriteAPIError(w, http.StatusInternalServerError, "store_not_configured", "store missing")
		return
	}
	var req taskPayload
	if apiErr := readJSON(r, &req); apiErr != nil {
		WriteAPIError(w, apiErr.Status, apiErr.Code, apiErr.Message)
		return
	}
	tc, err := payloadToTaskConfig(req, "")
	if err != nil {
		WriteAPIError(w, http.StatusBadRequest, "invalid_payload", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	// The task must point at a provider that exists AND offer the model
	// it selected — otherwise it could never be probed.
	if err := h.requireProvider(ctx, tc.ProviderID, tc.Model); err != nil {
		writeProviderError(w, err)
		return
	}
	if err := h.cfg.Store.CreateTask(ctx, tc); err != nil {
		if errors.Is(err, config.ErrAlreadyExists) {
			WriteAPIError(w, http.StatusConflict, "task_exists", err.Error())
			return
		}
		WriteAPIError(w, http.StatusInternalServerError, "create_failed", err.Error())
		return
	}
	// Re-read so we return persisted IDs/timestamps.
	stored, _ := h.cfg.Store.GetTask(ctx, tc.ID)
	if stored != nil {
		providers, err := h.providerIndex(ctx)
		if err != nil {
			writeJSON(w, http.StatusCreated, map[string]any{"id": tc.ID})
			return
		}
		writeJSON(w, http.StatusCreated, h.view(stored, providers))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": tc.ID})
}

func (h *AdminTaskHandlers) patch(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Store == nil {
		WriteAPIError(w, http.StatusInternalServerError, "store_not_configured", "store missing")
		return
	}
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	existing, err := h.cfg.Store.GetTask(ctx, id)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			WriteAPIError(w, http.StatusNotFound, "task_not_found", id)
			return
		}
		WriteAPIError(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	var req taskPayload
	if apiErr := readJSON(r, &req); apiErr != nil {
		WriteAPIError(w, apiErr.Status, apiErr.Code, apiErr.Message)
		return
	}
	updated, err := mergeTaskConfig(existing, req)
	if err != nil {
		WriteAPIError(w, http.StatusBadRequest, "invalid_payload", err.Error())
		return
	}
	if err := h.requireProvider(ctx, updated.ProviderID, updated.Model); err != nil {
		writeProviderError(w, err)
		return
	}
	if err := h.cfg.Store.UpdateTask(ctx, updated); err != nil {
		WriteAPIError(w, http.StatusInternalServerError, "update_failed", err.Error())
		return
	}
	stored, _ := h.cfg.Store.GetTask(ctx, id)
	if stored != nil {
		providers, err := h.providerIndex(ctx)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		writeJSON(w, http.StatusOK, h.view(stored, providers))
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *AdminTaskHandlers) delete(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Store == nil {
		WriteAPIError(w, http.StatusInternalServerError, "store_not_configured", "store missing")
		return
	}
	id := r.PathValue("id")
	if err := h.cfg.Store.DeleteTask(r.Context(), id); err != nil {
		if errors.Is(err, config.ErrNotFound) {
			WriteAPIError(w, http.StatusNotFound, "task_not_found", id)
			return
		}
		WriteAPIError(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// ---- start / stop (§18.12) ----

// start flips a task's persisted Enabled=true and registers its cron entry.
// Order matters: persist first, then Add — so a transient scheduler error
// leaves the row readable as "enabled but not yet registered", which the
// admin can retry. Already-enabled tasks short-circuit (idempotent).
func (h *AdminTaskHandlers) start(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Store == nil {
		WriteAPIError(w, http.StatusInternalServerError, "store_not_configured", "store missing")
		return
	}
	if h.cfg.Scheduler == nil {
		WriteAPIError(w, http.StatusServiceUnavailable, "scheduler_disabled",
			"scheduler is not configured in this build")
		return
	}
	id := r.PathValue("id")
	ctx := r.Context()
	existing, err := h.cfg.Store.GetTask(ctx, id)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			WriteAPIError(w, http.StatusNotFound, "task_not_found", id)
			return
		}
		WriteAPIError(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	if existing.Enabled {
		// Idempotent — already running.
		h.respondStoredTask(w, http.StatusOK, existing)
		return
	}
	// Stop here wraps in a store context with a small timeout so a slow
	// disk doesn't block the request indefinitely.
	storeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	existing.Enabled = true
	if err := h.cfg.Store.UpdateTask(storeCtx, existing); err != nil {
		WriteAPIError(w, http.StatusInternalServerError, "start_persist_failed", err.Error())
		return
	}
	if err := h.cfg.Scheduler.Add(*existing); err != nil {
		// Roll back the persisted flip so DB and scheduler stay in sync.
		existing.Enabled = false
		_ = h.cfg.Store.UpdateTask(storeCtx, existing)
		WriteAPIError(w, http.StatusInternalServerError, "start_schedule_failed", err.Error())
		return
	}
	h.respondStoredTask(w, http.StatusOK, existing)
}

// stop retracts the task's cron entry first (so even a transient DB error
// here can't leave a phantom firing schedule), then flips the persisted
// Enabled=false. Already-disabled tasks short-circuit (idempotent).
func (h *AdminTaskHandlers) stop(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Store == nil {
		WriteAPIError(w, http.StatusInternalServerError, "store_not_configured", "store missing")
		return
	}
	if h.cfg.Scheduler == nil {
		WriteAPIError(w, http.StatusServiceUnavailable, "scheduler_disabled",
			"scheduler is not configured in this build")
		return
	}
	id := r.PathValue("id")
	ctx := r.Context()
	existing, err := h.cfg.Store.GetTask(ctx, id)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			WriteAPIError(w, http.StatusNotFound, "task_not_found", id)
			return
		}
		WriteAPIError(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	// Retract cron entry first. Remove() is idempotent (silently no-op
	// when the id isn't registered), so this is safe even on a task that
	// was never started.
	if err := h.cfg.Scheduler.Remove(id); err != nil {
		WriteAPIError(w, http.StatusInternalServerError, "stop_schedule_failed", err.Error())
		return
	}
	if !existing.Enabled {
		h.respondStoredTask(w, http.StatusOK, existing)
		return
	}
	storeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	existing.Enabled = false
	if err := h.cfg.Store.UpdateTask(storeCtx, existing); err != nil {
		WriteAPIError(w, http.StatusInternalServerError, "stop_persist_failed", err.Error())
		return
	}
	h.respondStoredTask(w, http.StatusOK, existing)
}

// respondStoredTask serialises a task via view + the provider index.
func (h *AdminTaskHandlers) respondStoredTask(w http.ResponseWriter, status int, stored *config.TaskConfig) {
	if stored == nil {
		writeJSON(w, status, map[string]any{"ok": true})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	providers, err := h.providerIndex(ctx)
	if err != nil {
		writeJSON(w, status, map[string]any{"ok": true})
		return
	}
	writeJSON(w, status, h.view(stored, providers))
}

// ---- helpers ----

// providerIndex loads every provider keyed by id, so the task list can be
// rendered without one lookup per row.
func (h *AdminTaskHandlers) providerIndex(ctx context.Context) (map[string]*provider.Provider, error) {
	if h.cfg.Providers == nil {
		return map[string]*provider.Provider{}, nil
	}
	providers, err := h.cfg.Providers.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*provider.Provider, len(providers))
	for i := range providers {
		out[providers[i].ID] = &providers[i]
	}
	return out, nil
}

// requireProvider checks that the provider exists and offers the model
// this task selected.
func (h *AdminTaskHandlers) requireProvider(ctx context.Context, providerID, model string) error {
	if h.cfg.Providers == nil {
		return fmt.Errorf("the provider registry is not available")
	}
	p, err := h.cfg.Providers.GetProvider(ctx, providerID)
	if err != nil {
		return fmt.Errorf("provider %q: %w", providerID, err)
	}
	if !p.HasModel(model) {
		return fmt.Errorf("%w: %q (provider %q)", errUnknownModel, model, p.Name)
	}
	return nil
}

// view assembles the wire shape of a task.
func (h *AdminTaskHandlers) view(t *config.TaskConfig, providers map[string]*provider.Provider) taskResponse {
	if t.PromptVars == nil {
		t.PromptVars = map[string]string{}
	}
	hasHook := len(t.Notify.WeComWebhookEnc) > 0 || t.Notify.WeComWebhook != ""
	out := taskResponse{
		ID:          t.ID,
		Name:        t.Name,
		Enabled:     t.Enabled,
		ProviderID:  t.ProviderID,
		Model:       t.Model,
		UserPrompt:  t.UserPrompt,
		PromptVars:  t.PromptVars,
		Schedule:    t.Schedule,
		CanaryGroup: t.CanaryGroup,
		Notify: notifyView{
			Enabled:    t.Notify.Enabled,
			Channel:    string(t.Notify.Channel),
			HasWebhook: hasHook,
		},
		HasWebhook:  hasHook,
		SecretError: t.SecretError,
		CreatedAt:   t.CreatedAt,
		UpdatedAt:   t.UpdatedAt,
	}
	if p, ok := providers[t.ProviderID]; ok && p != nil {
		pv := providerView(p)
		// The instance-wide TLS switch lives on the handler config, so it
		// is filled in here — the same way the provider handler does it.
		pv.InsecureSkipVerifyGlobal = h.cfg.InsecureSkipVerify
		out.Provider = pv
	}
	return out
}

func payloadToTaskConfig(p taskPayload, existingID string) (*config.TaskConfig, error) {
	if p.Name == "" {
		return nil, errors.New("name required")
	}
	if p.ProviderID == "" {
		return nil, errors.New("provider_id required")
	}
	if p.Model == "" {
		return nil, errors.New("model required")
	}
	if p.UserPrompt == "" {
		return nil, errors.New("user_prompt required")
	}
	if p.Schedule == "" {
		return nil, errors.New("schedule required")
	}
	// Default policy (§18.12): new tasks are created *paused*. Admins flip
	// them on via POST /admin/tasks/{id}/start. This avoids surprise cron
	// pressure from a half-configured task.
	enabled := false
	if p.Enabled != nil {
		enabled = *p.Enabled
	}
	channel := p.Notify.Channel
	if channel == "" {
		channel = string(config.NotifyWeCom)
	}
	tc := &config.TaskConfig{
		ID:         existingID,
		Name:       p.Name,
		Enabled:    enabled,
		ProviderID: p.ProviderID,
		Model:      p.Model,
		UserPrompt: p.UserPrompt,
		// SystemPrompt is intentionally left empty: monitoring tasks have no
		// system prompt. Clearing it here also drops a legacy value when an
		// operator edits an old task, and probe.Runner.Run clears it again
		// for rows/YAML that were never edited.
		PromptVars:  p.PromptVars,
		Schedule:    p.Schedule,
		CanaryGroup: p.CanaryGroup,
		Notify: config.NotifyConfig{
			Enabled:      p.Notify.Enabled,
			Channel:      config.NotifyChannel(channel),
			WeComWebhook: p.Notify.WeComWebhook,
		},
	}
	if tc.ID == "" {
		tc.ID = strings.ReplaceAll(p.Name, " ", "-")
	}
	return tc, nil
}

func mergeTaskConfig(existing *config.TaskConfig, p taskPayload) (*config.TaskConfig, error) {
	// Scalar fallbacks — empty in payload means "keep existing".
	if p.Name == "" {
		p.Name = existing.Name
	}
	if p.ProviderID == "" {
		p.ProviderID = existing.ProviderID
	}
	if p.Model == "" {
		p.Model = existing.Model
	}
	if p.UserPrompt == "" {
		p.UserPrompt = existing.UserPrompt
	}
	if p.Schedule == "" {
		p.Schedule = existing.Schedule
	}
	if p.CanaryGroup == "" {
		p.CanaryGroup = existing.CanaryGroup
	}
	// system_prompt is not merged from the stored task: it is no longer part
	// of a monitoring task and payloadToTaskConfig always writes an empty
	// value. Editing a legacy row therefore clears the old column.
	if p.Notify.Channel == "" {
		p.Notify.Channel = string(existing.Notify.Channel)
	}
	if p.PromptVars == nil {
		p.PromptVars = existing.PromptVars
	}
	// Secrets — empty patch keeps the existing ciphertext.
	if p.Notify.WeComWebhook == "" {
		p.Notify.WeComWebhook = existing.Notify.WeComWebhook
	}
	return payloadToTaskConfig(p, existing.ID)
}

// ---- run history ----

// defaultRunLimit + maxRunLimit cap the ?limit= query parameter so a
// malicious or buggy client can't ask for an unbounded result set.
const (
	defaultRunLimit = 20
	maxRunLimit     = 100
)

// listRuns returns the most recent probe executions for a task. It is the
// only endpoint that reads from the RunStore. The task's existence is
// verified against the config store so we return 404 (not an empty list)
// when callers typo an id.
func (h *AdminTaskHandlers) listRuns(w http.ResponseWriter, r *http.Request) {
	if h.cfg.RunStore == nil {
		WriteAPIError(w, http.StatusServiceUnavailable, "run_store_disabled",
			"probe run history is not enabled in this build")
		return
	}
	id := r.PathValue("id")
	if h.cfg.Store != nil {
		if _, err := h.cfg.Store.GetTask(r.Context(), id); err != nil {
			if errors.Is(err, config.ErrNotFound) {
				WriteAPIError(w, http.StatusNotFound, "task_not_found", id)
				return
			}
			WriteAPIError(w, http.StatusInternalServerError, "get_failed", err.Error())
			return
		}
	}
	limit := defaultRunLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			WriteAPIError(w, http.StatusBadRequest, "bad_limit",
				"limit must be a positive integer")
			return
		}
		if n > maxRunLimit {
			n = maxRunLimit
		}
		limit = n
	}
	runs, err := h.cfg.RunStore.ListByTask(r.Context(), id, limit)
	if err != nil {
		WriteAPIError(w, http.StatusInternalServerError, "runs_list_failed", err.Error())
		return
	}
	// Wrap in {"runs": [...]} so future fields (e.g. pagination cursors)
	// can be added without breaking the wire shape — same convention as
	// /admin/tasks which returns {"tasks": [...]} above.
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}
