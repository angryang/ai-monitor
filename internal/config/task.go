// Package config owns the declarative TaskConfig model plus the runtime
// configuration loader (yaml < env < flag). All cross-task config
// structures live here so other packages can compile against stable
// schemas.
package config

import (
	"fmt"
	"time"
)

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
	ID           string            `gorm:"primaryKey;size:64" yaml:"id"`
	Name         string            `gorm:"uniqueIndex;size:128;not null" yaml:"name" validate:"required,min=1,max=128"`
	Enabled      bool              `gorm:"not null;default:true" yaml:"enabled"`
	Protocol     Protocol          `gorm:"size:32;not null" yaml:"protocol" validate:"required,oneof=openai anthropic gemini"`
	BaseURL      string            `gorm:"size:512;not null" yaml:"base_url" validate:"required,url"`
	APIKey       string            `gorm:"-"` // runtime only; loaded by store
	APIKeyEnc    []byte            `gorm:"column:api_key_enc;type:blob"`
	Model        string            `gorm:"size:128;not null" yaml:"model" validate:"required"`
	SystemPrompt string            `gorm:"type:text" yaml:"system_prompt,omitempty"`
	UserPrompt   string            `gorm:"type:text" yaml:"user_prompt" validate:"required"`
	PromptVars   map[string]string `gorm:"type:text" yaml:"prompt_vars,omitempty"` // JSON
	Schedule     string            `gorm:"size:128;not null" yaml:"schedule" validate:"required"`
	Timeout      time.Duration     `gorm:"not null;default:30s" yaml:"timeout" validate:"min=1s,max=10m"`
	CanaryGroup  string            `gorm:"size:64;not null;default:default" yaml:"canary_group,omitempty"`
	Notify       NotifyConfig      `gorm:"embedded" yaml:"notify,omitempty"`
	OpenAI       *OpenAIConfig     `gorm:"embedded" yaml:"openai,omitempty"`
	Anthropic    *AnthropicConfig  `gorm:"embedded" yaml:"anthropic,omitempty"`
	Gemini       *GeminiConfig     `gorm:"embedded" yaml:"gemini,omitempty"`
	CreatedAt    time.Time         `gorm:"autoCreateTime"`
	UpdatedAt    time.Time         `gorm:"autoUpdateTime"`
}

// NotifyConfig is the embedded notification block.
type NotifyConfig struct {
	Enabled         bool          `gorm:"not null;default:false" yaml:"enabled"`
	Channel         NotifyChannel `gorm:"size:32;not null;default:wecom" yaml:"channel" validate:"omitempty,oneof=wecom"`
	WeComWebhook    string        `gorm:"-"` // runtime only
	WeComWebhookEnc []byte        `gorm:"column:wecom_webhook_enc;type:blob"`
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
