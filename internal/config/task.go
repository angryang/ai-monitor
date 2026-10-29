// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

// Package config owns the declarative TaskConfig model plus the runtime
// configuration loader (yaml < env < flag). All cross-task config
// structures live here so other packages can compile against stable
// schemas.
package config

import (
	"encoding/json"
	"fmt"
	"time"
)

// jsonMarshal serialises a map to JSON or returns the empty string.
func jsonMarshal(m map[string]string) (string, error) {
	if len(m) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// jsonUnmarshalStringMap decodes a JSON object into a map[string]string.
func jsonUnmarshalStringMap(raw string) (map[string]string, error) {
	if raw == "" {
		return nil, nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Protocol enumerates the supported chat protocols.
type Protocol string

const (
	ProtocolOpenAI    Protocol = "openai"
	ProtocolAnthropic Protocol = "anthropic"
	ProtocolGemini    Protocol = "gemini"
)

// IsValid reports whether the protocol has a registered adapter.
func (p Protocol) IsValid() bool {
	switch p {
	case ProtocolOpenAI, ProtocolAnthropic, ProtocolGemini:
		return true
	}
	return false
}

// NotifyChannel enumerates supported notification channels.
type NotifyChannel string

const (
	NotifyWeCom NotifyChannel = "wecom"
)

// CanaryGroupLabel is the default value used when a task doesn't specify a
// canary bucket. See design.md §17.1 / P1-D for the rationale on this label.
const CanaryGroupLabel = "default"

// TaskConfigTableName is the SQL table name used for persisted rows. We
// override GORM's pluralization to keep the name aligned with design.md §12.
const TaskConfigTableName = "tasks"

// TableName satisfies GORM's Tabler interface. Pinning the name avoids the
// default pluralization (`task_configs`) which would otherwise be applied.
func (TaskConfig) TableName() string { return TaskConfigTableName }

// TaskConfig is the persisted representation of a probe task. The struct is
// GORM-tagged so SQLite can store it as a single row (see store_sqlite.go).
//
// The runtime-only fields (APIKey, WeComWebhook) are kept on the struct but
// excluded from the persistent columns via `gorm:"-"`. They are populated by
// the caller (loader, store) right before the task is executed.
//
// `*ProtocolConfig` pointers are *embedded*. GORM treats embedded structs as
// column-flattening by default; we still surround them with `,embedded` so
// future schema migrations treat them uniformly.
type TaskConfig struct {
	ID       string   `gorm:"primaryKey;size:64" yaml:"id"`
	Name     string   `gorm:"uniqueIndex;size:128;not null" yaml:"name" validate:"required,min=1,max=128"`
	Enabled  bool     `gorm:"not null;default:true" yaml:"enabled"`
	Protocol Protocol `gorm:"size:32;not null" yaml:"protocol" validate:"required,oneof=openai anthropic gemini"`
	BaseURL  string   `gorm:"size:512;not null" yaml:"base_url" validate:"required,url"`
	// InsecureSkipVerify disables upstream TLS certificate verification
	// for this task. Needed when the endpoint presents a self-signed cert
	// or a cert signed by a private CA the host doesn't trust (typical for
	// in-cluster gateways / internal model servers).
	//
	// It is OR'ed with the instance-wide switch (Config.InsecureSkipVerify),
	// so an operator who turned the global switch on cannot re-enable
	// verification for a single task — the global switch is an explicit
	// "this deployment does not verify certs" decision.
	//
	// Security note: skipping verification means the connection is
	// vulnerable to MITM. Keep it off unless the endpoint is on a trusted
	// network path.
	InsecureSkipVerify bool `gorm:"column:insecure_skip_verify;not null;default:false" yaml:"insecure_skip_verify,omitempty"`
	// ThinkingEnabled asks the upstream model to emit its reasoning trace
	// on this task's probes. Each protocol adapter maps the flag to its
	// own request shape (openai: `enable_thinking`, anthropic: `thinking`
	// block, gemini: `thinkingConfig`). When false the adapter sends NO
	// thinking parameter at all, so wire behaviour is identical to the
	// pre-flag behaviour — the upstream default applies.
	ThinkingEnabled bool   `gorm:"column:thinking_enabled;not null;default:false" yaml:"thinking_enabled,omitempty"`
	APIKey          string `gorm:"-"` // runtime only; loaded by store
	APIKeyEnc       []byte `gorm:"column:api_key_enc;type:blob"`
	// SecretError records why this row's encrypted secrets could not be
	// opened (almost always a master key mismatch). Runtime-only: never
	// persisted, never returned to the browser verbatim.
	//
	// When set, APIKey / Notify.WeComWebhook stay empty and the task is
	// surfaced to the admin UI so an operator can re-enter the secret
	// instead of losing the whole task definition.
	SecretError    string            `gorm:"-" json:"-"`
	Model          string            `gorm:"size:128;not null" yaml:"model" validate:"required"`
	SystemPrompt   string            `gorm:"type:text" yaml:"system_prompt,omitempty"`
	UserPrompt     string            `gorm:"type:text" yaml:"user_prompt" validate:"required"`
	PromptVars     map[string]string `gorm:"-" yaml:"prompt_vars,omitempty"` // runtime: decoded from PromptVarsJSON
	PromptVarsJSON string            `gorm:"column:prompt_vars;type:text"`   // persistent JSON form
	Schedule       string            `gorm:"size:128;not null" yaml:"schedule" validate:"required"`
	Timeout        time.Duration     `gorm:"-" yaml:"timeout" validate:"min=1s,max=10m"`
	TimeoutMS      int64             `gorm:"column:timeout_ms;not null;default:30000"`
	CanaryGroup    string            `gorm:"size:64;not null;default:default" yaml:"canary_group,omitempty"`
	Notify         NotifyConfig      `gorm:"embedded" yaml:"notify,omitempty"`
	OpenAI         *OpenAIConfig     `gorm:"embedded" yaml:"openai,omitempty"`
	Anthropic      *AnthropicConfig  `gorm:"embedded" yaml:"anthropic,omitempty"`
	Gemini         *GeminiConfig     `gorm:"embedded" yaml:"gemini,omitempty"`
	CreatedAt      time.Time         `gorm:"autoCreateTime"`
	UpdatedAt      time.Time         `gorm:"autoUpdateTime"`
}

// NotifyConfig is the embedded notification block.
type NotifyConfig struct {
	Enabled         bool          `gorm:"not null;default:false" yaml:"enabled"`
	Channel         NotifyChannel `gorm:"size:32;not null;default:wecom" yaml:"channel" validate:"omitempty,oneof=wecom"`
	WeComWebhook    string        `gorm:"-"` // runtime only
	WeComWebhookEnc []byte        `gorm:"column:wecom_webhook_enc;type:blob"`
}

// EffectiveTimeout returns Timeout if non-zero, otherwise the design default
// of 30 seconds. It also normalises the TimeoutMS column to the Timeout field
// for symmetry across create / read paths.
func (t *TaskConfig) EffectiveTimeout() time.Duration {
	if t.Timeout > 0 {
		return t.Timeout
	}
	if t.TimeoutMS > 0 {
		return time.Duration(t.TimeoutMS) * time.Millisecond
	}
	return 30 * time.Second
}

// Runnable reports whether the task can actually be probed.
//
// A task whose encrypted secrets could not be decrypted (SecretError set)
// carries an empty api key, so probing it would fail on every tick and
// spam the notification channel. Such tasks are still listed in the admin
// UI — they just stay out of the scheduler until the secret is re-entered.
func (t *TaskConfig) Runnable() bool {
	return t.SecretError == ""
}

// RunnableTasks filters a task list down to the tasks that can be probed.
// Used before handing tasks to the scheduler.
func RunnableTasks(tasks []TaskConfig) []TaskConfig {
	out := make([]TaskConfig, 0, len(tasks))
	for _, t := range tasks {
		if t.Runnable() {
			out = append(out, t)
		}
	}
	return out
}

// Normalise pulls Duration / map fields into the persistent integer / JSON
// columns. Called before any Create/Update so the SQLite schema always holds
// a numeric form. Also provisions empty protocol blocks with safe defaults
// so GORM's full-row Save() never writes NULL into NOT-NULL columns
// (e.g. max_tokens, alt) when the user picked a different protocol.
func (t *TaskConfig) Normalise() error {
	if t.Timeout > 0 {
		t.TimeoutMS = t.Timeout.Milliseconds()
	}
	if len(t.PromptVars) > 0 {
		raw, err := jsonMarshal(t.PromptVars)
		if err != nil {
			return fmt.Errorf("config: prompt_vars: %w", err)
		}
		t.PromptVarsJSON = raw
	}
	// Provision protocol-segment defaults so GORM never inserts NULL.
	if t.OpenAI == nil {
		t.OpenAI = &OpenAIConfig{StreamPath: ""}
	}
	if t.Anthropic == nil {
		t.Anthropic = &AnthropicConfig{MaxTokens: 1024}
	}
	if t.Gemini == nil {
		t.Gemini = &GeminiConfig{Alt: "sse"}
	}
	return nil
}

// Hydrate fills the runtime Duration / API-Key fields from the persistent
// columns after a read.
func (t *TaskConfig) Hydrate() error {
	if t.Timeout == 0 && t.TimeoutMS > 0 {
		t.Timeout = time.Duration(t.TimeoutMS) * time.Millisecond
	}
	if t.PromptVarsJSON != "" && len(t.PromptVars) == 0 {
		m, err := jsonUnmarshalStringMap(t.PromptVarsJSON)
		if err != nil {
			return fmt.Errorf("config: prompt_vars decode: %w", err)
		}
		t.PromptVars = m
	}
	return nil
}

// OpenAIConfig is the protocol-specific block for OpenAI-compatible APIs.
type OpenAIConfig struct {
	StreamPath       string `gorm:"size:256" yaml:"stream_path,omitempty" validate:"omitempty,startswith=/"`
	ExtraHeadersJSON string `gorm:"column:openai_extra_headers;type:text" yaml:"extra_headers,omitempty"`
}

// AnthropicConfig is the protocol-specific block for the Anthropic API.
type AnthropicConfig struct {
	MaxTokens        int    `gorm:"not null;default:1024" yaml:"max_tokens" validate:"min=1,max=200000"`
	ExtraHeadersJSON string `gorm:"column:anthropic_extra_headers;type:text" yaml:"extra_headers,omitempty"`
}

// GeminiConfig is the protocol-specific block for Google's Gemini API.
type GeminiConfig struct {
	Alt              string `gorm:"size:32;not null;default:sse" yaml:"alt" validate:"omitempty,oneof=sse ndjson"`
	ExtraHeadersJSON string `gorm:"column:gemini_extra_headers;type:text" yaml:"extra_headers,omitempty"`
}

// ValidateProtocolSegments enforces the protocol/section mutual-exclusion
// invariant (one TaskConfig with multiple protocol blocks configured is a
// misconfiguration and must surface immediately). Returning nil means the
// task is well-formed.
func (t *TaskConfig) ValidateProtocolSegments() error {
	switch t.Protocol {
	case ProtocolOpenAI:
		if t.Anthropic != nil && (t.Anthropic.MaxTokens != 0 || t.Anthropic.ExtraHeadersJSON != "") {
			return fmt.Errorf("config: anthropic block set for %s task: %w", t.Protocol, ErrInvalidConfig)
		}
		if t.Gemini != nil && (t.Gemini.Alt != "" && t.Gemini.Alt != "sse" || t.Gemini.ExtraHeadersJSON != "") {
			return fmt.Errorf("config: gemini block set for %s task: %w", t.Protocol, ErrInvalidConfig)
		}
	case ProtocolAnthropic:
		if t.OpenAI != nil && (t.OpenAI.StreamPath != "" && t.OpenAI.StreamPath != "/v1/chat/completions" || t.OpenAI.ExtraHeadersJSON != "") {
			return fmt.Errorf("config: openai block set for %s task: %w", t.Protocol, ErrInvalidConfig)
		}
		if t.Gemini != nil && (t.Gemini.Alt != "" && t.Gemini.Alt != "sse" || t.Gemini.ExtraHeadersJSON != "") {
			return fmt.Errorf("config: gemini block set for %s task: %w", t.Protocol, ErrInvalidConfig)
		}
	case ProtocolGemini:
		if t.OpenAI != nil && (t.OpenAI.StreamPath != "" && t.OpenAI.StreamPath != "/v1/chat/completions" || t.OpenAI.ExtraHeadersJSON != "") {
			return fmt.Errorf("config: openai block set for %s task: %w", t.Protocol, ErrInvalidConfig)
		}
		if t.Anthropic != nil && (t.Anthropic.MaxTokens != 0 && t.Anthropic.MaxTokens != 1024 || t.Anthropic.ExtraHeadersJSON != "") {
			return fmt.Errorf("config: anthropic block set for %s task: %w", t.Protocol, ErrInvalidConfig)
		}
	default:
		return fmt.Errorf("config: unknown protocol %q: %w", t.Protocol, ErrInvalidConfig)
	}
	return nil
}
