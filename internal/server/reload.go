package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/aibench/aibench/ai-monitor/internal/config"
)

// WatchReloader is the public interface of the hot-reload coordinator.
type WatchReloader interface {
	Run(ctx context.Context)
	Stop()
}

// TaskLister exposes the subset of config.ConfigStore used by the
// coordinator. We keep it local to avoid creating a new import edge in this
// package or pulling the scheduler package into config.
type TaskLister interface {
	ListTasks(ctx context.Context) ([]config.TaskConfig, error)
	Watch(ctx context.Context) (<-chan config.Event, error)
}

// ReloadFunc is invoked once per config event; the reloder decides how to
// diff and apply.
type ReloadFunc func(ctx context.Context, tasks []config.TaskConfig) error

// relCoordinator implements WatchReloader by bridging the Watch channel and
// a user-supplied ReloadFunc.
type relCoordinator struct {
	store      TaskLister
	reload     ReloadFunc
	log        *slog.Logger
	stopOnce   sync.Once
	stopCh     chan struct{}
	doneCh     chan struct{}
	mu         sync.Mutex
	cancelCtx  context.CancelFunc
}

// NewWatchReloader wires the store to a reload callback.
func NewWatchReloader(store TaskLister, reload ReloadFunc, log *slog.Logger) WatchReloader {
	if log == nil {
		log = slog.Default()
	}
	if reload == nil {
		reload = func(_ context.Context, _ []config.TaskConfig) error { return nil }
	}
	r := &relCoordinator{
		store:  store,
		reload: reload,
		log:    log,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	return r
}

// Run blocks running the watch loop until ctx is cancelled or Stop is called.
func (r *relCoordinator) Run(ctx context.Context) {
	wctx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.cancelCtx = cancel
	r.mu.Unlock()
	ch, err := r.store.Watch(wctx)
	if err != nil {
		r.log.Warn("watch_open_failed", slog.String("err", err.Error()))
		close(r.doneCh)
		return
	}
	defer close(r.doneCh)
	for {
		select {
		case <-wctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			r.applyEvent(wctx, ev)
		}
	}
}

// applyEvent re-fetches the full task list and hands it to the reload
// callback.
func (r *relCoordinator) applyEvent(ctx context.Context, ev config.Event) {
	t0 := time.Now()
	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tasks, err := r.store.ListTasks(listCtx)
	if err != nil {
		r.log.Warn("reload_list_failed",
			slog.String("event", string(ev.Type)),
			slog.String("task_id", ev.TaskID),
			slog.String("err", err.Error()))
		return
	}
	if err := r.reload(ctx, tasks); err != nil {
		r.log.Warn("reload_apply_failed",
			slog.String("event", string(ev.Type)),
			slog.String("task_id", ev.TaskID),
			slog.String("err", err.Error()))
		return
	}
	r.log.Info("reload_done",
		slog.String("event", string(ev.Type)),
		slog.String("task_id", ev.TaskID),
		slog.Int("task_count", len(tasks)),
		slog.Int64("duration_ms", time.Since(t0).Milliseconds()),
	)
}

// Stop terminates the run loop.
func (r *relCoordinator) Stop() {
	r.stopOnce.Do(func() {
		r.mu.Lock()
		if r.cancelCtx != nil {
			r.cancelCtx()
		}
		r.mu.Unlock()
	})
}
