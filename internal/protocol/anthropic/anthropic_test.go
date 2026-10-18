// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package anthropic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/protocol"
)

func newAdapter(t *testing.T) *Adapter {
	t.Helper()
	a, err := protocol.Lookup("anthropic")
	if err != nil {
		t.Fatalf("not registered: %v", err)
	}
	return a.(*Adapter)
}

func baseTask() *config.TaskConfig {
	return &config.TaskConfig{
		ID:           "t-1",
		Name:         "anthropic-test",
		Protocol:     config.ProtocolAnthropic,
		BaseURL:      "http://127.0.0.1:0",
		APIKey:       "sk-ant-test",
		Model:        "claude-3-5-sonnet-20241022",
		UserPrompt:   "hello",
		Anthropic:    &config.AnthropicConfig{MaxTokens: 256},
	}
}

func TestAnthropic_Probe_Success(t *testing.T) {
	adapter := newAdapter(t)
	const body = "" +
		"event: content_block_delta\n" +
		`data: {"index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"index":0,"delta":{"type":"text_delta","text":" there"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, body)
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer srv.Close()
	cfg := baseTask()
	cfg.BaseURL = srv.URL
	s, err := adapter.Probe(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	defer s.Close()
	var got []protocol.Chunk
	for c := range s.Chunks() {
		got = append(got, c)
	}
	if len(got) < 2 {
		t.Fatalf("got %d chunks; err=%v", len(got), s.Err())
	}
	if got[0].Delta != "hi" {
		t.Fatalf("first delta: got %q", got[0].Delta)
	}
	if err := s.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
}

func TestAnthropic_Probe_Unauthorized(t *testing.T) {
	adapter := newAdapter(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(403)
	}))
	defer srv.Close()
	cfg := baseTask()
	cfg.BaseURL = srv.URL
	_, err := adapter.Probe(context.Background(), cfg)
	if !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("err: got %v, want ErrUnauthorized", err)
	}
}

func TestAnthropic_Probe_Timeout(t *testing.T) {
	adapter := newAdapter(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	cfg := baseTask()
	cfg.BaseURL = srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	s, err := adapter.Probe(ctx, cfg)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	defer s.Close()
	count := 0
loop:
	for {
		select {
		case _, ok := <-s.Chunks():
			if !ok {
				break loop
			}
			count++
		case <-time.After(3 * time.Second):
			t.Fatalf("chunks did not close; got %d", count)
		}
	}
}

func TestAnthropic_Validate_InvalidMaxTokens(t *testing.T) {
	adapter := newAdapter(t)
	cfg := baseTask()
	cfg.Anthropic.MaxTokens = 0
	if err := adapter.Validate(cfg); !errors.Is(err, protocol.ErrProtocolCfg) {
		t.Fatalf("err: got %v", err)
	}
}

func TestAnthropic_Validate_WrongProtocol(t *testing.T) {
	adapter := newAdapter(t)
	cfg := baseTask()
	cfg.Protocol = config.ProtocolOpenAI
	if err := adapter.Validate(cfg); !errors.Is(err, protocol.ErrProtocolCfg) {
		t.Fatalf("err: got %v", err)
	}
}

func TestAnthropic_Headers_PassThrough(t *testing.T) {
	captured := struct {
		Key string
		Ver string
	}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Key = r.Header.Get("x-api-key")
		captured.Ver = r.Header.Get("anthropic-version")
		w.WriteHeader(200)
		_, _ = fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	adapter := newAdapter(t)
	cfg := baseTask()
	cfg.BaseURL = srv.URL
	s, err := adapter.Probe(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	for range s.Chunks() {
	}
	if captured.Key == "" {
		t.Fatalf("missing x-api-key header")
	}
	if captured.Ver == "" {
		t.Fatalf("missing anthropic-version header")
	}
}

func TestAnthropic_Probe_SendsExtraHeaders(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Release-Name")
		w.WriteHeader(200)
		_, _ = fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	adapter := newAdapter(t)
	cfg := baseTask()
	cfg.BaseURL = srv.URL
	cfg.Anthropic.ExtraHeadersJSON = `{"X-Release-Name":"claude-canary-v3"}`
	s, err := adapter.Probe(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	for range s.Chunks() {
	}
	if got != "claude-canary-v3" {
		t.Fatalf("X-Release-Name: got %q", got)
	}
}


func TestAnthropic_Probe_GeneratesReqID(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("X-Req-Id")
		w.WriteHeader(200)
		_, _ = fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	adapter := newAdapter(t)
	cfg := baseTask()
	cfg.BaseURL = srv.URL
	s, err := adapter.Probe(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	for range s.Chunks() {
	}
	if captured == "" {
		t.Fatalf("X-Req-Id missing")
	}
}
