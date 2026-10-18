package scheduler

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angryang/ai-monitor/internal/config"
)

func TestScheduler_AddRemove(t *testing.T) {
	var ran atomic.Int32
	s := New(Options{
		MaxConcurrent: 1,
		Handler: func(_ context.Context, _ config.TaskConfig) {
			ran.Add(1)
		},
	})
	if err := s.Add(config.TaskConfig{ID: "t1", Schedule: "@every 100ms"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Remove("t1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Stop after no firings; just confirm Remove is silent on missing id.
	if err := s.Remove("nope"); err != nil {
		t.Fatalf("Remove unknown: %v", err)
	}
}

func TestScheduler_FiresEntry(t *testing.T) {
	var fired atomic.Int32
	handler := func(_ context.Context, _ config.TaskConfig) {
		fired.Add(1)
	}
	s := New(Options{
		MaxConcurrent: 4,
		Handler:       handler,
	})
	if err := s.Add(config.TaskConfig{ID: "t1", Schedule: "@every 50ms"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && fired.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	_ = s.Stop(context.Background())
	if fired.Load() < 1 {
		t.Fatalf("expected >=1 fire, got %d", fired.Load())
	}
}

func TestScheduler_MaxConcurrent(t *testing.T) {
	// Track peak in-flight goroutines using a channel + waitgroup.
	var inFlight atomic.Int32
	peak := atomic.Int32{}
	gate := make(chan struct{})
	var wg sync.WaitGroup
	handler := func(_ context.Context, _ config.TaskConfig) {
		cur := inFlight.Add(1)
		for {
			p := peak.Load()
			if cur <= p {
				break
			}
			if peak.CompareAndSwap(p, cur) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		inFlight.Add(-1)
		<-gate
	}
	s := New(Options{
		MaxConcurrent: 2,
		Handler:       handler,
	})
	for i := 0; i < 5; i++ {
		_ = s.Add(config.TaskConfig{ID: "x", Schedule: "@every 10ms"})
	}
	_ = s.Start(context.Background())
	time.Sleep(100 * time.Millisecond)
	_ = s.Stop(context.Background())
	close(gate)
	wg.Wait()
	if peak.Load() > 2 {
		t.Fatalf("peak in-flight %d exceeds MaxConcurrent=2", peak.Load())
	}
}

func TestScheduler_Reload(t *testing.T) {
	called := atomic.Int32{}
	s := New(Options{
		MaxConcurrent: 4,
		Handler: func(_ context.Context, _ config.TaskConfig) {
			called.Add(1)
		},
	})
	tasks := []config.TaskConfig{
		{ID: "a", Schedule: "@every 50ms"},
		{ID: "b", Schedule: "@every 50ms"},
	}
	if err := s.Reload(context.Background(), tasks); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	// Reload should produce no error; removing b should leave only a.
	if err := s.Reload(context.Background(), []config.TaskConfig{{ID: "a", Schedule: "@every 50ms"}}); err != nil {
		t.Fatalf("Reload drop: %v", err)
	}
	if err := s.Remove("a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := s.Remove("b"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

func TestScheduler_InvalidCron(t *testing.T) {
	s := New(Options{MaxConcurrent: 1, Handler: func(_ context.Context, _ config.TaskConfig) {}})
	err := s.Add(config.TaskConfig{ID: "t1", Schedule: "not a valid cron"})
	if err == nil {
		t.Fatalf("expected error for invalid cron")
	}
	if !errors.Is(err, err) {
		t.Logf("got error: %v", err)
	}
}

func TestScheduler_JitterApplied(t *testing.T) {
	// We can't measure jitter directly, but ensure the call still happens.
	called := atomic.Int32{}
	s := New(Options{
		MaxConcurrent: 4,
		Jitter:        100 * time.Millisecond,
		Handler: func(_ context.Context, _ config.TaskConfig) {
			called.Add(1)
		},
	})
	if err := s.Add(config.TaskConfig{ID: "x", Schedule: "@every 50ms"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_ = s.Start(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && called.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	_ = s.Stop(context.Background())
	if called.Load() < 1 {
		t.Fatalf("expected at least one fire; got %d", called.Load())
	}
}

func TestScheduler_Add_DisabledIsNoOp(t *testing.T) {
	var fired atomic.Int32
	s := New(Options{
		MaxConcurrent: 1,
		Handler: func(_ context.Context, _ config.TaskConfig) {
			fired.Add(1)
		},
	})
	// Enabled=false → Add silently returns nil; the task must NEVER fire.
	if err := s.Add(config.TaskConfig{ID: "off", Schedule: "@every 50ms", Enabled: false}); err != nil {
		t.Fatalf("Add disabled: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	_ = s.Stop(context.Background())
	if fired.Load() != 0 {
		t.Fatalf("disabled task fired %d times; want 0", fired.Load())
	}
}

func TestScheduler_Reload_DisabledSkipped(t *testing.T) {
	s := New(Options{
		MaxConcurrent: 4,
		Handler:       func(_ context.Context, _ config.TaskConfig) {},
	})
	tasks := []config.TaskConfig{
		{ID: "on", Schedule: "@every 50ms", Enabled: true},
		{ID: "off", Schedule: "@every 50ms", Enabled: false},
		{ID: "also-off", Schedule: "@every 50ms", Enabled: false},
	}
	if err := s.Reload(context.Background(), tasks); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	cs := s.(*cronScheduler)
	ids := cs.entryIDs()
	sort.Strings(ids)
	want := []string{"on"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("entryIDs() = %v, want %v", ids, want)
	}
}

func TestScheduler_Reload_DisablingActiveRetracts(t *testing.T) {
	s := New(Options{
		MaxConcurrent: 4,
		Handler:       func(_ context.Context, _ config.TaskConfig) {},
	})
	// Step 1: enabled.
	if err := s.Reload(context.Background(), []config.TaskConfig{
		{ID: "alpha", Schedule: "@every 50ms", Enabled: true},
	}); err != nil {
		t.Fatalf("Reload step1: %v", err)
	}
	cs := s.(*cronScheduler)
	if got := cs.entryIDs(); len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("step1 entryIDs() = %v, want [alpha]", got)
	}
	// Step 2: flip to disabled.
	if err := s.Reload(context.Background(), []config.TaskConfig{
		{ID: "alpha", Schedule: "@every 50ms", Enabled: false},
	}); err != nil {
		t.Fatalf("Reload step2: %v", err)
	}
	if got := cs.entryIDs(); len(got) != 0 {
		t.Fatalf("step2 entryIDs() = %v, want [] (disabled task should be retracted)", got)
	}
}
