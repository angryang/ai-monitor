// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

// Package request implements the AI request manager.
//
// A Request is one predefined AI request: a system / user prompt, its
// ${var} substitutions, the AI service provider it is sent to and the ONE
// model selected from that provider's candidate list. The provider
// registry (internal/provider) owns the upstream coordinates — protocol,
// base URL, credentials, models — so one upstream can be shared by many
// requests and edited in exactly one place.
//
// The unit of organisation is the request itself, not a group of them:
// there is no "request group" concept any more. A request points at a
// provider, and the operator runs one request, several requests, or all
// of them.
//
// A run may also carry one-shot content overrides (see ContentOverride):
// the operator can type a different prompt for a single execution without
// rewriting the predefined request.
//
// Manual executions deliberately do not feed the monitoring signal: they
// are ad-hoc probes, not samples. See probe.RunOptions.Manual.
package request

import (
	"context"
	"errors"
	"time"
)

// Sentinel errors returned by Store implementations. Callers MUST use
// errors.Is to match them.
var (
	ErrNotFound      = errors.New("request: not found")
	ErrAlreadyExists = errors.New("request: already exists")
	ErrInvalidConfig = errors.New("request: invalid config")
)

// TableName is the SQL table name for persisted requests.
const TableName = "ai_requests"

// Request is one predefined AI request.
//
// The provider coordinates live in the provider registry, not here: they
// are shared by every request that targets the same upstream. What stays
// on the request is what makes it distinct — the prompts and their
// variables.
type Request struct {
	// ID is the canonical request id (uuid v4 on create).
	ID string `gorm:"primaryKey;size:64" json:"id"`
	// ProviderID is the AI service provider this request is sent to.
	// It is the link between a request and its upstream: two requests
	// pointing at the same provider share its URL and credentials.
	ProviderID string `gorm:"column:provider_id;size:64;index;not null" json:"provider_id"`
	// Model is the ONE model this request selected from its provider's
	// Models list. It is validated against that list on write, so a
	// request can never silently run against a model the provider no
	// longer offers.
	Model string `gorm:"size:128;not null" json:"model"`
	// Name identifies the request in the UI.
	Name        string `gorm:"size:128;not null" json:"name"`
	Description string `gorm:"size:512" json:"description"`

	// SystemPrompt / UserPrompt are the request body. UserPrompt is
	// required; SystemPrompt is optional.
	SystemPrompt string `gorm:"type:text" json:"system_prompt"`
	UserPrompt   string `gorm:"type:text;not null" json:"user_prompt"`
	// PromptVars are the ${var} substitutions for the prompts. They are
	// stored as JSON in PromptVarsJSON and decoded on read.
	PromptVars     map[string]string `gorm:"-" json:"prompt_vars,omitempty"`
	PromptVarsJSON string            `gorm:"column:prompt_vars;type:text" json:"-"`

	// Order controls the position of the request in the list. Requests
	// are listed in ascending order; the tiebreaker is the id.
	//
	// The column is named sort_order because ORDER is a reserved SQL
	// keyword and the raw ORDER BY clause below would fail on it.
	Order int `gorm:"column:sort_order;not null;default:0" json:"order"`

	// ThinkingOverride optionally overrides the provider's thinking
	// switch for this request. nil means "inherit the provider setting".
	ThinkingOverride *bool `gorm:"column:thinking_override" json:"thinking_override,omitempty"`

	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

// TableName pins the SQL name to `ai_requests`.
func (Request) TableName() string { return TableName }

// ThinkingEnabled resolves the effective reasoning-trace switch for this
// request: its own override when set, otherwise the provider's default.
func (r *Request) ThinkingEnabled(providerDefault bool) bool {
	if r == nil {
		return providerDefault
	}
	if r.ThinkingOverride != nil {
		return *r.ThinkingOverride
	}
	return providerDefault
}

// Store persists AI requests.
//
// The interface mirrors config.ConfigStore: small, contract-first, so the
// HTTP layer can be tested against a fake.
type Store interface {
	// ListRequests returns every request, ordered by Order then id.
	ListRequests(ctx context.Context) ([]Request, error)
	// GetRequest returns one request. Returns ErrNotFound when it
	// doesn't exist.
	GetRequest(ctx context.Context, id string) (*Request, error)
	// CreateRequest inserts a new request.
	CreateRequest(ctx context.Context, r *Request) error
	// UpdateRequest overwrites an existing request.
	UpdateRequest(ctx context.Context, r *Request) error
	// DeleteRequest removes a request.
	DeleteRequest(ctx context.Context, id string) error
	// ListRequestsByProvider returns the requests that target one
	// provider. The provider delete route uses it to refuse removing a
	// provider that is still in use.
	ListRequestsByProvider(ctx context.Context, providerID string) ([]Request, error)
}
