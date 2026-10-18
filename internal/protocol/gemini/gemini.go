// Package gemini implements the Google Gemini streaming adapter.
// It supports the v1/streamGenerateContent endpoint, which returns a JSON
// array of incremental responses (terminated when finishReason is set).
package gemini

import (
	"bufio"
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

// Adapter is the Gemini adapter.
type Adapter struct {
	ua   string
	http *http.Client
	rid  func() string
}

// NewAdapter constructs a Gemini adapter.
func NewAdapter(deps protocol.AdapterDeps) protocol.ChatProtocolAdapter {
	if deps.HTTPClient == nil {
		deps.HTTPClient = http.DefaultClient
	}
	ua := deps.UserAgent
	if ua == "" {
		ua = "ai-monitor/1.0 (+gemini)"
	}
	return &Adapter{ua: ua, http: deps.HTTPClient, rid: deps.RequestIDGenerator}
}

// Name returns the protocol name.
func (a *Adapter) Name() string { return "gemini" }

// Validate enforces Gemini section invariants.
func (a *Adapter) Validate(cfg *config.TaskConfig) error {
	if cfg.Protocol != config.ProtocolGemini {
		return fmt.Errorf("gemini: protocol mismatch: %q: %w", cfg.Protocol, protocol.ErrProtocolCfg)
	}
	if cfg.BaseURL == "" {
		return fmt.Errorf("gemini: base_url required: %w", protocol.ErrProtocolCfg)
	}
	if cfg.APIKey == "" {
		return fmt.Errorf("gemini: apikey required: %w", protocol.ErrProtocolCfg)
	}
	if cfg.Gemini != nil && cfg.Gemini.Alt != "" && cfg.Gemini.Alt != "sse" && cfg.Gemini.Alt != "ndjson" {
		return fmt.Errorf("gemini: alt must be sse or ndjson: %w", protocol.ErrProtocolCfg)
	}
	return nil
}

// stream implements protocol.ProbeStream for Gemini.
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

// Chunks returns the channel.
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

// Probe POSTs to /v1beta/models/{model}:streamGenerateContent.
func (a *Adapter) Probe(ctx context.Context, cfg *config.TaskConfig) (protocol.ProbeStream, error) {
	streamPath, err := joinURL(cfg.BaseURL, "/v1beta/models/"+cfg.Model+":streamGenerateContent")
	if err != nil {
		return nil, fmt.Errorf("gemini: %w: %w", err, protocol.ErrProtocolCfg)
	}
	body, err := buildRequest(cfg)
	if err != nil {
		return nil, fmt.Errorf("gemini: marshal request: %w: %w", err, protocol.ErrProtocolCfg)
	}
	// §18.13 — clone for probe_runs persistence.
	savedBody, reqTruncated := protocol.TruncateRequest(body, protocol.DefaultRequestCapture)
	httpCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(httpCtx, http.MethodPost, streamPath, strings.NewReader(string(body)))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("gemini: new request: %w: %w", err, protocol.ErrUpstreamDown)
	}
	req.Header.Set("Content-Type", "application/json")
	q := req.URL.Query()
	key := cfg.APIKey
	q.Set("key", key)
	req.URL.RawQuery = q.Encode()
	req.Header.Set("User-Agent", a.ua)
	// Auto-generated X-Req-Id (user value via extra_headers below wins).
	rid := protocol.GenerateRequestID(req, a.rid)
	if cfg.Gemini != nil {
		if err := protocol.ApplyExtraHeaders(req, cfg.Gemini.ExtraHeadersJSON); err != nil {
			cancel()
			return nil, fmt.Errorf("gemini: %w: %w", err, protocol.ErrProtocolCfg)
		}
	}

	resp, err := a.http.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("gemini: do: %w: %w", err, protocol.ErrUpstreamDown)
	}
	if err := protocol.ClassifyHTTPStatus(resp.StatusCode); err != nil {
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("gemini: status %d: %w", resp.StatusCode, err)
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
	}
	_ = reqTruncated
	go a.consume(s, tap, httpCtx)
	return s, nil
}

// consume reads the JSON array stream frame by frame. Each frame is a JSON
// object; the response body opens with `[` and individual frames are
// separated by `},{` (or no separator if formatted).
func (a *Adapter) consume(s *stream, body io.Reader, ctx context.Context) {
	defer close(s.ch)
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		reader := bufio.NewReader(body)
		idx := 0
		var pending strings.Builder
		depth := 0
		closed := false
		for !closed {
			b, err := reader.ReadByte()
			if err != nil {
				// try to flush any pending partial frame
				if pending.Len() > 0 && depth == 0 {
					a.parseAndEmit(s, pending.String(), &idx, ctx)
				}
				return
			}
			if ctx.Err() != nil {
				return
			}
			if b == ']' && depth == 0 && pending.Len() == 0 {
				closed = true
				continue
			}
			if b == '{' {
				depth++
				pending.WriteByte(b)
				continue
			}
			if b == '}' {
				pending.WriteByte(b)
				depth--
				if depth == 0 {
					a.parseAndEmit(s, pending.String(), &idx, ctx)
					pending.Reset()
					// Comma between frames is dropped silently.
					_, _ = reader.Peek(1)
					if n, _ := reader.Peek(1); len(n) > 0 && n[0] == ',' {
						_, _ = reader.Discard(1)
					}
				}
				continue
			}
			// Inside an object: write everything (including commas/colons).
			if depth > 0 {
				pending.WriteByte(b)
			}
		}
	}()

	<-scanDone
	if ctx.Err() != nil {
		s.setErr(ctx.Err())
	}
}

// parseAndEmit decodes one Gemini JSON frame and pushes the candidate text
// as a chunk. The finalize flag is true when the frame contains a
// finishReason (terminal frame per the Gemini API).
func (a *Adapter) parseAndEmit(s *stream, raw string, idx *int, ctx context.Context) {
	var frame geminiFrame
	if err := json.Unmarshal([]byte(raw), &frame); err != nil {
		// Some frames may be empty or whitespace; ignore them silently.
		return
	}
	for _, cand := range frame.Candidates {
		for _, part := range cand.Content.Parts {
			if part.Text == "" {
				continue
			}
			final := cand.FinishReason != ""
			c := protocol.Chunk{
				Delta:     part.Text,
				ArrivedAt: time.Now().UTC(),
				Index:     *idx,
				IsFinal:   final,
			}
			*idx++
			select {
			case s.ch <- c:
			case <-ctx.Done():
				return
			}
			if final {
				return
			}
		}
	}
	if len(frame.Candidates) == 0 {
		// try old-style {text: ...} response
		var alt struct {
			Text string `json:"text"`
		}
		if jerr := json.Unmarshal([]byte(raw), &alt); jerr == nil && alt.Text != "" {
			c := protocol.Chunk{Delta: alt.Text, ArrivedAt: time.Now().UTC(), Index: *idx}
			*idx++
			select {
			case s.ch <- c:
			case <-ctx.Done():
				return
			}
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

// geminiFrame mirrors the relevant subset of the Gemini response payload.
type geminiFrame struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
}

// reqBody is the request payload.
type reqBody struct {
	Contents []reqContent `json:"contents"`
}

// reqContent represents one turn in the conversation.
type reqContent struct {
	Parts []reqPart `json:"parts"`
	Role  string    `json:"role"`
}

// reqPart is one element of content parts.
type reqPart struct {
	Text string `json:"text"`
}

// buildRequest builds the payload.
func buildRequest(cfg *config.TaskConfig) ([]byte, error) {
	userPrompt := expandVars(cfg.UserPrompt, cfg.PromptVars)
	sysPrompt := expandVars(cfg.SystemPrompt, cfg.PromptVars)
	parts := []reqPart{{Text: userPrompt}}
	if sysPrompt != "" {
		parts = append([]reqPart{{Text: sysPrompt}}, parts...)
	}
	body := reqBody{
		Contents: []reqContent{{Role: "user", Parts: parts}},
	}
	return json.Marshal(body)
}

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

// init registers the adapter at package load time.
func init() {
	protocol.Register("gemini", NewAdapter)
}
