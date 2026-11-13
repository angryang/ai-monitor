// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package probe

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/metrics"
	"github.com/angryang/ai-monitor/internal/notifier"
	"github.com/angryang/ai-monitor/internal/protocol"
)

// countingSink records every metric call so a test can assert that a
// manual run left the monitoring pipeline alone.
type countingSink struct {
	mu       sync.Mutex
	ttft     int
	itl      int
	duration int
	chunks   int
	failure  int
	lastSucc int
}

func (c *countingSink) ObserveTTFT(string, string, string, float64) {
	c.mu.Lock()
	c.ttft++
	c.mu.Unlock()
}
func (c *countingSink) ObserveITL(string, string, string, float64) {
	c.mu.Lock()
	c.itl++
	c.mu.Unlock()
}
func (c *countingSink) ObserveDuration(string, string, string, string, float64) {
	c.mu.Lock()
	c.duration++
	c.mu.Unlock()
}
func (c *countingSink) IncChunk(string, string, string, int) { c.mu.Lock(); c.chunks++; c.mu.Unlock() }
func (c *countingSink) IncFailure(string, string, string, string) {
	c.mu.Lock()
	c.failure++
	c.mu.Unlock()
}
func (c *countingSink) SetTaskLastSuccess(string, time.Time) {
	c.mu.Lock()
	c.lastSucc++
	c.mu.Unlock()
}
func (c *countingSink) SetSchedulerLag(string, float64) {}
func (c *countingSink) Handler() http.Handler           { return http.NotFoundHandler() }

func (c *countingSink) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ttft + c.itl + c.duration + c.chunks + c.failure + c.lastSucc
}

// okAdapter returns one content chunk and a final chunk.
func okAdapter() *stubAdapter {
	return &stubAdapter{
		name: "openai",
		chunks: []protocol.Chunk{
			{Delta: "hello", ArrivedAt: time.Now().UTC()},
		},
		final: protocol.Chunk{Delta: "!", ArrivedAt: time.Now().UTC(), IsFinal: true},
	}
}

// manualTask is a minimal task config for the manual-mode tests.
func manualTask() config.TaskConfig {
	return config.TaskConfig{
		ID:         "req-item-1",
		Name:       "group / item",
		Enabled:    true,
		Protocol:   config.ProtocolOpenAI,
		BaseURL:    "https://e.test",
		APIKey:     "sk-test",
		Model:      "gpt-test",
		UserPrompt: "hi",
		Schedule:   "@every 1m",
		Timeout:    5 * time.Second,
		TimeoutMS:  5000,
		Notify:     config.NotifyConfig{Enabled: true, Channel: config.NotifyWeCom},
	}
}

// TestRunManual_SkipsMonitoringSideEffects pins the contract of a manual
// run: the probe happens and is recorded, but it leaves no trace on the
// monitoring signal — no Prometheus samples, no success-rate window, no
// notification.
//
// Without this, a single manual smoke test that happens to time out would
// look like a monitoring failure and could page somebody.
func TestRunManual_SkipsMonitoringSideEffects(t *testing.T) {
	adapter := okAdapter()
	sink := &countingSink{}
	notif := &capturingNotifier{}
	store := &memRunStore{}

	r := NewRunner(func(string) (RunnerAdapter, error) { return adapter, nil }, sink, notif, notifier.NewDeduper(time.Minute))
	r.Logger = defaultLogForTest()
	r.RunStore = store

	res := r.RunManual(context.Background(), manualTask(), RunOptions{
		BatchID: "bat_1",
		ProviderID: "prv_1",
		AIRequestID: "req_1",
	})

	if res.Status != StatusSuccess {
		t.Fatalf("status = %q, want %q (err=%v)", res.Status, StatusSuccess, res.Err)
	}
	if res.Source != SourceManual {
		t.Fatalf("Source = %q, want %q", res.Source, SourceManual)
	}
	if res.BatchID != "bat_1" || res.ProviderID != "prv_1" || res.AIRequestID != "req_1" {
		t.Fatalf("provenance lost: batch=%q provider=%q request=%q", res.BatchID, res.ProviderID, res.AIRequestID)
	}
	if got := sink.total(); got != 0 {
		t.Fatalf("manual run emitted %d metric calls; want 0", got)
	}
	if got := len(notif.Snap()); got != 0 {
		t.Fatalf("manual run fired %d notifications; want 0", got)
	}
	// The success-rate window must not have seen the run either.
	if win := r.getSlider(res.TaskID); win != nil && win.Count() != 0 {
		t.Fatalf("manual run fed the success-rate window (%d samples); want 0", win.Count())
	}
	// …but it must still be recorded in the history.
	runs := store.Snap()
	if len(runs) != 1 {
		t.Fatalf("history rows = %d, want 1", len(runs))
	}
	if runs[0].Source != SourceManual {
		t.Fatalf("persisted Source = %q, want %q", runs[0].Source, SourceManual)
	}
	if runs[0].BatchID != "bat_1" || runs[0].ProviderID != "prv_1" || runs[0].AIRequestID != "req_1" {
		t.Fatalf("persisted provenance lost: %+v", runs[0])
	}
}

// TestRun_EmitsMonitoringSideEffects is the counterpart: a scheduled run
// must still feed the metrics, the window and the notification channel.
func TestRun_EmitsMonitoringSideEffects(t *testing.T) {
	adapter := okAdapter()
	sink := &countingSink{}
	notif := &capturingNotifier{}
	store := &memRunStore{}

	r := NewRunner(func(string) (RunnerAdapter, error) { return adapter, nil }, sink, notif, notifier.NewDeduper(time.Minute))
	r.Logger = defaultLogForTest()
	r.RunStore = store

	res := r.Run(context.Background(), manualTask())

	if res.Status != StatusSuccess {
		t.Fatalf("status = %q, want %q (err=%v)", res.Status, StatusSuccess, res.Err)
	}
	if res.Source != SourceSchedule {
		t.Fatalf("Source = %q, want %q", res.Source, SourceSchedule)
	}
	if got := sink.total(); got == 0 {
		t.Fatalf("scheduled run emitted no metric calls; want some")
	}
	runs := store.Snap()
	if len(runs) != 1 {
		t.Fatalf("history rows = %d, want 1", len(runs))
	}
	if runs[0].Source != SourceSchedule {
		t.Fatalf("persisted Source = %q, want %q", runs[0].Source, SourceSchedule)
	}
}

// TestRunManual_FailureDoesNotAlert covers the case that matters most:
// a manual request that fails must not page anybody.
func TestRunManual_FailureDoesNotAlert(t *testing.T) {
	adapter := &stubAdapter{name: "openai", err: context.DeadlineExceeded}
	sink := &countingSink{}
	notif := &capturingNotifier{}
	store := &memRunStore{}

	r := NewRunner(func(string) (RunnerAdapter, error) { return adapter, nil }, sink, notif, notifier.NewDeduper(time.Minute))
	r.Logger = defaultLogForTest()
	r.RunStore = store

	res := r.RunManual(context.Background(), manualTask(), RunOptions{AIRequestID: "req_1"})

	if res.Status == StatusSuccess {
		t.Fatalf("status = success; want a failure")
	}
	if res.Err == nil {
		t.Fatalf("Err is nil; want the probe error")
	}
	if got := sink.total(); got != 0 {
		t.Fatalf("failed manual run emitted %d metric calls; want 0", got)
	}
	if got := len(notif.Snap()); got != 0 {
		t.Fatalf("failed manual run fired %d notifications; want 0", got)
	}
	// A failure must still be recorded — that is the whole point of
	// running it.
	runs := store.Snap()
	if len(runs) != 1 {
		t.Fatalf("history rows = %d, want 1", len(runs))
	}
	if runs[0].Status == StatusSuccess {
		t.Fatalf("persisted status = success; want the failure status")
	}
}

// TestRunManual_UnauthorizedDoesNotAlert pins the 401 path: an operator
// firing a request with a stale key should see the failure in the UI, not
// in the WeCom channel.
func TestRunManual_UnauthorizedDoesNotAlert(t *testing.T) {
	adapter := &stubAdapter{name: "openai", err: protocol.ErrUnauthorized}
	sink := &countingSink{}
	notif := &capturingNotifier{}
	store := &memRunStore{}

	r := NewRunner(func(string) (RunnerAdapter, error) { return adapter, nil }, sink, notif, notifier.NewDeduper(time.Minute))
	r.Logger = defaultLogForTest()
	r.RunStore = store

	res := r.RunManual(context.Background(), manualTask(), RunOptions{AIRequestID: "req_1"})

	if res.Status != StatusUnauthorized {
		t.Fatalf("status = %q, want %q", res.Status, StatusUnauthorized)
	}
	if got := len(notif.Snap()); got != 0 {
		t.Fatalf("unauthorized manual run fired %d notifications; want 0", got)
	}
	if got := sink.total(); got != 0 {
		t.Fatalf("unauthorized manual run emitted %d metric calls; want 0", got)
	}
}

// TestRunOptions_ManualIsForcedOn guards the API: RunManual must not be
// able to be talked out of the manual mode by the caller's options.
func TestRunOptions_ManualIsForcedOn(t *testing.T) {
	adapter := okAdapter()
	sink := &countingSink{}
	notif := &capturingNotifier{}
	store := &memRunStore{}

	r := NewRunner(func(string) (RunnerAdapter, error) { return adapter, nil }, sink, notif, notifier.NewDeduper(time.Minute))
	r.Logger = defaultLogForTest()
	r.RunStore = store

	res := r.RunManual(context.Background(), manualTask(), RunOptions{Manual: false, AIRequestID: "req_1"})
	if res.Source != SourceManual {
		t.Fatalf("Source = %q, want %q — RunManual must force the manual mode", res.Source, SourceManual)
	}
	if got := sink.total(); got != 0 {
		t.Fatalf("manual run emitted %d metric calls; want 0", got)
	}
}

// compile-time assertion that the test double still satisfies the sink
// contract after the interface changes.
var _ metrics.MetricSink = (*countingSink)(nil)

// Snap returns a copy of the rows the store received. It lives here
// rather than in runner_test.go so the shared test double keeps a single
// owner for its snapshot accessor.
func (m *memRunStore) Snap() []ProbeRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ProbeRun, len(m.runs))
	copy(out, m.runs)
	return out
}
