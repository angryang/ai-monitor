// Package server wires the HTTP surface area: /metrics, /healthz, /admin/*.
// A1 served only /healthz; G2 adds the metrics, admin/tasks, and admin/reload
// routes and graceful Shutdown.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/aibench/aibench/ai-monitor/internal/config"
	"github.com/aibench/aibench/ai-monitor/internal/metrics"
	"github.com/aibench/aibench/ai-monitor/internal/scheduler"
)

// Server wraps an http.Server with the routes declared by the design.
type Server struct {
	httpServer *http.Server
	store      config.ConfigStore
	sch        scheduler.Scheduler
	lastTick   atomic.Int64 // unix secs of the latest scheduler fire (for /healthz)
}

// Options carries the dependencies needed by the richer server implementation.
// All fields are optional; defaulted to internal safe values.
type Options struct {
	Addr    string
	Metrics http.Handler
	Healthz http.Handler // optional override
	Store   config.ConfigStore
	Scheduler scheduler.Scheduler
	Logger   LoggerFunc
	Timeout  time.Duration
}

// LoggerFunc matches the slog.Logger style signature used inside handler
// goroutines (a *slog.Logger would pull a heavy import for tests).
type LoggerFunc func(msg string, args ...any)

// New constructs a Server with the provided options.
func New(opts Options) *Server {
	addr := opts.Addr
	if addr == "" {
		addr = ":9090"
	}
	logf := opts.Logger
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Server{
		store: opts.Store,
		sch:   opts.Scheduler,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz(opts.Healthz))
	if opts.Metrics != nil {
		mux.Handle("GET /metrics", opts.Metrics)
	}
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("GET /admin/tasks", s.handleListTasks())
	adminMux.HandleFunc("POST /admin/reload", s.handleReload())
	mux.Handle("/admin/", adminMux)

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	hs := &http.Server{
		Addr:              addr,
		Handler:           withRequestLog(mux, logf),
		ReadHeaderTimeout: timeout,
		ReadTimeout:       timeout,
		WriteTimeout:      timeout,
		IdleTimeout:       2 * timeout,
	}
	s.httpServer = hs
	return s
}

// SetLastTick is a hook the scheduler uses to update the health-check metric.
// We expose it so the assembly in main can hook the scheduler callback
// without coupling scheduler ↔ server directly.
func (s *Server) SetLastTick(unixSec int64) { s.lastTick.Store(unixSec) }

// Handle serves as a middleware-free access point for tests.
func (s *Server) Handle() http.Handler { return s.httpServer.Handler }

// Start blocks running the HTTP server until Shutdown is called.
func (s *Server) Start() error {
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown gracefully drains in-flight requests up to the supplied deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

// Addr returns the underlying server address (useful for tests bound to ":0").
func (s *Server) Addr() string {
	return s.httpServer.Addr
}

// handleHealthz returns 200 when DB ping succeeds; 503 otherwise.
func (s *Server) handleHealthz(custom http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if custom != nil {
			custom.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 1*time.Second)
		defer cancel()
		if s.store != nil {
			if _, err := s.store.ListTasks(ctx); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"status":"db_unavailable"}`))
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
}

// handleListTasks returns the full task list as JSON (no secrets).
func (s *Server) handleListTasks() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if s.store == nil {
			http.Error(w, `{"error":"store not configured"}`, http.StatusInternalServerError)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		tasks, err := s.store.ListTasks(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Redact secrets before exposing via HTTP.
		for i := range tasks {
			tasks[i].APIKey = ""
			tasks[i].APIKeyEnc = nil
			tasks[i].Notify.WeComWebhook = ""
			tasks[i].Notify.WeComWebhookEnc = nil
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"tasks": tasks})
	}
}

// handleReload re-reads tasks from the store and reloads the scheduler.
func (s *Server) handleReload() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.store == nil || s.sch == nil {
			http.Error(w, `{"error":"not configured"}`, http.StatusInternalServerError)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		tasks, err := s.store.ListTasks(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.sch.Reload(ctx, tasks); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"reloaded":true}`))
	}
}

// withRequestLog is a tiny middleware that records every request's method,
// path, status, and duration.
func withRequestLog(next http.Handler, logf LoggerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)
		logf("http_request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Ensure metrics package is imported even when callers only use the HTTP
// surface for the registry. Avoids an unused-import error if a future
// refactor removes the metrics field.
var _ = metrics.CanaryGroupLabel
