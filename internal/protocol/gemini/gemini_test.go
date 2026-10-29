// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package gemini

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

func newAdapter(t *testing.T) *Adapter {
	t.Helper()
	a, err := protocol.Lookup("gemini")
	if err != nil {
		t.Fatalf("not registered: %v", err)
	}
	return a.(*Adapter)
}

func baseTask() *config.TaskConfig {
	return &config.TaskConfig{
		ID:         "t-1",
		Name:       "gemini-test",
		Protocol:   config.ProtocolGemini,
		BaseURL:    "http://127.0.0.1:0",
		APIKey:     "gemini-key",
		Model:      "gemini-1.5-pro",
		UserPrompt: "hello",
		Gemini:     &config.GeminiConfig{Alt: "sse"},
	}
}

func TestGemini_Probe_Success(t *testing.T) {
	adapter := newAdapter(t)
	// Gemini returns a JSON array; we craft it so the parse path exercises
	// the depth counter.
	const body = `[` +
		`{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":""}]},` +
		`{"candidates":[{"content":{"parts":[{"text":" there"}]},"finishReason":"STOP"}]}` +
		`]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, body)
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

func TestGemini_Probe_Unauthorized(t *testing.T) {
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

func TestGemini_Probe_Timeout(t *testing.T) {
	adapter := newAdapter(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `[{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":""}]}`)
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
			t.Fatalf("chunks not closed; got %d", count)
		}
	}
}

func TestGemini_Validate_InvalidAlt(t *testing.T) {
	adapter := newAdapter(t)
	cfg := baseTask()
	cfg.Gemini.Alt = "wibble"
	if err := adapter.Validate(cfg); !errors.Is(err, protocol.ErrProtocolCfg) {
		t.Fatalf("err: got %v", err)
	}
}

func TestGemini_Validate_WrongProtocol(t *testing.T) {
	adapter := newAdapter(t)
	cfg := baseTask()
	cfg.Protocol = config.ProtocolOpenAI
	if err := adapter.Validate(cfg); !errors.Is(err, protocol.ErrProtocolCfg) {
		t.Fatalf("err: got %v", err)
	}
}

func TestGemini_Headers_QueryKey(t *testing.T) {
	capturedKey := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedKey = r.URL.Query().Get("key")
		w.WriteHeader(200)
		_, _ = fmt.Fprint(w, `[{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}]`)
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
	if capturedKey != "gemini-key" {
		t.Fatalf("key: got %q", capturedKey)
	}
}

func TestGemini_Probe_SendsExtraHeaders(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Tenant-Id")
		w.WriteHeader(200)
		_, _ = fmt.Fprint(w, `[{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}]`)
	}))
	defer srv.Close()
	adapter := newAdapter(t)
	cfg := baseTask()
	cfg.BaseURL = srv.URL
	cfg.Gemini.ExtraHeadersJSON = `{"X-Tenant-Id":"tenant-canary-001"}`
	s, err := adapter.Probe(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	for range s.Chunks() {
	}
	if got != "tenant-canary-001" {
		t.Fatalf("X-Tenant-Id: got %q", got)
	}
}

func TestGemini_Probe_GeneratesReqID(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("X-Req-Id")
		w.WriteHeader(200)
		_, _ = fmt.Fprint(w, `[{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}]`)
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

// TestGemini_Probe_CapturesThoughtParts verifies §18.14: Gemini marks
// reasoning parts with "thought": true, and those must be routed to the
// thinking column instead of the answer.
func TestGemini_Probe_CapturesThoughtParts(t *testing.T) {
	adapter := newAdapter(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `[{"candidates":[{"content":{"parts":[`+
			`{"text":"想一想","thought":true},`+
			`{"text":"答案"}`+
			`]}}]}]`)
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
	if content != "答案" {
		t.Fatalf("content = %q, want %q", content, "答案")
	}
	cap := s.ContentCapture()
	if cap.ThinkingContent != "想一想" {
		t.Fatalf("thinking = %q", cap.ThinkingContent)
	}
}

// TestGemini_BuildRequest_ThinkingSwitch verifies the per-task thinking
// switch: on → generationConfig.thinkingConfig.thinkingBudget = -1
// (dynamic); off → generationConfig omitted entirely.
func TestGemini_BuildRequest_ThinkingSwitch(t *testing.T) {
	off := baseTask()
	raw, err := buildRequest(off)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if strings.Contains(string(raw), "generationConfig") {
		t.Fatalf("thinking off must omit generationConfig: %s", raw)
	}

	on := baseTask()
	on.ThinkingEnabled = true
	raw, err = buildRequest(on)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	var body struct {
		GenerationConfig *struct {
			ThinkingConfig *struct {
				ThinkingBudget int `json:"thinkingBudget"`
			} `json:"thinkingConfig"`
		} `json:"generationConfig"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.GenerationConfig == nil || body.GenerationConfig.ThinkingConfig == nil {
		t.Fatalf("thinking on must include thinkingConfig: %s", raw)
	}
	if body.GenerationConfig.ThinkingConfig.ThinkingBudget != -1 {
		t.Fatalf("thinkingBudget = %d, want -1 (dynamic)", body.GenerationConfig.ThinkingConfig.ThinkingBudget)
	}
}
