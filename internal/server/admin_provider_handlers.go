// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

// The AI service provider routes: the registry of upstream AI endpoints
// the operator can point monitoring tasks and AI requests at.
//
// A provider offers several models; tasks and requests each pick one.
// Connectivity is checked on demand per model, not on save. The write
// routes refuse to remove a model that is still in use.
// See patchProvider / checkStored for the two rules above.
// a provider is only persisted after the upstream has accepted a request
// with its own credentials and model. An entry that cannot be reached is
// worse than no entry at all — the monitoring pipeline would keep probing
// a dead endpoint and page somebody about it every few minutes. The
// operator can override the gate per request with "force": true, and the
// verdict of the check is always returned either way.
//
// Reads are open to any authenticated role; writes are admin-only.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/angryang/ai-monitor/internal/auth"
	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/provider"
	"github.com/angryang/ai-monitor/internal/request"
	"github.com/google/uuid"
)

// errUnknownModel marks a model selection that the provider does not
// offer. Both the request and the task handlers map it to the 400
// "unknown_model" code, while a missing provider stays "unknown_provider".
var errUnknownModel = errors.New("model is not offered by the provider")

// ProviderChecker verifies that ONE model of a provider is reachable.
//
// It is an interface so the handler tests can stub the upstream call.
// *provider.Checker is the production implementation.
type ProviderChecker interface {
	CheckModel(ctx context.Context, p *provider.Provider, model string) provider.CheckResult
}

// AdminProviderHandlersConfig carries the dependencies for the provider
// routes.
type AdminProviderHandlersConfig struct {
	Auth *auth.Middleware
	// Store persists the providers. Optional; when nil every route
	// answers 503 so admins know the feature is disabled.
	Store provider.Store
	// Checker runs on-demand connectivity checks. Optional; when nil the
	// check routes answer 503. Write routes no longer depend on it.
	Checker ProviderChecker
	// CheckTimeout bounds one connectivity check. Optional; defaults to
	// provider.DefaultCheckTimeout.
	CheckTimeout time.Duration
	// Requests is the AI request registry. It is consulted before a
	// provider is deleted: a provider that still has requests attached
	// must not be removed, or those requests would silently stop
	// working. Optional; when nil the delete is not guarded.
	Requests request.Store
	// Tasks is the monitoring-task store. It is consulted before a
	// provider is deleted: a provider that still has monitoring tasks
	// attached must not be removed, or those tasks would keep a dangling
	// provider id and fail on every tick. Optional; when nil (or when the
	// store cannot answer the question) the delete is not guarded.
	Tasks config.ConfigStore
	// InsecureSkipVerify mirrors the instance-wide TLS switch so the API
	// can report it alongside each provider.
	InsecureSkipVerify bool
	Logger             LoggerFunc
}

// AdminProviderHandlers is the bundle returned by
// NewAdminProviderHandlers.
type AdminProviderHandlers struct {
	cfg AdminProviderHandlersConfig
}

// NewAdminProviderHandlers wires the bundle.
func NewAdminProviderHandlers(cfg *AdminProviderHandlersConfig) *AdminProviderHandlers {
	if cfg == nil {
		cfg = &AdminProviderHandlersConfig{}
	}
	return &AdminProviderHandlers{cfg: *cfg}
}

// Mount registers every provider route on parent.
//
// The same role policy as /admin/tasks applies: reads need a session,
// writes need the admin role.
func (h *AdminProviderHandlers) Mount(mux *http.ServeMux) *http.ServeMux {
	read := func(next http.HandlerFunc) http.Handler {
		if h.cfg.Auth == nil {
			return next
		}
		return h.cfg.Auth.Authenticate(next)
	}
	adminAuth := func(next http.HandlerFunc) http.Handler {
		if h.cfg.Auth == nil {
			return next
		}
		return h.cfg.Auth.RequireRole(auth.RoleAdmin, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
		}))
	}

	mux.Handle("GET /admin/providers", read(h.list))
	mux.Handle("GET /admin/providers/{id}", read(h.get))
	mux.Handle("POST /admin/providers/check", adminAuth(h.check))
	mux.Handle("POST /admin/providers/{id}/check", adminAuth(h.checkStored))

	mux.Handle("POST /admin/providers", adminAuth(h.create))
	mux.Handle("PATCH /admin/providers/{id}", adminAuth(h.patch))
	mux.Handle("DELETE /admin/providers/{id}", adminAuth(h.delete))
	return mux
}

func (h *AdminProviderHandlers) log(msg string, args ...any) {
	if h.cfg.Logger != nil {
		h.cfg.Logger(msg, args...)
	}
}

// ---- wire shapes ----

// The protocol blocks use their own wire types instead of the
// config.*Config structs: those carry GORM / yaml tags but no JSON ones,
// so their Go field names would leak into the API contract (and the
// frontend already speaks the snake_case shape used by /admin/tasks).

type openAIBlockPayload struct {
	StreamPath   string `json:"stream_path,omitempty"`
	ExtraHeaders string `json:"extra_headers,omitempty"`
}

type anthropicBlockPayload struct {
	MaxTokens    int    `json:"max_tokens,omitempty"`
	ExtraHeaders string `json:"extra_headers,omitempty"`
}

type geminiBlockPayload struct {
	Alt          string `json:"alt,omitempty"`
	ExtraHeaders string `json:"extra_headers,omitempty"`
}

// toConfig converts the wire blocks into their config counterparts. A
// nil receiver means "not mentioned" and stays nil.
func (p *openAIBlockPayload) toConfig() *config.OpenAIConfig {
	if p == nil {
		return nil
	}
	return &config.OpenAIConfig{StreamPath: p.StreamPath, ExtraHeadersJSON: p.ExtraHeaders}
}

func (p *anthropicBlockPayload) toConfig() *config.AnthropicConfig {
	if p == nil {
		return nil
	}
	return &config.AnthropicConfig{MaxTokens: p.MaxTokens, ExtraHeadersJSON: p.ExtraHeaders}
}

func (p *geminiBlockPayload) toConfig() *config.GeminiConfig {
	if p == nil {
		return nil
	}
	return &config.GeminiConfig{Alt: p.Alt, ExtraHeadersJSON: p.ExtraHeaders}
}

// openAIView and friends convert the stored blocks back into the wire
// shape.
func openAIView(c *config.OpenAIConfig) *openAIBlockPayload {
	if c == nil {
		return nil
	}
	return &openAIBlockPayload{StreamPath: c.StreamPath, ExtraHeaders: c.ExtraHeadersJSON}
}

func anthropicView(c *config.AnthropicConfig) *anthropicBlockPayload {
	if c == nil {
		return nil
	}
	return &anthropicBlockPayload{MaxTokens: c.MaxTokens, ExtraHeaders: c.ExtraHeadersJSON}
}

func geminiView(c *config.GeminiConfig) *geminiBlockPayload {
	if c == nil {
		return nil
	}
	return &geminiBlockPayload{Alt: c.Alt, ExtraHeaders: c.ExtraHeadersJSON}
}

// derefString turns an optional wire string into a plain value.
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// providerPayload is the request body for creating / updating a provider.
// The pointer fields let PATCH tell "not mentioned" (keep the stored value)
// apart from an explicit false.
type providerPayload struct {
	Name string `json:"name"`
	// Description is a pointer so a PATCH that omits it keeps the stored
	// value while an explicit "" clears it.
	Description *string `json:"description,omitempty"`
	// Enabled is a pointer for the same reason.
	Enabled  *bool  `json:"enabled,omitempty"`
	Protocol string `json:"protocol"`
	BaseURL  string `json:"base_url"`
	// APIKey is write-only. An empty string on PATCH keeps the stored
	// key; a non-empty string replaces it.
	APIKey string `json:"api_key,omitempty"`
	// Models is the ordered candidate list — the write shape. The first
	// entry is the primary model.
	Models []string `json:"models"`
	// Model has two compatibility roles:
	//   - on the write routes, a single model when Models is omitted;
	//   - on the check routes, the ONE model to probe (defaults to Models[0]).
	Model string `json:"model,omitempty"`
	// Timeout is a human-readable duration ("30s", "1m30s").
	Timeout string `json:"timeout,omitempty"`
	// InsecureSkipVerify skips upstream TLS verification.
	InsecureSkipVerify *bool `json:"insecure_skip_verify,omitempty"`
	// ThinkingEnabled is the provider's default reasoning-trace switch.
	ThinkingEnabled *bool `json:"thinking_enabled,omitempty"`
	// Force is accepted but has no effect: saving no longer runs a
	// connectivity check. Kept so older clients do not fail on the
	// unknown-field guard.
	Force bool `json:"force,omitempty"`

	OpenAI    *openAIBlockPayload    `json:"openai,omitempty"`
	Anthropic *anthropicBlockPayload `json:"anthropic,omitempty"`
	Gemini    *geminiBlockPayload    `json:"gemini,omitempty"`
}

// providerResponse is the wire shape of one provider. The api key is
// never included — only whether one is configured.
type providerResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	Protocol    string `json:"protocol"`
	BaseURL     string   `json:"base_url"`
	Models      []string `json:"models"`
	// Timeout is rendered as a human-readable duration.
	Timeout string `json:"timeout"`
	// InsecureSkipVerify is the provider's own switch;
	// InsecureSkipVerifyGlobal mirrors the instance-wide one so the UI
	// can explain why verification is still off.
	InsecureSkipVerify       bool `json:"insecure_skip_verify"`
	InsecureSkipVerifyGlobal bool `json:"insecure_skip_verify_global"`
	ThinkingEnabled          bool `json:"thinking_enabled"`
	// HasAPIKey reports whether a key is configured.
	HasAPIKey bool `json:"has_api_key"`
	// SecretError is non-empty when the stored key could not be
	// decrypted with the current master key.
	SecretError string `json:"secret_error,omitempty"`

	OpenAI    *openAIBlockPayload    `json:"openai,omitempty"`
	Anthropic *anthropicBlockPayload `json:"anthropic,omitempty"`
	Gemini    *geminiBlockPayload    `json:"gemini,omitempty"`

	// LastVerified* record the verdict of the most recent on-demand
	// check (run against one particular model).
	LastVerifiedAt      *time.Time `json:"last_verified_at,omitempty"`
	LastVerifiedOK      bool       `json:"last_verified_ok"`
	LastVerifiedMessage string     `json:"last_verified_message,omitempty"`

	// Check carries the verdict of the check that ran with this request.
	// It is present on the write routes and on the check routes.
	Check *provider.CheckResult `json:"check,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ---- handlers ----

// list returns every provider.
func (h *AdminProviderHandlers) list(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Store == nil {
		WriteAPIError(w, http.StatusServiceUnavailable, "provider_store_disabled",
			"the AI provider registry is not enabled in this build")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	providers, err := h.cfg.Store.ListProviders(ctx)
	if err != nil {
		WriteAPIError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	out := make([]providerResponse, 0, len(providers))
	for i := range providers {
		out = append(out, h.view(&providers[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}

// get returns one provider.
func (h *AdminProviderHandlers) get(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Store == nil {
		WriteAPIError(w, http.StatusServiceUnavailable, "provider_store_disabled",
			"the AI provider registry is not enabled in this build")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	p, err := h.cfg.Store.GetProvider(ctx, r.PathValue("id"))
	if err != nil {
		h.writeStoreError(w, err, "provider")
		return
	}
	writeJSON(w, http.StatusOK, h.view(p))
}

// create registers a new provider. No upstream call is made on save.
func (h *AdminProviderHandlers) create(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Store == nil {
		WriteAPIError(w, http.StatusServiceUnavailable, "provider_store_disabled",
			"the AI provider registry is not enabled in this build")
		return
	}
	var req providerPayload
	if apiErr := readJSON(r, &req); apiErr != nil {
		WriteAPIError(w, apiErr.Status, apiErr.Code, apiErr.Message)
		return
	}
	p, err := providerFromPayload(req, "")
	if err != nil {
		WriteAPIError(w, http.StatusBadRequest, "invalid_payload", err.Error())
		return
	}
	if err := validateProvider(p); err != nil {
		WriteAPIError(w, http.StatusBadRequest, "invalid_payload", err.Error())
		return
	}
	// Saving does not contact the upstream. With several models per
	// provider, probing all of them on every edit would be slow and one
	// unreachable model would block the whole save; the operator tests
	// individual models on demand instead.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := h.cfg.Store.CreateProvider(ctx, p); err != nil {
		h.writeStoreError(w, err, "provider")
		return
	}
	h.log("provider_created", "provider_id", p.ID, "name", p.Name, "models", len(p.Models))
	stored, err := h.cfg.Store.GetProvider(ctx, p.ID)
	if err != nil {
		writeJSON(w, http.StatusCreated, map[string]any{"id": p.ID})
		return
	}
	writeJSON(w, http.StatusCreated, h.view(stored))
}

// patch updates an existing provider. No upstream call is made on save.
func (h *AdminProviderHandlers) patch(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Store == nil {
		WriteAPIError(w, http.StatusServiceUnavailable, "provider_store_disabled",
			"the AI provider registry is not enabled in this build")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	existing, err := h.cfg.Store.GetProvider(ctx, r.PathValue("id"))
	if err != nil {
		h.writeStoreError(w, err, "provider")
		return
	}
	var req providerPayload
	if apiErr := readJSON(r, &req); apiErr != nil {
		WriteAPIError(w, apiErr.Status, apiErr.Code, apiErr.Message)
		return
	}
	updated, err := mergeProvider(existing, req)
	if err != nil {
		WriteAPIError(w, http.StatusBadRequest, "invalid_payload", err.Error())
		return
	}
	// Validate the merged result, not the partial payload: a PATCH that
	// only flips thinking_enabled must not be rejected for lacking a name
	// or a protocol.
	if err := validateProvider(updated); err != nil {
		WriteAPIError(w, http.StatusBadRequest, "invalid_payload", err.Error())
		return
	}
	// A model that disappears from the list must not still be selected by
	// a running job — refuse the edit and name the jobs to re-point.
	if removed := removedModels(existing.Models, updated.Models); len(removed) > 0 {
		if h.rejectModelInUse(w, ctx, existing.ID, removed) {
			return
		}
	}
	// Saving is not a connectivity check: keep the previous verdict.
	updated.LastVerifiedAt = existing.LastVerifiedAt
	updated.LastVerifiedOK = existing.LastVerifiedOK
	updated.LastVerifiedMessage = existing.LastVerifiedMessage

	if err := h.cfg.Store.UpdateProvider(ctx, updated); err != nil {
		h.writeStoreError(w, err, "provider")
		return
	}
	h.log("provider_updated", "provider_id", updated.ID, "models", len(updated.Models))
	stored, err := h.cfg.Store.GetProvider(ctx, updated.ID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeJSON(w, http.StatusOK, h.view(stored))
}

// delete removes a provider.
func (h *AdminProviderHandlers) delete(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Store == nil {
		WriteAPIError(w, http.StatusServiceUnavailable, "provider_store_disabled",
			"the AI provider registry is not enabled in this build")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	id := r.PathValue("id")

	// A request points at its provider by id. Deleting a provider that
	// still has requests attached would leave those requests unusable
	// without any visible change in the UI, so refuse it and name the
	// requests that need to be re-pointed first.
	if h.cfg.Requests != nil {
		used, err := h.cfg.Requests.ListRequestsByProvider(ctx, id)
		if err != nil {
			h.log("provider_usage_check_failed", "provider_id", id, "error", err.Error())
			WriteAPIError(w, http.StatusInternalServerError, "provider_usage_check_failed",
				"could not check whether the provider is still in use")
			return
		}
		if len(used) > 0 {
			names := make([]string, 0, len(used))
			for i := range used {
				names = append(names, used[i].Name)
			}
			WriteAPIErrorDetails(w, http.StatusConflict, "provider_in_use",
				fmt.Sprintf("provider is still used by %d AI request(s)", len(used)),
				map[string]any{"requests": names})
			return
		}
	}

	// The same holds for monitoring tasks: they name their provider, so a
	// provider that is still in use must not be removed.
	if lister, ok := h.cfg.Tasks.(config.TaskProviderLister); ok && lister != nil {
		used, err := lister.ListTasksByProvider(ctx, id)
		if err != nil {
			h.log("provider_usage_check_failed", "provider_id", id, "error", err.Error())
			WriteAPIError(w, http.StatusInternalServerError, "provider_usage_check_failed",
				"could not check whether the provider is still in use")
			return
		}
		if len(used) > 0 {
			names := make([]string, 0, len(used))
			for i := range used {
				names = append(names, used[i].Name)
			}
			WriteAPIErrorDetails(w, http.StatusConflict, "provider_in_use",
				fmt.Sprintf("provider is still used by %d monitoring task(s)", len(used)),
				map[string]any{"tasks": names})
			return
		}
	}

	if err := h.cfg.Store.DeleteProvider(ctx, id); err != nil {
		h.writeStoreError(w, err, "provider")
		return
	}
	h.log("provider_deleted", "provider_id", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// check verifies a provider described by the request body without saving
// it. It is what the form's "test" button calls.
func (h *AdminProviderHandlers) check(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Checker == nil {
		WriteAPIError(w, http.StatusServiceUnavailable, "provider_check_disabled",
			"the provider connectivity check is not enabled in this build")
		return
	}
	var req providerPayload
	if apiErr := readJSON(r, &req); apiErr != nil {
		WriteAPIError(w, apiErr.Status, apiErr.Code, apiErr.Message)
		return
	}
	p, err := providerFromPayload(req, "")
	if err != nil {
		WriteAPIError(w, http.StatusBadRequest, "invalid_payload", err.Error())
		return
	}
	if err := validateProvider(p); err != nil {
		WriteAPIError(w, http.StatusBadRequest, "invalid_payload", err.Error())
		return
	}
	// req.Model names the ONE model to probe. An empty value means the
	// primary model, which is what the form sends for an ad-hoc check.
	result := h.runCheck(r.Context(), p, req.Model)
	writeJSON(w, http.StatusOK, result)
}

// checkStored verifies a saved provider with its stored credentials.
// The api key never leaves the server, so this is the only way to
// re-verify a provider without re-typing its key.
func (h *AdminProviderHandlers) checkStored(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Store == nil {
		WriteAPIError(w, http.StatusServiceUnavailable, "provider_store_disabled",
			"the AI provider registry is not enabled in this build")
		return
	}
	if h.cfg.Checker == nil {
		WriteAPIError(w, http.StatusServiceUnavailable, "provider_check_disabled",
			"the provider connectivity check is not enabled in this build")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	p, err := h.cfg.Store.GetProvider(ctx, r.PathValue("id"))
	if err != nil {
		h.writeStoreError(w, err, "provider")
		return
	}
	// The body is optional: {"model":"..."} selects one model, an empty
	// body checks the primary one.
	var body struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	model := body.Model
	if model == "" {
		model = p.PrimaryModel()
	}
	if !p.HasModel(model) {
		WriteAPIError(w, http.StatusBadRequest, "unknown_model",
			fmt.Sprintf("model %q is not offered by this provider", model))
		return
	}
	result := h.runCheck(r.Context(), p, model)
	// Persist the verdict so the list badge reflects this check. A failed
	// write is only logged: the check result is still useful to the caller.
	p.LastVerifiedAt = &result.CheckedAt
	p.LastVerifiedOK = result.OK
	p.LastVerifiedMessage = result.Message
	if err := h.cfg.Store.UpdateProvider(ctx, p); err != nil {
		h.log("provider_check_persist_failed", "provider_id", p.ID, "error", err.Error())
	}
	writeJSON(w, http.StatusOK, result)
}

// ---- helpers ----

// removedModels returns the entries of old that are gone from next.
func removedModels(old, next []string) []string {
	keep := make(map[string]bool, len(next))
	for _, m := range next {
		keep[m] = true
	}
	var out []string
	for _, m := range old {
		if !keep[m] {
			out = append(out, m)
		}
	}
	return out
}

// rejectModelInUse writes a 409 when a model being removed from a provider
// is still selected by an AI request or a monitoring task. It reports
// whether a response was written.
func (h *AdminProviderHandlers) rejectModelInUse(w http.ResponseWriter, ctx context.Context, providerID string, removed []string) bool {
	gone := make(map[string]bool, len(removed))
	for _, m := range removed {
		gone[m] = true
	}
	var reqNames, taskNames []string

	if h.cfg.Requests != nil {
		used, err := h.cfg.Requests.ListRequestsByProvider(ctx, providerID)
		if err != nil {
			h.log("provider_usage_check_failed", "provider_id", providerID, "error", err.Error())
			WriteAPIError(w, http.StatusInternalServerError, "provider_usage_check_failed",
				"could not check whether the model is still in use")
			return true
		}
		for i := range used {
			if gone[used[i].Model] {
				reqNames = append(reqNames, used[i].Name)
			}
		}
	}
	if lister, ok := h.cfg.Tasks.(config.TaskProviderLister); ok && lister != nil {
		used, err := lister.ListTasksByProvider(ctx, providerID)
		if err != nil {
			h.log("provider_usage_check_failed", "provider_id", providerID, "error", err.Error())
			WriteAPIError(w, http.StatusInternalServerError, "provider_usage_check_failed",
				"could not check whether the model is still in use")
			return true
		}
		for i := range used {
			if gone[used[i].Model] {
				taskNames = append(taskNames, used[i].Name)
			}
		}
	}
	if len(reqNames) == 0 && len(taskNames) == 0 {
		return false
	}
	WriteAPIErrorDetails(w, http.StatusConflict, "model_in_use",
		fmt.Sprintf("model(s) %v are still selected by %d AI request(s) and %d monitoring task(s)",
			removed, len(reqNames), len(taskNames)),
		map[string]any{"models": removed, "requests": reqNames, "tasks": taskNames})
	return true
}

// runCheck performs one connectivity check for a single model under the
// configured deadline.
func (h *AdminProviderHandlers) runCheck(parent context.Context, p *provider.Provider, model string) provider.CheckResult {
	timeout := h.cfg.CheckTimeout
	if timeout <= 0 {
		timeout = provider.DefaultCheckTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	if model == "" {
		model = p.PrimaryModel()
	}
	return h.cfg.Checker.CheckModel(ctx, p, model)
}

// view assembles the wire shape of a provider, including the
// instance-wide TLS switch that only the handler knows about.
func (h *AdminProviderHandlers) view(p *provider.Provider) providerResponse {
	v := providerView(p)
	v.InsecureSkipVerifyGlobal = h.cfg.InsecureSkipVerify
	return *v
}

// providerView is the handler-independent form, used when a provider is
// embedded in another resource (e.g. an AI request) and there is no
// provider handler around to supply the global TLS setting.
func providerView(p *provider.Provider) *providerResponse {
	models := p.Models
	if len(models) == 0 && p.Model != "" {
		models = []string{p.Model}
	}
	return &providerResponse{
		ID:                  p.ID,
		Name:                p.Name,
		Description:         p.Description,
		Enabled:             p.Enabled,
		Protocol:            string(p.Protocol),
		BaseURL:             p.BaseURL,
		Models:              models,
		Timeout:             p.EffectiveTimeout().String(),
		InsecureSkipVerify:  p.InsecureSkipVerify,
		ThinkingEnabled:     p.ThinkingEnabled,
		HasAPIKey:           p.APIKey != "" || len(p.APIKeyEnc) > 0,
		SecretError:         p.SecretError,
		OpenAI:              openAIView(p.OpenAI),
		Anthropic:           anthropicView(p.Anthropic),
		Gemini:              geminiView(p.Gemini),
		LastVerifiedAt:      p.LastVerifiedAt,
		LastVerifiedOK:      p.LastVerifiedOK,
		LastVerifiedMessage: p.LastVerifiedMessage,
		CreatedAt:           p.CreatedAt,
		UpdatedAt:           p.UpdatedAt,
	}
}

// validateProvider checks the required fields of a complete provider.
func validateProvider(p *provider.Provider) error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("name required")
	}
	if !p.Protocol.IsValid() {
		return fmt.Errorf("protocol invalid: %q", p.Protocol)
	}
	if strings.TrimSpace(p.BaseURL) == "" {
		return errors.New("base_url required")
	}
	if len(p.Models) == 0 {
		return errors.New("models required")
	}
	for _, m := range p.Models {
		if strings.TrimSpace(m) == "" {
			return errors.New("models must not contain an empty name")
		}
	}
	return nil
}

// providerFromPayload parses the payload and builds the provider. It
// validates only what is present; required-field checks happen on the
// merged result in createProvider / patchProvider, so a partial PATCH is
// accepted.
func providerFromPayload(p providerPayload, id string) (*provider.Provider, error) {
	if p.Protocol != "" && !config.Protocol(p.Protocol).IsValid() {
		return nil, fmt.Errorf("protocol invalid: %q", p.Protocol)
	}
	timeout := provider.DefaultTimeout
	if p.Timeout != "" {
		d, err := time.ParseDuration(p.Timeout)
		if err != nil {
			return nil, fmt.Errorf("timeout: %w (want e.g. \"30s\", \"1m30s\")", err)
		}
		if d < time.Second || d > provider.MaxTimeout {
			return nil, fmt.Errorf("timeout: must be between 1s and %s", provider.MaxTimeout)
		}
		timeout = d
	}
	// Models is the canonical write shape. A lone `model` is accepted
	// from older clients; on the check routes it also names the model to
	// probe, so it must not overwrite an explicit Models list.
	models := p.Models
	if len(models) == 0 && p.Model != "" {
		models = []string{p.Model}
	}
	out := &provider.Provider{
		ID:          id,
		Name:        p.Name,
		Description: derefString(p.Description),
		Enabled:     true,
		Protocol:    config.Protocol(p.Protocol),
		BaseURL:     p.BaseURL,
		APIKey:      p.APIKey,
		Models:      models,
		TimeoutMS:   timeout.Milliseconds(),
		OpenAI:      p.OpenAI.toConfig(),
		Anthropic:   p.Anthropic.toConfig(),
		Gemini:      p.Gemini.toConfig(),
	}
	if out.ID == "" {
		out.ID = uuid.NewString()
	}
	if p.Enabled != nil {
		out.Enabled = *p.Enabled
	}
	if p.InsecureSkipVerify != nil {
		out.InsecureSkipVerify = *p.InsecureSkipVerify
	}
	if p.ThinkingEnabled != nil {
		out.ThinkingEnabled = *p.ThinkingEnabled
	}
	return out, nil
}

// mergeProvider applies a PATCH payload onto a stored provider. Fields
// the payload didn't mention keep their stored value; the api key is only
// replaced when a non-empty value arrives — sending an empty string must
// not wipe a configured key.
func mergeProvider(existing *provider.Provider, req providerPayload) (*provider.Provider, error) {
	updated, err := providerFromPayload(req, existing.ID)
	if err != nil {
		return nil, err
	}
	if req.Name == "" {
		updated.Name = existing.Name
	}
	if req.Description == nil {
		updated.Description = existing.Description
	}
	if req.Enabled == nil {
		updated.Enabled = existing.Enabled
	}
	if req.Protocol == "" {
		updated.Protocol = existing.Protocol
	}
	if req.BaseURL == "" {
		updated.BaseURL = existing.BaseURL
	}
	if len(req.Models) == 0 && req.Model == "" {
		updated.Models = existing.Models
		updated.ModelsJSON = existing.ModelsJSON
	}
	if req.Timeout == "" {
		updated.TimeoutMS = existing.TimeoutMS
	}
	if req.APIKey == "" {
		updated.APIKey = existing.APIKey
		updated.APIKeyEnc = existing.APIKeyEnc
	}
	if req.InsecureSkipVerify == nil {
		updated.InsecureSkipVerify = existing.InsecureSkipVerify
	}
	if req.ThinkingEnabled == nil {
		updated.ThinkingEnabled = existing.ThinkingEnabled
	}
	if req.OpenAI == nil {
		updated.OpenAI = existing.OpenAI
	}
	if req.Anthropic == nil {
		updated.Anthropic = existing.Anthropic
	}
	if req.Gemini == nil {
		updated.Gemini = existing.Gemini
	}
	return updated, nil
}

// writeStoreError maps a store error onto the HTTP status codes.
func (h *AdminProviderHandlers) writeStoreError(w http.ResponseWriter, err error, kind string) {
	switch {
	case errors.Is(err, provider.ErrNotFound):
		WriteAPIError(w, http.StatusNotFound, kind+"_not_found", err.Error())
	case errors.Is(err, provider.ErrAlreadyExists):
		WriteAPIError(w, http.StatusConflict, kind+"_exists", err.Error())
	case errors.Is(err, provider.ErrInvalidConfig):
		WriteAPIError(w, http.StatusBadRequest, "invalid_payload", err.Error())
	default:
		WriteAPIError(w, http.StatusInternalServerError, kind+"_write_failed", err.Error())
	}
}
