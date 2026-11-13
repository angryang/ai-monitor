// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package request

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/logging"
	"github.com/angryang/ai-monitor/internal/probe"
	"github.com/angryang/ai-monitor/internal/provider"
	"github.com/google/uuid"
	"golang.org/x/sync/semaphore"
)

// BatchState describes where a batch execution stands.
type BatchState string

const (
	// BatchRunning means at least one request is still in flight.
	BatchRunning BatchState = "running"
	// BatchDone means every accepted request has finished.
	BatchDone BatchState = "done"
)

// DefaultBatchConcurrency is the number of requests a batch fires at the
// same time when the caller doesn't say otherwise.
//
// It is deliberately smaller than the monitoring scheduler's default (16):
// a manual batch is an explicit operator action aimed at a few providers,
// and firing 16 simultaneous requests at the same upstream is more likely
// to trip a rate limit than to be useful.
const DefaultBatchConcurrency = 4

// MaxBatchConcurrency caps the caller-supplied concurrency.
const MaxBatchConcurrency = 32

// maxRetainedBatches bounds the in-memory batch registry.
const maxRetainedBatches = 64

// Prober executes one ad-hoc probe. It mirrors the subset of
// *probe.Runner the service needs so tests can stub the upstream.
type Prober interface {
	// RunManual executes one probe without touching the monitoring
	// statistics, metrics or notifications.
	RunManual(ctx context.Context, t config.TaskConfig, opts probe.RunOptions) probe.Result
}

// SkippedRequest records a request that was not executed and why.
type SkippedRequest struct {
	// ID is the request id.
	ID string `json:"id"`
	// Name is the display name of the skipped request.
	Name string `json:"name"`
	// Reason explains the skip in a form fit for the UI.
	Reason string `json:"reason"`
}

// Skip reasons.
const (
	SkipReasonNotFound        = "not_found"
	SkipReasonSecretError     = "secret_error"
	SkipReasonNoAPIKey        = "no_api_key"
	SkipReasonProviderMissing = "provider_missing"
)

// BatchItemResult is the outcome of one request inside a batch.
type BatchItemResult struct {
	RequestID   string `json:"request_id"`
	RequestName string `json:"request_name"`
	// ProviderID / ProviderName say which upstream the request was sent
	// to. A batch may span several providers — that is the point of the
	// flat model.
	ProviderID   string `json:"provider_id"`
	ProviderName string `json:"provider_name"`
	Protocol     string `json:"protocol"`
	Model        string `json:"model"`
	// Status mirrors probe.StatusSuccess / StatusFailure /
	// StatusUnauthorized.
	Status string `json:"status"`
	// Error is the failure message, empty on success.
	Error string `json:"error,omitempty"`
	// DurationMS / TTFTMS / Chunks mirror the probe result.
	DurationMS int64 `json:"duration_ms"`
	TTFTMS     int64 `json:"ttft_ms"`
	Chunks     int   `json:"chunks"`
	// RequestID is the X-Req-Id sent upstream, for cross-referencing the
	// provider's own logs.
	XRequestID string `json:"x_request_id,omitempty"`
	// Input / Thinking / Output are the captured contents.
	Input    string `json:"input,omitempty"`
	Thinking string `json:"thinking,omitempty"`
	Output   string `json:"output,omitempty"`
	// StartedAt / EndedAt bracket the execution.
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
}

// Batch is one execution of one or more requests.
//
// A batch is memory-only state owned by the service; the durable record of
// what happened lives in probe_runs (source = manual), so a restart loses
// the live progress but not the history.
type Batch struct {
	ID string `json:"batch_id"`
	// State is BatchRunning or BatchDone.
	State BatchState `json:"state"`
	// Total is the number of accepted requests.
	Total int `json:"total"`
	// Completed counts the finished requests.
	Completed int `json:"completed"`
	// Succeeded / Failed split the finished requests by outcome.
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	// Skipped lists the requests that were never sent.
	Skipped []SkippedRequest `json:"skipped"`
	// Results holds one entry per completed request, in completion order.
	Results []BatchItemResult `json:"results"`
	// StartedAt / EndedAt bracket the whole batch.
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
}

// RunOptions controls one batch execution.
type RunOptions struct {
	// Concurrency caps the number of in-flight requests. 0 means
	// DefaultBatchConcurrency; values above MaxBatchConcurrency are
	// clamped.
	Concurrency int
	// Overrides carries operator-supplied, per-execution request content,
	// keyed by request id. Entries whose id is not part of this batch are
	// ignored, so a stale selection in the UI cannot change another run.
	// Nothing here is persisted.
	Overrides map[string]ContentOverride
}

// ServiceOptions carries the tunables of a Service.
type ServiceOptions struct {
	// Concurrency is the default number of in-flight requests per batch.
	Concurrency int
	// Logger overrides the default logger. Optional.
	Logger *slog.Logger
	// Providers resolves the upstream a request points at. It is
	// required to execute anything: without it every request is skipped
	// with SkipReasonProviderMissing.
	Providers provider.Store
}

// Service executes AI requests on operator demand.
type Service struct {
	store     Store
	providers provider.Store
	prober    Prober
	logger    *slog.Logger

	defaultConcurrency int

	mu      sync.RWMutex
	batches map[string]*Batch
	order   []string
	cancels []context.CancelFunc
}

// runTarget pairs a request with the provider it will be sent to.
type runTarget struct {
	provider provider.Provider
	request  Request
}

// NewService wires a service. The prober may be nil, in which case the
// execution methods report that manual execution is unavailable.
func NewService(store Store, prober Prober, opts ServiceOptions) *Service {
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultBatchConcurrency
	}
	if opts.Concurrency > MaxBatchConcurrency {
		opts.Concurrency = MaxBatchConcurrency
	}
	log := opts.Logger
	if log == nil {
		log = logging.L()
	}
	return &Service{
		store:              store,
		providers:          opts.Providers,
		prober:             prober,
		logger:             log,
		defaultConcurrency: opts.Concurrency,
		batches:            map[string]*Batch{},
	}
}

// ErrNoProber is returned when the service was built without a prober, so
// manual execution cannot work.
var ErrNoProber = fmt.Errorf("request: manual execution is not configured in this build")

// Provider resolution failures. They are separate sentinels because the
// UI has to tell them apart: a request pointing at a deleted provider
// needs a different fix than one whose provider lost its key.
var (
	// ErrProviderMissing means the provider a request points at does not
	// exist (any more).
	ErrProviderMissing = errors.New("request: provider missing")
	// ErrProviderSecret means the provider's stored key could not be
	// decrypted with the current master key.
	ErrProviderSecret = errors.New("request: provider api key cannot be decrypted")
	// ErrProviderNoKey means the provider has no api key configured.
	ErrProviderNoKey = errors.New("request: provider has no api key")
)

// RunRequests starts a batch that executes the given request ids.
//
// Ids that don't resolve are recorded as skipped rather than failing the
// whole batch — a stale selection in the UI shouldn't cost the operator
// the requests that are perfectly fine.
func (s *Service) RunRequests(ctx context.Context, requestIDs []string, opts RunOptions) (*Batch, error) {
	if s.prober == nil {
		return nil, ErrNoProber
	}
	if s.store == nil {
		return nil, fmt.Errorf("request: store is not configured")
	}
	if len(requestIDs) == 0 {
		return nil, fmt.Errorf("request: no requests selected: %w", ErrInvalidConfig)
	}
	return s.start(ctx, requestIDs, opts)
}

// start resolves the requests, assembles the probe configurations and
// kicks off the batch in the background.
func (s *Service) start(ctx context.Context, requestIDs []string, opts RunOptions) (*Batch, error) {
	targets := make([]runTarget, 0, len(requestIDs))
	skipped := make([]SkippedRequest, 0)
	seen := map[string]bool{}

	for _, id := range requestIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		req, err := s.store.GetRequest(ctx, id)
		if err != nil {
			skipped = append(skipped, SkippedRequest{
				ID:     id,
				Name:   id,
				Reason: SkipReasonNotFound,
			})
			continue
		}
		p, err := s.providerFor(ctx, req)
		if err != nil {
			skipped = append(skipped, SkippedRequest{
				ID:     req.ID,
				Name:   req.Name,
				Reason: skipReasonFor(err),
			})
			continue
		}
		// Apply an ad-hoc content override to a *copy* of the request.
		// The stored request is the template; this batch is a one-off, so
		// it must never write through to the store (or to a cached pointer
		// a Store implementation may have handed us).
		it := *req
		if o, ok := opts.Overrides[req.ID]; ok && o.IsSet() {
			if err := o.apply(&it); err != nil {
				return nil, err
			}
		}
		targets = append(targets, runTarget{provider: *p, request: it})
	}

	if len(targets) == 0 {
		return nil, fmt.Errorf("request: none of the selected requests could be prepared: %w", ErrInvalidConfig)
	}

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = s.defaultConcurrency
	}
	if concurrency > MaxBatchConcurrency {
		concurrency = MaxBatchConcurrency
	}

	batch := &Batch{
		ID:        "bat_" + uuid.NewString(),
		State:     BatchRunning,
		Total:     len(targets),
		Skipped:   skipped,
		Results:   []BatchItemResult{},
		StartedAt: time.Now().UTC(),
	}
	s.register(batch)

	// The batch outlives the HTTP request that started it: the handler
	// returns 202 immediately and the client polls for progress. We
	// therefore detach from the request context and cancel the batch via
	// its own context when the service shuts down.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.track(cancel)

	go s.execute(runCtx, batch, targets, concurrency)
	// Hand back a snapshot, not the live pointer: the background goroutine
	// keeps mutating the batch while the caller serialises it, and reading
	// those fields concurrently is a data race.
	return s.snapshot(batch), nil
}

// providerFor resolves the provider a request points at and makes sure it
// is usable.
func (s *Service) providerFor(ctx context.Context, req *Request) (*provider.Provider, error) {
	if s.providers == nil {
		return nil, fmt.Errorf("%w: the provider registry is not configured", ErrProviderMissing)
	}
	p, err := s.providers.GetProvider(ctx, req.ProviderID)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrProviderMissing, err.Error())
	}
	if p.SecretError != "" {
		return nil, fmt.Errorf("%w: provider %q", ErrProviderSecret, p.Name)
	}
	if p.APIKey == "" {
		return nil, fmt.Errorf("%w: provider %q", ErrProviderNoKey, p.Name)
	}
	return p, nil
}

// skipReasonFor maps a provider resolution failure onto a skip reason.
func skipReasonFor(err error) string {
	switch {
	case errors.Is(err, ErrProviderSecret):
		return SkipReasonSecretError
	case errors.Is(err, ErrProviderNoKey):
		return SkipReasonNoAPIKey
	default:
		return SkipReasonProviderMissing
	}
}

// execute fires the batch. Requests run concurrently up to the limit; each
// carries its own deadline so one slow upstream can't hold the batch
// hostage.
func (s *Service) execute(ctx context.Context, batch *Batch, targets []runTarget, concurrency int) {
	sem := semaphore.NewWeighted(int64(concurrency))
	var wg sync.WaitGroup

	for _, tgt := range targets {
		tgt := tgt
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Acquire before starting the timer: a request that waits in
			// the queue shouldn't burn its own timeout.
			if err := sem.Acquire(ctx, 1); err != nil {
				s.finish(batch, BatchItemResult{
					RequestID:    tgt.request.ID,
					RequestName:  tgt.request.Name,
					ProviderID:   tgt.provider.ID,
					ProviderName: tgt.provider.Name,
					Protocol:     string(tgt.provider.Protocol),
					Model:        effectiveModel(tgt.provider, tgt.request),
					Status:       probe.StatusFailure,
					Error:        err.Error(),
					StartedAt:    time.Now().UTC(),
					EndedAt:      time.Now().UTC(),
				})
				return
			}
			defer sem.Release(1)

			timeout := tgt.provider.EffectiveTimeout()
			reqCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			tc := taskConfigFor(tgt.provider, tgt.request)
			res := s.prober.RunManual(reqCtx, tc, probe.RunOptions{
				BatchID:     batch.ID,
				ProviderID:  tgt.provider.ID,
				AIRequestID: tgt.request.ID,
			})
			s.finish(batch, resultFrom(tgt.provider, tgt.request, res))
		}()
	}
	wg.Wait()

	s.mu.Lock()
	batch.State = BatchDone
	batch.EndedAt = time.Now().UTC()
	s.mu.Unlock()
}

// finish records one completed request in the batch.
func (s *Service) finish(batch *Batch, res BatchItemResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch.Results = append(batch.Results, res)
	batch.Completed++
	if res.Status == probe.StatusSuccess {
		batch.Succeeded++
	} else {
		batch.Failed++
	}
}

// register adds a batch to the registry, evicting the oldest one when the
// registry is full.
func (s *Service) register(b *Batch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches[b.ID] = b
	s.order = append(s.order, b.ID)
	for len(s.order) > maxRetainedBatches {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.batches, oldest)
	}
}

// Batch returns a snapshot of a batch. The returned value is a copy, so
// the caller can read it without racing the execution goroutine.
func (s *Service) Batch(id string) (*Batch, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.batches[id]
	if !ok {
		return nil, false
	}
	return snapshotLocked(b), true
}

// snapshot copies a batch under the service lock.
//
// The batch fields are written by the execution goroutine (under the same
// lock), so every read of them has to happen under it too — a bare copy
// would be a data race.
func (s *Service) snapshot(b *Batch) *Batch {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return snapshotLocked(b)
}

// snapshotLocked copies a batch. The caller must hold s.mu (read or write).
//
// The slices are copied with an empty (non-nil) base so an empty batch
// still marshals to `[]` rather than `null` — the frontend iterates them
// without a null check.
func snapshotLocked(b *Batch) *Batch {
	out := *b
	out.Skipped = append([]SkippedRequest{}, b.Skipped...)
	out.Results = append([]BatchItemResult{}, b.Results...)
	return &out
}

// track remembers a cancel function so the batch can be cancelled when
// the service shuts down.
func (s *Service) track(cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancels = append(s.cancels, cancel)
}

// CancelAll aborts every in-flight batch.
func (s *Service) CancelAll() {
	s.mu.Lock()
	cancels := s.cancels
	s.cancels = nil
	s.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}

// taskConfigFor assembles the probe configuration for one request.
//
// The provider supplies the upstream coordinates (protocol, URL,
// credentials, timeout) and the candidate models; the request supplies the
// prompts and the ONE model it selected. Only the protocol block matching
// the provider's protocol is carried over —
// config.TaskConfig.ValidateProtocolSegments rejects a config that sets
// more than one block, and the adapters only read their own.
func taskConfigFor(p provider.Provider, r Request) config.TaskConfig {
	tc := config.TaskConfig{
		ID:   SyntheticTaskID(r.ID),
		Name: p.Name + " / " + r.Name,
		// The synthetic task is never scheduled — it exists only for
		// the duration of one probe — but Enabled must be true so the
		// scheduler-independent runner treats it as runnable.
		Enabled:            true,
		Protocol:           p.Protocol,
		BaseURL:            p.BaseURL,
		APIKey:             p.APIKey,
		Model:              effectiveModel(p, r),
		SystemPrompt:       r.SystemPrompt,
		UserPrompt:         r.UserPrompt,
		PromptVars:         r.PromptVars,
		Timeout:            p.EffectiveTimeout(),
		TimeoutMS:          p.TimeoutMS,
		CanaryGroup:        "manual",
		InsecureSkipVerify: p.InsecureSkipVerify,
		ThinkingEnabled:    r.ThinkingEnabled(p.ThinkingEnabled),
	}
	switch p.Protocol {
	case config.ProtocolOpenAI:
		tc.OpenAI = p.OpenAI
	case config.ProtocolAnthropic:
		tc.Anthropic = p.Anthropic
	case config.ProtocolGemini:
		tc.Gemini = p.Gemini
	}
	return tc
}

// effectiveModel resolves the model a request actually runs against. The
// request's own selection wins; the provider's primary model is only a
// fallback for rows written before the multi-model migration.
func effectiveModel(p provider.Provider, r Request) string {
	if r.Model != "" {
		return r.Model
	}
	return p.PrimaryModel()
}

// SyntheticTaskID is the task id used for a manually executed request.
//
// The "req-" prefix keeps manual runs out of the monitoring task history
// (the per-task run listing filters on task_id) and out of the
// Prometheus label space, because manual runs never reach the sink.
func SyntheticTaskID(requestID string) string {
	return "req-" + requestID
}

// resultFrom converts a probe result into a batch item result.
func resultFrom(p provider.Provider, r Request, res probe.Result) BatchItemResult {
	out := BatchItemResult{
		RequestID:    r.ID,
		RequestName:  r.Name,
		ProviderID:   p.ID,
		ProviderName: p.Name,
		Protocol:     string(p.Protocol),
		Model:        effectiveModel(p, r),
		Status:       res.Status,
		DurationMS:   res.Duration().Milliseconds(),
		TTFTMS:       res.TTFT.Milliseconds(),
		Chunks:       res.Chunks,
		XRequestID:   res.RequestID,
		Input:        res.InputContent,
		Thinking:     res.ThinkingContent,
		Output:       res.OutputContent,
		StartedAt:    res.StartedAt,
		EndedAt:      res.EndedAt,
	}
	if res.Err != nil {
		out.Error = res.Err.Error()
	}
	if out.Status == "" {
		out.Status = probe.StatusFailure
	}
	return out
}
