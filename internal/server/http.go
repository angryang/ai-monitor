// Package server wires the HTTP surface area: /metrics, /healthz, /admin/*.
// The minimal A1 version only serves /healthz; richer endpoints are added in
// G2 (admin) and finalized in P1-B (graceful shutdown / structured logs).
package server

import (
	"context"
	"net/http"
	"time"
)

// Server wraps an http.Server with the routes declared by the design.
type Server struct {
	httpServer *http.Server
}

// Options carries the dependencies needed by the richer server implementation.
// At A1 everything is optional; defaulted to internal handlers.
type Options struct {
	Addr      string
	Metrics   http.Handler
	Healthz   http.Handler // if nil we serve a built-in 200 "ok".
	AdminMux  *http.ServeMux
	Logger    LoggerFunc
	Timeout   time.Duration
}

// LoggerFunc matches the slog.Logger style signature used inside handler
// goroutines (a *slog.Logger would pull a heavy import for tests).
type LoggerFunc func(msg string, args ...any)

// New constructs a Server with the provided options, falling back to safe
// defaults when a field is empty.
func New(opts Options) *Server {
	addr := opts.Addr
	if addr == "" {
		addr = ":9090"
	}
	logf := opts.Logger
	if logf == nil {
		logf = func(msg string, args ...any) {}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if opts.Healthz != nil {
			opts.Healthz.ServeHTTP(w, nil) // fallback path; unused in production
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	if opts.Metrics != nil {
		mux.Handle("GET /metrics", opts.Metrics)
	}
	if opts.AdminMux != nil {
		mux.Handle("/admin/", opts.AdminMux)
	}
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
	return &Server{httpServer: hs}
}

// Start blocks running the HTTP server until Shutdown is called or the
// listener fails.
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

// withRequestLog is a tiny middleware that records every request's method,
// path, and duration. We avoid pulling slog directly to keep this package
// import-light (logging package owns the JSON handler).
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
