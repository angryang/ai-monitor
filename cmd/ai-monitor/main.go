// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

// Command ai-monitor is the single-entry-point binary that loads config,
// opens the SQLite task store, schedules probes, and exposes /metrics +
// /healthz + /admin endpoints. Per design.md §10, the cmd/* package holds
// wiring only; all business logic lives in internal/*.
//
// §18 adds login + RBAC: at startup we load AI_MONITOR_JWT_SECRET, mount the
// user store on top of the same SQLite file, run EnsureSeedAdmin, and hand
// the resulting *auth.Middleware to the server so /auth/* and /admin/* routes
// pick up their authentication and role decorators.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/angryang/ai-monitor/internal/auth"
	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/logging"
	"github.com/angryang/ai-monitor/internal/metrics"
	"github.com/angryang/ai-monitor/internal/notifier"
	"github.com/angryang/ai-monitor/internal/notifier/wecom"
	"github.com/angryang/ai-monitor/internal/probe"
	"github.com/angryang/ai-monitor/internal/protocol"
	_ "github.com/angryang/ai-monitor/internal/protocol/anthropic"
	_ "github.com/angryang/ai-monitor/internal/protocol/gemini"
	_ "github.com/angryang/ai-monitor/internal/protocol/openai"
	"github.com/angryang/ai-monitor/internal/scheduler"
	"github.com/angryang/ai-monitor/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "ai-monitor: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	log := logging.Init()

	var (
		yamlPath = flag.String("config", "deploy/example.config.yaml", "path to YAML config")
		addr     = flag.String("addr", "", "HTTP listen addr (overrides config)")
	)
	flag.Parse()

	cfg, err := config.Load(*yamlPath, config.FlagOverrides{HTTPAddr: *addr})
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	// Cipher for the SQLite store.
	cipher, err := config.NewAESCipher(cfg.MasterKey)
	if err != nil {
		return fmt.Errorf("cipher: %w", err)
	}

	store, err := config.NewSqliteStore(config.SqliteOptions{Path: cfg.SQLitePath})
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	store.SetCipher(cipher)

	// §18: JWT signer + user store. AI_MONITOR_JWT_SECRET must be present
	// (>= 32 bytes); startup fails fast otherwise so a forgotten env var is
	// impossible to miss.
	jwtSecret := os.Getenv("AI_MONITOR_JWT_SECRET")
	if len(jwtSecret) < auth.MinSecretBytes {
		return fmt.Errorf("auth: AI_MONITOR_JWT_SECRET must be set to a value >= %d bytes (got %d)", auth.MinSecretBytes, len(jwtSecret))
	}
	signer, err := auth.NewJWTSigner([]byte(jwtSecret))
	if err != nil {
		return fmt.Errorf("auth: jwt signer: %w", err)
	}
	// best-effort zero of the env-derived secret so it doesn't linger.
	for i := range jwtSecret {
		//nolint:staticcheck // SA1029 intentionally invalid key — fine in a string range loop.
		jwtSecret = jwtSecret[:i] + jwtSecret[i:i+1] + jwtSecret[i+1:]
	}
	_ = jwtSecret

	userStore, err := auth.NewSqliteUserStore(auth.SqliteUserStoreOptions{Path: cfg.SQLitePath})
	if err != nil {
		return fmt.Errorf("auth: user store: %w", err)
	}
	defer userStore.Close()

	if _, err := auth.EnsureSeedAdmin(context.Background(), userStore, auth.SeedConfig{}, log); err != nil {
		return fmt.Errorf("auth: seed admin: %w", err)
	}
	mw := auth.NewMiddleware(signer, userStore)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Probe dependencies.
	sink := metrics.NewPromSink(metrics.PrometheusOptions{
		BucketsDuration: cfg.BucketsDuration,
		BucketsTTFT:     cfg.BucketsTTFT,
		BucketsITL:      cfg.BucketsITL,
		BucketsLag:      cfg.BucketsLag,
	})
	deduper := notifier.NewDeduper(cfg.DedupTTL)
	weCom := wecom.New(wecom.Config{WebhookURL: "http://localhost", Timeout: 5 * time.Second})
	multi := notifier.NewMultiNotifier(weCom)

	protocol.SetDeps(protocol.AdapterDeps{})

	runner := probe.NewRunner(func(name string) (probe.RunnerAdapter, error) {
		a, err := protocol.Lookup(name)
		if err != nil {
			return nil, err
		}
		return a, nil
	}, sink, multi, deduper)
	// Persist every probe execution to probe_runs so the admin UI can show
	// recent history. We share the same *gorm.DB handle as the config store
	// to keep the file handle / WAL / lock model single-source-of-truth.
	runStore := probe.NewSqliteRunStore(store.DB())
	if err := runStore.AutoMigrate(); err != nil {
		return fmt.Errorf("probe_runs: migrate: %w", err)
	}
	runner.RunStore = runStore

	// Make sure scheduler handler captures task context.
	schedulerLog := func(msg string, args ...any) {
		log.Info(msg, args...)
	}
	sched := scheduler.New(scheduler.Options{
		MaxConcurrent: cfg.MaxConcurrent,
		Jitter:        cfg.Jitter,
		Logger:        schedulerLog,
		Handler: func(_ context.Context, t config.TaskConfig) {
			runner.Run(ctx, t)
		},
	})

	tasks, err := store.ListTasks(ctx)
	if err != nil {
		return fmt.Errorf("list tasks: %w", err)
	}
	if err := sched.Reload(ctx, tasks); err != nil {
		return fmt.Errorf("scheduler reload: %w", err)
	}
	if err := sched.Start(ctx); err != nil {
		return fmt.Errorf("scheduler start: %w", err)
	}

	httpSrv := server.New(server.Options{
		Addr:      cfg.HTTPAddr,
		Metrics:   sink.Handler(),
		Store:     store,
		Scheduler: sched,
		Logger:    func(msg string, args ...any) { log.Info(msg, args...) },
		Auth:      mw,
		UserStore: userStore,
		RunStore:  runStore,
	})

	// Hot reload coordinator.
	rel := server.NewWatchReloader(store, func(_ context.Context, tasks []config.TaskConfig) error {
		return sched.Reload(ctx, tasks)
	}, log)
	relDone := make(chan struct{})
	go func() {
		defer close(relDone)
		rel.Run(ctx)
	}()

	var wg sync.WaitGroup
	wg.Add(1)
	errCh := make(chan error, 1)
	go func() {
		defer wg.Done()
		errCh <- httpSrv.Start()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-stop:
		log.Info("shutdown_signal_received", slog.String("signal", sig.String()))
	case err := <-errCh:
		if err != nil {
			log.Warn("http_server_failed", slog.String("err", err.Error()))
		}
	}

	// Graceful shutdown: 5s deadline.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		log.Warn("http_shutdown_failed", slog.String("err", err.Error()))
	}
	if err := sched.Stop(shutCtx); err != nil {
		log.Warn("scheduler_stop_failed", slog.String("err", err.Error()))
	}
	rel.Stop()
	wg.Wait()
	<-relDone
	log.Info("ai-monitor stop")
	return nil
}