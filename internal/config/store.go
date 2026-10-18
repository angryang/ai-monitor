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

// Sentinel errors returned by ConfigStore implementations. Callers MUST
// use errors.Is/As to match them.
var (
	ErrNotFound      = errors.New("config: not found")
	ErrAlreadyExists = errors.New("config: already exists")
	ErrInvalidConfig = errors.New("config: invalid")
)
