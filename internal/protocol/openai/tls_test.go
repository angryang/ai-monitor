// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/angryang/ai-monitor/internal/protocol"
)

// TestOpenAI_Probe_SelfSignedTLS is the regression test for the
// "x509: certificate signed by unknown authority" failure mode: an
// upstream behind a self-signed cert (or a private CA) is unreachable
// until certificate verification is explicitly skipped.
//
// httptest.NewTLSServer serves a self-signed cert that the default
// root pool rejects — exactly like the internal gateways this feature
// targets.
func TestOpenAI_Probe_SelfSignedTLS(t *testing.T) {
	adapter := newAdapter(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := baseTask()
	cfg.BaseURL = srv.URL

	// 1. Without the opt-in the handshake must fail with an x509 error.
	_, err := adapter.Probe(context.Background(), cfg)
	if err == nil {
		t.Fatalf("expected a TLS verification failure against a self-signed cert")
	}
	if !strings.Contains(err.Error(), "x509") && !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("expected an x509/certificate error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "protocol: upstream unavailable") {
		t.Fatalf("error should be classified as upstream-unavailable, got: %v", err)
	}

	// 2. With the per-task opt-in the same probe must succeed.
	cfg.InsecureSkipVerify = true
	s, err := adapter.Probe(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Probe with insecure_skip_verify: %v", err)
	}
	defer s.Close()

	var content string
	for c := range s.Chunks() {
		if c.Kind == protocol.ChunkContent {
			content += c.Delta
		}
	}
	if err := s.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if content != "ok" {
		t.Fatalf("content = %q, want %q", content, "ok")
	}
}

// TestOpenAI_Probe_SelfSignedTLS_GlobalSwitch verifies the instance-wide
// switch reaches the adapter: a task that never opted in still connects
// to a self-signed endpoint.
func TestOpenAI_Probe_SelfSignedTLS_GlobalSwitch(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	// Build an adapter with the global switch on, bypassing the shared
	// registry (which other tests may have configured differently).
	a := NewAdapter(protocol.AdapterDeps{InsecureSkipVerify: true}).(*Adapter)
	cfg := baseTask()
	cfg.BaseURL = srv.URL

	s, err := a.Probe(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Probe with the global switch on: %v", err)
	}
	defer s.Close()
	for range s.Chunks() {
	}
	if err := s.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
}
