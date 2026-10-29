// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package config

import (
	"context"
	"errors"
	"time"
)

// EventType distinguishes upsert vs delete events emitted by Watch.
type EventType string

const (
	EventUpsert EventType = "upsert"
	EventDelete EventType = "delete"
)

// Event is the payload pushed on the Watch channel.
type Event struct {
	Type   EventType
	TaskID string
	At     time.Time
}

// ConfigStore owns persistence of TaskConfig rows. SQLite is the default
// implementation; the interface here is the contract store_sqlite.go fulfils
// and tests rely on.
type ConfigStore interface {
	ListTasks(ctx context.Context) ([]TaskConfig, error)
	GetTask(ctx context.Context, id string) (*TaskConfig, error)
	CreateTask(ctx context.Context, t *TaskConfig) error
	UpdateTask(ctx context.Context, t *TaskConfig) error
	DeleteTask(ctx context.Context, id string) error
	Watch(ctx context.Context) (<-chan Event, error) // P1: change subscription
	Close() error
}

// LenientTaskLister is an optional extension of ConfigStore implemented by
// stores that can list tasks even when some rows fail to decrypt.
//
// It exists because a master key mismatch (rotated key, DB copied from
// another instance) would otherwise make ListTasks fail outright and take
// the whole service down — with no way to reach the admin UI and repair the
// affected task.
//
// Callers use a type assertion, so ConfigStore itself stays unchanged and
// test doubles don't have to implement the extra method:
//
//	if ll, ok := store.(config.LenientTaskLister); ok {
//	    tasks, secretErrs, err := ll.ListTasksLenient(ctx)
//	    // secretErrs holds one entry per task whose secrets failed to decrypt
//	    // err is non-nil only when the listing itself failed
//	}
//
// Those tasks are still returned, with TaskConfig.SecretError set and the
// secrets left empty.
//
// The third return value separates "some rows are broken" (secretErrs, the
// listing is still usable) from "the listing failed" (err). Callers MUST
// propagate err instead of continuing with an empty task list: an empty
// list is indistinguishable from "the user has no tasks", so swallowing the
// error makes every task look like it silently vanished.
type LenientTaskLister interface {
	ListTasksLenient(ctx context.Context) ([]TaskConfig, []error, error)
}

// LenientTaskGetter is the single-row counterpart of LenientTaskLister.
type LenientTaskGetter interface {
	GetTaskLenient(ctx context.Context, id string) (*TaskConfig, error)
}

// Sentinel errors returned by ConfigStore implementations. Callers MUST
// use errors.Is/As to match them.
var (
	ErrNotFound      = errors.New("config: not found")
	ErrAlreadyExists = errors.New("config: already exists")
	ErrInvalidConfig = errors.New("config: invalid")
)
