// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/protocol"
	"github.com/angryang/ai-monitor/internal/protocol/openai"
)

// newChecker wires a checker over the real OpenAI adapter. Using the real
// adapter is the point: the check must exercise the same request the
// monitoring pipeline would send.
func newChecker(t *testing.T, timeout time.Duration) *Checker {
	t.Helper()
	return NewChecker(CheckerOptions{
		Adapters: func(name string) (protocol.ChatProtocolAdapter, error) {
			if name != string(config.ProtocolOpenAI) {
				return nil, protocol.ErrProtocolCfg
			}
			return openai.NewAdapter(protocol.AdapterDeps{}), nil
		},
		Timeout: timeout,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// sseServer answers with one SSE frame and a terminal [DONE].
func sseServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testProvider(baseURL string) *Provider {
	return &Provider{
		ID:       "prov-1",
		Name:     "alpha",
		Protocol: config.ProtocolOpenAI,
		BaseURL:  baseURL,
		APIKey:   "sk-secret",
		Model:    "gpt-test",
	}
}

// TestChecker_Success pins the happy path: a reachable upstream with
// accepted credentials is reported as OK.
func TestChecker_Success(t *testing.T) {
	srv := sseServer(t)
	c := newChecker(t, 5*time.Second)

	res := c.Check(context.Background(), testProvider(srv.URL))
	if !res.OK {
		t.Fatalf("check failed: kind=%s message=%s", res.Kind, res.Message)
	}
	if res.Kind != CheckOK {
		t.Fatalf("kind = %q, want %q", res.Kind, CheckOK)
	}
	if res.Protocol != "openai" {
		t.Fatalf("protocol = %q", res.Protocol)
	}
	if !strings.Contains(res.URL, srv.URL) {
		t.Fatalf("url = %q, want it to point at the test server", res.URL)
	}
	if res.CheckedAt.IsZero() {
		t.Fatalf("checked_at is zero")
	}
}

// TestChecker_Unauthorized pins the 401 classification: a rejected key is
// the operator's most common mistake and must be called out by name.
func TestChecker_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided"}}`))
	}))
	t.Cleanup(srv.Close)

	c := newChecker(t, 5*time.Second)
	res := c.Check(context.Background(), testProvider(srv.URL))
	if res.OK {
		t.Fatalf("check succeeded against a 401 upstream")
	}
	if res.Kind != CheckUnauthorized {
		t.Fatalf("kind = %q, want %q", res.Kind, CheckUnauthorized)
	}
	if res.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
	if !strings.Contains(res.Message, "Incorrect API key provided") {
		t.Fatalf("message = %q, want the upstream's own explanation", res.Message)
	}
}

// TestChecker_NotFound pins the 404 classification — a wrong base URL or
// a model the upstream does not know.
func TestChecker_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"model not found"}}`))
	}))
	t.Cleanup(srv.Close)

	c := newChecker(t, 5*time.Second)
	res := c.Check(context.Background(), testProvider(srv.URL))
	if res.OK {
		t.Fatalf("check succeeded against a 404 upstream")
	}
	if res.Kind != CheckInvalidRequest {
		t.Fatalf("kind = %q, want %q", res.Kind, CheckInvalidRequest)
	}
	if res.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
}

// TestChecker_UpstreamDown pins the 5xx classification.
func TestChecker_UpstreamDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	c := newChecker(t, 5*time.Second)
	res := c.Check(context.Background(), testProvider(srv.URL))
	if res.OK {
		t.Fatalf("check succeeded against a 502 upstream")
	}
	if res.Kind != CheckUpstreamDown {
		t.Fatalf("kind = %q, want %q", res.Kind, CheckUpstreamDown)
	}
	if res.StatusCode != 502 {
		t.Fatalf("status = %d, want 502", res.StatusCode)
	}
}

// TestChecker_NetworkError pins the "never reached the upstream" case —
// a closed port. It must not be confused with an upstream that answered.
func TestChecker_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening any more

	c := newChecker(t, 5*time.Second)
	res := c.Check(context.Background(), testProvider(url))
	if res.OK {
		t.Fatalf("check succeeded against a closed port")
	}
	if res.Kind != CheckNetwork {
		t.Fatalf("kind = %q, want %q (message=%s)", res.Kind, CheckNetwork, res.Message)
	}
	if res.StatusCode != 0 {
		t.Fatalf("status = %d, want 0 (no response was received)", res.StatusCode)
	}
}

// TestChecker_Timeout pins the deadline case.
func TestChecker_Timeout(t *testing.T) {
	// The handler outlives the client's deadline on purpose: that is
	// what a stuck upstream looks like. The sleep is bounded so the
	// test server can shut down.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)

	c := newChecker(t, 300*time.Millisecond)
	res := c.Check(context.Background(), testProvider(srv.URL))
	if res.OK {
		t.Fatalf("check succeeded against a hanging upstream")
	}
	if res.Kind != CheckTimeout {
		t.Fatalf("kind = %q, want %q (message=%s)", res.Kind, CheckTimeout, res.Message)
	}
}

// TestChecker_RejectsIncompleteProvider pins the "nothing was sent"
// cases: a provider with a missing field is a configuration problem, not
// a network one.
func TestChecker_RejectsIncompleteProvider(t *testing.T) {
	c := newChecker(t, time.Second)

	cases := map[string]*Provider{
		"no protocol": {ID: "p", Name: "n", BaseURL: "https://e.test", APIKey: "k", Model: "m"},
		"no base url": {ID: "p", Name: "n", Protocol: config.ProtocolOpenAI, APIKey: "k", Model: "m"},
		"no model":    {ID: "p", Name: "n", Protocol: config.ProtocolOpenAI, BaseURL: "https://e.test", APIKey: "k"},
		"no api key":  {ID: "p", Name: "n", Protocol: config.ProtocolOpenAI, BaseURL: "https://e.test", Model: "m"},
	}
	for name, p := range cases {
		res := c.Check(context.Background(), p)
		if res.OK {
			t.Fatalf("%s: check succeeded; want a config error", name)
		}
		if res.Kind != CheckConfig {
			t.Fatalf("%s: kind = %q, want %q", name, res.Kind, CheckConfig)
		}
	}
}

// TestChecker_NilProvider pins that a missing provider is a config error
// rather than a panic.
func TestChecker_NilProvider(t *testing.T) {
	c := newChecker(t, time.Second)
	res := c.Check(context.Background(), nil)
	if res.OK {
		t.Fatalf("check succeeded for a nil provider")
	}
	if res.Kind != CheckConfig {
		t.Fatalf("kind = %q, want %q", res.Kind, CheckConfig)
	}
}

// TestChecker_UnknownProtocol pins that an unregistered protocol is
// reported as a configuration problem.
func TestChecker_UnknownProtocol(t *testing.T) {
	c := NewChecker(CheckerOptions{Adapters: func(string) (protocol.ChatProtocolAdapter, error) {
		return nil, protocol.ErrProtocolCfg
	}})
	p := &Provider{
		ID:       "p",
		Name:     "n",
		Protocol: config.ProtocolGemini,
		BaseURL:  "https://e.test",
		APIKey:   "k",
		Model:    "m",
	}
	res := c.Check(context.Background(), p)
	if res.Kind != CheckConfig {
		t.Fatalf("kind = %q, want %q", res.Kind, CheckConfig)
	}
}
