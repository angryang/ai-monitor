// Command ai-monitor is the single-entry-point binary that loads config,
// opens the SQLite task store, schedules probes, and exposes /metrics +
// /healthz + /admin endpoints. Per design.md §10, the cmd/* package holds
// wiring only; all business logic lives in internal/*.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aibench/aibench/ai-monitor/internal/logging"
	"github.com/aibench/aibench/ai-monitor/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "ai-monitor: %v\n", err)
		os.Exit(1)
	}
}

// run is split out so tests / future automation can stub main behaviour.
func run() error {
	log := logging.Init()
	log.Info("ai-monitor start", slog.String("version", "dev"))

	httpServer := server.New(server.Options{
		Addr:   ":9090",
		Logger: func(msg string, args ...any) { log.Info(msg, args...) },
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.Start()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-stop:
		log.Info("shutdown_signal_received", slog.String("signal", sig.String()))
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("server: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("server shutdown: %w", err)
	}
	log.Info("ai-monitor stop")
	return nil
}
