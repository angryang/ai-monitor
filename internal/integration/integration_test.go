//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aibench/aibench/ai-monitor/internal/config"
	"github.com/aibench/aibench/ai-monitor/internal/metrics"
	"github.com/aibench/aibench/ai-monitor/internal/notifier"
	"github.com/aibench/aibench/ai-monitor/internal/notifier/wecom"
	"github.com/aibench/aibench/ai-monitor/internal/probe"
	"github.com/aibench/aibench/ai-monitor/internal/protocol"
	_ "github.com/aibench/aibench/ai-monitor/internal/protocol/openai"
	"github.com/aibench/aibench/ai-monitor/internal/scheduler"
	"github.com/aibench/aibench/ai-monitor/internal/server"
)

// TestE2E_ProbeFiresAndMetricsExposed exercises the whole stack with a mock
// upstream. A scheduler tick at @every 50ms drives the probe runner, whose
// results are exposed via /metrics.
func TestE2E_ProbeFiresAndMetricsExposed(t *testing.T) {
	// Upstream mock returning OpenAI SSE chunks.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"\"}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\" world\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer srv.Close()

	// Set up dependencies.
	ctx := context.Background()
	cs, err := config.NewSqliteStore(config.SqliteOptions{Path: ":memory:"})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer cs.Close()
	cs.SetCipher(mustCipher(t))

	tc := &config.TaskConfig{
		ID:         "t-prod",
		Name:       "prod-demo",
		Enabled:    true,
		Protocol:   config.ProtocolOpenAI,
		BaseURL:    srv.URL,
		APIKey:     "sk-test",
		Model:      "gpt-4o",
		UserPrompt: "hi",
		Schedule:   "@every 50ms",
		Timeout:    1 * time.Second,
	}
	tcCopy := *tc // preserve original (CreateTask may mutate the input via GORM)
	if err := cs.CreateTask(ctx, tc); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := tcCopy.ValidateProtocolSegments(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	loaded, _ := cs.GetTask(ctx, tc.ID)
	if err := loaded.ValidateProtocolSegments(); err != nil {
		t.Fatalf("loaded validate: %v", err)
	}

	sink := metrics.NewPromSink(metrics.PrometheusOptions{})
	dedup := notifier.NewDeduper(5 * time.Minute)
	adapter, err := protocol.Lookup("openai")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	runner := probe.NewRunner(func(name string) (probe.RunnerAdapter, error) {
		return adapter, nil
	}, sink, nil, dedup)

	var fired atomic.Int32
	sched := scheduler.New(scheduler.Options{
		MaxConcurrent: 4,
		Handler: func(_ context.Context, t config.TaskConfig) {
			fired.Add(1)
			runner.Run(context.Background(), t)
		},
	})
	// Use the freshly-loaded task (with hydrated durations) for the
	// scheduler, not the in-memory `tc` (which GORM may have mutated).
	if err := sched.Reload(ctx, []config.TaskConfig{*loaded}); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	tasks, _ := cs.ListTasks(ctx)
	if err := sched.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sched.Stop(context.Background()) }()
	_ = tasks // suppress unused warning

	// Wait for at least 1 fire.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && fired.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if fired.Load() == 0 {
		t.Fatalf("scheduler did not fire")
	}

	// Stand up the HTTP layer and check /metrics.
	httpSrv := server.New(server.Options{
		Addr:    ":0",
		Metrics: sink.Handler(),
		Store:   cs,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	httpSrv.Handle().ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	want := []string{"ai_probe_request_duration_seconds", "ai_probe_ttft_seconds"}
	for _, w := range want {
		if !strings.Contains(string(body), w) {
			t.Fatalf("missing metric %q; body=%s", w, string(body))
		}
	}
}

// TestE2E_AlertSent fires an alert through to a mock WeCom endpoint and
// verifies the body shape.
func TestE2E_AlertSent(t *testing.T) {
	hitCh := make(chan map[string]any, 4)
	wecomSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		if json.Unmarshal(body, &m) == nil {
			hitCh <- m
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"errcode":0}`))
	}))
	defer wecomSrv.Close()

	sink := metrics.NewPromSink(metrics.PrometheusOptions{})
	dedup := notifier.NewDeduper(5 * time.Minute)
	notifier := wecom.New(wecom.Config{WebhookURL: wecomSrv.URL})

	// Adapter that always returns 401 to trigger RuleUnauthorized.
	alwaysFail := &alwaysFailAdapter{name: "openai"}
	runner := probe.NewRunner(func(name string) (probe.RunnerAdapter, error) {
		return alwaysFail, nil
	}, sink, notifier, dedup)
	runner.ConsecFailureN = 1

	cfg := config.TaskConfig{
		ID:       "t-auth",
		Name:     "auth-probe",
		Protocol: config.ProtocolOpenAI,
		BaseURL:  "http://example",
		APIKey:   "sk-test",
		Model:    "gpt-4o",
		Notify:   config.NotifyConfig{Enabled: true},
	}
	runner.Run(context.Background(), cfg)

	select {
	case got := <-hitCh:
		if got["rule"] != "unauthorized_immediate" {
			t.Fatalf("rule: got %v, want unauthorized_immediate", got["rule"])
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("did not receive alert POST")
	}
}

type alwaysFailAdapter struct{ name string }

func (a *alwaysFailAdapter) Name() string { return a.name }
func (a *alwaysFailAdapter) Validate(_ *config.TaskConfig) error {
	return nil
}
func (a *alwaysFailAdapter) Probe(_ context.Context, _ *config.TaskConfig) (protocol.ProbeStream, error) {
	return nil, protocol.ErrUnauthorized
}

func mustCipher(t *testing.T) config.Cipher {
	t.Helper()
	key := []byte("01234567890123456789012345678901")
	c, err := config.NewAESCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	return c
}

// avoid "imported and not used" on json later.
var _ = json.Marshal
var _ = fmt.Sprintf