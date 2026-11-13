// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

// Package provider implements the AI service provider registry.
//
// A provider is the reusable description of one upstream AI endpoint:
// protocol, base URL, credentials, the list of available models and the
// protocol-specific options. Monitoring tasks and AI requests both need
// this information, so it is defined once here and referenced from
// everywhere instead of being copy-pasted into every row.
//
// A provider offers several models; every task and every request picks
// exactly one of them and stores that choice on its own row. Connectivity
// is no longer a gate on saving — it is an on-demand check the operator
// runs against one chosen model (see connectivity.go).
//
// Secrets follow the same rules as monitoring tasks: the api key is
// sealed with the instance master key (config.Cipher) before it touches
// disk and never leaves the process in the clear — the HTTP layer reports
// `has_api_key` instead of the key itself.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/angryang/ai-monitor/internal/config"
)

// Sentinel errors returned by Store implementations. Callers MUST use
// errors.Is to match them.
var (
	ErrNotFound      = errors.New("provider: not found")
	ErrAlreadyExists = errors.New("provider: already exists")
	ErrInvalidConfig = errors.New("provider: invalid config")
)

// DefaultTimeout is the per-request deadline used when a provider doesn't
// specify one. It mirrors config.TaskConfig.EffectiveTimeout's default.
const DefaultTimeout = 30 * time.Second

// MaxTimeout caps the per-request deadline. A request that runs longer than
// this is almost certainly a stuck upstream.
const MaxTimeout = 10 * time.Minute

// TableName is the SQL table name for persisted providers.
const TableName = "ai_providers"

// Provider describes one upstream AI service.
//
// The provider coordinates live here rather than on the monitoring tasks
// because they are shared: every task pointing at the same endpoint uses
// the same URL and the same credentials. The model is only a *candidate
// list* here — each task and each request selects one entry from Models.
type Provider struct {
	// ID is the canonical provider id (uuid v4 on create).
	ID string `gorm:"primaryKey;size:64" json:"id"`
	// Name is unique across providers — it is what the operator picks from.
	Name string `gorm:"uniqueIndex;size:128;not null" json:"name"`
	// Description is free text shown next to the name in the UI.
	Description string `gorm:"size:512" json:"description"`
	// Enabled marks the provider as usable. A disabled provider is kept
	// for reference but is never handed out to a caller.
	Enabled bool `gorm:"not null;default:true" json:"enabled"`

	// --- provider coordinates ---

	Protocol config.Protocol `gorm:"size:32;not null" json:"protocol"`
	BaseURL  string          `gorm:"size:512;not null" json:"base_url"`
	// APIKey is the runtime-only plaintext. It is never serialised and
	// never persisted in the clear; see APIKeyEnc.
	APIKey string `gorm:"-" json:"-"`
	// APIKeyEnc holds the AES-GCM sealed key. The layout matches
	// config.TaskConfig.APIKeyEnc so both stores share one master key.
	APIKeyEnc []byte `gorm:"column:api_key_enc;type:blob" json:"-"`
	// Models is the ordered list of model names this provider offers. The
	// first entry is the primary model (used as the default selection).
	// Runtime-only: the persisted form is ModelsJSON.
	Models []string `gorm:"-" json:"models"`
	// ModelsJSON is the JSON-encoded Models list. It lives in its own
	// column so adding or removing a model never needs a schema change.
	ModelsJSON string `gorm:"column:models;type:text;not null" json:"-"`
	// Model is the legacy single-model column. It is kept only because
	// SQLite cannot drop or relax a NOT NULL column in place: every write
	// mirrors Models[0] into it so inserts keep working. New code must
	// never read it — use Models / PrimaryModel().
	Model string `gorm:"size:128;not null" json:"-"`
	// TimeoutMS is the per-request deadline in milliseconds.
	TimeoutMS int64 `gorm:"not null;default:30000" json:"timeout_ms"`
	// InsecureSkipVerify skips upstream TLS verification for this provider.
	InsecureSkipVerify bool `gorm:"column:insecure_skip_verify;not null;default:false" json:"insecure_skip_verify"`
	// ThinkingEnabled is the provider's default reasoning-trace switch.
	ThinkingEnabled bool `gorm:"column:thinking_enabled;not null;default:false" json:"thinking_enabled"`

	// Protocol-specific blocks. They mirror config.TaskConfig so the same
	// adapter configuration applies to a monitoring task and to a manual
	// request.
	OpenAI    *config.OpenAIConfig    `gorm:"embedded" json:"openai,omitempty"`
	Anthropic *config.AnthropicConfig `gorm:"embedded" json:"anthropic,omitempty"`
	Gemini    *config.GeminiConfig    `gorm:"embedded" json:"gemini,omitempty"`

	// SecretError is non-empty when the stored api key could not be
	// decrypted with the current master key. The provider is still listed
	// so the operator can repair it, but it cannot be used until the key
	// is re-entered.
	SecretError string `gorm:"-" json:"secret_error,omitempty"`

	// LastVerified* record the outcome of the most recent on-demand
	// connectivity check (which was run against one particular model).
	LastVerifiedAt      *time.Time `gorm:"column:last_verified_at" json:"last_verified_at,omitempty"`
	LastVerifiedOK      bool       `gorm:"column:last_verified_ok;not null;default:false" json:"last_verified_ok"`
	LastVerifiedMessage string     `gorm:"column:last_verified_message;size:512" json:"last_verified_message,omitempty"`

	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

// TableName pins the SQL name to `ai_providers`.
func (Provider) TableName() string { return TableName }

// NormaliseModels trims, de-duplicates and serialises the model list.
//
// It is deliberately forgiving about blank / repeated entries (the UI
// already filters them, but an API client need not), and is the single
// place where Models, ModelsJSON and the legacy Model column are kept in
// sync. It also accepts a provider that only carries the legacy scalar
// Model, which is how the one-time migration and older call sites build
// rows.
func (p *Provider) NormaliseModels() error {
	if p == nil {
		return fmt.Errorf("provider: nil provider: %w", ErrInvalidConfig)
	}
	if len(p.Models) == 0 {
		if strings.TrimSpace(p.Model) != "" {
			p.Models = []string{strings.TrimSpace(p.Model)}
		} else if p.ModelsJSON != "" {
			if err := json.Unmarshal([]byte(p.ModelsJSON), &p.Models); err != nil {
				return fmt.Errorf("provider: invalid models json: %w: %w", err, ErrInvalidConfig)
			}
		}
	}
	seen := make(map[string]bool, len(p.Models))
	out := make([]string, 0, len(p.Models))
	for _, m := range p.Models {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) == 0 {
		return fmt.Errorf("provider: missing models: %w", ErrInvalidConfig)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("provider: encode models: %w", err)
	}
	p.Models = out
	p.ModelsJSON = string(raw)
	// Keep the legacy NOT NULL column in step so CREATE never fails.
	p.Model = out[0]
	return nil
}

// HydrateModels reconstructs Models from the persisted JSON. Rows written
// before the multi-model migration have no JSON yet and fall back to the
// legacy scalar column.
func (p *Provider) HydrateModels() {
	if p == nil {
		return
	}
	if p.ModelsJSON != "" {
		var list []string
		if err := json.Unmarshal([]byte(p.ModelsJSON), &list); err == nil && len(list) > 0 {
			p.Models = list
			if p.Model == "" {
				p.Model = list[0]
			}
			return
		}
	}
	if p.Model != "" {
		p.Models = []string{p.Model}
		p.ModelsJSON = ""
	}
}

// PrimaryModel returns the first (default) model of the provider.
func (p *Provider) PrimaryModel() string {
	if p == nil {
		return ""
	}
	if len(p.Models) > 0 {
		return p.Models[0]
	}
	return p.Model
}

// HasModel reports whether name is one of the provider's models.
func (p *Provider) HasModel(name string) bool {
	if p == nil {
		return false
	}
	for _, m := range p.Models {
		if m == name {
			return true
		}
	}
	return len(p.Models) == 0 && p.Model == name
}

// EffectiveTimeout returns the per-request deadline, falling back to
// DefaultTimeout when the column is unset.
func (p *Provider) EffectiveTimeout() time.Duration {
	if p == nil {
		return DefaultTimeout
	}
	if p.TimeoutMS > 0 {
		return time.Duration(p.TimeoutMS) * time.Millisecond
	}
	return DefaultTimeout
}

// Runnable reports whether the provider can actually be used. A provider
// whose api key could not be decrypted carries an empty key, so every
// request would fail — it is listed but never handed out.
func (p *Provider) Runnable() bool {
	return p != nil && p.SecretError == "" && p.APIKey != ""
}

// Store persists AI service providers.
//
// The interface mirrors config.ConfigStore: small, contract-first, so the
// HTTP layer can be tested against a fake.
type Store interface {
	// ListProviders returns every provider, oldest first. Rows whose api
	// key cannot be decrypted are still returned with SecretError set and
	// an empty APIKey so the operator can repair them.
	ListProviders(ctx context.Context) ([]Provider, error)
	// GetProvider returns one provider. Returns ErrNotFound when it
	// doesn't exist.
	GetProvider(ctx context.Context, id string) (*Provider, error)
	// CreateProvider inserts a new provider. Returns ErrAlreadyExists
	// when the name is taken.
	CreateProvider(ctx context.Context, p *Provider) error
	// UpdateProvider overwrites an existing provider.
	UpdateProvider(ctx context.Context, p *Provider) error
	// DeleteProvider removes a provider.
	DeleteProvider(ctx context.Context, id string) error
}
