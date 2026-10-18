// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

// Package metrics defines the MetricSink abstraction used by the runner.
// The default implementation (PromSink) reports to Prometheus via pull mode.
package metrics

import (
	"net/http"
	"time"
)

// MetricSink is the public surface used by other packages. Implementations
// must be safe for concurrent calls across goroutines.
type MetricSink interface {
	ObserveTTFT(taskID, protocol, model string, seconds float64)
	ObserveITL(taskID, protocol, model string, seconds float64)
	ObserveDuration(taskID, protocol, model, status string, seconds float64)
	IncChunk(taskID, protocol, model string, n int)
	IncFailure(taskID, protocol, model, errorType string)
	SetTaskLastSuccess(taskID string, ts time.Time)
	SetSchedulerLag(taskID string, seconds float64)
	Handler() http.Handler
}

// CanaryGroupLabel is the default value used when a metric call doesn't
// supply a canary group. Mirrors config.CanaryGroupLabel but duplicated to
// avoid an import cycle.
const CanaryGroupLabel = "default"
