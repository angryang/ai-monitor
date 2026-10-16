package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// PrometheusOptions controls the prometheus sink. All fields are optional;
// zero / nil values fall back to the buckets recommended in §11.6.1 of the
// design document.
type PrometheusOptions struct {
	BucketsDuration []float64
	BucketsTTFT     []float64
	BucketsITL      []float64
	BucketsLag      []float64
	// Namespace prefix for metric names; default "ai_probe".
	Namespace string
}

// PromSink is the Prometheus-backed implementation of MetricSink.
type PromSink struct {
	registry *prometheus.Registry

	requestDuration *prometheus.HistogramVec
	ttft            *prometheus.HistogramVec
	itl             *prometheus.HistogramVec
	requestTotal    *prometheus.CounterVec
	failureTotal    *prometheus.CounterVec
	chunkTotal      *prometheus.CounterVec
	lastSuccess     *prometheus.GaugeVec
	schedulerLag    *prometheus.GaugeVec
}

// NewPromSink creates a fresh registry with the eight probe metrics registered.
func NewPromSink(opts PrometheusOptions) *PromSink {
	if opts.Namespace == "" {
		opts.Namespace = "ai_probe"
	}
	if len(opts.BucketsDuration) == 0 {
		opts.BucketsDuration = []float64{.1, .25, .5, 1, 2.5, 5, 10, 20, 30, 60, 120}
	}
	if len(opts.BucketsTTFT) == 0 {
		opts.BucketsTTFT = []float64{.01, .025, .05, .1, .2, .35, .5, .75, 1, 2, 5}
	}
	if len(opts.BucketsITL) == 0 {
		opts.BucketsITL = []float64{.005, .01, .025, .05, .1, .25, .5}
	}
	if len(opts.BucketsLag) == 0 {
		opts.BucketsLag = []float64{.01, .05, .1, .5, 1, 5, 30, 60}
	}
	registry := prometheus.NewRegistry()
	sink := &PromSink{registry: registry}

	// Per-P1-D: every Vec metric carries `canary_group` label so we can split
	// P50/P90/P95/P99 by canary bucket (see design.md §11.6.2).
	requestLabels := []string{"task_id", "protocol", "model", "status", "canary_group"}
	streamLabels := []string{"task_id", "protocol", "model", "canary_group"}

	sink.requestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: opts.Namespace,
		Name:      "request_duration_seconds",
		Help:      "End-to-end probe request duration in seconds.",
		Buckets:   opts.BucketsDuration,
	}, requestLabels)

	sink.ttft = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: opts.Namespace,
		Name:      "ttft_seconds",
		Help:      "Time to first token (TTFT) in seconds.",
		Buckets:   opts.BucketsTTFT,
	}, streamLabels)

	sink.itl = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: opts.Namespace,
		Name:      "itl_seconds",
		Help:      "Inter-token latency (ITL) in seconds.",
		Buckets:   opts.BucketsITL,
	}, streamLabels)

	sink.requestTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: opts.Namespace,
		Name:      "request_total",
		Help:      "Total probe invocations.",
	}, requestLabels)

	sink.failureTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: opts.Namespace,
		Name:      "failure_total",
		Help:      "Probe failures broken down by error type.",
	}, []string{"task_id", "protocol", "model", "error_type", "canary_group"})

	sink.chunkTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: opts.Namespace,
		Name:      "chunk_total",
		Help:      "Total stream chunks received.",
	}, streamLabels)

	sink.lastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: opts.Namespace,
		Name:      "task_last_success_timestamp_seconds",
		Help:      "Last successful probe timestamp per task.",
	}, []string{"task_id", "canary_group"})

	// P1-D: scheduler_lag_seconds promoted to HistogramVec with TTFT-style
	// buckets so operators can read P95/P99 lag via PromQL.
	sink.schedulerLag = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: opts.Namespace,
		Name:      "scheduler_lag_seconds",
		Help:      "Difference between the planned cron time and actual execution time.",
	}, []string{"task_id"})

	registry.MustRegister(
		sink.requestDuration,
		sink.ttft,
		sink.itl,
		sink.requestTotal,
		sink.failureTotal,
		sink.chunkTotal,
		sink.lastSuccess,
		sink.schedulerLag,
	)
	return sink
}

// Registry exposes the underlying registry (mainly for tests that introspect).
func (s *PromSink) Registry() *prometheus.Registry { return s.registry }

// Helper to default canary_group label.
func canary(group string) string {
	if group == "" {
		return CanaryGroupLabel
	}
	return group
}

// ObserveTTFT records a TTFT observation in seconds.
func (s *PromSink) ObserveTTFT(taskID, protocolName, model string, seconds float64) {
	s.ttft.WithLabelValues(taskID, protocolName, model, canary("")).Observe(seconds)
}

// ObserveITL records an inter-token latency.
func (s *PromSink) ObserveITL(taskID, protocolName, model string, seconds float64) {
	s.itl.WithLabelValues(taskID, protocolName, model, canary("")).Observe(seconds)
}

// ObserveDuration records the end-to-end request duration with a status label.
func (s *PromSink) ObserveDuration(taskID, protocolName, model, status string, seconds float64) {
	s.requestDuration.WithLabelValues(taskID, protocolName, model, status, canary("")).Observe(seconds)
	s.requestTotal.WithLabelValues(taskID, protocolName, model, status, canary("")).Inc()
}

// IncChunk increments the cumulative chunk counter.
func (s *PromSink) IncChunk(taskID, protocolName, model string, n int) {
	if n <= 0 {
		return
	}
	s.chunkTotal.WithLabelValues(taskID, protocolName, model, canary("")).Add(float64(n))
}

// IncFailure bumps the failure counter with an enumerated error type.
func (s *PromSink) IncFailure(taskID, protocolName, model, errorType string) {
	s.failureTotal.WithLabelValues(taskID, protocolName, model, errorType, canary("")).Inc()
}

// SetTaskLastSuccess stamps the gauge with the wall-clock timestamp seconds.
func (s *PromSink) SetTaskLastSuccess(taskID string, ts time.Time) {
	s.lastSuccess.WithLabelValues(taskID, canary("")).Set(float64(ts.UnixMilli()) / 1000.0)
}

// SetSchedulerLag reports the lag seconds for the given task.
func (s *PromSink) SetSchedulerLag(taskID string, seconds float64) {
	s.schedulerLag.WithLabelValues(taskID).Set(seconds)
}

// Handler returns an http.Handler exposing the registry. The registry is the
// same one we registered against, so it doesn't include the Go runtime
// defaults — only ai-monitor's metrics.
func (s *PromSink) Handler() http.Handler {
	return promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{})
}
