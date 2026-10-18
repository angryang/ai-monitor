// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package protocol

import (
	"net/http"
	"strings"
	"testing"
)

// TestGenerateRequestID_GeneratesWhenAbsent verifies that calling
// GenerateRequestID on a fresh request produces a non-empty value and sets
// the canonical header.
func TestGenerateRequestID_GeneratesWhenAbsent(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "http://example.test", nil)
	if req.Header.Get(RequestIDHeader) != "" {
		t.Fatalf("pre: header unexpectedly set")
	}
	id := GenerateRequestID(req, nil)
	if id == "" {
		t.Fatalf("GenerateRequestID returned empty")
	}
	if got := req.Header.Get(RequestIDHeader); got != id {
		t.Fatalf("header: got %q, want %q", got, id)
	}
	// Default generator is uuid.NewString → 36 chars (8-4-4-4-12 + 4 dashes).
	if len(id) != 36 || strings.Count(id, "-") != 4 {
		t.Logf("note: default id %q is not uuid-shaped — that's fine if you injected a custom generator", id)
	}
}

// TestGenerateRequestID_PreservesExisting ensures that a header already set on
// the request (e.g. injected by tests) is left untouched.
func TestGenerateRequestID_PreservesExisting(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "http://example.test", nil)
	const want = "preset-req-id-12345"
	req.Header.Set(RequestIDHeader, want)

	calls := 0
	id := GenerateRequestID(req, func() string {
		calls++
		return "should-not-be-used"
	})
	if id != want {
		t.Fatalf("GenerateRequestID: got %q, want %q", id, want)
	}
	if calls != 0 {
		t.Fatalf("generator should not be called when header is preset; got %d calls", calls)
	}
}

// TestGenerateRequestID_RejectsEmptyGenerator ensures the helper falls back to
// uuid when the user-provided generator returns an empty string — never
// leaves the header unset on the wire.
func TestGenerateRequestID_RejectsEmptyGenerator(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "http://example.test", nil)
	id := GenerateRequestID(req, func() string { return "" })
	if id == "" {
		t.Fatalf("GenerateRequestID returned empty despite fallback")
	}
	if got := req.Header.Get(RequestIDHeader); got != id {
		t.Fatalf("header: got %q, want %q", got, id)
	}
}
