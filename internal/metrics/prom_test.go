package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPromSink_AllMetricsRegistered(t *testing.T) {
	sink := NewPromSink(PrometheusOptions{})
	// Touch every metric so its series exists (Vec metrics don't show up in
	// Gather output until they're observed at least once).
	sink.ObserveTTFT("t-touch", "openai", "gpt-4o", 0.001)
	sink.ObserveITL("t-touch", "openai", "gpt-4o", 0.001)
	sink.ObserveDuration("t-touch", "openai", "gpt-4o", "success", 0.001)
	sink.IncChunk("t-touch", "openai", "gpt-4o", 1)
	sink.IncFailure("t-touch", "openai", "gpt-4o", "timeout")
	sink.SetTaskLastSuccess("t-touch", time.Unix(0, 0).UTC())
	sink.SetSchedulerLag("t-touch", 0.001)

	mfs, err := sink.registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	names := map[string]bool{}
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	want := []string{
		"ai_probe_request_duration_seconds",
		"ai_probe_ttft_seconds",
		"ai_probe_itl_seconds",
		"ai_probe_request_total",
		"ai_probe_failure_total",
		"ai_probe_chunk_total",
		"ai_probe_task_last_success_timestamp_seconds",
		"ai_probe_scheduler_lag_seconds",
	}
	for _, n := range want {
		if !names[n] {
			t.Fatalf("missing metric %q (have %v)", n, names)
		}
	}
}

func TestPromSink_ObserveRecordsData(t *testing.T) {
	sink := NewPromSink(PrometheusOptions{})
	sink.ObserveTTFT("t-1", "openai", "gpt-4o", 0.123)
	sink.ObserveITL("t-1", "openai", "gpt-4o", 0.014)
	sink.ObserveDuration("t-1", "openai", "gpt-4o", "success", 1.42)
	sink.IncChunk("t-1", "openai", "gpt-4o", 5)
	sink.IncFailure("t-1", "openai", "gpt-4o", "timeout")
	sink.SetTaskLastSuccess("t-1", time.Unix(1234567890, 0).UTC())
	sink.SetSchedulerLag("t-1", 0.250)

	mfs, err := sink.registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if len(mfs) == 0 {
		t.Fatalf("expected metrics, got 0")
	}
	// Spot-check at least one series carries a sample.
	saw := false
	for _, mf := range mfs {
		if mf.GetName() == "ai_probe_ttft_seconds" {
			if len(mf.GetMetric()) == 0 {
				t.Fatalf("ttft metric has no series")
			}
			saw = true
		}
	}
	if !saw {
		t.Fatalf("ttft series not found")
	}
}

func TestPromSink_HandlerReturnsValid(t *testing.T) {
	sink := NewPromSink(PrometheusOptions{})
	sink.ObserveDuration("t-1", "openai", "gpt-4o", "success", 0.5)
	w := httptest.NewRecorder()
	sink.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"ai_probe_request_duration_seconds",
		"ai_probe_request_total",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q", want)
		}
	}
}

func TestPromSink_ObserveTTFT_IncrementsBuckets(t *testing.T) {
	sink := NewPromSink(PrometheusOptions{})
	// Observe a TTFT of 0.5s — must populate the matching bucket.
	sink.ObserveTTFT("t-1", "openai", "gpt-4o", 0.5)

	mfs, err := sink.registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "ai_probe_ttft_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			h := m.GetHistogram()
			if h == nil {
				t.Fatalf("expected histogram")
			}
			if h.GetSampleCount() != 1 {
				t.Fatalf("sample count: got %d, want 1", h.GetSampleCount())
			}
			if h.GetSampleSum() < 0.4 || h.GetSampleSum() > 0.6 {
				t.Fatalf("sample sum: got %f, want ~0.5", h.GetSampleSum())
			}
			return
		}
	}
	t.Fatalf("ttft metric not found")
}
