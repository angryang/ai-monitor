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
	"time"

	"github.com/aibench/aibench/ai-monitor/internal/config"
)

// Chunk is a single streaming fragment reported by the upstream API.
// ArrivedAt is the wall-clock timestamp the running process observed it; the
// runner uses it to derive TTFT and ITL metrics.
type Chunk struct {
	Delta     string
	ArrivedAt time.Time
	Index     int
	IsFinal   bool
}

// ProbeStream is the read-side of an active probe.
type ProbeStream interface {
	// Chunks returns the channel of payloads. Adapter MUST close it after
	// Err is set or ctx is cancelled.
	Chunks() <-chan Chunk
	// Err surfaces the terminal error (nil on success).
	Err() error
	// Close releases the underlying HTTP connection. Idempotent.
	Close() error
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
