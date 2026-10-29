// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package probe

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/metrics"
	"github.com/angryang/ai-monitor/internal/protocol"
	_ "github.com/angryang/ai-monitor/internal/protocol/openai"
)

// tlsUpstream serves the OpenAI SSE shape over TLS with a self-signed
// certificate — the shape of an internal model server / gateway.
func tlsUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"pong\"},\"finish_reason\":\"\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// realOpenAIRunner wires the runner to the registered openai adapter, the
// same way cmd/ai-monitor does.
func realOpenAIRunner(t *testing.T, globalInsecure bool) *Runner {
	t.Helper()
	protocol.SetDeps(protocol.AdapterDeps{InsecureSkipVerify: globalInsecure})
	return newRunnerForTest(adapterProxy(t), metrics.NewPromSink(metrics.PrometheusOptions{}), nil)
}

// adapterProxy resolves the openai adapter through the registry so the
// test exercises the real HTTP client selection.
func adapterProxy(t *testing.T) RunnerAdapter {
	t.Helper()
	a, err := protocol.Lookup("openai")
	if err != nil {
		t.Fatalf("lookup openai: %v", err)
	}
	return a
}

func tlsTask(baseURL string) config.TaskConfig {
	return config.TaskConfig{
		ID:         "tls-task",
		Name:       "tls-task",
		Enabled:    true,
		Protocol:   config.ProtocolOpenAI,
		BaseURL:    baseURL,
		APIKey:     "sk-test",
		Model:      "gpt-4o",
		UserPrompt: "ping",
		Schedule:   "@every 1m",
	}
}

// TestRunner_SelfSignedUpstream_FailsWithoutOptIn is the regression test
// for the "x509: certificate signed by unknown authority" report: the
// probe must fail (and be classified as an upstream failure) while the
// task has not opted out of certificate verification.
func TestRunner_SelfSignedUpstream_FailsWithoutOptIn(t *testing.T) {
	srv := tlsUpstream(t)
	r := realOpenAIRunner(t, false)

	res := r.Run(context.Background(), tlsTask(srv.URL))
	if res.Status == StatusSuccess {
		t.Fatalf("expected the probe to fail against a self-signed cert")
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "certificate") {
		t.Fatalf("expected a certificate error, got: %v", res.Err)
	}
}

// TestRunner_SelfSignedUpstream_SucceedsWithTaskOptIn verifies that the
// per-task switch turns the very same probe into a success.
func TestRunner_SelfSignedUpstream_SucceedsWithTaskOptIn(t *testing.T) {
	srv := tlsUpstream(t)
	r := realOpenAIRunner(t, false)

	task := tlsTask(srv.URL)
	task.InsecureSkipVerify = true

	res := r.Run(context.Background(), task)
	if res.Status != StatusSuccess {
		t.Fatalf("expected success with insecure_skip_verify, got %q: %v", res.Status, res.Err)
	}
	if res.OutputContent != "pong" {
		t.Fatalf("output = %q, want %q", res.OutputContent, "pong")
	}
}

// TestRunner_SelfSignedUpstream_SucceedsWithGlobalSwitch verifies the
// instance-wide switch reaches the adapter even for a task that never
// opted in.
func TestRunner_SelfSignedUpstream_SucceedsWithGlobalSwitch(t *testing.T) {
	srv := tlsUpstream(t)
	r := realOpenAIRunner(t, true)

	res := r.Run(context.Background(), tlsTask(srv.URL))
	if res.Status != StatusSuccess {
		t.Fatalf("expected success with the global switch on, got %q: %v", res.Status, res.Err)
	}
}
