// Package logging centralizes slog initialization so that every package can
// pull a single JSON-handler logger with consistent fields (time, level, msg,
// and any extras injected via WithAttrs/With).
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
)

var (
	rootOnce sync.Once
	rootLog  *slog.Logger
)

// Init builds the root JSON-handler logger exactly once for the whole process.
// Subsequent calls reuse the cached logger.
func Init() *slog.Logger {
	rootOnce.Do(func() {
		rootLog = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}))
		slog.SetDefault(rootLog)
	})
	return rootLog
}

// InitWith overrides the destination writer and minimum level; intended for
// tests that need to capture output.
func InitWith(w io.Writer, level slog.Level) *slog.Logger {
	rootLog = slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(rootLog)
	rootOnce = sync.Once{}
	rootOnce.Do(func() {})
	return rootLog
}

// L returns the cached root logger (or a default JSON one if Init was never
// called) for callers that don't want to thread a logger through DI.
func L() *slog.Logger {
	if rootLog == nil {
		return Init()
	}
	return rootLog
}

// TaskIDKey is the context-key type used by the runner to propagate task_id to
// downstream log calls. Using a private type avoids collisions with other
// contexts (per design.md §16).
type taskIDKey struct{}

// WithTaskID returns a child context tagged with the given task id.
func WithTaskID(ctx context.Context, taskID string) context.Context {
	return context.WithValue(ctx, taskIDKey{}, taskID)
}

// TaskIDFromContext retrieves the task id (empty string if unset).
func TaskIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(taskIDKey{}).(string); ok {
		return v
	}
	return ""
}

// FromContext returns a logger derived from the root, augmented with the
// task_id field if the context carries one. Falls back to the root logger.
func FromContext(ctx context.Context) *slog.Logger {
	l := L()
	if id := TaskIDFromContext(ctx); id != "" {
		return l.With(slog.String("task_id", id))
	}
	return l
}
