// Package server: /admin/tasks CRUD with RBAC. Viewers can read; only
// admins can create / update / delete. Secret columns (apikey, webhook)
// are encrypted server-side and **never** returned to the client.
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
	Logger    LoggerFunc
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
func (h *AdminTaskHandlers) Mount(mux *http.ServeMux) *http.ServeMux {
	// Reads — any authenticated role.
	mux.Handle("GET /admin/tasks", h.cfg.Auth.Authenticate(http.HandlerFunc(h.list)))
	mux.Handle("GET /admin/tasks/{id}", h.cfg.Auth.Authenticate(http.HandlerFunc(h.get)))
	mux.Handle("GET /admin/tasks/{id}/runs", h.cfg.Auth.Authenticate(http.HandlerFunc(h.listRuns)))

	// Writes — admin only.
	adminAuth := func(next http.HandlerFunc) http.Handler {
		return h.cfg.Auth.RequireRole(auth.RoleAdmin, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
		}))
	}
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

// list returns the full task list as JSON (no secrets).
func (h *AdminTaskHandlers) list(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Store == nil {
		WriteAPIError(w, http.StatusInternalServerError, "store_not_configured", "store missing")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	tasks, err := h.cfg.Store.ListTasks(ctx)
	if err != nil {
		WriteAPIError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	// Convert to the wire-format taskResponse so we emit camelCase keys
	// (id/name/enabled/...) — the frontend's `Task` type expects those.
	// Going through `config.TaskConfig` directly would leak PascalCase.
	out := make([]taskResponse, len(tasks))
	for i := range tasks {
		// Capture has-api-key / has-webhook BEFORE we nil the ciphertext,
		// so the UI's "configured (encrypted)" indicator stays correct.
		hasKey := len(tasks[i].APIKeyEnc) > 0
		hasHook := len(tasks[i].Notify.WeComWebhookEnc) > 0
		tasks[i].APIKey = ""
		tasks[i].APIKeyEnc = nil
		tasks[i].Notify.WeComWebhook = ""
		tasks[i].Notify.WeComWebhookEnc = nil
		resp := redactTask(tasks[i])
		resp.HasAPIKey = hasKey
		resp.Notify.HasWebhook = hasHook
		out[i] = resp
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
		WriteAPIError(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	// Capture has-* flags BEFORE nil-ing ciphertext, then restore on the
	// response. See list() for the rationale.
	hasKey := len(t.APIKeyEnc) > 0
	hasHook := len(t.Notify.WeComWebhookEnc) > 0
	t.APIKey = ""
	t.APIKeyEnc = nil
	t.Notify.WeComWebhook = ""
	t.Notify.WeComWebhookEnc = nil
	// Same camelCase conversion as list() — see comment there.
	resp := redactTask(*t)
	resp.HasAPIKey = hasKey
	resp.Notify.HasWebhook = hasHook
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

type taskPayload struct {
	ID         string            `json:"id,omitempty"`
	Name       string            `json:"name"`
	Enabled    *bool             `json:"enabled,omitempty"`
	Protocol   string            `json:"protocol"`
	BaseURL    string            `json:"base_url"`
	APIKey     string            `json:"api_key,omitempty"`
	Model      string            `json:"model"`
	SystemPrompt string          `json:"system_prompt,omitempty"`
	UserPrompt string            `json:"user_prompt"`
	PromptVars map[string]string `json:"prompt_vars,omitempty"`
	Schedule   string            `json:"schedule"`
	// Timeout is a human-readable duration string (e.g. "30s", "1m30s").
	// We accept it as a string here rather than `time.Duration` because
	// Go's stdlib time.Duration JSON decoder requires the unit suffix
	// (e.g. "30s" works but "30" alone is rejected), and accepting a
	// string keeps the wire shape stable if we ever add new units.
	Timeout      string  `json:"timeout,omitempty"`
	CanaryGroup  string  `json:"canary_group,omitempty"`
	Notify     notifyPayload `json:"notify"`
	OpenAI     *config.OpenAIConfig    `json:"openai,omitempty"`
	Anthropic  *config.AnthropicConfig `json:"anthropic,omitempty"`
	Gemini     *config.GeminiConfig    `json:"gemini,omitempty"`
}

type notifyPayload struct {
	Enabled     bool   `json:"enabled"`
	Channel     string `json:"channel,omitempty"`
	WeComWebhook string `json:"wecom_webhook,omitempty"`
}

// taskResponse is the JSON shape returned to clients — secrets are
// guaranteed absent regardless of what the store has on disk.
type taskResponse struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Enabled    bool              `json:"enabled"`
	Protocol   string            `json:"protocol"`
	BaseURL    string            `json:"base_url"`
	Model      string            `json:"model"`
	SystemPrompt string          `json:"system_prompt"`
	UserPrompt string            `json:"user_prompt"`
	PromptVars map[string]string `json:"prompt_vars"`
	Schedule   string            `json:"schedule"`
	Timeout    string            `json:"timeout"`
	CanaryGroup string           `json:"canary_group"`
	Notify     notifyView        `json:"notify"`
	OpenAI     *config.OpenAIConfig    `json:"openai,omitempty"`
	Anthropic  *config.AnthropicConfig `json:"anthropic,omitempty"`
	Gemini     *config.GeminiConfig    `json:"gemini,omitempty"`
	HasAPIKey  bool              `json:"has_api_key"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
}

type notifyView struct {
	Enabled      bool   `json:"enabled"`
	Channel      string `json:"channel"`
	HasWebhook   bool   `json:"has_webhook"`
}

// redactTask strips secrets from t so we never leak api_key / webhook.
func redactTask(t config.TaskConfig) taskResponse {
	if t.PromptVars == nil {
		t.PromptVars = map[string]string{}
	}
	return taskResponse{
		ID:           t.ID,
		Name:         t.Name,
		Enabled:      t.Enabled,
		Protocol:     string(t.Protocol),
		BaseURL:      t.BaseURL,
		Model:        t.Model,
		SystemPrompt: t.SystemPrompt,
		UserPrompt:   t.UserPrompt,
		PromptVars:   t.PromptVars,
		Schedule:     t.Schedule,
		Timeout:      t.EffectiveTimeout().String(),
		CanaryGroup:  t.CanaryGroup,
		Notify: notifyView{
			Enabled:    t.Notify.Enabled,
			Channel:    string(t.Notify.Channel),
			HasWebhook: len(t.Notify.WeComWebhookEnc) > 0 || t.Notify.WeComWebhook != "",
		},
		OpenAI:     t.OpenAI,
		Anthropic:  t.Anthropic,
		Gemini:     t.Gemini,
		HasAPIKey:  len(t.APIKeyEnc) > 0 || t.APIKey != "",
		CreatedAt:  t.CreatedAt,
		UpdatedAt:  t.UpdatedAt,
	}
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
	if err := h.cfg.Store.CreateTask(r.Context(), tc); err != nil {
		if errors.Is(err, config.ErrAlreadyExists) {
			WriteAPIError(w, http.StatusConflict, "task_exists", err.Error())
			return
		}
		WriteAPIError(w, http.StatusInternalServerError, "create_failed", err.Error())
		return
	}
	// Re-read so we return persisted IDs/timestamps.
	stored, _ := h.cfg.Store.GetTask(r.Context(), tc.ID)
	if stored != nil {
		// Capture has-* flags BEFORE nil-ing ciphertext; see list().
		hasKey := len(stored.APIKeyEnc) > 0
		hasHook := len(stored.Notify.WeComWebhookEnc) > 0
		stored.APIKey = ""
		stored.APIKeyEnc = nil
		stored.Notify.WeComWebhook = ""
		stored.Notify.WeComWebhookEnc = nil
		resp := redactTask(*stored)
		resp.HasAPIKey = hasKey
		resp.Notify.HasWebhook = hasHook
		writeJSON(w, http.StatusCreated, resp)
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
	existing, err := h.cfg.Store.GetTask(r.Context(), id)
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
	if err := h.cfg.Store.UpdateTask(r.Context(), updated); err != nil {
		WriteAPIError(w, http.StatusInternalServerError, "update_failed", err.Error())
		return
	}
	stored, _ := h.cfg.Store.GetTask(r.Context(), id)
	if stored != nil {
		// Capture has-* flags BEFORE nil-ing ciphertext; see list().
		hasKey := len(stored.APIKeyEnc) > 0
		hasHook := len(stored.Notify.WeComWebhookEnc) > 0
		stored.APIKey = ""
		stored.APIKeyEnc = nil
		stored.Notify.WeComWebhook = ""
		stored.Notify.WeComWebhookEnc = nil
		resp := redactTask(*stored)
		resp.HasAPIKey = hasKey
		resp.Notify.HasWebhook = hasHook
		writeJSON(w, http.StatusOK, resp)
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
		respondStoredTask(w, http.StatusOK, existing)
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
	respondStoredTask(w, http.StatusOK, existing)
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
		respondStoredTask(w, http.StatusOK, existing)
		return
	}
	storeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	existing.Enabled = false
	if err := h.cfg.Store.UpdateTask(storeCtx, existing); err != nil {
		WriteAPIError(w, http.StatusInternalServerError, "stop_persist_failed", err.Error())
		return
	}
	respondStoredTask(w, http.StatusOK, existing)
}

// respondStoredTask serialises a task via redactTask + has-* flags, sharing
// the same nil-ciphertext-before-capture discipline as list/get/create/patch.
func respondStoredTask(w http.ResponseWriter, status int, stored *config.TaskConfig) {
	if stored == nil {
		writeJSON(w, status, map[string]any{"ok": true})
		return
	}
	hasKey := len(stored.APIKeyEnc) > 0
	hasHook := len(stored.Notify.WeComWebhookEnc) > 0
	stored.APIKey = ""
	stored.APIKeyEnc = nil
	stored.Notify.WeComWebhook = ""
	stored.Notify.WeComWebhookEnc = nil
	resp := redactTask(*stored)
	resp.HasAPIKey = hasKey
	resp.Notify.HasWebhook = hasHook
	writeJSON(w, status, resp)
}

// ---- helpers ----

func payloadToTaskConfig(p taskPayload, existingID string) (*config.TaskConfig, error) {
	if p.Name == "" {
		return nil, errors.New("name required")
	}
	if p.Protocol == "" {
		return nil, errors.New("protocol required")
	}
	if !config.Protocol(p.Protocol).IsValid() {
		return nil, errors.New("protocol invalid")
	}
	if p.BaseURL == "" {
		return nil, errors.New("base_url required")
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
	// Parse timeout string ("30s", "1m", "500ms", …). Empty string is
	// allowed here — config.TaskConfig.EffectiveTimeout() supplies the
	// 30s default downstream.
	var timeout time.Duration
	if p.Timeout != "" {
		d, err := time.ParseDuration(p.Timeout)
		if err != nil {
			return nil, fmt.Errorf("timeout: %w (want e.g. \"30s\", \"1m30s\")", err)
		}
		if d < time.Second || d > 10*time.Minute {
			return nil, errors.New("timeout: must be between 1s and 10m")
		}
		timeout = d
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
		ID:           existingID,
		Name:         p.Name,
		Enabled:      enabled,
		Protocol:     config.Protocol(p.Protocol),
		BaseURL:      p.BaseURL,
		APIKey:       p.APIKey,
		Model:        p.Model,
		SystemPrompt: p.SystemPrompt,
		UserPrompt:   p.UserPrompt,
		PromptVars:   p.PromptVars,
		Schedule:     p.Schedule,
		Timeout:      timeout,
		CanaryGroup:  p.CanaryGroup,
		Notify: config.NotifyConfig{
			Enabled:      p.Notify.Enabled,
			Channel:      config.NotifyChannel(channel),
			WeComWebhook: p.Notify.WeComWebhook,
		},
		OpenAI:    p.OpenAI,
		Anthropic: p.Anthropic,
		Gemini:    p.Gemini,
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
	if p.Protocol == "" {
		p.Protocol = string(existing.Protocol)
	}
	if p.BaseURL == "" {
		p.BaseURL = existing.BaseURL
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
	if p.SystemPrompt == "" {
		p.SystemPrompt = existing.SystemPrompt
	}
	if p.Notify.Channel == "" {
		p.Notify.Channel = string(existing.Notify.Channel)
	}
	if p.PromptVars == nil {
		p.PromptVars = existing.PromptVars
	}
	if p.Timeout == "" {
		p.Timeout = existing.Timeout.String()
	}
	// Protocol-specific blocks — apply *per-field* fallback so callers can
	// update just one knob (e.g. stream_path) without wiping stream_path or
	// max_tokens or any NOT-NULL protocol column.
	if p.OpenAI != nil {
		if existing.OpenAI != nil {
			if p.OpenAI.StreamPath == "" {
				p.OpenAI.StreamPath = existing.OpenAI.StreamPath
			}
			if p.OpenAI.ExtraHeadersJSON == "" {
				p.OpenAI.ExtraHeadersJSON = existing.OpenAI.ExtraHeadersJSON
			}
		}
	} else if existing.OpenAI != nil {
		clone := *existing.OpenAI
		p.OpenAI = &clone
	}
	if p.Anthropic != nil {
		if existing.Anthropic != nil {
			if p.Anthropic.MaxTokens == 0 {
				p.Anthropic.MaxTokens = existing.Anthropic.MaxTokens
			}
			if p.Anthropic.ExtraHeadersJSON == "" {
				p.Anthropic.ExtraHeadersJSON = existing.Anthropic.ExtraHeadersJSON
			}
		}
	} else if existing.Anthropic != nil {
		clone := *existing.Anthropic
		p.Anthropic = &clone
	}
	if p.Gemini != nil {
		if existing.Gemini != nil {
			if p.Gemini.Alt == "" {
				p.Gemini.Alt = existing.Gemini.Alt
			}
			if p.Gemini.ExtraHeadersJSON == "" {
				p.Gemini.ExtraHeadersJSON = existing.Gemini.ExtraHeadersJSON
			}
		} else if p.Gemini.Alt == "" {
			p.Gemini.Alt = "sse"
		}
	} else if existing.Gemini != nil {
		clone := *existing.Gemini
		p.Gemini = &clone
	}
	// Secrets — empty patch keeps the existing ciphertext.
	if p.APIKey == "" {
		p.APIKey = existing.APIKey
	}
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