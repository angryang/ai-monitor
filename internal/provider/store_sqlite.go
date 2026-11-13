// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/angryang/ai-monitor/internal/config"
	"gorm.io/gorm"
)

// SqliteStore is the default Store implementation. It shares the GORM
// handle with the task store so everything lives in one SQLite file with
// one WAL and one busy-timeout — the same single-source-of-truth rule the
// request store and the probe run store follow.
type SqliteStore struct {
	db     *gorm.DB
	cipher config.Cipher
	mu     sync.RWMutex
}

// NewSqliteStore wraps an already-opened GORM handle and runs the
// migration for the provider table.
func NewSqliteStore(db *gorm.DB) (*SqliteStore, error) {
	if db == nil {
		return nil, fmt.Errorf("provider: nil db handle")
	}
	if err := db.AutoMigrate(&Provider{}); err != nil {
		return nil, fmt.Errorf("provider: migrate: %w", err)
	}
	return &SqliteStore{db: db}, nil
}

// SetCipher attaches an encryption backend. Pass nil to disable
// encryption. The cipher must be the same instance the task store uses —
// one master key per instance.
func (s *SqliteStore) SetCipher(c config.Cipher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cipher = c
}

func (s *SqliteStore) cipherOrNil() config.Cipher {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cipher
}

// ListProviders returns every provider, oldest first.
//
// Decryption failures are tolerated: the provider is returned with an empty
// APIKey and SecretError set. A single row written under a different master
// key must not hide every other provider from the operator — that is
// exactly the moment they need the UI to work.
func (s *SqliteStore) ListProviders(ctx context.Context) ([]Provider, error) {
	var providers []Provider
	if err := s.db.WithContext(ctx).Order("created_at ASC, id ASC").Find(&providers).Error; err != nil {
		return nil, fmt.Errorf("provider: list providers: %w", err)
	}
	for i := range providers {
		s.hydrateProvider(&providers[i])
	}
	return providers, nil
}

// GetProvider returns one provider by id.
func (s *SqliteStore) GetProvider(ctx context.Context, id string) (*Provider, error) {
	var p Provider
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&p).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, fmt.Errorf("provider: provider %q: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("provider: get provider: %w", err)
	}
	s.hydrateProvider(&p)
	return &p, nil
}

// CreateProvider inserts a new provider.
func (s *SqliteStore) CreateProvider(ctx context.Context, p *Provider) error {
	if p == nil {
		return fmt.Errorf("provider: nil provider: %w", ErrInvalidConfig)
	}
	if err := s.prepareProvider(p); err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).Create(p).Error; err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("provider: provider name %q: %w", p.Name, ErrAlreadyExists)
		}
		return fmt.Errorf("provider: create provider: %w", err)
	}
	return nil
}

// UpdateProvider overwrites an existing provider.
func (s *SqliteStore) UpdateProvider(ctx context.Context, p *Provider) error {
	if p == nil {
		return fmt.Errorf("provider: nil provider: %w", ErrInvalidConfig)
	}
	if err := s.prepareProvider(p); err != nil {
		return err
	}
	res := s.db.WithContext(ctx).Model(&Provider{}).Where("id = ?", p.ID).Updates(map[string]any{
		"name":                    p.Name,
		"description":             p.Description,
		"enabled":                 p.Enabled,
		"protocol":                p.Protocol,
		"base_url":                p.BaseURL,
		"api_key_enc":             p.APIKeyEnc,
		"models":                  p.ModelsJSON,
		"model":                   p.Model, // legacy NOT NULL mirror
		"timeout_ms":              p.TimeoutMS,
		"insecure_skip_verify":    p.InsecureSkipVerify,
		"thinking_enabled":        p.ThinkingEnabled,
		"stream_path":             openAIStreamPath(p),
		"openai_extra_headers":    openAIExtraHeaders(p),
		"max_tokens":              anthropicMaxTokens(p),
		"anthropic_extra_headers": anthropicExtraHeaders(p),
		"alt":                     geminiAlt(p),
		"gemini_extra_headers":    geminiExtraHeaders(p),
		"last_verified_at":        p.LastVerifiedAt,
		"last_verified_ok":        p.LastVerifiedOK,
		"last_verified_message":   p.LastVerifiedMessage,
		// updated_at is deliberately absent: GORM's autoUpdateTime fills
		// it in. Naming it here would write the caller's zero timestamp
		// over the real one.
	})
	if res.Error != nil {
		if isUniqueViolation(res.Error) {
			return fmt.Errorf("provider: provider name %q: %w", p.Name, ErrAlreadyExists)
		}
		return fmt.Errorf("provider: update provider: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("provider: provider %q: %w", p.ID, ErrNotFound)
	}
	return nil
}

// DeleteProvider removes a provider.
func (s *SqliteStore) DeleteProvider(ctx context.Context, id string) error {
	res := s.db.WithContext(ctx).Where("id = ?", id).Delete(&Provider{})
	if res.Error != nil {
		return fmt.Errorf("provider: delete provider: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("provider: provider %q: %w", id, ErrNotFound)
	}
	return nil
}

// ---- helpers ----

// prepareProvider validates the provider, seals the api key and normalises
// the embedded protocol blocks so GORM never writes NULL into a NOT NULL
// column.
func (s *SqliteStore) prepareProvider(p *Provider) error {
	if p.ID == "" {
		return fmt.Errorf("provider: missing id: %w", ErrInvalidConfig)
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("provider: missing name: %w", ErrInvalidConfig)
	}
	if !p.Protocol.IsValid() {
		return fmt.Errorf("provider: protocol %q: %w", p.Protocol, ErrInvalidConfig)
	}
	if strings.TrimSpace(p.BaseURL) == "" {
		return fmt.Errorf("provider: missing base_url: %w", ErrInvalidConfig)
	}
	if err := p.NormaliseModels(); err != nil {
		return err
	}
	if p.TimeoutMS <= 0 {
		p.TimeoutMS = DefaultTimeout.Milliseconds()
	}
	if p.TimeoutMS > MaxTimeout.Milliseconds() {
		return fmt.Errorf("provider: timeout %dms exceeds the %s cap: %w", p.TimeoutMS, MaxTimeout, ErrInvalidConfig)
	}
	// Same protocol-block defaults as config.TaskConfig.Normalise.
	if p.OpenAI == nil {
		p.OpenAI = &config.OpenAIConfig{}
	}
	if p.Anthropic == nil {
		p.Anthropic = &config.AnthropicConfig{MaxTokens: 1024}
	}
	if p.Gemini == nil {
		p.Gemini = &config.GeminiConfig{Alt: "sse"}
	}
	if c := s.cipherOrNil(); c != nil && p.APIKey != "" {
		enc, err := c.Encrypt([]byte(p.APIKey))
		if err != nil {
			return fmt.Errorf("provider: encrypt api_key: %w", err)
		}
		p.APIKeyEnc = enc
	}
	// Never keep the plaintext around after the write.
	p.APIKey = ""
	return nil
}

// hydrateProvider decrypts the api key and records a decryption failure in
// SecretError instead of returning an error.
//
// The ciphertext is deliberately kept when the decryption fails: the row
// may still be readable once the operator restores the right master key,
// and dropping it here would destroy the stored secret on the next
// unrelated write (a rename, say).
func (s *SqliteStore) hydrateProvider(p *Provider) {
	p.HydrateModels()
	c := s.cipherOrNil()
	if c == nil || len(p.APIKeyEnc) == 0 {
		return
	}
	raw, err := c.Decrypt(p.APIKeyEnc)
	if err != nil {
		p.SecretError = err.Error()
		p.APIKey = ""
		return
	}
	p.APIKey = string(raw)
}

// isUniqueViolation reports whether err is a UNIQUE constraint failure.
// GORM wraps the driver's error, so we look for the SQLite message rather
// than a concrete error type.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key")
}

// The embedded protocol blocks are flattened into columns by GORM. These
// accessors read them back safely — the blocks are pointers and may be
// nil on a partially-populated row.
func openAIStreamPath(p *Provider) string {
	if p.OpenAI == nil {
		return ""
	}
	return p.OpenAI.StreamPath
}

func openAIExtraHeaders(p *Provider) string {
	if p.OpenAI == nil {
		return ""
	}
	return p.OpenAI.ExtraHeadersJSON
}

func anthropicMaxTokens(p *Provider) int {
	if p.Anthropic == nil {
		return 1024
	}
	return p.Anthropic.MaxTokens
}

func anthropicExtraHeaders(p *Provider) string {
	if p.Anthropic == nil {
		return ""
	}
	return p.Anthropic.ExtraHeadersJSON
}

func geminiAlt(p *Provider) string {
	if p.Gemini == nil {
		return "sse"
	}
	return p.Gemini.Alt
}

func geminiExtraHeaders(p *Provider) string {
	if p.Gemini == nil {
		return ""
	}
	return p.Gemini.ExtraHeadersJSON
}
