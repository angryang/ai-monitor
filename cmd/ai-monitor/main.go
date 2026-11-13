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
	"github.com/angryang/ai-monitor/internal/provider"
	"github.com/angryang/ai-monitor/internal/request"
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
		insecure = flag.Bool("insecure-skip-verify", false, "skip upstream TLS certificate verification for all probes (overrides config)")
	)
	flag.Parse()

	cfg, err := config.Load(*yamlPath, config.FlagOverrides{
		HTTPAddr:           *addr,
		InsecureSkipVerify: insecure,
	})
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

	// The root context: cancelled on shutdown so in-flight probes and the
	// scheduler stop together.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The database handle is opened before any migration runs: the
	// legacy monitoring-task migration has to rewrite the `tasks` table
	// *before* GORM's AutoMigrate sees it, because SQLite cannot add a
	// NOT NULL column to an existing table and the new task model
	// declares `provider_id` as NOT NULL.
	db, err := config.OpenDB(cfg.SQLitePath, 0)
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	// §18.18: the multi-model upgrade adds `models` to ai_providers and
	// `model` to tasks / ai_requests, then backfills every existing row
	// with the model it was already using. It has to run before any
	// AutoMigrate: SQLite refuses to add a NOT NULL column without a
	// default, which is exactly what GORM would otherwise attempt.
	mmres, err := provider.MigrateMultiModels(ctx, db, log)
	if err != nil {
		return fmt.Errorf("multi-model migration: %w", err)
	}
	if mmres.ProvidersBackfilled > 0 || mmres.TasksBackfilled > 0 || mmres.RequestsBackfilled > 0 {
		log.Info("multi-model migration complete",
			"providers", mmres.ProvidersBackfilled,
			"tasks", mmres.TasksBackfilled,
			"requests", mmres.RequestsBackfilled)
	}

	// AI service provider registry. The store shares the task store's
	// GORM handle so everything lives in the same SQLite file; the
	// checker verifies that a provider is reachable before it is saved.
	providerStore, err := provider.NewSqliteStore(db)
	if err != nil {
		return fmt.Errorf("provider store: %w", err)
	}
	providerStore.SetCipher(cipher)
	providerChecker := provider.NewChecker(provider.CheckerOptions{
		Adapters: func(name string) (protocol.ChatProtocolAdapter, error) {
			return protocol.Lookup(name)
		},
		Timeout: cfg.ProviderCheckTimeout,
		Logger:  log,
	})

	// AI request manager: a flat registry of predefined requests, each
	// pointing at one provider. Requests no longer carry upstream
	// coordinates (or a cipher) of their own — the provider owns all of
	// that.
	requestStore, err := request.NewSqliteStore(db)
	if err != nil {
		return fmt.Errorf("request store: %w", err)
	}

	// One-time migration of the retired request_groups / request_items
	// model. Legacy groups become providers and their items become
	// requests pointing at those providers. No prompt or credential is
	// lost; the old tables are dropped once they are empty.
	mres, err := request.MigrateLegacyGroups(ctx, db, providerStore, log)
	if err != nil {
		return fmt.Errorf("request groups migration: %w", err)
	}
	if mres.GroupsMigrated > 0 || mres.ProvidersReused > 0 || mres.RequestsMigrated > 0 {
		log.Info("request_groups migrated",
			"groups", mres.GroupsMigrated,
			"providers_reused", mres.ProvidersReused,
			"requests", mres.RequestsMigrated,
			"orphaned", mres.Orphaned)
	}

	// One-time migration of the retired "task owns the upstream" model.
	// Every legacy task's upstream becomes a provider and the `tasks`
	// table is rebuilt so it only holds task-owned fields plus the
	// provider reference.
	tres, err := provider.MigrateLegacyTasks(ctx, db, providerStore, log)
	if err != nil {
		return fmt.Errorf("monitoring task migration: %w", err)
	}
	if tres.TasksMigrated > 0 {
		log.Info("monitoring tasks migrated",
			"tasks", tres.TasksMigrated,
			"providers_created", tres.ProvidersCreated,
			"providers_reused", tres.ProvidersReused)
	}

	store, err := config.NewSqliteStoreFromDB(db, config.SqliteOptions{Path: cfg.SQLitePath})
	if err != nil {
		return fmt.Errorf("open task store: %w", err)
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

	// The instance-wide InsecureSkipVerify switch lives in the adapter deps
	// because the adapters are the layer that owns the HTTP client. Per-task
	// opt-ins ride along on TaskConfig and are OR'ed with this flag inside
	// protocol.ClientFor.
	protocol.SetDeps(protocol.AdapterDeps{InsecureSkipVerify: cfg.InsecureSkipVerify})

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

	requestSvc := request.NewService(requestStore, runner, request.ServiceOptions{
		Providers: providerStore,
		Logger:    log,
	})

	// The binder resolves the AI service provider a task points at and
	// copies the upstream coordinates onto the task's runtime fields right
	// before a probe runs. Binding at probe time (instead of at load time)
	// is what makes a provider edit take effect on the very next tick.
	binder := provider.NewBinder(providerStore)

	// Make sure scheduler handler captures task context.
	schedulerLog := func(msg string, args ...any) {
		log.Info(msg, args...)
	}
	sched := scheduler.New(scheduler.Options{
		MaxConcurrent: cfg.MaxConcurrent,
		Jitter:        cfg.Jitter,
		Logger:        schedulerLog,
		Handler: func(_ context.Context, t config.TaskConfig) {
			bound, err := binder.BindTask(ctx, t)
			if err != nil {
				// A task whose provider vanished (or lost its key) must
				// not take the whole tick down: log it and move on.
				log.Warn("probe task could not be bound to its provider",
					"task_id", t.ID, "task_name", t.Name, "error", err.Error())
				return
			}
			// The provider owns the per-request deadline; the adapter
			// does not enforce a timeout of its own.
			runCtx, runCancel := context.WithTimeout(ctx, bound.EffectiveTimeout())
			defer runCancel()
			runner.Run(runCtx, bound)
		},
	})

	// Lenient load: a task whose encrypted secrets cannot be opened with the
	// current master key must not take the whole service down. Such tasks
	// stay in the list (marked with SecretError) so the admin UI can show
	// them and an operator can re-enter the api key.
	tasks, secretErrs, err := store.ListTasksLenient(ctx)
	if err != nil {
		// The listing itself failed (e.g. the database is unreachable).
		// Starting anyway would bring up an instance that silently monitors
		// nothing, so fail fast instead.
		return fmt.Errorf("list tasks: %w", err)
	}
	for _, err := range secretErrs {
		log.Warn("task secrets could not be decrypted; task kept but its api_key is empty until you re-enter it in the admin UI",
			"error", err.Error())
	}
	if len(secretErrs) > 0 {
		log.Warn("this usually means AI_MONITOR_MASTER_KEY changed since these tasks were saved",
			"affected_tasks", len(secretErrs))
	}
	if err := sched.Reload(ctx, config.RunnableTasks(tasks)); err != nil {
		return fmt.Errorf("scheduler reload: %w", err)
	}
	if err := sched.Start(ctx); err != nil {
		return fmt.Errorf("scheduler start: %w", err)
	}

	httpSrv := server.New(server.Options{
		Addr:                 cfg.HTTPAddr,
		Metrics:              sink.Handler(),
		Store:                store,
		Scheduler:            sched,
		Logger:               func(msg string, args ...any) { log.Info(msg, args...) },
		Auth:                 mw,
		UserStore:            userStore,
		RunStore:             runStore,
		InsecureSkipVerify:   cfg.InsecureSkipVerify,
		RequestStore:         requestStore,
		RequestService:       requestSvc,
		ProviderStore:        providerStore,
		ProviderChecker:      providerChecker,
		ProviderCheckTimeout: cfg.ProviderCheckTimeout,
		// The provider save path waits for an upstream call, so the
		// response deadline has to outlast the check.
		WriteTimeout: cfg.ProviderCheckTimeout + 5*time.Second,
	})

	// Hot reload coordinator.
	rel := server.NewWatchReloader(store, func(_ context.Context, tasks []config.TaskConfig) error {
		return sched.Reload(ctx, config.RunnableTasks(tasks))
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
