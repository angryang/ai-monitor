// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package config

import (
	"fmt"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DefaultBusyTimeout is the SQLite busy timeout used when the caller does
// not specify one.
const DefaultBusyTimeout = 5 * time.Second

// OpenDB opens the SQLite database file and returns the raw GORM handle.
//
// The handle is shared by every store in the process — the task store, the
// provider registry, the AI request registry and the probe run history —
// so they all live in one file with one WAL and one busy-timeout.
//
// The handle is opened *before* any schema migration runs on purpose: the
// legacy monitoring-task migration has to rewrite the `tasks` table before
// GORM's AutoMigrate sees it, because AutoMigrate cannot add a NOT NULL
// column to an existing SQLite table.
func OpenDB(path string, busyTimeout time.Duration) (*gorm.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("config: sqlite path required")
	}
	if busyTimeout <= 0 {
		busyTimeout = DefaultBusyTimeout
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)", path, int(busyTimeout/time.Millisecond))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("config: sqlite open: %w", err)
	}
	return db, nil
}
