// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/metrics"
	"github.com/angryang/ai-monitor/internal/notifier"
	"github.com/angryang/ai-monitor/internal/protocol"
)

type stubAdapter struct {
	name    string
	chunks  []protocol.Chunk
	final   protocol.Chunk
	err     error
	called  atomic.Int32
	cfgHook func(*config.TaskConfig)
}

func (s *stubAdapter) Name() string { return s.name }
func (s *stubAdapter) Validate(_ *config.TaskConfig) error {
	return nil
}
func (s *stubAdapter) Probe(_ context.Context, c *config.TaskConfig) (protocol.ProbeStream, error) {
	s.called.Add(1)
	if s.cfgHook != nil {
		s.cfgHook(c)
	}
	if s.err != nil {
		return nil, s.err
	}
	ch := make(chan protocol.Chunk, len(s.chunks)+1)
	for _, c := range s.chunks {
		if c.IsFinal {
			close(ch)
			return &memStream{ch: ch}, nil
		}
		ch <- c
	}
	ch <- s.final
	close(ch)
	return &memStream{ch: ch}, nil
}

type memStream struct {
	ch        chan protocol.Chunk
	requestID string
	reqBody   []byte
	respHead  []byte
	respTail  []byte
	truncated bool
	// §18.14 — semantic content the stub pretends to have captured.
	capture protocol.ContentCapture
}

func (m *memStream) Chunks() <-chan protocol.Chunk { return m.ch }
func (m *memStream) Err() error                    { return nil }
func (m *memStream) Close() error                  { return nil }
func (m *memStream) RequestID() string             { return m.requestID }
func (m *memStream) RequestBody() []byte           { return m.reqBody }
func (m *memStream) ResponseSnapshot() (head, tail []byte, truncated bool) {
	return m.respHead, m.respTail, m.truncated
}
func (m *memStream) ContentCapture() protocol.ContentCapture { return m.capture }

type capturingNotifier struct {
	mu  sync.Mutex
	got []notifier.AlertEvent
}

func (c *capturingNotifier) Name() string { return "test" }
func (c *capturingNotifier) Send(_ context.Context, evt notifier.AlertEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, evt)
	return nil
}
func (c *capturingNotifier) Snap() []notifier.AlertEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]notifier.AlertEvent, len(c.got))
	copy(out, c.got)
	return out
}

// captureAdapter — stub that injects known capture fields via a custom
// memStream and verifies the runner plumbs them into the Result.
type captureAdapter struct {
	stubAdapter
	memOverride *memStream
}

func (c *captureAdapter) Probe(_ context.Context, cfg *config.TaskConfig) (protocol.ProbeStream, error) {
	c.called.Add(1)
	if c.cfgHook != nil {
		c.cfgHook(cfg)
	}
	if c.err != nil {
		return nil, c.err
	}
	// Mirror stubAdapter's chunk plumbing so tests can supply both a
	// chunk list and a captured-stream override.
	ch := make(chan protocol.Chunk, len(c.chunks)+1)
	for _, chunk := range c.chunks {
		if chunk.IsFinal {
			close(ch)
			m := *c.memOverride
			m.ch = ch
			return &m, nil
		}
		ch <- chunk
	}
	ch <- c.final
	close(ch)
	m := *c.memOverride // copy so each probe gets fresh struct
	m.ch = ch
	return &m, nil
}

func TestRunner_ProbeCapturesWireBytes(t *testing.T) {
	sink := metrics.NewPromSink(metrics.PrometheusOptions{})
	captured := &memStream{
		requestID: "rid-test-001",
		reqBody:   []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`),
		respHead:  []byte(`data: {"choices":[{"delta":{"content":"hi"}}]}`),
		respTail:  []byte(`data: [DONE]`),
		truncated: false,
	}
	adapter := &captureAdapter{
		stubAdapter: stubAdapter{name: "openai"},
		memOverride: captured,
	}
	runStore := &memRunStore{}
	r := NewRunner(func(name string) (RunnerAdapter, error) { return adapter, nil }, sink, nil, notifier.NewDeduper(time.Minute))
	r.Logger = defaultLogForTest()
	r.RunStore = runStore

	task := config.TaskConfig{ID: "rid", Name: "rid", Protocol: config.ProtocolOpenAI, Enabled: true, BaseURL: "https://e.test", Model: "x", UserPrompt: "hi", Schedule: "@every 1m"}
	res := r.Run(context.Background(), task)

	if res.RequestID != "rid-test-001" {
		t.Fatalf("RequestID on Result = %q, want rid-test-001", res.RequestID)
	}
	if string(res.RequestBody) != string(captured.reqBody) {
		t.Fatalf("RequestBody mismatch: got %q, want %q", res.RequestBody, captured.reqBody)
	}
	if string(res.ResponseHead) != string(captured.respHead) {
		t.Fatalf("ResponseHead mismatch: got %q, want %q", res.ResponseHead, captured.respHead)
	}
	if string(res.ResponseTail) != string(captured.respTail) {
		t.Fatalf("ResponseTail mismatch: got %q, want %q", res.ResponseTail, captured.respTail)
	}
	if res.ResponseTruncated {
		t.Fatalf("ResponseTruncated should be false; got true")
	}

	// Round-trip into ProbeRun + FromResult and verify DB column shape.
	row := FromResult(res)
	if row.RequestID != "rid-test-001" {
		t.Fatalf("FromResult RequestID = %q", row.RequestID)
	}
	if row.RequestBody != string(captured.reqBody) {
		t.Fatalf("FromResult RequestBody = %q", row.RequestBody)
	}
	if row.ResponseHead != string(captured.respHead) {
		t.Fatalf("FromResult ResponseHead = %q", row.ResponseHead)
	}
	if row.ResponseTail != string(captured.respTail) {
		t.Fatalf("FromResult ResponseTail = %q", row.ResponseTail)
	}
}

func TestRunner_ProbeCaptureMarksTruncation(t *testing.T) {
	// When the adapter reports ResponseTruncated=true the runner should
	// surface it on the Result AND on the persisted ProbeRun.
	captured := &memStream{
		respHead:  []byte("a"),
		respTail:  []byte("z"),
		truncated: true,
	}
	sink := metrics.NewPromSink(metrics.PrometheusOptions{})
	adapter := &captureAdapter{
		stubAdapter: stubAdapter{name: "openai"},
		memOverride: captured,
	}
	runStore := &memRunStore{}
	r := NewRunner(func(name string) (RunnerAdapter, error) { return adapter, nil }, sink, nil, notifier.NewDeduper(time.Minute))
	r.Logger = defaultLogForTest()
	r.RunStore = runStore

	task := config.TaskConfig{ID: "t", Name: "t", Protocol: config.ProtocolOpenAI, Enabled: true, BaseURL: "https://e.test", Model: "x", UserPrompt: "hi", Schedule: "@every 1m"}
	res := r.Run(context.Background(), task)
	if !res.ResponseTruncated {
		t.Fatalf("ResponseTruncated should be true; got false")
	}
	if !FromResult(res).ResponseTruncated {
		t.Fatalf("ProbeRun.ResponseTruncated should be true; got false")
	}
}

// memRunStore is a tiny in-memory RunStore for the capture tests; the
// runner only writes through it.
type memRunStore struct {
	mu   sync.Mutex
	runs []ProbeRun
}

func (m *memRunStore) Save(_ context.Context, run *ProbeRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs = append(m.runs, *run)
	return nil
}
func (m *memRunStore) ListByTask(_ context.Context, _ string, _ int) ([]ProbeRun, error) {
	return nil, nil
}
func (m *memRunStore) Prune(_ context.Context, _ int) error { return nil }
func (m *memRunStore) Close() error                       { return nil }

func newRunnerForTest(a RunnerAdapter, sink metrics.MetricSink, n notifier.Notifier) *Runner {
	r := NewRunner(func(name string) (RunnerAdapter, error) {
		return a, nil
	}, sink, n, notifier.NewDeduper(5*time.Minute))
	r.Logger = defaultLogForTest()
	return r
}

func TestRunner_TTFT_It_Duration(t *testing.T) {
	now := time.Now().UTC()
	adapter := &stubAdapter{
		name: "openai",
		chunks: []protocol.Chunk{
			{Delta: "a", ArrivedAt: now.Add(100 * time.Millisecond), Index: 0},
			{Delta: "b", ArrivedAt: now.Add(200 * time.Millisecond), Index: 1, IsFinal: true},
		},
		final: protocol.Chunk{ArrivedAt: now.Add(350 * time.Millisecond), Index: 2},
	}
	sink := metrics.NewPromSink(metrics.PrometheusOptions{})
	n := &capturingNotifier{}
	r := newRunnerForTest(adapter, sink, n)

	res := r.Run(context.Background(), config.TaskConfig{
		ID: "t-1", Name: "t-1", Protocol: config.ProtocolOpenAI,
		BaseURL: "http://example", APIKey: "k", Model: "gpt-4o",
		UserPrompt: "hi",
	})
	if res.Status != StatusSuccess {
		t.Fatalf("status: got %q, want success", res.Status)
	}
	if res.Chunks < 1 {
		t.Fatalf("chunks should be at least 1; got %d", res.Chunks)
	}
	if res.TTFT <= 0 {
		t.Fatalf("ttft must be > 0")
	}
	if res.Duration() <= 0 {
		t.Fatalf("duration must be > 0")
	}
}

func TestRunner_UnauthorizedImmediate(t *testing.T) {
	adapter := &stubAdapter{name: "openai", err: protocol.ErrUnauthorized}
	sink := metrics.NewPromSink(metrics.PrometheusOptions{})
	n := &capturingNotifier{}
	r := newRunnerForTest(adapter, sink, n)
	res := r.Run(context.Background(), config.TaskConfig{
		ID: "t-2", Name: "t-2", Protocol: config.ProtocolOpenAI, BaseURL: "http://x", APIKey: "k", Model: "gpt-4o",
		Notify: config.NotifyConfig{Enabled: true},
	})
	if res.Status != StatusUnauthorized {
		t.Fatalf("status: got %q, want unauthorized", res.Status)
	}
	if got := n.Snap(); len(got) == 0 || got[0].Rule != notifier.RuleUnauthorized {
		t.Fatalf("notifier should have received unauthorized; got %+v", got)
	}
}

func TestRunner_ConsecutiveFailures(t *testing.T) {
	adapter := &stubAdapter{name: "openai", err: errors.New("network down")}
	if unwrap, ok := adapter.err.(interface{ Unwrap() error }); ok {
		_ = unwrap
	}
	sink := metrics.NewPromSink(metrics.PrometheusOptions{})
	n := &capturingNotifier{}
	r := newRunnerForTest(adapter, sink, n)
	r.ConsecFailureN = 3
	cfg := config.TaskConfig{
		ID: "t-3", Name: "t-3", Protocol: config.ProtocolOpenAI,
		BaseURL: "http://x", APIKey: "k", Model: "gpt-4o",
		Notify: config.NotifyConfig{Enabled: true},
	}
	r.Run(context.Background(), cfg) // 1
	r.Run(context.Background(), cfg) // 2
	r.Run(context.Background(), cfg) // 3 → fires
	got := n.Snap()
	has := false
	for _, e := range got {
		if e.Rule == notifier.RuleConsecFailure {
			has = true
		}
	}
	if !has {
		t.Fatalf("expected consec-failure alert within first 3; got %+v", got)
	}
	// Run a 4th failure; the deduper must suppress because it's < 5min.
	r.Run(context.Background(), cfg)
	got = n.Snap()
	count := 0
	for _, e := range got {
		if e.Rule == notifier.RuleConsecFailure {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("dedup failed: consec-failure alerts: got %d, want 1", count)
	}
}

func TestRunner_SlowRequest(t *testing.T) {
	now := time.Now().UTC()
	// Build chunks so TTFT fires the slow rule.
	adapter := &stubAdapter{
		name: "openai",
		chunks: []protocol.Chunk{
			{Delta: "x", ArrivedAt: now.Add(15 * time.Second)},
		},
		final: protocol.Chunk{IsFinal: true, ArrivedAt: now.Add(16 * time.Second)},
	}
	sink := metrics.NewPromSink(metrics.PrometheusOptions{})
	n := &capturingNotifier{}
	r := newRunnerForTest(adapter, sink, n)
	r.SlowThreshold = 10 * time.Second
	cfg := config.TaskConfig{
		ID: "t-4", Name: "t-4", Protocol: config.ProtocolOpenAI,
		BaseURL: "http://x", APIKey: "k", Model: "gpt-4o",
		Notify: config.NotifyConfig{Enabled: true},
	}
	r.Run(context.Background(), cfg)
	got := n.Snap()
	has := false
	for _, e := range got {
		if e.Rule == notifier.RuleSlowRequest {
			has = true
		}
	}
	if !has {
		t.Fatalf("expected slow-request alert; got %+v", got)
	}
}

func TestRunner_SlidingWindow_LowSuccessRate(t *testing.T) {
	// Adapter that alternates success/failure so the slider drops below 0.8.
	flip := atomic.Bool{}
	flip.Store(true)
	type flippable struct{}
	adapter := &flippableAdapter{
		name: "openai",
		next: func() ([]protocol.Chunk, error) {
			ok := flip.Load()
			flip.Store(!ok)
			if ok {
				now := time.Now().UTC()
				return []protocol.Chunk{
					{Delta: "ok", ArrivedAt: now.Add(50 * time.Millisecond)},
				}, nil
			}
			return nil, errors.New("boom")
		},
	}
	sink := metrics.NewPromSink(metrics.PrometheusOptions{})
	n := &capturingNotifier{}
	r := newRunnerForTest(adapter, sink, n)
	r.SuccessRateWindow = 200
	r.SuccessRateThreshold = 0.8
	cfg := config.TaskConfig{
		ID: "t-5", Name: "t-5", Protocol: config.ProtocolOpenAI,
		BaseURL: "http://x", APIKey: "k", Model: "gpt-4o",
		Notify: config.NotifyConfig{Enabled: true},
	}
	// Run 30 invocations: alternating success/failure should drive rate to ~0.5.
	for i := 0; i < 30; i++ {
		r.Run(context.Background(), cfg)
	}
	got := n.Snap()
	has := false
	for _, e := range got {
		if e.Rule == notifier.RuleSuccessRateLow {
			has = true
		}
	}
	if !has {
		t.Fatalf("expected low-success-rate alert; got %+v", got)
	}
}

func TestRunner_NotifyDisabled(t *testing.T) {
	adapter := &stubAdapter{name: "openai", err: protocol.ErrUnauthorized}
	sink := metrics.NewPromSink(metrics.PrometheusOptions{})
	n := &capturingNotifier{}
	r := newRunnerForTest(adapter, sink, n)
	cfg := config.TaskConfig{
		ID: "t-6", Name: "t-6", Protocol: config.ProtocolOpenAI,
		BaseURL: "http://x", APIKey: "k", Model: "gpt-4o",
		Notify: config.NotifyConfig{Enabled: false},
	}
	r.Run(context.Background(), cfg)
	if len(n.Snap()) != 0 {
		t.Fatalf("Notify.Enabled=false should suppress alerts; got %+v", n.Snap())
	}
}

// flippableAdapter is a stub AdapterLookup adapter whose result depends on a
// closure (used by TestRunner_SlidingWindow).
type flippableAdapter struct {
	name string
	next func() ([]protocol.Chunk, error)
}

func (f *flippableAdapter) Name() string { return f.name }
func (f *flippableAdapter) Validate(*config.TaskConfig) error { return nil }
func (f *flippableAdapter) Probe(_ context.Context, _ *config.TaskConfig) (protocol.ProbeStream, error) {
	chunks, err := f.next()
	if err != nil {
		return nil, err
	}
	ch := make(chan protocol.Chunk, len(chunks)+1)
	for _, c := range chunks {
		ch <- c
	}
	ch <- protocol.Chunk{IsFinal: true}
	close(ch)
	return &memStream{ch: ch}, nil
}

// defaultLogForTest is a no-op JSON logger for tests.
func defaultLogForTest() (l *slog.Logger) { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// TestRunner_ProbePersistsSemanticContent verifies §18.14 end-to-end:
// input / thinking / output are captured on the Result and land in the
// persisted ProbeRun row.
func TestRunner_ProbePersistsSemanticContent(t *testing.T) {
	captured := &memStream{
		requestID: "rid-content-001",
		capture: protocol.ContentCapture{
			ThinkingContent: "先推理一下",
		},
	}
	adapter := &captureAdapter{
		stubAdapter: stubAdapter{
			name: "openai",
			chunks: []protocol.Chunk{
				{Delta: "答案", ArrivedAt: time.Now().UTC()},
				{Delta: "！", ArrivedAt: time.Now().UTC()},
			},
			final: protocol.Chunk{Delta: "", ArrivedAt: time.Now().UTC(), IsFinal: true},
		},
		memOverride: captured,
	}
	sink := metrics.NewPromSink(metrics.PrometheusOptions{})
	runStore := &memRunStore{}
	r := NewRunner(func(name string) (RunnerAdapter, error) { return adapter, nil }, sink, nil, notifier.NewDeduper(time.Minute))
	r.Logger = defaultLogForTest()
	r.RunStore = runStore

	task := config.TaskConfig{
		ID: "c", Name: "c", Protocol: config.ProtocolOpenAI, Enabled: true,
		BaseURL: "https://e.test", Model: "x",
		SystemPrompt: "你是助手", UserPrompt: "你好",
		Schedule: "@every 1m",
	}
	res := r.Run(context.Background(), task)

	if !strings.Contains(res.InputContent, "[system]\n你是助手") {
		t.Fatalf("InputContent = %q", res.InputContent)
	}
	if !strings.Contains(res.InputContent, "[user]\n你好") {
		t.Fatalf("InputContent = %q", res.InputContent)
	}
	if res.ThinkingContent != "先推理一下" {
		t.Fatalf("ThinkingContent = %q", res.ThinkingContent)
	}
	if res.OutputContent != "答案！" {
		t.Fatalf("OutputContent = %q", res.OutputContent)
	}

	row := FromResult(res)
	if row.InputContent != res.InputContent {
		t.Fatalf("row.InputContent = %q", row.InputContent)
	}
	if row.ThinkingContent != res.ThinkingContent {
		t.Fatalf("row.ThinkingContent = %q", row.ThinkingContent)
	}
	if row.OutputContent != res.OutputContent {
		t.Fatalf("row.OutputContent = %q", row.OutputContent)
	}

	// The runner writes the row to the store; verify the persisted shape.
	runStore.mu.Lock()
	defer runStore.mu.Unlock()
	if len(runStore.runs) != 1 {
		t.Fatalf("expected 1 persisted run, got %d", len(runStore.runs))
	}
	got := runStore.runs[0]
	if got.OutputContent != "答案！" {
		t.Fatalf("persisted OutputContent = %q", got.OutputContent)
	}
	if got.ThinkingContent != "先推理一下" {
		t.Fatalf("persisted ThinkingContent = %q", got.ThinkingContent)
	}
}

// TestRunner_ProbeFailureKeepsRequestID verifies §18.14: a failed probe
// must still record the X-Req-Id and the upstream error body, otherwise
// the most interesting failures leave no trace to chase.
func TestRunner_ProbeFailureKeepsRequestID(t *testing.T) {
	reqErr := protocol.NewRequestError(
		"rid-fail-401",
		[]byte(`{"model":"x"}`),
		`{"error":{"message":"Incorrect API key provided"}}`,
		fmt.Errorf("openai: status 401: %w", protocol.ErrUnauthorized),
	)
	adapter := &stubAdapter{name: "openai", err: reqErr}
	sink := metrics.NewPromSink(metrics.PrometheusOptions{})
	runStore := &memRunStore{}
	r := NewRunner(func(name string) (RunnerAdapter, error) { return adapter, nil }, sink, nil, notifier.NewDeduper(time.Minute))
	r.Logger = defaultLogForTest()
	r.RunStore = runStore

	task := config.TaskConfig{ID: "f", Name: "f", Protocol: config.ProtocolOpenAI, Enabled: true, BaseURL: "https://e.test", Model: "x", UserPrompt: "hi", Schedule: "@every 1m"}
	res := r.Run(context.Background(), task)

	if res.Status != StatusUnauthorized {
		t.Fatalf("Status = %q, want %q", res.Status, StatusUnauthorized)
	}
	if res.RequestID != "rid-fail-401" {
		t.Fatalf("RequestID = %q, want rid-fail-401", res.RequestID)
	}
	if !strings.Contains(res.ResponseErrorBody, "Incorrect API key") {
		t.Fatalf("ResponseErrorBody = %q", res.ResponseErrorBody)
	}
	// Input is captured even when the request itself fails.
	if !strings.Contains(res.InputContent, "hi") {
		t.Fatalf("InputContent = %q", res.InputContent)
	}

	runStore.mu.Lock()
	defer runStore.mu.Unlock()
	if len(runStore.runs) != 1 {
		t.Fatalf("expected 1 persisted run, got %d", len(runStore.runs))
	}
	if runStore.runs[0].RequestID != "rid-fail-401" {
		t.Fatalf("persisted RequestID = %q", runStore.runs[0].RequestID)
	}
}
