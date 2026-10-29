// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package protocol

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/angryang/ai-monitor/internal/config"
)

func TestBuildInput_IncludesSystemAndUser(t *testing.T) {
	cfg := &config.TaskConfig{
		SystemPrompt: "You are a helpful assistant.",
		UserPrompt:   "你好",
	}
	got := BuildInput(cfg)
	if !strings.Contains(got, "[system]\nYou are a helpful assistant.") {
		t.Fatalf("missing system block, got %q", got)
	}
	if !strings.Contains(got, "[user]\n你好") {
		t.Fatalf("missing user block, got %q", got)
	}
}

func TestBuildInput_ExpandsPromptVars(t *testing.T) {
	cfg := &config.TaskConfig{
		UserPrompt: "say ${word} to ${who}",
		PromptVars: map[string]string{"word": "hi", "who": "bob"},
	}
	got := BuildInput(cfg)
	if !strings.Contains(got, "say hi to bob") {
		t.Fatalf("prompt vars not expanded, got %q", got)
	}
}

func TestBuildInput_OmitsEmptySystem(t *testing.T) {
	cfg := &config.TaskConfig{UserPrompt: "only user"}
	got := BuildInput(cfg)
	if strings.Contains(got, "[system]") {
		t.Fatalf("expected no system block, got %q", got)
	}
	if !strings.HasPrefix(got, "[user]\n") {
		t.Fatalf("unexpected layout: %q", got)
	}
}

func TestExpandVars_LeavesUnknownVars(t *testing.T) {
	got := ExpandVars("a ${known} b ${unknown}", map[string]string{"known": "K"})
	if got != "a K b ${unknown}" {
		t.Fatalf("got %q", got)
	}
}

func TestTextAccumulator_TruncatesAtLimit(t *testing.T) {
	acc := NewTextAccumulator(10)
	acc.WriteString("0123456789")
	acc.WriteString("ABCDEF")
	if got := acc.String(); got != "0123456789" {
		t.Fatalf("got %q, want first 10 bytes", got)
	}
	if !acc.Truncated() {
		t.Fatal("expected truncated flag")
	}
}

func TestTextAccumulator_TruncatesOnRuneBoundary(t *testing.T) {
	// "中" is 3 bytes. A 4-byte limit must not cut a rune in half.
	acc := NewTextAccumulator(4)
	acc.WriteString("中中")
	got := acc.String()
	if got != "中" {
		t.Fatalf("got %q, want a single intact rune", got)
	}
	if !acc.Truncated() {
		t.Fatal("expected truncated flag")
	}
}

func TestTextAccumulator_NoTruncationUnderLimit(t *testing.T) {
	acc := NewTextAccumulator(100)
	acc.WriteString("hello")
	acc.WriteString(" world")
	if got := acc.String(); got != "hello world" {
		t.Fatalf("got %q", got)
	}
	if acc.Truncated() {
		t.Fatal("unexpected truncated flag")
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := FirstNonEmpty("", "", "third"); got != "third" {
		t.Fatalf("got %q", got)
	}
	if got := FirstNonEmpty("", ""); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestRequestError_UnwrapsToSentinel(t *testing.T) {
	inner := fmt.Errorf("openai: status 401: %w", ErrUnauthorized)
	err := NewRequestError("rid-1", []byte(`{}`), "boom", inner)
	// Error() renders the wrapped cause, not the response body.
	if !strings.Contains(err.Error(), "status 401") {
		t.Fatalf("Error() = %q", err.Error())
	}
	if err.ResponseBody != "boom" {
		t.Fatalf("ResponseBody = %q", err.ResponseBody)
	}
	// The runner classifies failures with errors.Is, so the sentinel must
	// survive the wrapper.
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatal("errors.Is could not reach the sentinel")
	}
}

func TestReadErrorBody_TrimsAndBounds(t *testing.T) {
	got := ReadErrorBody(strings.NewReader(`{"error":"nope"}`), 1024)
	if got != `{"error":"nope"}` {
		t.Fatalf("got %q", got)
	}
	// Over the cap -> truncated to the cap.
	got = ReadErrorBody(strings.NewReader(strings.Repeat("x", 100)), 10)
	if len(got) != 10 {
		t.Fatalf("len = %d, want 10", len(got))
	}
}

func TestReadErrorBody_HandlesInvalidUTF8(t *testing.T) {
	// A lone 0xFF is not valid UTF-8; it must be trimmed rather than
	// written into the DB verbatim.
	got := ReadErrorBody(strings.NewReader("ok\xff"), 1024)
	if strings.ContainsRune(got, 0xFF) {
		t.Fatalf("invalid rune survived: %q", got)
	}
}
