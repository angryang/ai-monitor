// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/logging"
	"github.com/angryang/ai-monitor/internal/protocol"
)

// DefaultCheckTimeout bounds one connectivity check. The check only waits
// for the upstream's response headers, so this is generous: a healthy
// endpoint answers in well under a second, and anything slower than this
// is a problem worth reporting.
const DefaultCheckTimeout = 10 * time.Second

// checkPrompt is the prompt sent by the connectivity check. It is a
// placeholder: the check aborts as soon as the upstream accepts the
// request, so the answer is never read.
const checkPrompt = "ping"

// maxMessageLen bounds the human-readable message kept with a check
// result. Upstream error bodies can be arbitrarily long; the admin UI
// only needs the first sentence or two.
const maxMessageLen = 1000

// CheckKind classifies the outcome of a connectivity check. The kinds map
// onto the distinct operator actions: fix the key, fix the URL, fix the
// model, or wait for the upstream to come back.
type CheckKind string

const (
	// CheckOK means the upstream accepted the request.
	CheckOK CheckKind = "ok"
	// CheckConfig means the provider description itself is incomplete or
	// inconsistent — nothing was sent to the network.
	CheckConfig CheckKind = "config_error"
	// CheckUnauthorized means the upstream rejected the credentials
	// (401 / 403).
	CheckUnauthorized CheckKind = "unauthorized"
	// CheckInvalidRequest means the upstream rejected the request shape
	// (404 on the path or the model, 400 on the payload).
	CheckInvalidRequest CheckKind = "invalid_request"
	// CheckUpstreamDown means the upstream answered with a 5xx.
	CheckUpstreamDown CheckKind = "upstream_unavailable"
	// CheckTimeout means the upstream did not answer within the
	// check's deadline.
	CheckTimeout CheckKind = "timeout"
	// CheckNetwork means the request never reached the upstream — DNS
	// failure, refused connection, TLS handshake failure.
	CheckNetwork CheckKind = "network_error"
	// CheckUnknown is the fallback for an error we cannot classify.
	CheckUnknown CheckKind = "unknown"
)

// CheckResult is the outcome of one connectivity check.
type CheckResult struct {
	// OK is true only for CheckOK.
	OK bool `json:"ok"`
	// Kind classifies the outcome.
	Kind CheckKind `json:"kind"`
	// Protocol / Model / URL describe what was checked.
	Protocol string `json:"protocol"`
	Model    string `json:"model"`
	URL      string `json:"url"`
	// LatencyMS is the round-trip time of the check.
	LatencyMS int64 `json:"latency_ms"`
	// StatusCode is the upstream's HTTP status code. 0 means no response
	// was received at all.
	StatusCode int `json:"status_code,omitempty"`
	// Message explains the outcome. On failure it carries the upstream's
	// own error message when there was one.
	Message string `json:"message"`
	// CheckedAt is when the check ran.
	CheckedAt time.Time `json:"checked_at"`
}

// AdapterLookup resolves a protocol name to its adapter. It mirrors
// probe.AdapterLookup so both can be wired to the same registry.
type AdapterLookup func(name string) (protocol.ChatProtocolAdapter, error)

// CheckerOptions carries the dependencies of a Checker.
type CheckerOptions struct {
	// Adapters resolves a protocol name to its adapter. Required.
	Adapters AdapterLookup
	// Timeout bounds one check. Defaults to DefaultCheckTimeout.
	Timeout time.Duration
	// Logger receives one line per failed check. Optional.
	Logger *slog.Logger
}

// Checker verifies that a provider is reachable and usable.
//
// It deliberately reuses the protocol adapters instead of hand-rolling a
// second set of URLs and headers: the question "is this provider
// reachable?" must have the same answer as "will a monitoring task
// pointed at this provider succeed?", and the only way to guarantee that
// is to send the very same request.
type Checker struct {
	adapters AdapterLookup
	timeout  time.Duration
	logger   *slog.Logger
}

// NewChecker constructs a Checker, applying defaults for the optional
// fields.
func NewChecker(opts CheckerOptions) *Checker {
	c := &Checker{
		adapters: opts.Adapters,
		timeout:  opts.Timeout,
		logger:   opts.Logger,
	}
	if c.timeout <= 0 {
		c.timeout = DefaultCheckTimeout
	}
	if c.logger == nil {
		c.logger = logging.L()
	}
	return c
}

// Check runs a connectivity check against the provider's primary model.
// It is the convenient single-argument entry point; callers that need to
// test one particular model use CheckModel.
func (c *Checker) Check(ctx context.Context, p *Provider) CheckResult {
	var model string
	if p != nil {
		model = p.PrimaryModel()
	}
	return c.CheckModel(ctx, p, model)
}

// CheckModel verifies that the upstream described by p accepts a request
// for one specific model.
//
// The check sends the provider's own request — protocol, url, credentials,
// the chosen model and protocol-specific options — and stops at the
// response headers. A successful check therefore proves that the host
// resolves, the TLS certificate is acceptable, the credentials are
// accepted and that particular model exists. It does not prove that the
// model answers well; that is what the monitoring probe is for.
//
// Saving a provider no longer runs this check automatically: with several
// models per provider, probing all of them on every edit would be slow and
// would turn one bad model into a blocker for the whole save. The operator
// tests the models they care about, on demand.
//
// Check never returns an error: a failed check is a normal outcome that
// the caller has to render, not an exceptional one.
func (c *Checker) CheckModel(ctx context.Context, p *Provider, model string) CheckResult {
	started := time.Now()
	res := CheckResult{CheckedAt: started.UTC()}
	if p != nil {
		res.Protocol = string(p.Protocol)
		res.Model = model
		res.URL = endpointURL(p, model)
	}
	finish := func(kind CheckKind, status int, msg string) CheckResult {
		res.Kind = kind
		res.StatusCode = status
		res.Message = truncate(msg, maxMessageLen)
		res.OK = kind == CheckOK
		res.LatencyMS = time.Since(started).Milliseconds()
		return res
	}

	if p == nil {
		return finish(CheckConfig, 0, "provider is missing")
	}
	if !p.Protocol.IsValid() {
		return finish(CheckConfig, 0, fmt.Sprintf("protocol %q is not supported", p.Protocol))
	}
	if strings.TrimSpace(p.BaseURL) == "" {
		return finish(CheckConfig, 0, "base_url is empty")
	}
	if strings.TrimSpace(model) == "" {
		return finish(CheckConfig, 0, "model is empty")
	}
	if len(p.Models) > 0 && !p.HasModel(model) {
		return finish(CheckConfig, 0, fmt.Sprintf("model %q is not offered by this provider", model))
	}
	if p.APIKey == "" {
		return finish(CheckConfig, 0, "api_key is empty")
	}
	if c.adapters == nil {
		return finish(CheckConfig, 0, "no protocol adapters are registered")
	}
	adapter, err := c.adapters(string(p.Protocol))
	if err != nil {
		return finish(CheckConfig, 0, fmt.Sprintf("no adapter for protocol %q: %v", p.Protocol, err))
	}
	tc := taskConfigFor(p, model)
	if err := adapter.Validate(&tc); err != nil {
		return finish(CheckConfig, 0, err.Error())
	}

	// The check honours both its own deadline and the provider's
	// configured one — a provider that only tolerates 5s per request
	// must not be probed with a 30s deadline.
	deadline := c.timeout
	if own := p.EffectiveTimeout(); own > 0 && own < deadline {
		deadline = own
	}
	if deadline < time.Second {
		deadline = time.Second
	}
	checkCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	stream, err := adapter.Probe(checkCtx, &tc)
	if err != nil {
		result := c.classify(res, err)
		c.logger.Warn("provider_check_failed",
			slog.String("provider", p.Name),
			slog.String("protocol", res.Protocol),
			slog.String("kind", string(result.Kind)),
			slog.Int("status", result.StatusCode),
			slog.String("err", result.Message),
		)
		return result
	}
	// The upstream answered. Abort the generation right away: we only
	// needed to know that the request was accepted.
	_ = stream.Close()
	return finish(CheckOK, 0, "upstream accepted the request")
}

// classify turns an adapter error into a check result.
func (c *Checker) classify(res CheckResult, err error) CheckResult {
	kind := CheckUnknown
	status := 0
	msg := err.Error()

	var reqErr *protocol.RequestError
	if errors.As(err, &reqErr) {
		status = reqErr.StatusCode
		if reqErr.ResponseBody != "" {
			// The upstream's own explanation is the most actionable
			// thing we have: "Incorrect API key provided: sk-***".
			msg = reqErr.ResponseBody
		}
		if reqErr.RequestID != "" {
			msg = fmt.Sprintf("%s (x-req-id: %s)", msg, reqErr.RequestID)
		}
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		kind = CheckTimeout
	case errors.Is(err, protocol.ErrUnauthorized):
		kind = CheckUnauthorized
	case errors.Is(err, protocol.ErrProtocolCfg):
		kind = CheckInvalidRequest
	case errors.Is(err, protocol.ErrUpstreamDown):
		// The adapter maps both "the server answered 5xx" and "the
		// request never left the host" onto ErrUpstreamDown. The status
		// code tells them apart: 0 means we never got a response.
		if status >= 500 {
			kind = CheckUpstreamDown
		} else {
			kind = CheckNetwork
		}
	}

	res.Kind = kind
	res.StatusCode = status
	res.Message = truncate(msg, maxMessageLen)
	res.OK = kind == CheckOK
	return res
}

// taskConfigFor assembles the probe configuration used by the check.
//
// Only the protocol block matching the provider's protocol is carried
// over — the adapters read their own block and ignore the others, and
// config.TaskConfig.ValidateProtocolSegments treats a config with several
// blocks as a misconfiguration.
func taskConfigFor(p *Provider, model string) config.TaskConfig {
	tc := config.TaskConfig{
		// The synthetic id never reaches the scheduler: the check
		// never goes through it, and no run history is written.
		ID:                 "provider-check",
		Name:               p.Name,
		Enabled:            true,
		Protocol:           p.Protocol,
		BaseURL:            p.BaseURL,
		APIKey:             p.APIKey,
		Model:              model,
		UserPrompt:         checkPrompt,
		Timeout:            p.EffectiveTimeout(),
		TimeoutMS:          p.TimeoutMS,
		CanaryGroup:        "provider",
		InsecureSkipVerify: p.InsecureSkipVerify,
		ThinkingEnabled:    p.ThinkingEnabled,
	}
	switch p.Protocol {
	case config.ProtocolOpenAI:
		if p.OpenAI != nil {
			tc.OpenAI = p.OpenAI
		}
	case config.ProtocolAnthropic:
		tc.Anthropic = p.Anthropic
		if tc.Anthropic == nil {
			tc.Anthropic = &config.AnthropicConfig{MaxTokens: 1024}
		}
	case config.ProtocolGemini:
		tc.Gemini = p.Gemini
		if tc.Gemini == nil {
			tc.Gemini = &config.GeminiConfig{Alt: "sse"}
		}
	}
	return tc
}

// endpointURL mirrors the URL the adapter will contact, so the admin UI
// can show the operator exactly which endpoint was verified. The
// authoritative request is the adapter's — this is a display helper.
func endpointURL(p *Provider, model string) string {
	if p == nil {
		return ""
	}
	base := strings.TrimSuffix(p.BaseURL, "/")
	switch p.Protocol {
	case config.ProtocolOpenAI:
		path := "/v1/chat/completions"
		if p.OpenAI != nil && p.OpenAI.StreamPath != "" {
			path = p.OpenAI.StreamPath
		}
		return base + "/" + strings.TrimPrefix(path, "/")
	case config.ProtocolAnthropic:
		return base + "/v1/messages"
	case config.ProtocolGemini:
		return base + "/v1beta/models/" + model + ":streamGenerateContent"
	}
	return p.BaseURL
}

// truncate bounds a string to max runes, appending an ellipsis marker
// when it had to cut.
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
