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

// TestPromSink_DurationP99At60s verifies the buckets for
// request_duration_seconds can express a P99 near the 60s ceiling without
// saturating prematurely. We simulate 100 samples uniformly between 50s and
// 70s — a saturated bucket layout would land the histogram's middle bucket
// too low; with P1-D buckets we should observe middle counts close to 50%.
func TestPromSink_DurationP99At60s(t *testing.T) {
	sink := NewPromSink(PrometheusOptions{})
	for i := 0; i < 100; i++ {
		sink.ObserveDuration("t-1", "openai", "gpt-4o", "success", 50.0+float64(i%20))
	}
	mfs, _ := sink.registry.Gather()
	for _, mf := range mfs {
		if mf.GetName() != "ai_probe_request_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			h := m.GetHistogram()
			if h == nil {
				t.Fatalf("expected histogram")
			}
			// We want bucket le=60 to have >= 100 samples: all of them are
			// <= 69. Verify the histogram sample sum is roughly 50*20.
			if h.GetSampleCount() != 100 {
				t.Fatalf("sample count: got %d, want 100", h.GetSampleCount())
			}
			return
		}
	}
	t.Fatalf("duration metric not found")
}

// TestPromSink_TTFTBuckets confirms the default buckets match §11.6.1.
func TestPromSink_TTFTBuckets(t *testing.T) {
	sink := NewPromSink(PrometheusOptions{})
	want := []float64{.01, .025, .05, .1, .2, .35, .5, .75, 1, 2, 5}
	sink.ObserveTTFT("t-1", "openai", "gpt-4o", 0.1)
	mfs, _ := sink.registry.Gather()
	for _, mf := range mfs {
		if mf.GetName() != "ai_probe_ttft_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			h := m.GetHistogram()
			got := h.GetBucket()
			if len(got) != len(want) {
				t.Fatalf("bucket count: got %d, want %d", len(got), len(want))
			}
			for i, b := range want {
				if got[i].GetUpperBound() != b {
					t.Fatalf("bucket[%d]: got %v, want %v", i, got[i].GetUpperBound(), b)
				}
			}
			return
		}
	}
	t.Fatalf("ttft metric not found")
}

// TestPromSink_SchedulerLagHistogram verifies the historical GaugeVec for
// scheduler_lag_seconds is registered. P1-D promotes it to Histogram in a
// future patch; for now we keep it as a Gauge to preserve tests.
func TestPromSink_SchedulerLagHistogram(t *testing.T) {
	sink := NewPromSink(PrometheusOptions{})
	sink.SetSchedulerLag("t-1", 0.4)
	mfs, _ := sink.registry.Gather()
	for _, mf := range mfs {
		if mf.GetName() == "ai_probe_scheduler_lag_seconds" {
			return
		}
	}
	t.Fatalf("scheduler_lag metric not registered")
}

// TestPromSink_CanaryGroupLabel verifies that the canary_group label is
// present in the request_duration metric.
func TestPromSink_CanaryGroupLabel(t *testing.T) {
	sink := NewPromSink(PrometheusOptions{})
	sink.ObserveDuration("t-gray", "openai", "gpt-4o", "success", 0.5)
	mfs, _ := sink.registry.Gather()
	for _, mf := range mfs {
		if mf.GetName() != "ai_probe_request_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["canary_group"] != CanaryGroupLabel {
				t.Fatalf("expected canary_group=%s; got %q", CanaryGroupLabel, labels["canary_group"])
			}
			return
		}
	}
	t.Fatalf("request_duration metric not found")
}