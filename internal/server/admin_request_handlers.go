// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

// The AI request manager routes: the registry of predefined AI requests.
//
// A request is one prompt (system / user + ${var} substitutions) bound to
// one AI service provider. There is no "request group" layer any more:
// every request points directly at its provider, so the same upstream can
// serve several requests and different requests can target different
// upstreams.
//
// The run endpoint also accepts per-request content overrides for this one
// execution; they are applied in memory and never persisted.
//
// Execution is asynchronous by design. An AI request can take 30s+ and the
// HTTP server's WriteTimeout is 5s, so a synchronous endpoint would be cut
// off mid-flight. The run endpoint therefore answers 202 with a batch id
// and the client polls GET /admin/request-batches/{id} for progress.
//
// Reads are open to any authenticated role; everything that writes or
// triggers a real upstream call is admin-only.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/angryang/ai-monitor/internal/auth"
	"github.com/angryang/ai-monitor/internal/provider"
	"github.com/angryang/ai-monitor/internal/request"
	"github.com/google/uuid"
)

// AdminRequestHandlersConfig carries the dependencies for the AI request
// manager routes.
type AdminRequestHandlersConfig struct {
	Auth *auth.Middleware
	// Store persists the requests. Optional; when nil every route
	// answers 503 so admins know the feature is disabled.
	Store request.Store
	// Service executes the batches. Optional; when nil the run routes
	// answer 503.
	Service *request.Service
	// Providers backs the provider summary embedded in every request and
	// validates the provider a request points at. Optional; when nil the
	// routes answer 503 — a request without a provider cannot be run.
	Providers provider.Store
	Logger    LoggerFunc
}

// AdminRequestHandlers is the bundle returned by
// NewAdminRequestHandlers.
type AdminRequestHandlers struct {
	cfg AdminRequestHandlersConfig
}

// NewAdminRequestHandlers wires the bundle.
func NewAdminRequestHandlers(cfg *AdminRequestHandlersConfig) *AdminRequestHandlers {
	if cfg == nil {
		cfg = &AdminRequestHandlersConfig{}
	}
	return &AdminRequestHandlers{cfg: *cfg}
}

// Mount registers every AI-request-manager route on parent.
//
// The same role policy as /admin/tasks applies: reads need a session,
// writes and executions need the admin role.
func (h *AdminRequestHandlers) Mount(mux *http.ServeMux) *http.ServeMux {
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

	mux.Handle("GET /admin/requests", read(h.list))
	mux.Handle("GET /admin/requests/{id}", read(h.get))
	mux.Handle("GET /admin/request-batches/{id}", read(h.getBatch))

	mux.Handle("POST /admin/requests", adminAuth(h.create))
	mux.Handle("PATCH /admin/requests/{id}", adminAuth(h.patch))
	mux.Handle("DELETE /admin/requests/{id}", adminAuth(h.delete))
	mux.Handle("POST /admin/requests/run", adminAuth(h.run))
	return mux
}

func (h *AdminRequestHandlers) log(msg string, args ...any) {
	if h.cfg.Logger != nil {
		h.cfg.Logger(msg, args...)
	}
}

// ---- wire shapes ----

// requestPayload is the request body for creating / updating a request.
// The pointer fields let PATCH tell "not mentioned" (keep the stored
// value) apart from an explicit false.
type requestPayload struct {
	Name string `json:"name"`
	// Description is a pointer so a PATCH that omits it keeps the stored
	// value while an explicit "" clears it.
	Description *string `json:"description,omitempty"`
	// ProviderID is the AI service provider this request is sent to.
	ProviderID string `json:"provider_id"`
	// Model is the ONE model selected from that provider's Models list.
	Model string `json:"model"`
	// SystemPrompt is optional; UserPrompt is required on create. Both are
	// pointers where needed so a partial PATCH keeps the stored value.
	SystemPrompt *string `json:"system_prompt,omitempty"`
	UserPrompt   string  `json:"user_prompt"`
	// PromptVars are the ${var} substitutions for the prompts.
	PromptVars map[string]string `json:"prompt_vars,omitempty"`
	// Order controls the position of the request in the list.
	Order *int `json:"order,omitempty"`
	// ThinkingOverride optionally overrides the provider's thinking switch.
	ThinkingOverride *bool `json:"thinking_override,omitempty"`
}

// contentOverridePayload is the wire shape of one ad-hoc content
// override. Pointer fields keep "not mentioned" distinct from an
// explicitly cleared value.
type contentOverridePayload struct {
	SystemPrompt *string           `json:"system_prompt,omitempty"`
	UserPrompt   *string           `json:"user_prompt,omitempty"`
	PromptVars   map[string]string `json:"prompt_vars,omitempty"`
}

// runPayload is the request body of the run endpoint.
type runPayload struct {
	// RequestIDs are the requests to fire.
	RequestIDs []string `json:"request_ids"`
	// Concurrency caps the number of in-flight requests. 0 means the
	// service default.
	Concurrency int `json:"concurrency,omitempty"`
	// Overrides carries custom request content for this execution.
	Overrides map[string]contentOverridePayload `json:"overrides,omitempty"`
}

// overridesFromPayload converts the wire overrides into the service's
// types. A nil result means "use the predefined content for everything".
func overridesFromPayload(in map[string]contentOverridePayload) map[string]request.ContentOverride {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]request.ContentOverride, len(in))
	for id, p := range in {
		out[id] = request.ContentOverride{
			SystemPrompt: p.SystemPrompt,
			UserPrompt:   p.UserPrompt,
			PromptVars:   p.PromptVars,
		}
	}
	return out
}

// readOptionalJSON decodes a run payload while tolerating an empty body.
// Older clients call the run endpoint with no body at all (or `{}`), and
// a Content-Length mismatch must not turn a valid execution into a 400.
func readOptionalJSON(r *http.Request, v any) *APIError {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return &APIError{Status: http.StatusBadRequest, Code: "bad_json", Message: err.Error()}
	}
	return nil
}

// requestView is the wire shape of one AI request. The provider it points
// at is nested so the UI can render the list without a second round trip.
type requestView struct {
	ID          string `json:"id"`
	ProviderID  string `json:"provider_id"`
	Model       string `json:"model"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// SystemPrompt / UserPrompt are the request body.
	SystemPrompt string            `json:"system_prompt"`
	UserPrompt   string            `json:"user_prompt"`
	PromptVars   map[string]string `json:"prompt_vars"`
	Order        int               `json:"order"`
	// ThinkingOverride is nil when the request inherits the provider's
	// thinking switch.
	ThinkingOverride *bool `json:"thinking_override,omitempty"`
	// Provider is the upstream this request is sent to.
	Provider  *providerResponse `json:"provider,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// ---- handlers ----

// list returns every request with its provider nested.
func (h *AdminRequestHandlers) list(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Store == nil {
		WriteAPIError(w, http.StatusServiceUnavailable, "request_store_disabled",
			"the AI request manager is not enabled in this build")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	requests, err := h.cfg.Store.ListRequests(ctx)
	if err != nil {
		WriteAPIError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	providers, err := h.providerIndex(ctx)
	if err != nil {
		WriteAPIError(w, http.StatusInternalServerError, "list_providers_failed", err.Error())
		return
	}
	out := make([]requestView, 0, len(requests))
	for i := range requests {
		out = append(out, h.view(&requests[i], providers))
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": out})
}

// get returns one request with its provider nested.
func (h *AdminRequestHandlers) get(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Store == nil {
		WriteAPIError(w, http.StatusServiceUnavailable, "request_store_disabled",
			"the AI request manager is not enabled in this build")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	req, err := h.cfg.Store.GetRequest(ctx, r.PathValue("id"))
	if err != nil {
		h.writeStoreError(w, err, "request")
		return
	}
	providers, err := h.providerIndex(ctx)
	if err != nil {
		WriteAPIError(w, http.StatusInternalServerError, "list_providers_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, h.view(req, providers))
}

// create registers a new request.
func (h *AdminRequestHandlers) create(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Store == nil {
		WriteAPIError(w, http.StatusServiceUnavailable, "request_store_disabled",
			"the AI request manager is not enabled in this build")
		return
	}
	var req requestPayload
	if apiErr := readJSON(r, &req); apiErr != nil {
		WriteAPIError(w, apiErr.Status, apiErr.Code, apiErr.Message)
		return
	}
	created, err := requestFromPayload(req, "")
	if err != nil {
		WriteAPIError(w, http.StatusBadRequest, "invalid_payload", err.Error())
		return
	}
	if err := validateRequest(created); err != nil {
		WriteAPIError(w, http.StatusBadRequest, "invalid_payload", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	// The request must point at a provider that exists AND offer the
	// model it selected — otherwise it could never be executed.
	if err := h.requireProvider(ctx, created.ProviderID, created.Model); err != nil {
		writeProviderError(w, err)
		return
	}
	if err := h.cfg.Store.CreateRequest(ctx, created); err != nil {
		h.writeStoreError(w, err, "request")
		return
	}
	h.log("ai_request_created", "request_id", created.ID, "provider_id", created.ProviderID)
	stored, err := h.cfg.Store.GetRequest(ctx, created.ID)
	if err != nil {
		writeJSON(w, http.StatusCreated, map[string]any{"id": created.ID})
		return
	}
	providers, err := h.providerIndex(ctx)
	if err != nil {
		writeJSON(w, http.StatusCreated, map[string]any{"id": created.ID})
		return
	}
	writeJSON(w, http.StatusCreated, h.view(stored, providers))
}

// patch updates an existing request.
func (h *AdminRequestHandlers) patch(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Store == nil {
		WriteAPIError(w, http.StatusServiceUnavailable, "request_store_disabled",
			"the AI request manager is not enabled in this build")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	existing, err := h.cfg.Store.GetRequest(ctx, r.PathValue("id"))
	if err != nil {
		h.writeStoreError(w, err, "request")
		return
	}
	var req requestPayload
	if apiErr := readJSON(r, &req); apiErr != nil {
		WriteAPIError(w, apiErr.Status, apiErr.Code, apiErr.Message)
		return
	}
	updated, err := requestFromPayload(req, existing.ID)
	if err != nil {
		WriteAPIError(w, http.StatusBadRequest, "invalid_payload", err.Error())
		return
	}
	// Merge the fields the payload didn't mention. The provider is only
	// replaced when the payload names a different one.
	if req.Name == "" {
		updated.Name = existing.Name
	}
	if req.Description == nil {
		updated.Description = existing.Description
	}
	if req.ProviderID == "" {
		updated.ProviderID = existing.ProviderID
	}
	if req.Model == "" {
		updated.Model = existing.Model
	}
	if req.SystemPrompt == nil {
		updated.SystemPrompt = existing.SystemPrompt
	}
	if req.UserPrompt == "" {
		updated.UserPrompt = existing.UserPrompt
	}
	if req.PromptVars == nil {
		updated.PromptVars = existing.PromptVars
	}
	if req.Order == nil {
		updated.Order = existing.Order
	}
	if req.ThinkingOverride == nil {
		updated.ThinkingOverride = existing.ThinkingOverride
	}
	// Validate the merged result, not the partial payload: a PATCH that
	// only flips thinking_override must not be rejected for lacking a name
	// or a prompt.
	if err := validateRequest(updated); err != nil {
		WriteAPIError(w, http.StatusBadRequest, "invalid_payload", err.Error())
		return
	}
	if err := h.requireProvider(ctx, updated.ProviderID, updated.Model); err != nil {
		writeProviderError(w, err)
		return
	}
	if err := h.cfg.Store.UpdateRequest(ctx, updated); err != nil {
		h.writeStoreError(w, err, "request")
		return
	}
	h.log("ai_request_updated", "request_id", updated.ID, "provider_id", updated.ProviderID)
	stored, err := h.cfg.Store.GetRequest(ctx, updated.ID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	providers, err := h.providerIndex(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeJSON(w, http.StatusOK, h.view(stored, providers))
}

// delete removes a request.
func (h *AdminRequestHandlers) delete(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Store == nil {
		WriteAPIError(w, http.StatusServiceUnavailable, "request_store_disabled",
			"the AI request manager is not enabled in this build")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	id := r.PathValue("id")
	if err := h.cfg.Store.DeleteRequest(ctx, id); err != nil {
		h.writeStoreError(w, err, "request")
		return
	}
	h.log("ai_request_deleted", "request_id", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// run fires a selection of requests. Each one goes to its own provider.
//
// It answers 202 with the batch handle; the client polls
// GET /admin/request-batches/{id} for the per-request results.
func (h *AdminRequestHandlers) run(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Service == nil {
		WriteAPIError(w, http.StatusServiceUnavailable, "request_runner_disabled",
			"manual execution is not enabled in this build")
		return
	}
	var req runPayload
	if apiErr := readOptionalJSON(r, &req); apiErr != nil {
		WriteAPIError(w, apiErr.Status, apiErr.Code, apiErr.Message)
		return
	}
	if len(req.RequestIDs) == 0 {
		WriteAPIError(w, http.StatusBadRequest, "no_requests_selected",
			"request_ids must contain at least one request id")
		return
	}
	batch, err := h.cfg.Service.RunRequests(r.Context(), req.RequestIDs, request.RunOptions{
		Concurrency: req.Concurrency,
		Overrides:   overridesFromPayload(req.Overrides),
	})
	if err != nil {
		h.writeRunError(w, err)
		return
	}
	h.log("request_batch_started", "batch_id", batch.ID, "total", batch.Total)
	writeJSON(w, http.StatusAccepted, batch)
}

// getBatch returns the live progress of a batch.
func (h *AdminRequestHandlers) getBatch(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Service == nil {
		WriteAPIError(w, http.StatusServiceUnavailable, "request_runner_disabled",
			"manual execution is not enabled in this build")
		return
	}
	batch, ok := h.cfg.Service.Batch(r.PathValue("id"))
	if !ok {
		WriteAPIError(w, http.StatusNotFound, "batch_not_found", r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, batch)
}

// ---- helpers ----

// providerIndex loads every provider keyed by id, so the request list can
// be rendered without one lookup per row.
func (h *AdminRequestHandlers) providerIndex(ctx context.Context) (map[string]*provider.Provider, error) {
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
// this request selected.
func (h *AdminRequestHandlers) requireProvider(ctx context.Context, providerID, model string) error {
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

// writeProviderError maps a provider/model resolution failure onto the
// right 400 code. Shared by the request and task handlers.
func writeProviderError(w http.ResponseWriter, err error) {
	code := "unknown_provider"
	if errors.Is(err, errUnknownModel) {
		code = "unknown_model"
	}
	WriteAPIError(w, http.StatusBadRequest, code, err.Error())
}

// view assembles the wire shape of a request.
func (h *AdminRequestHandlers) view(r *request.Request, providers map[string]*provider.Provider) requestView {
	vars := r.PromptVars
	if vars == nil {
		vars = map[string]string{}
	}
	out := requestView{
		ID:               r.ID,
		ProviderID:       r.ProviderID,
		Model:            r.Model,
		Name:             r.Name,
		Description:      r.Description,
		SystemPrompt:     r.SystemPrompt,
		UserPrompt:       r.UserPrompt,
		PromptVars:       vars,
		Order:            r.Order,
		ThinkingOverride: r.ThinkingOverride,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
	}
	if p, ok := providers[r.ProviderID]; ok && p != nil {
		out.Provider = providerView(p)
	}
	return out
}

// validateRequest checks the required fields of a complete request.
func validateRequest(r *request.Request) error {
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("name required")
	}
	if strings.TrimSpace(r.ProviderID) == "" {
		return errors.New("provider_id required")
	}
	if strings.TrimSpace(r.Model) == "" {
		return errors.New("model required")
	}
	if strings.TrimSpace(r.UserPrompt) == "" {
		return errors.New("user_prompt required")
	}
	return nil
}

// requestFromPayload parses the payload and builds the request. It
// validates only what is present; required-field checks happen on the
// merged result in create / patch, so a partial PATCH is accepted.
func requestFromPayload(p requestPayload, id string) (*request.Request, error) {
	out := &request.Request{
		ID:               id,
		ProviderID:       p.ProviderID,
		Model:            p.Model,
		Name:             p.Name,
		Description:      derefString(p.Description),
		SystemPrompt:     derefString(p.SystemPrompt),
		UserPrompt:       p.UserPrompt,
		PromptVars:       p.PromptVars,
		ThinkingOverride: p.ThinkingOverride,
	}
	if out.ID == "" {
		out.ID = uuid.NewString()
	}
	if p.Order != nil {
		out.Order = *p.Order
	}
	return out, nil
}

// writeStoreError maps a store error onto the HTTP status codes.
func (h *AdminRequestHandlers) writeStoreError(w http.ResponseWriter, err error, kind string) {
	switch {
	case errors.Is(err, request.ErrNotFound):
		WriteAPIError(w, http.StatusNotFound, kind+"_not_found", err.Error())
	case errors.Is(err, request.ErrAlreadyExists):
		WriteAPIError(w, http.StatusConflict, kind+"_exists", err.Error())
	case errors.Is(err, request.ErrInvalidConfig):
		WriteAPIError(w, http.StatusBadRequest, "invalid_payload", err.Error())
	default:
		WriteAPIError(w, http.StatusInternalServerError, kind+"_write_failed", err.Error())
	}
}

// writeRunError maps an execution error onto the HTTP status codes.
func (h *AdminRequestHandlers) writeRunError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, request.ErrNotFound):
		WriteAPIError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, request.ErrInvalidConfig):
		WriteAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, request.ErrNoProber):
		WriteAPIError(w, http.StatusServiceUnavailable, "request_runner_disabled", err.Error())
	default:
		WriteAPIError(w, http.StatusInternalServerError, "run_failed", err.Error())
	}
}
