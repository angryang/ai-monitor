// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package request

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"gorm.io/gorm"
)

// SqliteStore is the default Store implementation. It shares the GORM
// handle with the task store so both live in one SQLite file with one WAL
// and one busy-timeout — the same single-source-of-truth rule the probe
// run store follows.
type SqliteStore struct {
	db *gorm.DB
	mu sync.RWMutex
}

// NewSqliteStore wraps an already-opened GORM handle and runs the
// migration for the request table.
func NewSqliteStore(db *gorm.DB) (*SqliteStore, error) {
	if db == nil {
		return nil, fmt.Errorf("request: nil db handle")
	}
	if err := db.AutoMigrate(&Request{}); err != nil {
		return nil, fmt.Errorf("request: migrate: %w", err)
	}
	return &SqliteStore{db: db}, nil
}

// ListRequests returns every request, ordered by Order then id.
func (s *SqliteStore) ListRequests(ctx context.Context) ([]Request, error) {
	var requests []Request
	if err := s.db.WithContext(ctx).
		Order("sort_order ASC, id ASC").
		Find(&requests).Error; err != nil {
		return nil, fmt.Errorf("request: list requests: %w", err)
	}
	for i := range requests {
		hydrateRequest(&requests[i])
	}
	return requests, nil
}

// GetRequest returns one request by id.
func (s *SqliteStore) GetRequest(ctx context.Context, id string) (*Request, error) {
	var r Request
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&r).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, fmt.Errorf("request: request %q: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("request: get request: %w", err)
	}
	hydrateRequest(&r)
	return &r, nil
}

// CreateRequest inserts a new request.
func (s *SqliteStore) CreateRequest(ctx context.Context, r *Request) error {
	if r == nil {
		return fmt.Errorf("request: nil request: %w", ErrInvalidConfig)
	}
	if err := prepareRequest(r); err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).Create(r).Error; err != nil {
		return fmt.Errorf("request: create request: %w", err)
	}
	return nil
}

// UpdateRequest overwrites an existing request.
func (s *SqliteStore) UpdateRequest(ctx context.Context, r *Request) error {
	if r == nil {
		return fmt.Errorf("request: nil request: %w", ErrInvalidConfig)
	}
	if err := prepareRequest(r); err != nil {
		return err
	}
	res := s.db.WithContext(ctx).Model(&Request{}).Where("id = ?", r.ID).Updates(map[string]any{
		"provider_id":       r.ProviderID,
		"model":             r.Model,
		"name":              r.Name,
		"description":       r.Description,
		"system_prompt":     r.SystemPrompt,
		"user_prompt":       r.UserPrompt,
		"prompt_vars":       r.PromptVarsJSON,
		"sort_order":        r.Order,
		"thinking_override": r.ThinkingOverride,
		// updated_at is deliberately absent: GORM's autoUpdateTime fills
		// it in. Naming it here would write the caller's zero timestamp
		// over the real one.
	})
	if res.Error != nil {
		return fmt.Errorf("request: update request: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("request: request %q: %w", r.ID, ErrNotFound)
	}
	return nil
}

// DeleteRequest removes a request.
func (s *SqliteStore) DeleteRequest(ctx context.Context, id string) error {
	res := s.db.WithContext(ctx).Where("id = ?", id).Delete(&Request{})
	if res.Error != nil {
		return fmt.Errorf("request: delete request: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("request: request %q: %w", id, ErrNotFound)
	}
	return nil
}

// ListRequestsByProvider returns the requests that target one provider.
func (s *SqliteStore) ListRequestsByProvider(ctx context.Context, providerID string) ([]Request, error) {
	if providerID == "" {
		return nil, nil
	}
	var requests []Request
	if err := s.db.WithContext(ctx).
		Where("provider_id = ?", providerID).
		Order("sort_order ASC, id ASC").
		Find(&requests).Error; err != nil {
		return nil, fmt.Errorf("request: list requests by provider: %w", err)
	}
	for i := range requests {
		hydrateRequest(&requests[i])
	}
	return requests, nil
}

// ---- helpers ----

// prepareRequest validates the request and serialises the prompt
// variables.
func prepareRequest(r *Request) error {
	if r.ID == "" {
		return fmt.Errorf("request: missing id: %w", ErrInvalidConfig)
	}
	if strings.TrimSpace(r.ProviderID) == "" {
		return fmt.Errorf("request: missing provider_id: %w", ErrInvalidConfig)
	}
	if strings.TrimSpace(r.Model) == "" {
		return fmt.Errorf("request: missing model: %w", ErrInvalidConfig)
	}
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("request: missing name: %w", ErrInvalidConfig)
	}
	if strings.TrimSpace(r.UserPrompt) == "" {
		return fmt.Errorf("request: missing user_prompt: %w", ErrInvalidConfig)
	}
	if len(r.PromptVars) > 0 {
		raw, err := json.Marshal(r.PromptVars)
		if err != nil {
			return fmt.Errorf("request: prompt_vars: %w", err)
		}
		r.PromptVarsJSON = string(raw)
	} else {
		r.PromptVarsJSON = ""
	}
	return nil
}

// hydrateRequest decodes the prompt variables.
func hydrateRequest(r *Request) {
	if r.PromptVarsJSON == "" || len(r.PromptVars) > 0 {
		return
	}
	m := map[string]string{}
	if err := json.Unmarshal([]byte(r.PromptVarsJSON), &m); err != nil {
		return
	}
	r.PromptVars = m
}
