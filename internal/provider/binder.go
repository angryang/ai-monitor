// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/angryang/ai-monitor/internal/config"
)

// Binding failures. They are separate sentinels because the operator has to
// be able to tell them apart: a task pointing at a deleted provider needs a
// different fix than one whose provider lost its key.
var (
	// ErrMissing means the provider a task points at does not exist
	// (any more).
	ErrMissing = errors.New("provider: not found")
	// ErrSecret means the provider's stored key could not be decrypted
	// with the current master key.
	ErrSecret = errors.New("provider: api key cannot be decrypted")
	// ErrNoKey means the provider has no api key configured at all.
	ErrNoKey = errors.New("provider: has no api key")
	// ErrModel means the model the task/request selected is no longer in
	// the provider's candidate list.
	ErrModel = errors.New("provider: model is not offered")
)

// Binder resolves the AI service provider a monitoring task points at and
// copies the upstream coordinates onto the task's runtime-only fields.
//
// The task itself stores a provider id plus the ONE model it selected.
// Everything else the probe needs — protocol, base URL, credentials,
// timeout, TLS and thinking switches, the protocol-specific block — comes
// from the provider. The binding happens right before a probe runs, not
// when the task is loaded, so editing provider coordinates takes effect on
// the very next tick without reloading the scheduler.
//
// This mirrors how the AI request manager resolves its upstream
// (request.Service), which keeps the two features consistent: "the provider
// is the single source of truth for the upstream" applies everywhere.
type Binder struct {
	store Store
}

// NewBinder wires a binder on top of a provider store.
func NewBinder(store Store) *Binder {
	return &Binder{store: store}
}

// BindTask returns a copy of t with the provider's upstream coordinates
// filled in.
//
// The input task is not modified: the caller keeps its own copy of the
// persisted row, and the probe layer must never see a half-bound task.
//
// Only the protocol block matching the provider's protocol is carried
// over. config.TaskConfig.ValidateProtocolSegments rejects a config that
// sets more than one block, and the adapters only read their own.
func (b *Binder) BindTask(ctx context.Context, t config.TaskConfig) (config.TaskConfig, error) {
	if b == nil || b.store == nil {
		return t, fmt.Errorf("%w: the provider registry is not configured", ErrMissing)
	}
	if t.ProviderID == "" {
		return t, fmt.Errorf("%w: the task has no provider", ErrMissing)
	}
	p, err := b.store.GetProvider(ctx, t.ProviderID)
	if err != nil {
		return t, fmt.Errorf("%w: %s", ErrMissing, err.Error())
	}
	if p.SecretError != "" {
		return t, fmt.Errorf("%w: provider %q", ErrSecret, p.Name)
	}
	if p.APIKey == "" {
		return t, fmt.Errorf("%w: provider %q", ErrNoKey, p.Name)
	}
	// The model belongs to the task, not to the provider: the provider
	// only offers candidates. Fall back to the primary model for rows
	// written before the multi-model migration.
	model := strings.TrimSpace(t.Model)
	if model == "" {
		model = p.PrimaryModel()
	}
	if len(p.Models) > 0 && !p.HasModel(model) {
		return t, fmt.Errorf("%w: %q is not offered by provider %q", ErrModel, model, p.Name)
	}

	out := t
	out.Protocol = p.Protocol
	out.BaseURL = p.BaseURL
	out.APIKey = p.APIKey
	out.APIKeyEnc = p.APIKeyEnc
	out.Model = model
	out.Timeout = p.EffectiveTimeout()
	out.TimeoutMS = p.TimeoutMS
	out.InsecureSkipVerify = p.InsecureSkipVerify
	out.ThinkingEnabled = p.ThinkingEnabled
	// Start from a clean slate: a task may carry blocks from an earlier
	// binding, and ValidateProtocolSegments would reject the result.
	out.OpenAI = nil
	out.Anthropic = nil
	out.Gemini = nil
	switch p.Protocol {
	case config.ProtocolOpenAI:
		out.OpenAI = p.OpenAI
	case config.ProtocolAnthropic:
		out.Anthropic = p.Anthropic
	case config.ProtocolGemini:
		out.Gemini = p.Gemini
	}
	return out, nil
}

// BindTasks binds a batch of tasks.
//
// A task whose provider cannot be resolved is skipped instead of failing
// the whole batch: one dangling provider must not stop every other task
// from being scheduled. The skipped ids are returned so the caller can log
// them loudly.
func (b *Binder) BindTasks(ctx context.Context, tasks []config.TaskConfig) ([]config.TaskConfig, []error) {
	out := make([]config.TaskConfig, 0, len(tasks))
	var errs []error
	for _, t := range tasks {
		bound, err := b.BindTask(ctx, t)
		if err != nil {
			errs = append(errs, fmt.Errorf("provider: bind task %q: %w", t.Name, err))
			continue
		}
		out = append(out, bound)
	}
	return out, errs
}
