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
	StartedAt time.Time `gorm:"not null;index" json:"started_at"`
	EndedAt   time.Time `gorm:"not null" json:"ended_at"`
	DurationMS int64 `gorm:"not null" json:"duration_ms"`
	TTFTMS     int64 `json:"ttft_ms"`        // 0 if no chunk arrived
	ITLAvgMS   int64 `json:"itl_avg_ms"`     // 0 if no inter-chunk samples
	ITLCount   int   `json:"itl_count"`
	Chunks     int   `gorm:"not null" json:"chunks"`

	// §18.13 — captured request / response context. Indexed on
	// RequestID so a future "find by upstream trace id" query is cheap.
	// All four byte-arrays are bounded (DefaultRequestCapture + head/tail
	// RespCapture). The two `Truncated` flags are debug aids so the UI
	// can surface "… N bytes truncated …" without recomputing from
	// upstream counters.
	RequestID        string `gorm:"size:128;index" json:"request_id,omitempty"`
	RequestBody      string `gorm:"type:text" json:"request_body,omitempty"`
	ResponseHead     string `gorm:"type:text" json:"response_head,omitempty"`
	ResponseTail     string `gorm:"type:text" json:"response_tail,omitempty"`
	RequestTruncated bool   `gorm:"not null;default:false" json:"request_truncated"`
	ResponseTruncated bool  `gorm:"not null;default:false" json:"response_truncated"`

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
		TaskID:    r.TaskID,
		TaskName:  r.TaskName,
		Protocol:  r.Protocol,
		Model:     r.Model,
		Status:    r.Status,
		Err:       errStr,
		StartedAt: r.StartedAt,
		EndedAt:   r.EndedAt,
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
