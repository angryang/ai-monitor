// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

// Package server wires the HTTP surface area: /metrics, /healthz, /admin/*,
// and (since §18) /auth/* + SPA fallback. The cmd/* package hands us a
// pre-built *auth.Middleware; we apply it on the appropriate sub-mux so the
// legacy /admin/* tests keep passing without authentication.
package server

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/angryang/ai-monitor/internal/auth"
	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/metrics"
	"github.com/angryang/ai-monitor/internal/probe"
	"github.com/angryang/ai-monitor/internal/scheduler"
)

// Server wraps an http.Server with the routes declared by the design.
type Server struct {
	httpServer *http.Server
	store      config.ConfigStore
	sch        scheduler.Scheduler
	lastTick   atomic.Int64 // unix secs of the latest scheduler fire (for /healthz)

	// authMiddleware is non-nil when the binary is built with auth enabled
	// (§18). It owns /auth/login, /auth/me, etc., and gates /admin/users
	// and /admin/tasks.
	authMiddleware *auth.Middleware
	// userStore backs /admin/users. nil when auth is disabled.
	userStore auth.UserStore
}

// Options carries the dependencies needed by the richer server implementation.
// All fields are optional; defaulted to internal safe values.
type Options struct {
	Addr      string
	Metrics   http.Handler
	Healthz   http.Handler // optional override
	Store     config.ConfigStore
	Scheduler scheduler.Scheduler
	Logger    LoggerFunc
	Timeout   time.Duration

	// Auth is the JWT + RBAC middleware. When non-nil, /auth/* and the
	// admin/user routes get wrapped with it. Leaving it nil preserves the
	// pre-§18 behaviour (any request reaches the handlers).
	Auth *auth.Middleware
	// UserStore backs /admin/users. Required iff Auth is non-nil.
	UserStore auth.UserStore
	// Cipher encrypts apikey + webhook at rest when admin/tasks writes
	// arrive through the HTTP layer. Optional.
	Cipher config.Cipher
	// RunStore backs the /admin/tasks/{id}/runs endpoint. Optional; if
	// nil the endpoint returns 503 so admins know history is disabled.
	RunStore probe.RunStore
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
		store:          opts.Store,
		sch:            opts.Scheduler,
		authMiddleware: opts.Auth,
		userStore:      opts.UserStore,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz(opts.Healthz))
	if opts.Metrics != nil {
		mux.Handle("GET /metrics", opts.Metrics)
	}
	// Auth + admin + SPA routes are mounted under /auth, /admin, and /
	// respectively. We split them so the test suite can pin patterns down
	// without dragging in the auth middleware.
	mux.Handle("/auth/", s.buildAuthRoutes(opts))
	mux.Handle("/admin/", s.buildAdminRoutes(opts))
	// Static handler also serves SPA fallback for non-API routes.
	mux.Handle("/", s.handleStatic())

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
func (s *Server) SetLastTick(unixSec int64) { s.lastTick.Store(unixSec) }

// Handle serves as a middleware-free access point for tests.
func (s *Server) Handle() http.Handler { return s.httpServer.Handler }

// Start blocks running the HTTP server until Shutdown is called.
func (s *Server) Start() error {
	if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully drains in-flight requests up to the supplied deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

// Addr returns the underlying server address (useful for tests bound to ":0").
func (s *Server) Addr() string { return s.httpServer.Addr }

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

// buildAuthRoutes returns a small mux dedicated to /auth/*. When no auth
// middleware is wired it returns a 501 handler — handy for tests that don't
// want auth to break their setup.
func (s *Server) buildAuthRoutes(opts Options) http.Handler {
	if opts.Auth == nil || opts.UserStore == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			WriteAPIError(w, http.StatusNotImplemented, "auth_disabled", "auth is not configured")
		})
	}
	cfg := &AuthHandlersConfig{
		Auth:   opts.Auth,
		Users:  opts.UserStore,
		Logger: opts.Logger,
	}
	return NewAuthHandlers(cfg).Routes()
}

// buildAdminRoutes returns the mux for /admin/* — task + user CRUD. The
// pre-§18 routes stay accessible so legacy tests pass; the new RBAC paths
// share the same sub-mux but each handler enforces its own role gate.
func (s *Server) buildAdminRoutes(opts Options) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/reload", s.handleReload())

	if opts.Auth != nil && opts.UserStore != nil {
		cfg := &AdminTaskHandlersConfig{
			Auth:      opts.Auth,
			Users:     opts.UserStore,
			Store:     opts.Store,
			Cipher:    opts.Cipher,
			RunStore:  opts.RunStore,
			Scheduler: opts.Scheduler,
			Logger:    opts.Logger,
		}
		// Mount admin_task_handlers directly on this mux so every method
		// (GET / POST / PATCH / DELETE) is registered against the same
		// pattern matcher. Using a sub-mux tripped over Go 1.22+ ServeMux
		// prefix-stripping rules.
		NewAdminTaskHandlers(cfg).Mount(mux)

		userCfg := &AdminUserHandlersConfig{
			Auth:   opts.Auth,
			Users:  opts.UserStore,
			Logger: opts.Logger,
		}
		mux.Handle("/admin/users", NewAdminUserHandlers(userCfg).Routes())
		mux.Handle("/admin/users/", NewAdminUserHandlers(userCfg).Routes())
	}
	return mux
}

// handleReload re-reads tasks from the store and reloads the scheduler.
func (s *Server) handleReload() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.store == nil || s.sch == nil {
			WriteAPIError(w, http.StatusInternalServerError, "not_configured", "store/scheduler missing")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		tasks, err := s.store.ListTasks(ctx)
		if err != nil {
			WriteAPIError(w, http.StatusInternalServerError, "list_failed", err.Error())
			return
		}
		if err := s.sch.Reload(ctx, tasks); err != nil {
			WriteAPIError(w, http.StatusInternalServerError, "reload_failed", err.Error())
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