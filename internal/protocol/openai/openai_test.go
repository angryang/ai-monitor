package openai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aibench/aibench/ai-monitor/internal/config"
	"github.com/aibench/aibench/ai-monitor/internal/protocol"
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
		ID:       "t-1",
		Name:     "openai-test",
		Protocol: config.ProtocolOpenAI,
		BaseURL:  "http://127.0.0.1:0",
		APIKey:   "sk-test",
		Model:    "gpt-4o",
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
