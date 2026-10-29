// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

// Package probe owns the orchestration logic: given a TaskConfig, run the
// registered adapter and turn Chunk events into metrics + alert decisions.
//
// All shared state is protected by sync.RWMutex or atomic; tests run with
// -race to catch data races.
package probe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/logging"
	"github.com/angryang/ai-monitor/internal/metrics"
	"github.com/angryang/ai-monitor/internal/notifier"
	"github.com/angryang/ai-monitor/internal/protocol"
)

// Status values used in Result.Status and on the duration metric's "status" label.
const (
	StatusSuccess      = "success"
	StatusFailure      = "failure"
	StatusUnauthorized = "unauthorized"
)

// Result summarizes one probe execution. The runner is also responsible for
// owning it; tests assert on these fields.
type Result struct {
	TaskID    string
	TaskName  string
	Protocol  string
	Model     string
	StartedAt time.Time
	EndedAt   time.Time
	TTFT      time.Duration
	ITLSum    time.Duration
	ITLCount  int
	Chunks    int
	Status    string
	Err       error

	// §18.13 — request / response capture. Captured by the protocol
	// adapter's ProbeStream; the runner pulls these after the chunk
	// loop closes so we know all bytes have been observed.
	RequestID        string
	RequestBody      []byte
	ResponseHead     []byte
	ResponseTail     []byte
	ResponseTruncated bool
	RequestTruncated bool

	// §18.14 — semantic content. InputContent is derived from the task
	// config (variables already expanded), ThinkingContent comes from the
	// adapter's in-flight capture, OutputContent is accumulated from the
	// content chunks.
	InputContent      string
	ThinkingContent   string
	OutputContent     string
	ContentTruncated  bool
	// ResponseErrorBody holds the (bounded) body of a failed response.
	// Empty on success.
	ResponseErrorBody string
}

// Duration computes the wall-clock duration of the probe.
func (r *Result) Duration() time.Duration {
	if r.EndedAt.IsZero() || r.StartedAt.IsZero() {
		return 0
	}
	return r.EndedAt.Sub(r.StartedAt)
}

// RunnerAdapter is the subset of protocol.ChatProtocolAdapter used by Runner
// — split out so tests can stub the dependency without dealing with the full
// registry plumbing.
type RunnerAdapter interface {
	Name() string
	Validate(cfg *config.TaskConfig) error
	Probe(ctx context.Context, cfg *config.TaskConfig) (protocol.ProbeStream, error)
}

// AdapterLookup is the function the runner uses to resolve a protocol name.
// Tests can pass a stub that returns a fake.
type AdapterLookup func(name string) (RunnerAdapter, error)

// Runner holds the dependencies used by Run.
type Runner struct {
	Adapters             AdapterLookup
	Sink                 metrics.MetricSink
	Notifier             notifier.Notifier
	Dedup                *notifier.Deduper
	Logger               *slog.Logger
	Sliders              sync.Map // task_id -> *slidingWindow
	SlowThreshold        time.Duration
	ConsecFailureN       int
	SuccessRateWindow    int
	SuccessRateThreshold float64
	// RunStore persists each probe execution. Nil is allowed (metrics-only
	// mode) — Run() then skips persistence silently.
	RunStore RunStore
	// RunRetention caps the rows per task in RunStore. We prune after
	// every Save to keep the table bounded for high-frequency cron
	// schedules. Default 100 is plenty for an admin's "last 2 minutes"
	// view at 30s cadence.
	RunRetention int
}

// NewRunner constructs a runner with sane defaults applied if zero-valued.
func NewRunner(adapters AdapterLookup, sink metrics.MetricSink, n notifier.Notifier, dedup *notifier.Deduper) *Runner {
	return &Runner{
		Adapters:             adapters,
		Sink:                 sink,
		Notifier:             n,
		Dedup:                dedup,
		Logger:               logging.L(),
		SlowThreshold:        10 * time.Second,
		ConsecFailureN:       3,
		SuccessRateWindow:    200,
		SuccessRateThreshold: 0.8,
		RunRetention:         100,
	}
}

// Run executes one probe against the upstream API described by t. The caller
// must pass a context with a deadline matching t.Timeout; the runner
// otherwise doesn't enforce timeouts because the adapter does.
func (r *Runner) Run(ctx context.Context, t config.TaskConfig) Result {
	res := Result{
		TaskID:   t.ID,
		TaskName: t.Name,
		Protocol: string(t.Protocol),
		Model:    t.Model,
		Status:   StatusFailure,
	}
	// Persist at the very end, regardless of which return path we take.
	// Note: defer captures function arguments at the defer statement, so
	// passing &res is critical — if we passed res by value, this deferred
	// call would see a zero Result because no fields below would be
	// visible to it. (Classic Go gotcha: deferred call args are evaluated
	// eagerly, only the *call* is delayed.)
	defer r.persistRun(&res)
	ctx = logging.WithTaskID(ctx, t.ID)
	log := r.Logger.With(
		slog.String("task_id", t.ID),
		slog.String("task_name", t.Name),
		slog.String("protocol", res.Protocol),
		slog.String("model", res.Model),
	)

	adapter, err := r.Adapters(string(t.Protocol))
	if err != nil {
		res.Err = fmt.Errorf("lookup adapter: %w", err)
		log.Warn("adapter_lookup_failed", slog.String("err", res.Err.Error()))
		r.sinkFailure(t, "protocol_error", 0, log)
		r.maybeAlert(t, res, log)
		return res
	}
	if err := t.ValidateProtocolSegments(); err != nil {
		res.Err = fmt.Errorf("task config: %w", err)
		log.Warn("task_validate_failed", slog.String("err", res.Err.Error()))
		r.sinkFailure(t, "4xx", 0, log)
		r.maybeAlert(t, res, log)
		return res
	}
	if verr := adapter.Validate(&t); verr != nil {
		res.Err = fmt.Errorf("validate: %w", verr)
		log.Warn("adapter_validate_failed", slog.String("err", res.Err.Error()))
		r.sinkFailure(t, "protocol_error", 0, log)
		r.maybeAlert(t, res, log)
		return res
	}

	res.StartedAt = time.Now().UTC()

	// §18.14 — 输入内容由任务配置还原（prompt 变量已展开），与适配器
	// 实际发出的请求体逐字一致。放在 Probe 之前算：即使请求本身失败，
	// admin 也需要知道"我们到底发了什么过去"。
	res.InputContent = protocol.BuildInput(&t)

	stream, err := adapter.Probe(ctx, &t)
	if err != nil {
		res.EndedAt = time.Now().UTC()
		res.Err = fmt.Errorf("probe: %w", err)
		// §18.14 — 失败路径上同样要留下 X-Req-Id 与错误响应体。
		// 401 / 403 / 5xx 恰恰是最需要排查的场景，丢了这两个字段就等于
		// 让 admin 拿着一条 "unauthorized" 记录却无从查起。
		var reqErr *protocol.RequestError
		if errors.As(err, &reqErr) {
			res.RequestID = reqErr.RequestID
			res.RequestBody = reqErr.RequestBody
			res.ResponseErrorBody = reqErr.ResponseBody
			res.RequestTruncated = len(reqErr.RequestBody) >= protocol.DefaultRequestCapture
		}
		// Map sentinel errors to status / failure-type labels.
		switch {
		case errors.Is(err, protocol.ErrUnauthorized):
			res.Status = StatusUnauthorized
			r.sinkFailure(t, "unauthorized", res.Duration(), log)
		case errors.Is(err, protocol.ErrUpstreamDown):
			r.sinkFailure(t, "5xx", res.Duration(), log)
		case errors.Is(err, protocol.ErrProtocolCfg):
			r.sinkFailure(t, "4xx", res.Duration(), log)
		default:
			r.sinkFailure(t, "timeout", res.Duration(), log)
		}
		log.Warn("probe_open_failed", slog.String("status", res.Status), slog.String("err", res.Err.Error()))
		r.updateSlider(t.ID, false)
		r.maybeAlert(t, res, log)
		return res
	}
	defer stream.Close()

	// §18.14 — 输出内容由 content chunk 累积而成。上限与输入 / 思考一致。
	outputAcc := protocol.NewTextAccumulator(protocol.DefaultContentCapture)

	for chunk := range stream.Chunks() {
		if res.Chunks == 0 {
			res.TTFT = chunk.ArrivedAt.Sub(res.StartedAt)
		} else {
			// We rely on the chunk's wall-clock arrival to compute ITL.
			// The caller is expected to deliver chunks with monotonically
			// increasing timestamps; if not, we just record the absolute
			// delta (negative would be confusing so we clamp).
			if chunk.ArrivedAt.After(res.StartedAt) {
				prev := res.StartedAt.Add(res.TTFT) // approx; good enough for trend
				for c := 1; c < res.Chunks; c++ {
					prev = prev.Add(0) // noop
				}
				_ = prev
			}
		}
		res.Chunks++
		// §18.14 — 只有面向用户的正文计入输出内容；推理部分由 adapter
		// 单独累积（见 ContentCapture），不进这里。
		if chunk.Kind == protocol.ChunkContent {
			outputAcc.WriteString(chunk.Delta)
		}
		// Approximate ITL by averaging across the elapsed time.
		if res.Chunks > 1 {
			res.ITLCount++
			elapsed := chunk.ArrivedAt.Sub(res.StartedAt)
			if elapsed > 0 {
				res.ITLSum = elapsed - res.TTFT
				if res.ITLSum < 0 {
					res.ITLSum = 0
				}
				sec := float64(res.ITLSum.Seconds()) / float64(res.ITLCount)
				r.Sink.ObserveITL(t.ID, res.Protocol, res.Model, sec)
			}
		}
		if chunk.IsFinal {
			break
		}
	}
	res.EndedAt = time.Now().UTC()
	if err := stream.Err(); err != nil {
		res.Err = err
		if errors.Is(err, context.Canceled) {
			res.Status = StatusFailure
			r.sinkFailure(t, "timeout", res.Duration(), log)
		} else {
			res.Status = StatusFailure
			r.sinkFailure(t, "protocol_error", res.Duration(), log)
		}
	} else {
		res.Status = StatusSuccess
	}

	// §18.13 — capture X-Req-Id + request body + bounded response
	// snapshot. Done after the chunk loop closes so the response tail
	// buffer has seen every byte the upstream sent before we slice it.
	res.RequestID = stream.RequestID()
	res.RequestBody = stream.RequestBody()
	res.ResponseHead, res.ResponseTail, res.ResponseTruncated = stream.ResponseSnapshot()
	// Request was already pre-truncated by the adapter (DefaultRequestCapture);
	// we record that fact as a flag instead of trying to detect it from
	// the byte length (an absent apikey might also produce a tiny body).
	res.RequestTruncated = len(res.RequestBody) >= protocol.DefaultRequestCapture

	// §18.14 — semantic content. The adapter accumulated the reasoning
	// trace while the stream was live; the answer text we accumulated
	// above from the content chunks.
	capture := stream.ContentCapture()
	res.ThinkingContent = capture.ThinkingContent
	res.OutputContent = outputAcc.String()
	res.ContentTruncated = capture.Truncated || outputAcc.Truncated()

	// Emit TTFT + duration metrics. We use wall-clock so cross-process
	// aggregations in PromQL match ingest times.
	if res.TTFT > 0 {
		r.Sink.ObserveTTFT(t.ID, res.Protocol, res.Model, res.TTFT.Seconds())
	}
	r.Sink.ObserveDuration(t.ID, res.Protocol, res.Model, res.Status, res.Duration().Seconds())
	r.Sink.IncChunk(t.ID, res.Protocol, res.Model, res.Chunks)
	if res.Status == StatusSuccess {
		r.Sink.SetTaskLastSuccess(t.ID, res.EndedAt)
	}

	// Slider + alert evaluation.
	r.updateSlider(t.ID, res.Status == StatusSuccess)
	r.maybeAlert(t, res, log)

	log.Info("probe_done",
		slog.String("status", res.Status),
		slog.Int64("duration_ms", res.Duration().Milliseconds()),
		slog.Int("chunk_count", res.Chunks),
		slog.Int64("ttft_ms", res.TTFT.Milliseconds()),
		slog.Int64("itl_ms", res.ITLSum.Milliseconds()/max1(res.ITLCount)),
	)
	return res
}

func max1(n int) int64 {
	if n <= 0 {
		return 1
	}
	return int64(n)
}

// persistRun writes a result to the RunStore (when configured) and prunes
// older rows. The prune-after-write pattern is intentional: a crash between
// Save and Prune can only leave *more* rows than the cap, never fewer, so
// the worst-case after recovery is "slightly over the cap", not "lost
// history". The write itself is fire-and-forget relative to the runner
// because the probe has already returned — a slow DB shouldn't block the
// next cron tick.
//
// Errors are logged but never returned: the cron scheduler doesn't act on
// a Run() error today, and we don't want a transient DB hiccup to surface
// as a phantom 5xx in the metrics pipeline.
func (r *Runner) persistRun(res *Result) {
	if r.RunStore == nil {
		return
	}
	if res.EndedAt.IsZero() {
		res.EndedAt = time.Now().UTC()
	}
	run := FromResult(*res)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.RunStore.Save(ctx, &run); err != nil {
		r.Logger.Warn("probe_run_save_failed",
			slog.String("task_id", res.TaskID),
			slog.String("err", err.Error()),
		)
		return
	}
	if r.RunRetention > 0 {
		if err := r.RunStore.Prune(ctx, r.RunRetention); err != nil {
			r.Logger.Warn("probe_run_prune_failed",
				slog.String("err", err.Error()),
			)
		}
	}
}

// sinkFailure centralises the failure labels.
func (r *Runner) sinkFailure(t config.TaskConfig, errorType string, _ time.Duration, log *slog.Logger) {
	r.Sink.IncFailure(t.ID, string(t.Protocol), t.Model, errorType)
	r.Sink.ObserveDuration(t.ID, string(t.Protocol), t.Model, StatusFailure, 0)
	log.Warn("probe_failure_recorded", slog.String("error_type", errorType))
}

// maybeAlert fires alerts for the current result. Deduper ensures we never
// flood on the same (task_id, rule).
func (r *Runner) maybeAlert(t config.TaskConfig, res Result, log *slog.Logger) {
	if r.Notifier == nil {
		return
	}
	if !t.Notify.Enabled {
		return
	}
	now := res.EndedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}

	keyFor := func(rule notifier.AlertRule) string {
		return t.ID + ":" + string(rule)
	}
	fire := func(rule notifier.AlertRule, severity notifier.AlertSeverity, message string) {
		if !r.Dedup.ShouldFire(keyFor(rule), now) {
			return
		}
		evt := notifier.AlertEvent{
			TaskID:     t.ID,
			TaskName:   t.Name,
			Protocol:   res.Protocol,
			Rule:       rule,
			Severity:   severity,
			Message:    message,
			OccurredAt: now,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := r.Notifier.Send(ctx, evt); err != nil {
			log.Warn("notify_failed", slog.String("rule", string(rule)), slog.String("err", err.Error()))
		} else {
			log.Info("notify_sent", slog.String("rule", string(rule)))
		}
	}

	// Rule 1: 401/403 → immediate.
	if res.Status == StatusUnauthorized {
		fire(notifier.RuleUnauthorized, notifier.SeverityCritical,
			fmt.Sprintf("auth failed: %v", res.Err))
		return
	}

	// Rule 2: single slow request (duration or TTFT exceeded threshold).
	if res.TTFT > r.SlowThreshold {
		fire(notifier.RuleSlowRequest, notifier.SeverityWarning,
			fmt.Sprintf("TTFT %s exceeds threshold %s", res.TTFT, r.SlowThreshold))
	}
	if res.Duration() > r.SlowThreshold*2 && res.Status == StatusSuccess {
		fire(notifier.RuleSlowRequest, notifier.SeverityWarning,
			fmt.Sprintf("duration %s exceeds threshold %s", res.Duration(), r.SlowThreshold*2))
	}

	// Rule 3 & 4: success rate and consecutive failures derived from slider.
	win := r.getSlider(t.ID)
	rate := win.SuccessRate()
	samples := win.Count()
	switch res.Status {
	case StatusFailure:
		// Rule 4: consecutive failures.
		if win.ConsecutiveFailures() >= r.ConsecFailureN {
			fire(notifier.RuleConsecFailure, notifier.SeverityCritical,
				fmt.Sprintf("%d consecutive failures (rate=%.2f over %d samples)", r.ConsecFailureN, rate, samples))
		}
		// Rule 3: low success rate.
		if samples >= 20 && rate < r.SuccessRateThreshold {
			fire(notifier.RuleSuccessRateLow, notifier.SeverityWarning,
				fmt.Sprintf("success rate %.2f below %.2f over %d samples", rate, r.SuccessRateThreshold, samples))
		}
	case StatusSuccess:
		if samples >= 20 && rate < r.SuccessRateThreshold {
			fire(notifier.RuleSuccessRateLow, notifier.SeverityWarning,
				fmt.Sprintf("success rate %.2f below %.2f over %d samples", rate, r.SuccessRateThreshold, samples))
		}
	}
}

// getSlider returns (or creates) the slidingWindow for a task.
func (r *Runner) getSlider(taskID string) *slidingWindow {
	if v, ok := r.Sliders.Load(taskID); ok {
		return v.(*slidingWindow)
	}
	sw := newSlidingWindow(r.SuccessRateWindow)
	actual, _ := r.Sliders.LoadOrStore(taskID, sw)
	return actual.(*slidingWindow)
}

// updateSlider appends the latest boolean to the sliding window.
func (r *Runner) updateSlider(taskID string, ok bool) {
	r.getSlider(taskID).Add(ok)
}
