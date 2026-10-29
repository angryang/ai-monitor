// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

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

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/protocol"
)

// Adapter is the Anthropic adapter.
type Adapter struct {
	ua   string
	http *http.Client
	rid  func() string
	// insecureSkipVerify mirrors deps.InsecureSkipVerify — the instance-wide
	// "don't verify upstream certs" switch.
	insecureSkipVerify bool
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
	return &Adapter{
		ua:                 ua,
		http:               deps.HTTPClient,
		rid:                deps.RequestIDGenerator,
		insecureSkipVerify: deps.InsecureSkipVerify,
	}
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
	// §18.13 — captured for probe_runs persistence.
	requestID   string
	requestBody []byte
	respCapture *protocol.RespCapture
	// §18.14 — reasoning content accumulated while consuming the stream.
	thinking *protocol.TextAccumulator
}

// RequestID returns the X-Req-Id used on the outbound request.
func (s *stream) RequestID() string { return s.requestID }

// RequestBody returns the raw JSON body posted to upstream.
func (s *stream) RequestBody() []byte { return s.requestBody }

// ResponseSnapshot returns the bounded first/last bytes of the response body.
func (s *stream) ResponseSnapshot() (head, tail []byte, truncated bool) {
	if s.respCapture == nil {
		return nil, nil, false
	}
	return s.respCapture.Snapshot()
}

// ContentCapture returns the reasoning content seen so far (§18.14).
func (s *stream) ContentCapture() protocol.ContentCapture {
	return protocol.ContentCapture{
		ThinkingContent: s.thinking.String(),
		Truncated:       s.thinking.Truncated(),
	}
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
	// §18.13 — clone for probe_runs persistence.
	savedBody, reqTruncated := protocol.TruncateRequest(body, protocol.DefaultRequestCapture)
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
	// Auto-generated X-Req-Id (user value via extra_headers below wins).
	rid := protocol.GenerateRequestID(req, a.rid)
	if cfg.Anthropic != nil {
		if err := protocol.ApplyExtraHeaders(req, cfg.Anthropic.ExtraHeadersJSON); err != nil {
			cancel()
			return nil, fmt.Errorf("anthropic: %w: %w", err, protocol.ErrProtocolCfg)
		}
	}

	resp, err := protocol.ClientFor(a.http, a.insecureSkipVerify, cfg).Do(req)
	if err != nil {
		cancel()
		// §18.14 — keep the X-Req-Id attached to the failure so admins can
		// grep the upstream logs for this exact attempt.
		return nil, protocol.NewRequestError(rid, savedBody, "",
			fmt.Errorf("anthropic: do: %w: %w", err, protocol.ErrUpstreamDown))
	}
	if cerr := protocol.ClassifyHTTPStatus(resp.StatusCode); cerr != nil {
		// §18.14 — the 4xx/5xx body usually says exactly what went wrong.
		errBody := protocol.ReadErrorBody(resp.Body, protocol.DefaultErrorBodyCapture)
		_ = resp.Body.Close()
		cancel()
		return nil, protocol.NewRequestError(rid, savedBody, errBody,
			fmt.Errorf("anthropic: status %d: %w", resp.StatusCode, cerr))
	}
	cap := protocol.NewRespCapture(protocol.DefaultRespHeadCapture, protocol.DefaultRespTailCapture)
	tap := io.TeeReader(resp.Body, cap)
	s := &stream{
		ch:          make(chan protocol.Chunk, 32),
		cancel:      cancel,
		body:        resp.Body,
		requestID:   rid,
		requestBody: savedBody,
		respCapture: cap,
		thinking:    protocol.NewTextAccumulator(protocol.DefaultContentCapture),
	}
	_ = reqTruncated
	go a.consume(s, tap, httpCtx)
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
					// §18.14 — extended-thinking blocks arrive as
					// "thinking_delta" with the text in "thinking".
					Thinking string `json:"thinking"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(payload), &deltaEvent); err != nil {
				s.setErr(fmt.Errorf("anthropic: parse delta: %w", err))
				return
			}
			// §18.14 — route reasoning deltas to their own accumulator.
			if deltaEvent.Delta.Thinking != "" {
				s.thinking.WriteString(deltaEvent.Delta.Thinking)
				continue
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
//
// Thinking — present only when the task's thinking switch is on. Anthropic
// requires budget_tokens >= 1024 and max_tokens > budget_tokens, so
// buildRequest derives a budget from the configured max_tokens and bumps
// max_tokens when the two would collide.
type reqMessage struct {
	Model     string       `json:"model"`
	Messages  []msgBlk     `json:"messages"`
	MaxTokens int          `json:"max_tokens"`
	Stream    bool         `json:"stream"`
	System    string       `json:"system,omitempty"`
	Thinking  *thinkingCfg `json:"thinking,omitempty"`
}

// thinkingCfg is the extended-thinking request block.
type thinkingCfg struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

// msgBlk is one entry of the messages array.
type msgBlk struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// buildRequest builds the payload, expanding prompt variables.
func buildRequest(cfg *config.TaskConfig) ([]byte, error) {
	userPrompt := protocol.ExpandVars(cfg.UserPrompt, cfg.PromptVars)
	sysPrompt := protocol.ExpandVars(cfg.SystemPrompt, cfg.PromptVars)
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
	if cfg.ThinkingEnabled {
		// budget_tokens floor is 1024 and must stay below max_tokens.
		// Generous budgets only make sense when max_tokens can afford one.
		budget := 1024
		if maxTokens >= 2048 {
			budget = maxTokens / 2
		}
		if maxTokens <= budget {
			maxTokens = budget + 1024
		}
		body.MaxTokens = maxTokens
		body.Thinking = &thinkingCfg{Type: "enabled", BudgetTokens: budget}
	}
	return json.Marshal(body)
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
