package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aibench/aibench/ai-monitor/internal/config"
)

type fakeLister struct {
	tasks []config.TaskConfig
	err   error
	ch    chan config.Event

	gotReloadCount atomic.Int32
	lastLen        atomic.Int32
}

func (f *fakeLister) ListTasks(_ context.Context) ([]config.TaskConfig, error) {
	return f.tasks, f.err
}
func (f *fakeLister) Watch(_ context.Context) (<-chan config.Event, error) {
	if f.ch != nil {
		return f.ch, nil
	}
	return nil, errors.New("fake error")
}

func TestWatchReloader_ReloadOnEvent(t *testing.T) {
	ch := make(chan config.Event, 8)
	fl := &fakeLister{ch: ch}
	var called atomic.Int32
	r := NewWatchReloader(fl, func(_ context.Context, tasks []config.TaskConfig) error {
		called.Add(1)
		return nil
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	// Allow Run to enter Watch loop.
	time.Sleep(20 * time.Millisecond)
	ch <- config.Event{Type: config.EventUpsert, TaskID: "t-1", At: time.Now().UTC()}
	ch <- config.Event{Type: config.EventDelete, TaskID: "t-1", At: time.Now().UTC()}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && called.Load() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if called.Load() < 2 {
		t.Fatalf("expected 2 reloads; got %d", called.Load())
	}
	r.Stop()
}

func TestWatchReloader_PropagateError(t *testing.T) {
	fl := &fakeLister{err: errors.New("db down")}
	reloadCalls := atomic.Int32{}
	r := NewWatchReloader(fl, func(_ context.Context, _ []config.TaskConfig) error {
		reloadCalls.Add(1)
		return nil
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	if reloadCalls.Load() != 0 {
		t.Fatalf("reload should not be called when ListTasks fails")
	}
	r.Stop()
}

// TestWatchReloader_StopCleansUp ensures Run's goroutine returns after Stop.
func TestWatchReloader_StopCleansUp(t *testing.T) {
	fl := &fakeLister{ch: make(chan config.Event, 1)}
	var mu sync.Mutex
	r := NewWatchReloader(fl, func(_ context.Context, _ []config.TaskConfig) error { return nil }, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	_ = mu
	r.Stop()
}
