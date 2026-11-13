// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package request

import (
	"fmt"
	"strings"
)

// ContentOverride is an operator-supplied replacement for the content of
// one predefined request, scoped to a single batch execution.
//
// It is deliberately transient: overrides travel with the run request,
// are applied to an in-memory copy of the Request and are never written
// back to the store. The saved request stays exactly as the operator
// defined it, so a one-off ad-hoc question cannot silently rewrite a
// template that other people rely on.
//
// Pointer fields distinguish "not mentioned" (keep the stored value)
// from an explicit empty string (clear it). PromptVars follows the same
// rule with a nil vs. empty map.
type ContentOverride struct {
	// SystemPrompt replaces Request.SystemPrompt when non-nil. An empty
	// string is valid — it removes the system prompt for this run.
	SystemPrompt *string `json:"system_prompt,omitempty"`
	// UserPrompt replaces Request.UserPrompt when non-nil. It must not be
	// blank: the probe adapters require a user prompt.
	UserPrompt *string `json:"user_prompt,omitempty"`
	// PromptVars replaces Request.PromptVars wholesale when non-nil. An
	// empty map clears every variable for this run.
	PromptVars map[string]string `json:"prompt_vars,omitempty"`
}

// IsSet reports whether the override actually changes anything. A run
// request that carries an all-empty override is equivalent to no override.
func (o ContentOverride) IsSet() bool {
	return o.SystemPrompt != nil || o.UserPrompt != nil || o.PromptVars != nil
}

// apply mutates a copy of the request for one execution.
//
// The map is cloned rather than edited in place: stores are allowed to
// hand back cached pointers, and writing through one would leak an
// ad-hoc override into the next run.
func (o ContentOverride) apply(r *Request) error {
	if o.UserPrompt != nil {
		if strings.TrimSpace(*o.UserPrompt) == "" {
			return fmt.Errorf("request: custom user_prompt is blank: %w", ErrInvalidConfig)
		}
		r.UserPrompt = *o.UserPrompt
	}
	if o.SystemPrompt != nil {
		r.SystemPrompt = *o.SystemPrompt
	}
	if o.PromptVars != nil {
		vars := make(map[string]string, len(o.PromptVars))
		for k, v := range o.PromptVars {
			vars[k] = v
		}
		r.PromptVars = vars
	}
	return nil
}
