// Package openai implements the OpenAI-compatible streaming chat adapter.
// It implements the protocol.ChatProtocolAdapter contract and is registered
// at init-time via the package-level init() below.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/protocol"
)

// Adapter is the OpenAI chat adapter.
type Adapter struct {
	ua   string
	http *http.Client
	rid  func() string
}

// NewAdapter returns a new OpenAI adapter.
func NewAdapter(deps protocol.AdapterDeps) protocol.ChatProtocolAdapter {
	if deps.HTTPClient == nil {
		deps.HTTPClient = http.DefaultClient
	}
	ua := deps.UserAgent
	if ua == "" {
		ua = "ai-monitor/1.0 (+openai)"
	}
	return &Adapter{ua: ua, http: deps.HTTPClient, rid: deps.RequestIDGenerator}
}

// Name returns the protocol name.
func (a *Adapter) Name() string { return "openai" }

// Validate enforces OpenAI section invariants.
func (a *Adapter) Validate(cfg *config.TaskConfig) error {
	if cfg.Protocol != config.ProtocolOpenAI {
		return fmt.Errorf("openai: protocol mismatch: %q: %w", cfg.Protocol, protocol.ErrProtocolCfg)
	}
	if cfg.BaseURL == "" {
		return fmt.Errorf("openai: base_url required: %w", protocol.ErrProtocolCfg)
	}
	if cfg.APIKey == "" {
		return fmt.Errorf("openai: apikey required: %w", protocol.ErrProtocolCfg)
	}
	if cfg.OpenAI != nil && cfg.OpenAI.StreamPath != "" && !strings.HasPrefix(cfg.OpenAI.StreamPath, "/") {
		return fmt.Errorf("openai: stream_path must start with /: %w", protocol.ErrProtocolCfg)
	}
	return nil
}

// stream is the live read side of an OpenAI request.
type stream struct {
	ch     chan protocol.Chunk
	cancel context.CancelFunc
	mu     sync.Mutex
	err    error
	closed bool
	body   io.ReadCloser
	once   sync.Once
	// §18.13 — captured so probe_runs can record what was actually
	// sent and what bytes came back.
	requestID    string
	requestBody  []byte
	respCapture  *protocol.RespCapture
}

// RequestID returns the X-Req-Id value used on the outbound request.
func (s *stream) RequestID() string { return s.requestID }

// RequestBody returns the raw JSON body posted to upstream.
func (s *stream) RequestBody() []byte { return s.requestBody }

// ResponseSnapshot snapshots the bounded first/last bytes of the response
// body (HAR-style). The capture is populated asynchronously by the consume
// goroutine; callers should wait for stream.Chunks() to close before reading.
func (s *stream) ResponseSnapshot() (head, tail []byte, truncated bool) {
	if s.respCapture == nil {
		return nil, nil, false
	}
	return s.respCapture.Snapshot()
}

// Chunks returns the channel.
func (s *stream) Chunks() <-chan protocol.Chunk { return s.ch }

// Err returns the terminal error.
func (s *stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close is idempotent: cancels the context, closes the response body, and
// triggers the consume goroutine to exit via the once branch.
func (s *stream) Close() error {
	s.once.Do(func() {
		s.cancel()
		if s.body != nil {
			_ = s.body.Close()
		}
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
	})
	return nil
}

// closed reports whether Close() has been called.
func (s *stream) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Probe opens a streaming request and starts the goroutine that feeds chunks
// into the channel.
func (a *Adapter) Probe(ctx context.Context, cfg *config.TaskConfig) (protocol.ProbeStream, error) {
	path := "/v1/chat/completions"
	if cfg.OpenAI != nil && cfg.OpenAI.StreamPath != "" {
		path = cfg.OpenAI.StreamPath
	}
	streamPath, err := joinURL(cfg.BaseURL, path)
	if err != nil {
		return nil, fmt.Errorf("openai: %w: %w", err, protocol.ErrProtocolCfg)
	}

	body, err := buildRequest(cfg)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal request: %w: %w", err, protocol.ErrProtocolCfg)
	}
	// Clone for §18.13 persistence. The original `body` is consumed by
	// http.NewRequestWithContext; cloning keeps the JSON intact for
	// probe_runs. We also pre-truncate at capture time so the persisted
	// column matches the on-the-wire bytes modulo extra_headers.
	savedBody, reqTruncated := protocol.TruncateRequest(body, protocol.DefaultRequestCapture)

	httpCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(httpCtx, http.MethodPost, streamPath, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("openai: new request: %w: %w", err, protocol.ErrUpstreamDown)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("User-Agent", a.ua)
	req.Header.Set("Accept", "text/event-stream")
	// Auto-generated X-Req-Id (user value via extra_headers below wins).
	rid := protocol.GenerateRequestID(req, a.rid)
	if cfg.OpenAI != nil {
		if err := protocol.ApplyExtraHeaders(req, cfg.OpenAI.ExtraHeadersJSON); err != nil {
			cancel()
			return nil, fmt.Errorf("openai: %w: %w", err, protocol.ErrProtocolCfg)
		}
	}

	resp, err := a.http.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("openai: do: %w: %w", err, protocol.ErrUpstreamDown)
	}
	if err := protocol.ClassifyHTTPStatus(resp.StatusCode); err != nil {
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("openai: status %d: %w", resp.StatusCode, err)
	}

	cap := protocol.NewRespCapture(protocol.DefaultRespHeadCapture, protocol.DefaultRespTailCapture)
	tap := io.TeeReader(resp.Body, cap)
	s := &stream{
		ch:          make(chan protocol.Chunk, 32),
		cancel:      cancel,
		body:        resp.Body,
		requestID:   rid,
		requestBody: savedBody, // pre-truncated to capture cap
		respCapture: cap,
	}
	// reqTruncated is captured *between* bytes via stream.requestBody above;
	// the adapter doesn't need to forward it explicitly — the persisted
	// row can detect truncation simply by len(body) > cap when read.
	_ = reqTruncated
	go a.consume(s, tap, httpCtx)
	return s, nil
}

// consume reads the SSE stream until EOF, ctx cancellation, or error. The
// body is force-closed on context cancellation so blocked reader goroutines
// wake up promptly — vital for short test timeouts and graceful shutdown.
func (a *Adapter) consume(s *stream, body io.Reader, ctx context.Context) {
	defer close(s.ch)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	// Run scanner in its own goroutine so we can yank the body if ctx fires.
	type scanResult struct {
		chunk protocol.Chunk
		ok    bool
		err   error
	}
	scanOut := make(chan scanResult, 1)
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		idx := 0
		for scanner.Scan() {
			if ctx.Err() != nil {
				return
			}
			line := scanner.Text()
			if line == "" || !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				return
			}
			var chunk openAIChunk
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				scanOut <- scanResult{err: err}
				return
			}
			for _, choice := range chunk.Choices {
				if choice.Delta.Content == "" && choice.FinishReason == "" {
					continue
				}
				now := time.Now().UTC()
				final := choice.FinishReason != ""
				c := protocol.Chunk{Delta: choice.Delta.Content, ArrivedAt: now, Index: idx, IsFinal: final}
				idx++
				select {
				case scanOut <- scanResult{chunk: c, ok: true}:
				case <-ctx.Done():
					return
				}
				if final {
					return
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			// Force-close to unblock scanner.
			if rc, ok := body.(io.Closer); ok && !s.isClosed() {
				_ = rc.Close()
			}
			<-scanDone
			s.setErr(ctx.Err())
			return
		case res := <-scanOut:
			if !s.isClosed() {
				if res.err != nil {
					s.setErr(res.err)
					<-scanDone
					return
				}
				if res.ok {
					select {
					case s.ch <- res.chunk:
					case <-ctx.Done():
						s.setErr(ctx.Err())
						return
					}
				}
			}
		case <-scanDone:
			return
		}
	}
}

// setErr stores the first non-nil error.
func (s *stream) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

// openAIChunk is the subset of the OpenAI SSE payload we care about.
type openAIChunk struct {
	Choices []struct {
		Index        int `json:"index"`
		Delta        struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// chatRequest is the payload POSTed to the stream endpoint.
type chatRequest struct {
	Model    string        `json:"model"`
	Stream   bool          `json:"stream"`
	Messages []chatMessage `json:"messages"`
}

// chatMessage is one element of the messages array.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// joinURL joins base + path with a single '/' separator.
func joinURL(base, path string) (string, error) {
	if strings.HasSuffix(base, "/") && strings.HasPrefix(path, "/") {
		return base + strings.TrimPrefix(path, "/"), nil
	}
	if !strings.HasSuffix(base, "/") && !strings.HasPrefix(path, "/") {
		return base + "/" + path, nil
	}
	return base + path, nil
}

// buildRequest serialises the OpenAI chat payload, applying prompt-variable
// expansion for UserPrompt.
func buildRequest(cfg *config.TaskConfig) ([]byte, error) {
	userPrompt := expandVars(cfg.UserPrompt, cfg.PromptVars)
	sysPrompt := expandVars(cfg.SystemPrompt, cfg.PromptVars)
	msgs := []chatMessage{{Role: "user", Content: userPrompt}}
	if sysPrompt != "" {
		// OpenAI prefers system as the first message.
		msgs = append([]chatMessage{{Role: "system", Content: sysPrompt}}, msgs...)
	}
	payload := chatRequest{
		Model:    cfg.Model,
		Stream:   true,
		Messages: msgs,
	}
	return json.Marshal(payload)
}

// expandVars replaces each ${var} occurrence with the value from the
// supplied map. Unknown variables are left untouched.
func expandVars(s string, vars map[string]string) string {
	if len(vars) == 0 {
		return s
	}
	out := s
	for k, v := range vars {
		out = strings.ReplaceAll(out, "${"+k+"}", v)
	}
	return out
}

// init registers the adapter at package load time.
func init() {
	protocol.Register("openai", NewAdapter)
}
