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
// The upstream coordinates (protocol, base URL, credentials, model and the
// protocol-specific options) are NOT stored on the task any more: they live
// in the AI service provider registry (internal/provider), and a task names
// the provider it probes through ProviderID. This mirrors the AI request
// manager, where a request also carries a provider reference instead of a
// copy of the endpoint coordinates.
//
// The upstream fields therefore became runtime-only (`gorm:"-"`). They are
// filled in by the provider binder (provider.BindTask) right before a probe
// runs, and are never persisted.
//
// The runtime-only fields (APIKey, WeComWebhook) are kept on the struct but
// excluded from the persistent columns via `gorm:"-"`. They are populated by
// the caller (loader, store) right before the task is executed.
//
// `*ProtocolConfig` pointers are *embedded*. GORM treats embedded structs as
// column-flattening by default; we still surround them with `,embedded` so
// future schema migrations treat them uniformly.
type TaskConfig struct {
	ID      string `gorm:"primaryKey;size:64" yaml:"id"`
	Name    string `gorm:"uniqueIndex;size:128;not null" yaml:"name" validate:"required,min=1,max=128"`
	Enabled bool   `gorm:"not null;default:true" yaml:"enabled"`
	// ProviderID names the AI service provider this task probes. It is
	// the task's only link to an upstream: two tasks pointing at the same
	// provider share its URL, credentials and model.
	//
	// The column carries a NOT NULL constraint so a task can never
	// reference a missing provider. It was added to existing tables by the
	// migration in internal/provider/migrate.go, which backfilled it
	// before the constraint took effect.
	ProviderID string `gorm:"column:provider_id;size:64;index;not null" json:"provider_id" yaml:"provider_id" validate:"required"`
	// Protocol is the upstream protocol, inherited from the provider at
	// bind time. Runtime-only.
	Protocol Protocol `gorm:"-" json:"protocol" yaml:"protocol"`
	// BaseURL is the upstream base URL, inherited from the provider at
	// bind time. Runtime-only.
	BaseURL string `gorm:"-" json:"base_url" yaml:"base_url"`
	// InsecureSkipVerify disables upstream TLS certificate verification
	// for this task. It is inherited from the provider at bind time.
	//
	// It is OR'ed with the instance-wide switch (Config.InsecureSkipVerify),
	// so an operator who turned the global switch on cannot re-enable
	// verification for a single task — the global switch is an explicit
	// "this deployment does not verify certs" decision.
	//
	// Security note: skipping verification means the connection is
	// vulnerable to MITM. Keep it off unless the endpoint is on a trusted
	// network path.
	InsecureSkipVerify bool `gorm:"-" json:"insecure_skip_verify" yaml:"insecure_skip_verify,omitempty"`
	// ThinkingEnabled asks the upstream model to emit its reasoning trace
	// on this task's probes. Each protocol adapter maps the flag to its
	// own request shape (openai: `enable_thinking`, anthropic: `thinking`
	// block, gemini: `thinkingConfig`). When false the adapter sends NO
	// thinking parameter at all, so wire behaviour is identical to the
	// pre-flag behaviour — the upstream default applies.
	//
	// It is inherited from the provider at bind time. Runtime-only.
	ThinkingEnabled bool `gorm:"-" json:"thinking_enabled" yaml:"thinking_enabled,omitempty"`
	// APIKey is the decrypted upstream credential, inherited from the
	// provider at bind time. Runtime-only, never persisted here.
	APIKey string `gorm:"-" json:"-"`
	// APIKeyEnc is the sealed upstream credential. It belongs to the
	// provider now; the field stays on the struct so the binder can hand
	// a fully-formed credential to the probe layer.
	APIKeyEnc []byte `gorm:"-" json:"-"`
	// SecretError records why this row's encrypted secrets could not be
	// opened (almost always a master key mismatch). Runtime-only: never
	// persisted, never returned to the browser verbatim.
	//
	// When set, APIKey / Notify.WeComWebhook stay empty and the task is
	// surfaced to the admin UI so an operator can re-enter the secret
	// instead of losing the whole task definition.
	SecretError string `gorm:"-" json:"-"`
	// Model is the ONE model this task selected from its provider's
	// candidate list. Unlike the rest of the upstream coordinates it is
	// persisted: the choice belongs to the task, not to the provider. The
	// HTTP layer validates it against provider.Models on write, and the
	// binder refuses to run a task whose model disappeared from the list.
	Model string `gorm:"size:128;not null" json:"model" yaml:"model"`
	// SystemPrompt is NOT used by monitoring tasks. probe.Runner.Run clears
	// it before every scheduled probe. It remains on the struct because
	// AI requests build synthetic TaskConfigs for one-shot manual
	// executions, where the operator may author a system prompt. The YAML
	// tag is kept so legacy configs still parse; the value is ignored
	// for scheduled tasks.
	SystemPrompt   string            `gorm:"type:text" yaml:"system_prompt,omitempty"`
	UserPrompt     string            `gorm:"type:text" yaml:"user_prompt" validate:"required"`
	PromptVars     map[string]string `gorm:"-" yaml:"prompt_vars,omitempty"` // runtime: decoded from PromptVarsJSON
	PromptVarsJSON string            `gorm:"column:prompt_vars;type:text"`   // persistent JSON form
	Schedule       string            `gorm:"size:128;not null" yaml:"schedule" validate:"required"`
	Timeout        time.Duration     `gorm:"-" yaml:"timeout" validate:"min=1s,max=10m"`
	TimeoutMS      int64             `gorm:"-" yaml:"timeout_ms"`
	CanaryGroup    string            `gorm:"size:64;not null;default:default" yaml:"canary_group,omitempty"`
	Notify         NotifyConfig      `gorm:"embedded" yaml:"notify,omitempty"`
	// The protocol-specific blocks are inherited from the provider at bind
	// time. Runtime-only.
	OpenAI    *OpenAIConfig    `gorm:"-" json:"openai,omitempty" yaml:"openai,omitempty"`
	Anthropic *AnthropicConfig `gorm:"-" json:"anthropic,omitempty" yaml:"anthropic,omitempty"`
	Gemini    *GeminiConfig    `gorm:"-" json:"gemini,omitempty" yaml:"gemini,omitempty"`
	CreatedAt time.Time        `gorm:"autoCreateTime"`
	UpdatedAt time.Time        `gorm:"autoUpdateTime"`
}

// NotifyConfig is the embedded notification block.
//
// The column names carry an explicit `notify_` prefix because the
// embedded `Enabled` field would otherwise collide with TaskConfig.Enabled
// on the `enabled` column: GORM flattens embedded structs without a
// prefix, so both fields would map to `tasks.enabled` and one would
// silently overwrite the other. The prefixed names keep the task's own
// enable flag and its notification switch independent.
type NotifyConfig struct {
	Enabled         bool          `gorm:"column:notify_enabled;not null;default:false" yaml:"enabled"`
	Channel         NotifyChannel `gorm:"column:notify_channel;size:32;not null;default:wecom" yaml:"channel" validate:"omitempty,oneof=wecom"`
	WeComWebhook    string        `gorm:"-"` // runtime only
	WeComWebhookEnc []byte        `gorm:"column:notify_wecom_webhook_enc;type:blob"`
}

// EffectiveTimeout returns Timeout if non-zero, otherwise the design default
// of 30 seconds.
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
// carries an empty webhook, so notifying it would fail on every tick. Such
// tasks are still listed in the admin UI — they just stay out of the
// scheduler until the secret is re-entered.
//
// The upstream credentials live on the provider now; a task whose provider
// is missing or unusable is filtered out by the provider resolver before it
// reaches the scheduler.
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

// Normalise pulls the map field into its persistent JSON column. Called
// before any Create/Update so the SQLite schema always holds a JSON-encoded
// value.
//
// The upstream coordinates are not normalised here any more: they are
// runtime-only fields filled in by the provider resolver (see
// provider.ResolveTask), and the timeout follows the provider as well.
func (t *TaskConfig) Normalise() error {
	if len(t.PromptVars) > 0 {
		raw, err := jsonMarshal(t.PromptVars)
		if err != nil {
			return fmt.Errorf("config: prompt_vars: %w", err)
		}
		t.PromptVarsJSON = raw
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
