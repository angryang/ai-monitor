// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"testing"
	"time"
)

// testContext gives every test a 1-second deadline-bounded context. The helper
// lets timeout-sensitive tests (e.g. TestShutdown_*) avoid hanging the suite.
func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	t.Cleanup(cancel)
	return ctx
}
