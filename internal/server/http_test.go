package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHealthz_Ok exercises the production /healthz handler through the
// built-in net/http test server.
func TestHealthz_Ok(t *testing.T) {
	srv := New(Options{Addr: ":9090"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	// New() wires the mux inside; reach in to route directly.
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if got := strings.TrimSpace(string(body)); got != "ok" {
		t.Fatalf("body: got %q, want %q", got, "ok")
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("content-type: got %q", got)
	}
}

// TestHealthz_UseProvidedAddr ensures the Options.Addr value is forwarded to
// the http.Server (paranoia check: future refactors could break this).
func TestHealthz_UseProvidedAddr(t *testing.T) {
	srv := New(Options{Addr: ":0"})
	if srv.Addr() != ":0" {
		t.Fatalf("Addr(): got %q, want %q", srv.Addr(), ":0")
	}
}

// TestNew_DefaultAddr verifies the §10 default port fallback.
func TestNew_DefaultAddr(t *testing.T) {
	srv := New(Options{})
	if srv.Addr() != ":9090" {
		t.Fatalf("default Addr(): got %q, want %q", srv.Addr(), ":9090")
	}
}

// TestMetricsRouteRegistered proves the G2 wiring point will land on the
// right path when a metrics handler is supplied.
func TestMetricsRouteRegistered(t *testing.T) {
	called := false
	mh := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	srv := New(Options{Addr: ":9090", Metrics: mh})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if !called {
		t.Fatalf("metrics handler not invoked")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
}

// TestShutdown_NoActiveConnections returns immediately when no clients are
// attached (catches deadlocks on Shutdown that would otherwise surface only
// during graceful exit).
func TestShutdown_NoActiveConnections(t *testing.T) {
	srv := New(Options{Addr: ":9090"})
	err := srv.Shutdown(testContext(t))
	if err != nil && err != http.ErrServerClosed {
		t.Fatalf("Shutdown: %v", err)
	}
}
