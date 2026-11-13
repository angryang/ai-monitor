// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

// Package probe owns the orchestration logic. This file defines the
// persistent record of a single probe execution. One ProbeRun row is
// written per Run() invocation; rows are kept capped (see retention) so the
// SQLite table doesn't grow unbounded for high-frequency cron schedules.
package probe

import (
	"context"
	"time"
)

// ProbeRun is the persisted record of one probe execution. We deliberately
// do *not* store API key / webhook material; the run describes the call,
// not the credentials.
type ProbeRun struct {
	// ID is auto-incremented by SQLite. We expose it as int64 so the admin
	// endpoint can paginate newest-first without a UUID lookup.
	ID int64 `gorm:"primaryKey;autoIncrement" json:"id"`
	// TaskID is the canonical task id (e.g. "minimax"). Indexed because
	// every admin query filters by it.
	TaskID string `gorm:"size:64;not null;index" json:"task_id"`
	// TaskName is denormalized so the run log still makes sense if the task
	// is later renamed or deleted (cascading delete would erase history).
	TaskName string `gorm:"size:128;not null" json:"task_name"`
	Protocol string `gorm:"size:32;not null" json:"protocol"`
	Model    string `gorm:"size:128;not null" json:"model"`

	// Status mirrors the probe.Result.Status string ("success" / "failure" /
	// "unauthorized"). Empty error means success; non-empty is a copy of
	// the upstream error message (trimmed to a safe length).
	Status string `gorm:"size:32;not null" json:"status"`
	Err    string `gorm:"type:text" json:"error,omitempty"`

	// Timing fields. Times are UTC. Durations are stored in milliseconds
	// so the admin endpoint can render them without time.Duration
	// conversion gymnastics.
	StartedAt  time.Time `gorm:"not null;index" json:"started_at"`
	EndedAt    time.Time `gorm:"not null" json:"ended_at"`
	DurationMS int64     `gorm:"not null" json:"duration_ms"`
	TTFTMS     int64     `json:"ttft_ms"`    // 0 if no chunk arrived
	ITLAvgMS   int64     `json:"itl_avg_ms"` // 0 if no inter-chunk samples
	ITLCount   int       `json:"itl_count"`
	Chunks     int       `gorm:"not null" json:"chunks"`

	// §18.13 — captured request / response context. Indexed on
	// RequestID so a future "find by upstream trace id" query is cheap.
	// All four byte-arrays are bounded (DefaultRequestCapture + head/tail
	// RespCapture). The two `Truncated` flags are debug aids so the UI
	// can surface "… N bytes truncated …" without recomputing from
	// upstream counters.
	RequestID         string `gorm:"size:128;index" json:"request_id,omitempty"`
	RequestBody       string `gorm:"type:text" json:"request_body,omitempty"`
	ResponseHead      string `gorm:"type:text" json:"response_head,omitempty"`
	ResponseTail      string `gorm:"type:text" json:"response_tail,omitempty"`
	RequestTruncated  bool   `gorm:"not null;default:false" json:"request_truncated"`
	ResponseTruncated bool   `gorm:"not null;default:false" json:"response_truncated"`

	// §18.14 — 语义化内容。原始字节（request_body / response_head /
	// response_tail）适合取证，但不适合"读懂这次探测到底发生了什么"。
	// 这几列把一次探测拆成 admin 能直接读的三段：
	//
	//	InputContent    — 实际发给模型的输入（system + user，变量已展开）
	//	ThinkingContent — 模型的推理过程（reasoning / thinking）
	//	OutputContent   — 模型最终的回答正文
	//
	// 三列各自受 protocol.DefaultContentCapture（16 KiB）上限约束，
	// 超出部分截断并置位 ContentTruncated。
	InputContent    string `gorm:"type:text" json:"input_content,omitempty"`
	ThinkingContent string `gorm:"type:text" json:"thinking_content,omitempty"`
	OutputContent   string `gorm:"type:text" json:"output_content,omitempty"`
	// ContentTruncated 表示三段内容中至少有一段触达上限被截断。
	ContentTruncated bool `gorm:"not null;default:false" json:"content_truncated"`
	// ResponseErrorBody 保存失败响应体（4xx/5xx）的前若干字节——上游最常
	// 把真正的原因写在这里（"Incorrect API key provided"）。成功时为空。
	ResponseErrorBody string `gorm:"type:text" json:"response_error_body,omitempty"`

	// Source 说明这次探测的来源："schedule" 来自 cron 定时调度，
	// "manual" 来自运维在「AI 请求管理」里手动触发的一次性执行。
	// 手动执行同样入历史（便于回溯），但不参与成功率滑窗、不计指标、
	// 不触发告警。
	Source string `gorm:"size:16;not null;default:schedule;index" json:"source"`
	// BatchID 标记一次批量执行；同批次的多次运行共享该值。
	BatchID string `gorm:"size:64;index" json:"batch_id,omitempty"`
	// ProviderID / AIRequestID 指向发起该次手动执行的 AI 服务提供商与
	// AI 请求。定时调度的运行这两列为空。
	//
	// 这两列取代了旧的 group_id / item_id：请求组概念已移除，旧列保留
	// 在表里只是历史数据，不再写入。
	ProviderID  string `gorm:"size:64;index" json:"provider_id,omitempty"`
	AIRequestID string `gorm:"column:ai_request_id;size:64;index" json:"ai_request_id,omitempty"`
}

// TableName pins the SQL name to `probe_runs`. We avoid GORM's default
// pluralization (`probe_runs` is already correct but being explicit
// insulates the migration from name changes).
func (ProbeRun) TableName() string { return "probe_runs" }

// FromResult snapshots a probe.Result into a persistable ProbeRun. Centralising
// the conversion keeps the runner free of GORM types and makes it trivial to
// add fields later.
func FromResult(r Result) ProbeRun {
	itlAvg := int64(0)
	if r.ITLCount > 0 {
		itlAvg = r.ITLSum.Milliseconds() / int64(r.ITLCount)
	}
	errStr := ""
	if r.Err != nil {
		errStr = r.Err.Error()
		// Truncate absurdly long errors so SQLite text columns don't grow
		// without bound. 4 KiB is plenty for any HTTP / network error
		// message we've seen in practice.
		if len(errStr) > 4096 {
			errStr = errStr[:4096]
		}
	}
	return ProbeRun{
		TaskID:     r.TaskID,
		TaskName:   r.TaskName,
		Protocol:   r.Protocol,
		Model:      r.Model,
		Status:     r.Status,
		Err:        errStr,
		StartedAt:  r.StartedAt,
		EndedAt:    r.EndedAt,
		DurationMS: r.Duration().Milliseconds(),
		TTFTMS:     r.TTFT.Milliseconds(),
		ITLAvgMS:   itlAvg,
		ITLCount:   r.ITLCount,
		Chunks:     r.Chunks,
		// §18.13 — wire capture into the persisted row. bytes↔string
		// conversion is cheap relative to the SQLite write we're about
		// to do.
		RequestID:         r.RequestID,
		RequestBody:       string(r.RequestBody),
		ResponseHead:      string(r.ResponseHead),
		ResponseTail:      string(r.ResponseTail),
		RequestTruncated:  r.RequestTruncated,
		ResponseTruncated: r.ResponseTruncated,
		// §18.14 — semantic content.
		InputContent:      r.InputContent,
		ThinkingContent:   r.ThinkingContent,
		OutputContent:     r.OutputContent,
		ContentTruncated:  r.ContentTruncated,
		ResponseErrorBody: r.ResponseErrorBody,
		// Run provenance.
		Source:      r.Source,
		BatchID:     r.BatchID,
		ProviderID:  r.ProviderID,
		AIRequestID: r.AIRequestID,
	}
}

// RunStore persists probe runs. The interface is small enough that a single
// SQLite implementation is sufficient; defining the interface lets the
// runner depend on a contract rather than a concrete type, which is what
// makes the runner testable.
type RunStore interface {
	// Save appends a new run. Implementations must never panic on duplicate
	// ids (auto-increment handles uniqueness).
	Save(ctx context.Context, run *ProbeRun) error
	// ListByTask returns the most recent runs for taskID, newest first.
	// limit must be > 0; implementations should clamp absurd values
	// (e.g. cap at 1000) to prevent runaway queries.
	ListByTask(ctx context.Context, taskID string, limit int) ([]ProbeRun, error)
	// Prune keeps only the newest `keep` runs for each task. Called after
	// Save to enforce the per-task retention cap. Idempotent.
	Prune(ctx context.Context, keep int) error
	// Close releases the underlying connection (SQLite file handle).
	Close() error
}

// ManualRunFilter narrows a listing of ad-hoc (manually triggered) runs.
// Every field is optional; the zero value lists every manual run.
type ManualRunFilter struct {
	// BatchID selects the runs of one batch execution.
	BatchID string
	// ProviderID selects the runs sent to one AI service provider.
	ProviderID string
	// AIRequestID selects the runs of one AI request.
	AIRequestID string
	// Limit caps the number of rows. Must be > 0; implementations clamp
	// absurd values (e.g. cap at 1000).
	Limit int
}

// ManualRunStore is an optional extension of RunStore implemented by stores
// that can list ad-hoc runs. Callers type-assert for it and degrade to
// "no history" when it is absent, so a store that only cares about
// scheduled runs stays valid.
type ManualRunStore interface {
	// ListManual returns the most recent manually triggered runs matching
	// the filter, newest first.
	ListManual(ctx context.Context, filter ManualRunFilter) ([]ProbeRun, error)
}
