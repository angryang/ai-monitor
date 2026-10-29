// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angryang/ai-monitor/internal/config"
)

// failingLenientStore is a ConfigStore whose *lenient* listing fails hard —
// the "the database is unreachable / the row cannot be decoded" case.
//
// It implements config.LenientTaskLister so the handlers take the lenient
// branch; the point of the test is that a hard failure there must not be
// reported to the browser as "you have no tasks".
type failingLenientStore struct {
	*memStore
	listErr error
}

func (f *failingLenientStore) ListTasksLenient(context.Context) ([]config.TaskConfig, []error, error) {
	return nil, nil, f.listErr
}

// TestListTasks_ListFailureIsNotAnEmptyList is the regression test for
// "前端查询不到已经创建的任务".
//
// The failure mode: when the listing failed, the handler swallowed the error
// and answered 200 with an empty task list. The UI renders an empty list
// exactly like "you have no tasks", so every task silently vanished from
// the admin UI with no error anywhere. A failed listing must answer 500.
func TestListTasks_ListFailureIsNotAnEmptyList(t *testing.T) {
	store := &failingLenientStore{
		memStore: newMemStore(t, seedTask("alpha", true)),
		listErr:  errors.New("config: list: database is locked"),
	}
	h := NewAdminTaskHandlers(&AdminTaskHandlersConfig{
		Store:  store,
		Logger: func(string, ...any) {},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/tasks", nil)
	h.list(rec, req)
	if rec.Code != 500 {
		t.Fatalf("status: got %d, want 500 — a failed listing must not look like an empty list", rec.Code)
	}
}

// failingLenientLister is the reload-side twin of failingLenientStore.
type failingLenientLister struct {
	fakeLister
}

func (f *failingLenientLister) ListTasksLenient(context.Context) ([]config.TaskConfig, []error, error) {
	return nil, nil, errors.New("config: list: database is locked")
}

// TestWatchReloader_LenientListFailureKeepsSchedule guards the scheduler.
//
// When the lenient listing failed, the coordinator used to continue with an
// empty task list and hand it to the reload callback — which unschedules
// every probe in the instance. A failed listing must abort the reload and
// leave the previous schedule untouched.
func TestWatchReloader_LenientListFailureKeepsSchedule(t *testing.T) {
	ch := make(chan config.Event, 1)
	fl := &failingLenientLister{fakeLister{ch: ch}}
	reloadCalls := atomic.Int32{}
	r := NewWatchReloader(fl, func(_ context.Context, _ []config.TaskConfig) error {
		reloadCalls.Add(1)
		return nil
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	ch <- config.Event{Type: config.EventUpsert, TaskID: "t-1", At: time.Now().UTC()}
	time.Sleep(100 * time.Millisecond)
	if reloadCalls.Load() != 0 {
		t.Fatalf("reload must not run with an empty list when the listing failed; got %d calls", reloadCalls.Load())
	}
	r.Stop()
}
