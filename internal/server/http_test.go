package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/metrics"
)

// fakeStore implements config.ConfigStore (just ListTasks + Close; the rest
// isn't used in the tests below).
type fakeStore struct {
	tasks []config.TaskConfig
	err   error
}

func (f *fakeStore) ListTasks(_ context.Context) ([]config.TaskConfig, error) {
	return f.tasks, f.err
}
func (f *fakeStore) GetTask(_ context.Context, _ string) (*config.TaskConfig, error) { return nil, nil }
func (f *fakeStore) CreateTask(_ context.Context, _ *config.TaskConfig) error         { return nil }
func (f *fakeStore) UpdateTask(_ context.Context, _ *config.TaskConfig) error         { return nil }
func (f *fakeStore) DeleteTask(_ context.Context, _ string) error                    { return nil }
func (f *fakeStore) Watch(_ context.Context) (<-chan config.Event, error)             { return nil, nil }
func (f *fakeStore) Close() error                                                    { return nil }

func TestHealthz_Ok(t *testing.T) {
	store, err := config.NewSqliteStore(config.SqliteOptions{Path: ":memory:"})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()
	srv := New(Options{Addr: ":9090", Store: store})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	srv.Handle().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("body: %s", string(body))
	}
}

func TestHealthz_DBDown(t *testing.T) {
	store := &fakeStore{err: io.ErrUnexpectedEOF}
	srv := New(Options{Addr: ":9090", Store: store})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	srv.Handle().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
}

func TestMetrics_Exposed(t *testing.T) {
	sink := metrics.NewPromSink(metrics.PrometheusOptions{})
	// touch a metric so Gather returns at least one series.
	sink.ObserveDuration("x", "openai", "gpt-4o", "success", 0.1)
	srv := New(Options{Addr: ":9090", Metrics: sink.Handler()})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	srv.Handle().ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "ai_probe_request_duration_seconds") {
		t.Fatalf("missing request_duration metric; body=%s", string(body))
	}
}

func TestAdminListTasks_RedactsSecrets(t *testing.T) {
	store, err := config.NewSqliteStore(config.SqliteOptions{Path: ":memory:"})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()
	tc := &config.TaskConfig{
		ID:      "t-1",
		Name:    "demo",
		Enabled: true,
		Protocol: config.ProtocolOpenAI,
		BaseURL: "http://example",
		APIKey:  "sk-secret",
		Model:   "gpt-4o",
		UserPrompt: "hi",
		Notify: config.NotifyConfig{
			Enabled:      true,
			WeComWebhook: "https://webhook",
		},
		OpenAI: &config.OpenAIConfig{StreamPath: "/v1"},
	}
	if err := store.CreateTask(context.Background(), tc); err != nil {
		t.Fatalf("create: %v", err)
	}
	srv := New(Options{Addr: ":9090", Store: store})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/tasks", nil)
	srv.Handle().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if strings.Contains(string(body), "sk-secret") {
		t.Fatalf("api key leaked: %s", string(body))
	}
	if strings.Contains(string(body), "https://webhook") {
		t.Fatalf("webhook leaked: %s", string(body))
	}
	var parsed struct {
		Tasks []config.TaskConfig `json:"tasks"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(parsed.Tasks))
	}
}

func TestAdminReload_NotConfigured(t *testing.T) {
	srv := New(Options{Addr: ":9090"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	srv.Handle().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: %d", rec.Code)
	}
}