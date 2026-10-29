// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/protocol"
)

var _ = strings.Contains // keep import used

func newAdapter(t *testing.T) *Adapter {
	t.Helper()
	a, err := protocol.Lookup("openai")
	if err != nil {
		t.Fatalf("not registered: %v", err)
	}
	return a.(*Adapter)
}

func baseTask() *config.TaskConfig {
	return &config.TaskConfig{
		ID:         "t-1",
		Name:       "openai-test",
		Protocol:   config.ProtocolOpenAI,
		BaseURL:    "http://127.0.0.1:0",
		APIKey:     "sk-test",
		Model:      "gpt-4o",
		UserPrompt: "hello",
	}
}

func TestOpenAI_Probe_Success(t *testing.T) {
	adapter := newAdapter(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		for i, line := range []string{
			`data: {"choices":[{"delta":{"content":"hi"},"finish_reason":""}]}`,
			`data: {"choices":[{"delta":{"content":" there"},"finish_reason":""}]}`,
			`data: {"choices":[{"delta":{"content":"!"},"finish_reason":""}]}`,
			`data: {"choices":[{"delta":{"content":""},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		} {
			_, _ = io.WriteString(w, line)
			if i < 4 {
				_, _ = io.WriteString(w, "\n\n")
			}
			if flusher != nil {
				flusher.Flush()
			}
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
	if len(got) < 4 {
		t.Fatalf("got %d chunks, want >= 4", len(got))
	}
	if got[len(got)-1].IsFinal == false {
		t.Fatalf("last chunk should be final: %+v", got[len(got)-1])
	}
	if err := s.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
}

func TestOpenAI_Probe_Unauthorized(t *testing.T) {
	adapter := newAdapter(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
	}))
	defer srv.Close()
	cfg := baseTask()
	cfg.BaseURL = srv.URL
	_, err := adapter.Probe(context.Background(), cfg)
	if !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("err: got %v, want ErrUnauthorized", err)
	}
}

func TestOpenAI_Probe_Timeout(t *testing.T) {
	adapter := newAdapter(t)
	// Server writes one chunk promptly then idles; we cancel the client
	// context and verify the chunks channel eventually closes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		// Write one chunk so the consume goroutine has something to start.
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"},\"finish_reason\":\"\"}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Wait for client to give up. Use a max wait so this handler always
		// returns even if the client transport misbehaves.
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

	// Read chunks; eventually Chunks() must close (via context cancellation
	// propagating to the consume goroutine).
	count := 0
loop:
	for {
		select {
		case _, ok := <-s.Chunks():
			if !ok {
				break loop
			}
			count++
		case <-time.After(2 * time.Second):
			t.Fatalf("Chunks channel did not close after ctx cancel; received %d chunks", count)
		}
	}
}

func TestOpenAI_Validate_InvalidPath(t *testing.T) {
	adapter := newAdapter(t)
	cfg := baseTask()
	cfg.OpenAI = &config.OpenAIConfig{StreamPath: "no-leading-slash"}
	if err := adapter.Validate(cfg); !errors.Is(err, protocol.ErrProtocolCfg) {
		t.Fatalf("err: got %v, want ErrProtocolCfg", err)
	}
}

func TestOpenAI_Validate_NoAPIKey(t *testing.T) {
	adapter := newAdapter(t)
	cfg := baseTask()
	cfg.APIKey = ""
	if err := adapter.Validate(cfg); !errors.Is(err, protocol.ErrProtocolCfg) {
		t.Fatalf("err: got %v, want ErrProtocolCfg", err)
	}
}

func TestOpenAI_Validate_WrongProtocol(t *testing.T) {
	adapter := newAdapter(t)
	cfg := baseTask()
	cfg.Protocol = config.ProtocolAnthropic
	if err := adapter.Validate(cfg); !errors.Is(err, protocol.ErrProtocolCfg) {
		t.Fatalf("err: got %v, want ErrProtocolCfg", err)
	}
}

// TestOpenAI_Probe_Headers ensures Authorization + Content-Type headers
// reached the upstream (we only assert a couple to avoid over-specifying).
func TestOpenAI_Probe_Headers(t *testing.T) {
	captured := struct {
		Auth string
		UA   string
		Type string
	}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Auth = r.Header.Get("Authorization")
		captured.UA = r.Header.Get("User-Agent")
		captured.Type = r.Header.Get("Content-Type")
		w.WriteHeader(200)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
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
	if !strings.HasPrefix(captured.Auth, "Bearer ") {
		t.Fatalf("Auth: got %q", captured.Auth)
	}
	if !strings.HasPrefix(captured.Type, "application/json") {
		t.Fatalf("Type: got %q", captured.Type)
	}
	if captured.UA == "" {
		t.Fatalf("UA: empty")
	}
}

func TestOpenAI_Probe_SendsExtraHeaders(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Canary-Group")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	adapter := newAdapter(t)
	cfg := baseTask()
	cfg.BaseURL = srv.URL
	cfg.OpenAI = &config.OpenAIConfig{StreamPath: "/v1/chat/completions", ExtraHeadersJSON: `{"X-Canary-Group":"gray-2024q3","X-Trace-Id":"abc"}`}
	s, err := adapter.Probe(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	for range s.Chunks() {
	}
	if got != "gray-2024q3" {
		t.Fatalf("X-Canary-Group: got %q", got)
	}
}

func TestOpenAI_Probe_AuthHeaderNotOverridden(t *testing.T) {
	adapter := newAdapter(t)
	cfg := baseTask()
	cfg.OpenAI = &config.OpenAIConfig{ExtraHeadersJSON: `{"Authorization":"Bearer fake"}`}
	_, err := adapter.Probe(context.Background(), cfg)
	if err == nil {
		t.Fatalf("expected blacklist error")
	}
	if !errors.Is(err, protocol.ErrProtocolCfg) {
		t.Fatalf("err: %v", err)
	}
}

func TestExtraHeaders_BlackList(t *testing.T) {
	for _, header := range []string{"Host", "Content-Length", "Connection", "Authorization"} {
		req, _ := http.NewRequest(http.MethodPost, "http://example", nil)
		err := protocol.ApplyExtraHeaders(req, `{"`+header+`":"x"}`)
		if err == nil {
			t.Fatalf("expected error for %s", header)
		}
		if !errors.Is(err, protocol.ErrBlacklistHeader) {
			t.Fatalf("%s: got %v", header, err)
		}
	}
}

// TestOpenAI_Probe_GeneratesReqID verifies that ai-monitor auto-injects a
// canonical X-Req-Id per probe so upstream logs are correlatable.
func TestOpenAI_Probe_GeneratesReqID(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("X-Req-Id")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
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
		t.Fatalf("X-Req-Id missing from upstream request")
	}
	// Default generator is uuid.NewString → expect 36-char id with 4 hyphens.
	if len(captured) != 36 || strings.Count(captured, "-") != 4 {
		t.Fatalf("X-Req-Id %q does not look like a uuid", captured)
	}
}

// TestOpenAI_Probe_UserReqIDFromExtras asserts the user-supplied id wins
// when provided via extra_headers (consistent with §11 design intent:
// adapters only supply a default when the user hasn't picked one).
func TestOpenAI_Probe_UserReqIDFromExtras(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("X-Req-Id")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	adapter := newAdapter(t)
	cfg := baseTask()
	cfg.BaseURL = srv.URL
	cfg.OpenAI = &config.OpenAIConfig{ExtraHeadersJSON: `{"X-Req-Id":"upstream-trace-42"}`}
	s, err := adapter.Probe(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	for range s.Chunks() {
	}
	if captured != "upstream-trace-42" {
		t.Fatalf("X-Req-Id: got %q, want %q", captured, "upstream-trace-42")
	}
}

// TestOpenAI_Probe_CapturesReasoningAndRequestID verifies §18.14:
// the reasoning trace is captured separately from the answer, and the
// X-Req-Id is recoverable even when the upstream rejects the request.
func TestOpenAI_Probe_CapturesReasoningAndRequestID(t *testing.T) {
	adapter := newAdapter(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, line := range []string{
			`data: {"choices":[{"delta":{"reasoning_content":"先想一想"},"finish_reason":""}]}`,
			`data: {"choices":[{"delta":{"content":"答案"},"finish_reason":""}]}`,
			`data: {"choices":[{"delta":{"content":"! "},"finish_reason":""}]}`,
			`data: {"choices":[{"delta":{"content":""},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		} {
			_, _ = io.WriteString(w, line+"\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
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

	var content string
	for c := range s.Chunks() {
		if c.Kind == protocol.ChunkContent {
			content += c.Delta
		}
	}
	if content != "答案! " {
		t.Fatalf("content = %q, want %q", content, "答案! ")
	}
	cap := s.ContentCapture()
	if cap.ThinkingContent != "先想一想" {
		t.Fatalf("thinking = %q", cap.ThinkingContent)
	}
	if cap.Truncated {
		t.Fatal("unexpected truncation")
	}
	if s.RequestID() == "" {
		t.Fatal("expected a request id")
	}
}

// TestOpenAI_Probe_UnauthorizedKeepsRequestID verifies that a 401 still
// carries the X-Req-Id and the upstream error body, which are exactly
// what an admin needs to chase the problem.
func TestOpenAI_Probe_UnauthorizedKeepsRequestID(t *testing.T) {
	adapter := newAdapter(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":{"message":"Incorrect API key provided"}}`)
	}))
	defer srv.Close()
	cfg := baseTask()
	cfg.BaseURL = srv.URL

	_, err := adapter.Probe(context.Background(), cfg)
	if !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("err: got %v, want ErrUnauthorized", err)
	}
	var reqErr *protocol.RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("expected a RequestError, got %T", err)
	}
	if reqErr.RequestID == "" {
		t.Fatal("request id lost on the failure path")
	}
	if !strings.Contains(reqErr.ResponseBody, "Incorrect API key") {
		t.Fatalf("error body = %q", reqErr.ResponseBody)
	}
}

// TestOpenAI_BuildRequest_ThinkingSwitch verifies the per-task thinking
// switch: on → `enable_thinking: true` on the wire; off → the field is
// omitted entirely so the request is byte-identical to the pre-flag shape.
func TestOpenAI_BuildRequest_ThinkingSwitch(t *testing.T) {
	off := baseTask()
	raw, err := buildRequest(off)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if strings.Contains(string(raw), "enable_thinking") {
		t.Fatalf("thinking off must omit enable_thinking: %s", raw)
	}

	on := baseTask()
	on.ThinkingEnabled = true
	raw, err = buildRequest(on)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	var body struct {
		EnableThinking *bool `json:"enable_thinking"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.EnableThinking == nil || !*body.EnableThinking {
		t.Fatalf("thinking on must set enable_thinking=true: %s", raw)
	}
}
