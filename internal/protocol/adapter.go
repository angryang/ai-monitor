// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

// Package protocol defines the chat-protocol adapter contract that
// OpenAI/Anthropic/Gemini implement. The actual implementations live in
// subdirectories and register themselves at init() time.
//
// All adapters share the same shape:
//   - Name()  : unique protocol id ("openai"/"anthropic"/"gemini")
//   - Validate(&config.TaskConfig) error : per-protocol schema check
//   - Probe(ctx, &config.TaskConfig) (ProbeStream, error)
//     Streams chunks; the runner closes it.
package protocol

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/google/uuid"
)

// Chunk is a single streaming fragment reported by the upstream API.
// ArrivedAt is the wall-clock timestamp the running process observed it; the
// runner uses it to derive TTFT and ITL metrics.
type Chunk struct {
	Delta     string
	ArrivedAt time.Time
	Index     int
	IsFinal   bool
	// Kind distinguishes the model's reasoning trace from the answer text.
	// §18.14 — adapters emit reasoning as ChunkThinking so the runner can
	// persist it separately. The zero value is ChunkContent, which keeps
	// every pre-existing producer (and test fixture) correct.
	Kind ChunkKind
}

// ChunkKind classifies what a streamed fragment carries.
type ChunkKind uint8

const (
	// ChunkContent is the answer text destined for the user.
	ChunkContent ChunkKind = iota
	// ChunkThinking is the model's intermediate reasoning ("thinking",
	// "reasoning_content", Gemini thought parts). It is persisted for
	// inspection but is NOT part of the user-facing response stream, so it
	// does not count toward chunk / TTFT / ITL metrics.
	ChunkThinking
)

// ProbeStream is the read-side of an active probe.
type ProbeStream interface {
	// Chunks returns the channel of payloads. Adapter MUST close it after
	// Err is set or ctx is cancelled.
	Chunks() <-chan Chunk
	// Err surfaces the terminal error (nil on success).
	Err() error
	// Close releases the underlying HTTP connection. Idempotent.
	Close() error
	// RequestID returns the X-Req-Id value used on the outbound HTTP
	// request. Empty when the protocol never set the header (shouldn't
	// happen for OpenAI / Anthropic / Gemini, all of which auto-inject).
	RequestID() string
	// RequestBody returns the raw JSON body posted to the upstream API.
	// May be nil for adapters that build requests lazily; ai-monitor
	// never logs the body to places where nil could leak — handlers must
	// guard with len() before stringification.
	RequestBody() []byte
	// ResponseSnapshot returns the bounded first N + last M bytes of the
	// upstream response body, plus a flag indicating whether any bytes in
	// the middle were dropped. Adapters use RespCapture under the hood;
	// see respcap.go.
	ResponseSnapshot() (head []byte, tail []byte, truncated bool)
	// ContentCapture returns the semantic content observed while the stream
	// was consumed (§18.14) — today the model's reasoning trace.
	//
	// It must be read *after* Chunks() is closed; the adapter fills it in
	// as frames arrive, because the reasoning text cannot be recovered
	// from the bounded head/tail byte capture afterwards.
	ContentCapture() ContentCapture
}

// ChatProtocolAdapter is implemented by every supported protocol.
type ChatProtocolAdapter interface {
	Name() string
	Validate(cfg *config.TaskConfig) error
	Probe(ctx context.Context, cfg *config.TaskConfig) (ProbeStream, error)
}

// Sentinel errors usable with errors.Is to disambiguate outcomes.
var (
	ErrUnauthorized = errors.New("protocol: unauthorized")          // 401 / 403 → immediate alert
	ErrProtocolCfg  = errors.New("protocol: config invalid")        // schema-level validation
	ErrUpstreamDown = errors.New("protocol: upstream unavailable")  // network / DNS / timeout
)

// ClassifyHTTPStatus converts HTTP status codes to the matching sentinel.
// 401 / 403 → ErrUnauthorized (immediate alert), 5xx → ErrUpstreamDown,
// 4xx (other) → ErrProtocolCfg (likely a config/credential issue).
func ClassifyHTTPStatus(code int) error {
	switch {
	case code == 401 || code == 403:
		return ErrUnauthorized
	case code >= 500:
		return ErrUpstreamDown
	case code >= 400:
		return ErrProtocolCfg
	}
	return nil
}

// RequestIDHeader is the canonical HTTP header name ai-monitor uses to
// identify each outbound probe. The value is a string token — by default
// uuid v4 — that the AI server can echo into its logs so failures map 1:1
// back to ai-monitor's records.
//
// If the user has explicitly set X-Req-Id through OpenAI/Anthropic/Gemini
// ExtraHeadersJSON, that value wins; we only synthesise a default when the
// user hasn't picked one. ApplyExtraHeaders runs *after* this so its Set()
// call overrides our auto-generated value.
const RequestIDHeader = "X-Req-Id"

// GenerateRequestID ensures req carries RequestIDHeader. The optional
// generator lets tests inject deterministic ids; production callers pass nil
// to fall back to uuid.NewString.
//
// The returned string is the value that ended up in req.Header so adapters
// can include it in their slog lines for client-side correlation.
//
// Notes:
//   - Caller must invoke this BEFORE ApplyExtraHeaders so user-supplied
//     values still win.
//   - GenerateRequestID never returns empty; if the generator is broken
//     and returns "", it falls back to uuid.NewString.
func GenerateRequestID(req *http.Request, generator func() string) string {
	if existing := req.Header.Get(RequestIDHeader); existing != "" {
		return existing
	}
	rid := ""
	if generator != nil {
		rid = generator()
	}
	if rid == "" {
		rid = uuid.NewString()
	}
	req.Header.Set(RequestIDHeader, rid)
	return rid
}
