// Package scheduler owns cron-driven execution of probe tasks. It wraps
// robfig/cron/v3 with a weighted semaphore so concurrent tasks respect
// MaxConcurrent, and applies random jitter so clusters don't thunder.
package scheduler

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/aibench/aibench/ai-monitor/internal/config"
	"github.com/robfig/cron/v3"
	"golang.org/x/sync/semaphore"
)

// JobHandler is invoked whenever a cron tick fires for a task. The scheduler
// guarantees MaxConcurrent at the handler entry point.
type JobHandler func(ctx context.Context, task config.TaskConfig)

// Scheduler is the public surface used by other packages.
type Scheduler interface {
	Start(ctx context.Context) error
	Reload(ctx context.Context, tasks []config.TaskConfig) error
	Add(task config.TaskConfig) error
	Remove(taskID string) error
	Stop(ctx context.Context) error
}

// Options configures the cronScheduler.
type Options struct {
	Handler       JobHandler
	MaxConcurrent int
	Jitter        time.Duration
	Logger        LoggerFunc
	Now           func() time.Time // overrides for tests
	NewRandSource func() *rand.Rand
}

// LoggerFunc matches slog-style logging without forcing a dependency.
type LoggerFunc func(msg string, args ...any)

// DefaultNow returns wall clock UTC.
func DefaultNow() time.Time { return time.Now().UTC() }

// cronScheduler is the canonical implementation.
type cronScheduler struct {
	cron    *cron.Cron
	sem     *semaphore.Weighted
	handler JobHandler
	logger  LoggerFunc
	now     func() time.Time
	jitter  time.Duration
	mu      sync.Mutex
	entries map[string]cron.EntryID
	inflight sync.WaitGroup
}

// New constructs a Scheduler.
func New(opts Options) Scheduler {
	if opts.Handler == nil {
		opts.Handler = func(_ context.Context, _ config.TaskConfig) {}
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = 16
	}
	if opts.Jitter < 0 {
		opts.Jitter = 0
	}
	if opts.Logger == nil {
		opts.Logger = func(string, ...any) {}
	}
	if opts.Now == nil {
		opts.Now = DefaultNow
	}
	c := cron.New(cron.WithSeconds())
	return &cronScheduler{
		cron:    c,
		sem:     semaphore.NewWeighted(int64(opts.MaxConcurrent)),
		handler: opts.Handler,
		logger:  opts.Logger,
		now:     opts.Now,
		jitter:  opts.Jitter,
		entries: make(map[string]cron.EntryID),
	}
}

// Start runs the cron loop. Cancel ctx to stop the loop (Stop should also be
// called for graceful shutdown).
func (s *cronScheduler) Start(ctx context.Context) error {
	s.cron.Start()
	go func() {
		<-ctx.Done()
	}()
	return nil
}

// Stop blocks until in-flight jobs finish (or the supplied ctx is cancelled).
func (s *cronScheduler) Stop(ctx context.Context) error {
	stopCtx := s.cron.Stop()
	select {
	case <-stopCtx.Done():
	case <-ctx.Done():
	}
	s.inflight.Wait()
	return nil
}

// Add registers a single task. Panics if the cron expression is invalid.
func (s *cronScheduler) Add(task config.TaskConfig) error {
	if _, ok := s.entries[task.ID]; ok {
		return s.Remove(task.ID)
	}
	id, err := s.cron.AddFunc(task.Schedule, func() {
		s.fire(task)
	})
	if err != nil {
		return fmt.Errorf("scheduler: invalid cron expression %q: %w", task.Schedule, err)
	}
	s.mu.Lock()
	s.entries[task.ID] = id
	s.mu.Unlock()
	return nil
}

// Remove unregisters a task.
func (s *cronScheduler) Remove(taskID string) error {
	s.mu.Lock()
	id, ok := s.entries[taskID]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	delete(s.entries, taskID)
	s.mu.Unlock()
	s.cron.Remove(id)
	return nil
}

// Reload replaces the entire task set with the supplied list. Any task
// removed from the list is unregistered; new ones are added.
func (s *cronScheduler) Reload(ctx context.Context, tasks []config.TaskConfig) error {
	s.mu.Lock()
	oldIDs := make([]string, 0, len(s.entries))
	for k := range s.entries {
		oldIDs = append(oldIDs, k)
	}
	s.mu.Unlock()
	newIDs := make(map[string]struct{}, len(tasks))
	for _, t := range tasks {
		newIDs[t.ID] = struct{}{}
		if err := s.Add(t); err != nil {
			return err
		}
	}
	for _, id := range oldIDs {
		if _, keep := newIDs[id]; !keep {
			if err := s.Remove(id); err != nil {
				return err
			}
		}
	}
	return nil
}

// fire executes one cron tick. Acquires the semaphore (with jitter + ctx),
// then invokes the user handler.
func (s *cronScheduler) fire(task config.TaskConfig) {
	if s.jitter > 0 {
		jitter := time.Duration(rand.Int63n(int64(s.jitter)))
		select {
		case <-time.After(jitter):
		}
	}
	s.inflight.Add(1)
	go func() {
		defer s.inflight.Done()
		if !s.sem.TryAcquire(1) {
			s.logger("scheduler_failfast", "task_id", task.ID, "reason", "max_concurrent_reached")
			return
		}
		defer s.sem.Release(1)
		s.handler(context.Background(), task)
	}()
}

// fireSync is the test-friendly variant: blocks until the handler returns.
func (s *cronScheduler) fireSync(task config.TaskConfig) {
	if s.jitter > 0 {
		jitter := time.Duration(rand.Int63n(int64(s.jitter)))
		time.Sleep(jitter)
	}
	if !s.sem.TryAcquire(1) {
		s.logger("scheduler_failfast", "task_id", task.ID, "reason", "max_concurrent_reached")
		return
	}
	defer s.sem.Release(1)
	s.handler(context.Background(), task)
}
