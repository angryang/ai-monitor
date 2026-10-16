// Package anthropic implements the Anthropic Messages streaming adapter.
// It supports the standard event-stream format documented at
// https://docs.anthropic.com/en/api/messages-streaming.
package anthropic

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

	"github.com/aibench/aibench/ai-monitor/internal/config"
	"github.com/aibench/aibench/ai-monitor/internal/protocol"
)

// Adapter is the Anthropic adapter.
type Adapter struct {
	ua   string
	http *http.Client
}

// NewAdapter constructs an Anthropic adapter.
func NewAdapter(deps protocol.AdapterDeps) protocol.ChatProtocolAdapter {
	if deps.HTTPClient == nil {
		deps.HTTPClient = http.DefaultClient
	}
	ua := deps.UserAgent
	if ua == "" {
		ua = "ai-monitor/1.0 (+anthropic)"
	}
	return &Adapter{ua: ua, http: deps.HTTPClient}
}

// Name returns the protocol name.
func (a *Adapter) Name() string { return "anthropic" }

// Validate enforces Anthropic section invariants.
func (a *Adapter) Validate(cfg *config.TaskConfig) error {
	if cfg.Protocol != config.ProtocolAnthropic {
		return fmt.Errorf("anthropic: protocol mismatch: %q: %w", cfg.Protocol, protocol.ErrProtocolCfg)
	}
	if cfg.BaseURL == "" {
		return fmt.Errorf("anthropic: base_url required: %w", protocol.ErrProtocolCfg)
	}
	if cfg.APIKey == "" {
		return fmt.Errorf("anthropic: apikey required: %w", protocol.ErrProtocolCfg)
	}
	if cfg.Anthropic == nil {
		return fmt.Errorf("anthropic: missing anthropic config block: %w", protocol.ErrProtocolCfg)
	}
	if cfg.Anthropic.MaxTokens <= 0 || cfg.Anthropic.MaxTokens > 200000 {
		return fmt.Errorf("anthropic: max_tokens out of range: %w", protocol.ErrProtocolCfg)
	}
	return nil
}

// stream implements protocol.ProbeStream.
type stream struct {
	ch     chan protocol.Chunk
	cancel context.CancelFunc
	mu     sync.Mutex
	err    error
	closed bool
	body   io.ReadCloser
	once   sync.Once
}

// Chunks returns the read channel.
func (s *stream) Chunks() <-chan protocol.Chunk { return s.ch }

// Err returns the terminal error.
func (s *stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close is idempotent.
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

func (s *stream) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Probe POSTs to /v1/messages and starts consuming the SSE event stream.
func (a *Adapter) Probe(ctx context.Context, cfg *config.TaskConfig) (protocol.ProbeStream, error) {
	body, err := buildRequest(cfg)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w: %w", err, protocol.ErrProtocolCfg)
	}
	streamPath, err := joinURL(cfg.BaseURL, "/v1/messages")
	if err != nil {
		return nil, fmt.Errorf("anthropic: %w: %w", err, protocol.ErrProtocolCfg)
	}
	httpCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(httpCtx, http.MethodPost, streamPath, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("anthropic: new request: %w: %w", err, protocol.ErrUpstreamDown)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", cfg.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("User-Agent", a.ua)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := a.http.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("anthropic: do: %w: %w", err, protocol.ErrUpstreamDown)
	}
	if err := protocol.ClassifyHTTPStatus(resp.StatusCode); err != nil {
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("anthropic: status %d: %w", resp.StatusCode, err)
	}
	s := &stream{
		ch:     make(chan protocol.Chunk, 32),
		cancel: cancel,
		body:   resp.Body,
	}
	go a.consume(s, resp.Body, httpCtx)
	return s, nil
}

// consume reads SSE frames and turns content_block_delta frames into chunks.
// message_stop terminates the stream.
func (a *Adapter) consume(s *stream, body io.Reader, ctx context.Context) {
	defer close(s.ch)
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var event string
		idx := 0
		for scanner.Scan() {
			if ctx.Err() != nil {
				return
			}
			line := scanner.Text()
			if strings.TrimSpace(line) == "" {
				event = ""
				continue
			}
			if strings.HasPrefix(line, "event:") {
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				continue
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if event != "content_block_delta" {
				if event == "message_stop" {
					return
				}
				continue
			}
			var deltaEvent struct {
				Index int `json:"index"`
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(payload), &deltaEvent); err != nil {
				s.setErr(fmt.Errorf("anthropic: parse delta: %w", err))
				return
			}
			if deltaEvent.Delta.Text == "" {
				continue
			}
			c := protocol.Chunk{
				Delta:     deltaEvent.Delta.Text,
				ArrivedAt: time.Now().UTC(),
				Index:     idx,
				IsFinal:   event == "message_stop",
			}
			idx++
			select {
			case s.ch <- c:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			s.setErr(err)
		}
	}()

	<-scanDone
	if ctx.Err() != nil {
		s.setErr(ctx.Err())
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

// reqMessage is the Anthropic messages POST body.
type reqMessage struct {
	Model     string   `json:"model"`
	Messages  []msgBlk `json:"messages"`
	MaxTokens int      `json:"max_tokens"`
	Stream    bool     `json:"stream"`
	System    string   `json:"system,omitempty"`
}

// msgBlk is one entry of the messages array.
type msgBlk struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// buildRequest builds the payload, expanding prompt variables.
func buildRequest(cfg *config.TaskConfig) ([]byte, error) {
	userPrompt := expandVars(cfg.UserPrompt, cfg.PromptVars)
	sysPrompt := expandVars(cfg.SystemPrompt, cfg.PromptVars)
	maxTokens := 1024
	if cfg.Anthropic != nil && cfg.Anthropic.MaxTokens > 0 {
		maxTokens = cfg.Anthropic.MaxTokens
	}
	body := reqMessage{
		Model:     cfg.Model,
		MaxTokens: maxTokens,
		Stream:    true,
		System:    sysPrompt,
		Messages:  []msgBlk{{Role: "user", Content: userPrompt}},
	}
	return json.Marshal(body)
}

// expandVars replaces ${var} tokens with values from the supplied map.
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

func joinURL(base, path string) (string, error) {
	if strings.HasSuffix(base, "/") && strings.HasPrefix(path, "/") {
		return base + strings.TrimPrefix(path, "/"), nil
	}
	if !strings.HasSuffix(base, "/") && !strings.HasPrefix(path, "/") {
		return base + "/" + path, nil
	}
	return base + path, nil
}

// init registers the adapter.
func init() {
	protocol.Register("anthropic", NewAdapter)
}
