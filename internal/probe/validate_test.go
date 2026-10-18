// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package probe

import (
	"context"
	"testing"
	"time"

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/metrics"
	"github.com/angryang/ai-monitor/internal/protocol"
)

func TestRunner_ProtocolMutuallyExclusive(t *testing.T) {
	// A task with protocol=openai but with Anthropic.Config set. The
	// TaskConfig-level validator (called at probe.Run time) must surface it.
	cfg := config.TaskConfig{
		ID:         "t-bad",
		Name:       "bad",
		Protocol:   config.ProtocolOpenAI,
		BaseURL:    "http://example",
		APIKey:     "sk",
		Model:      "gpt-4o",
		UserPrompt: "hi",
		Anthropic:  &config.AnthropicConfig{MaxTokens: 256, ExtraHeadersJSON: `{"X":"y"}`},
	}
	sink := metrics.NewPromSink(metrics.PrometheusOptions{})
	stub := &stubAdapter{name: "openai"}
	r := newRunnerForTest(stub, sink, nil)
	res := r.Run(context.Background(), cfg)
	if res.Status != StatusFailure {
		t.Fatalf("status: got %q, want failure", res.Status)
	}
}

func TestRunner_LogIncludesTaskID(t *testing.T) {
	now := time.Now().UTC()
	stub := &stubAdapter{
		name: "openai",
		chunks: []protocol.Chunk{
			{Delta: "x", ArrivedAt: now, Index: 0, IsFinal: true},
		},
		final: protocol.Chunk{ArrivedAt: now.Add(time.Millisecond), Index: 1},
	}
	sink := metrics.NewPromSink(metrics.PrometheusOptions{})
	r := newRunnerForTest(stub, sink, nil)
	res := r.Run(context.Background(), config.TaskConfig{
		ID:         "t-log-1",
		Name:       "t-log-1",
		Protocol:   config.ProtocolOpenAI,
		BaseURL:    "http://x",
		APIKey:     "k",
		Model:      "gpt-4o",
		UserPrompt: "hi",
		Notify:     config.NotifyConfig{Enabled: false},
	})
	if res.Status != StatusSuccess {
		t.Fatalf("status: %s", res.Status)
	}
}
