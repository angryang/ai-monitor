// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

// Package notifier defines the AlertEvent / Notifier contract and provides a
// shared MultiNotifier fan-out. Concrete backends (e.g. wecom) live in
// subpackages.
package notifier

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// AlertSeverity classifies the urgency of an alert.
type AlertSeverity string

const (
	SeverityInfo     AlertSeverity = "info"
	SeverityWarning  AlertSeverity = "warning"
	SeverityCritical AlertSeverity = "critical"
)

// AlertRule identifies the rule that fired. Strings stay stable for downstream
// dashboards.
type AlertRule string

const (
	RuleConsecFailure  AlertRule = "consecutive_failures"
	RuleSlowRequest    AlertRule = "slow_request"
	RuleSuccessRateLow AlertRule = "success_rate_below_80"
	RuleUnauthorized   AlertRule = "unauthorized_immediate"
)

// AlertEvent is the payload sent to a Notifier.
type AlertEvent struct {
	TaskID     string
	TaskName   string
	Protocol   string
	Rule       AlertRule
	Severity   AlertSeverity
	Message    string
	OccurredAt time.Time
	Meta       map[string]string
}

// Notifier is the interface implemented by every channel backend.
type Notifier interface {
	Name() string
	Send(ctx context.Context, evt AlertEvent) error
}

// MultiNotifier fans out events to multiple backends. Errors are returned as
// the first non-nil backend error but all backends are tried.
type MultiNotifier struct {
	notifiers []Notifier
}

// NewMultiNotifier wraps a list of notifiers.
func NewMultiNotifier(notifiers ...Notifier) *MultiNotifier {
	return &MultiNotifier{notifiers: notifiers}
}

// Send runs every notifier in sequence. Returns nil if all succeed; otherwise
// the first non-nil error joined via errors.Join (Go 1.20+).
func (m *MultiNotifier) Send(ctx context.Context, evt AlertEvent) error {
	if m == nil {
		return nil
	}
	var errs []error
	for _, n := range m.notifiers {
		if err := n.Send(ctx, evt); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", n.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// Name returns a comma-joined list.
func (m *MultiNotifier) Name() string {
	if m == nil || len(m.notifiers) == 0 {
		return "multi"
	}
	names := make([]string, 0, len(m.notifiers))
	for _, n := range m.notifiers {
		names = append(names, n.Name())
	}
	return "multi[" + names[0] + "]" // admin/log only
}
