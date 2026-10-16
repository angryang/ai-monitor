package probe

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aibench/aibench/ai-monitor/internal/config"
	"github.com/aibench/aibench/ai-monitor/internal/metrics"
	"github.com/aibench/aibench/ai-monitor/internal/notifier"
	"github.com/aibench/aibench/ai-monitor/internal/protocol"
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
	ch chan protocol.Chunk
}

func (m *memStream) Chunks() <-chan protocol.Chunk { return m.ch }
func (m *memStream) Err() error                    { return nil }
func (m *memStream) Close() error                 { return nil }

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
