// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package request

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/probe"
	"github.com/angryang/ai-monitor/internal/provider"
)

// fakeProber stands in for the probe runner. It records the task config it
// was handed and can simulate a slow upstream so the concurrency limit is
// observable.
type fakeProber struct {
	mu sync.Mutex
	// calls records the (task, options) pairs in arrival order.
	calls []proberCall
	// inflight / peak track the concurrency actually observed.
	inflight int
	peak     int
	// delay is how long each call pretends to take.
	delay time.Duration
	// err makes every call fail.
	err error
}

type proberCall struct {
	task config.TaskConfig
	opts probe.RunOptions
}

func (f *fakeProber) RunManual(ctx context.Context, t config.TaskConfig, opts probe.RunOptions) probe.Result {
	f.mu.Lock()
	f.calls = append(f.calls, proberCall{task: t, opts: opts})
	f.inflight++
	if f.inflight > f.peak {
		f.peak = f.inflight
	}
	delay := f.delay
	err := f.err
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
		}
	}

	f.mu.Lock()
	f.inflight--
	f.mu.Unlock()

	res := probe.Result{
		TaskID:    t.ID,
		TaskName:  t.Name,
		Protocol:  string(t.Protocol),
		Model:     t.Model,
		Status:    probe.StatusSuccess,
		StartedAt: time.Now().UTC(),
		EndedAt:   time.Now().UTC(),
	}
	if err != nil {
		res.Status = probe.StatusFailure
		res.Err = err
	}
	return res
}

func (f *fakeProber) snapshot() ([]proberCall, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]proberCall, len(f.calls))
	copy(out, f.calls)
	return out, f.peak
}

// fakeProviderStore is the smallest provider.Store that lets the service
// resolve a request's upstream.
type fakeProviderStore struct {
	mu        sync.Mutex
	providers map[string]*provider.Provider
}

func newFakeProviderStore() *fakeProviderStore {
	return &fakeProviderStore{providers: map[string]*provider.Provider{}}
}

func (s *fakeProviderStore) add(p *provider.Provider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.providers[p.ID] = p
}

func (s *fakeProviderStore) GetProvider(_ context.Context, id string) (*provider.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.providers[id]
	if !ok {
		return nil, provider.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (s *fakeProviderStore) ListProviders(_ context.Context) ([]provider.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]provider.Provider, 0, len(s.providers))
	for _, p := range s.providers {
		out = append(out, *p)
	}
	return out, nil
}
func (s *fakeProviderStore) CreateProvider(_ context.Context, p *provider.Provider) error {
	s.add(p)
	return nil
}
func (s *fakeProviderStore) UpdateProvider(_ context.Context, p *provider.Provider) error {
	s.add(p)
	return nil
}
func (s *fakeProviderStore) DeleteProvider(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.providers, id)
	return nil
}

// waitBatch polls until the batch finishes. The service runs the batch in
// the background, so the test has to wait for it.
func waitBatch(t *testing.T, svc *Service, id string) *Batch {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, ok := svc.Batch(id)
		if ok && b.State == BatchDone {
			return b
		}
		time.Sleep(5 * time.Millisecond)
	}
	b, _ := svc.Batch(id)
	t.Fatalf("batch %s did not finish in time (state=%v)", id, b)
	return nil
}

// seedService builds a service over an in-memory store with two providers
// and the given number of requests. Requests alternate between the two
// providers to keep the "one request, one provider" rule under test.
func seedService(t *testing.T, count int) (*Service, *SqliteStore, *fakeProber, *fakeProviderStore, []string) {
	t.Helper()
	store := newTestStore(t)
	ctx := context.Background()

	providers := newFakeProviderStore()
	for _, name := range []string{"alpha", "beta"} {
		providers.add(&provider.Provider{
			ID:        "prv-" + name,
			Name:      name,
			Protocol:  config.ProtocolOpenAI,
			BaseURL:   "https://" + name + ".test",
			APIKey:    "sk-" + name,
			Models:    []string{"model-" + name},
			TimeoutMS: 30000,
		})
	}

	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		id := "req-" + string(rune('a'+i))
		pid := "prv-alpha"
		if i%2 == 1 {
			pid = "prv-beta"
		}
		model := "model-alpha"
		if i%2 == 1 {
			model = "model-beta"
		}
		r := &Request{
			ID:         id,
			ProviderID: pid,
			Name:       "req",
			UserPrompt: "hello",
			Model:      model,
			Order:      i,
		}
		if err := store.CreateRequest(ctx, r); err != nil {
			t.Fatalf("create request: %v", err)
		}
		ids = append(ids, id)
	}
	prober := &fakeProber{}
	svc := NewService(store, prober, ServiceOptions{Providers: providers})
	t.Cleanup(svc.CancelAll)
	return svc, store, prober, providers, ids
}

// TestService_RunRequests_Single pins "select one request, run one probe".
func TestService_RunRequests_Single(t *testing.T) {
	svc, _, prober, _, ids := seedService(t, 1)

	batch, err := svc.RunRequests(context.Background(), ids[:1], RunOptions{})
	if err != nil {
		t.Fatalf("run requests: %v", err)
	}
	if batch.Total != 1 {
		t.Fatalf("total = %d, want 1", batch.Total)
	}
	done := waitBatch(t, svc, batch.ID)
	if done.Completed != 1 || done.Succeeded != 1 || done.Failed != 0 {
		t.Fatalf("completed=%d succeeded=%d failed=%d, want 1/1/0",
			done.Completed, done.Succeeded, done.Failed)
	}
	if len(done.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(done.Results))
	}
	if done.Results[0].RequestID != ids[0] {
		t.Fatalf("result request = %q, want %q", done.Results[0].RequestID, ids[0])
	}
	if done.Results[0].ProviderID != "prv-alpha" {
		t.Fatalf("result provider = %q, want prv-alpha", done.Results[0].ProviderID)
	}
	calls, _ := prober.snapshot()
	if len(calls) != 1 {
		t.Fatalf("prober calls = %d, want 1", len(calls))
	}
}

// TestService_DifferentProviders is the point of the refactor: a single
// batch may send its requests to different upstreams, and each probe must
// carry the coordinates of the provider its own request points at.
func TestService_DifferentProviders(t *testing.T) {
	svc, _, prober, _, ids := seedService(t, 4)

	batch, err := svc.RunRequests(context.Background(), ids, RunOptions{})
	if err != nil {
		t.Fatalf("run requests: %v", err)
	}
	done := waitBatch(t, svc, batch.ID)
	if done.Total != 4 || done.Succeeded != 4 {
		t.Fatalf("total=%d succeeded=%d, want 4/4", done.Total, done.Succeeded)
	}

	calls, _ := prober.snapshot()
	if len(calls) != 4 {
		t.Fatalf("prober calls = %d, want 4", len(calls))
	}
	// Each call's base URL must match its own provider, not a shared one.
	seen := map[string]bool{}
	for i, c := range calls {
		if c.opts.BatchID != batch.ID {
			t.Fatalf("call %d batch id = %q, want %q", i, c.opts.BatchID, batch.ID)
		}
		if c.opts.AIRequestID == "" || c.opts.ProviderID == "" {
			t.Fatalf("call %d lost provenance: %+v", i, c.opts)
		}
		if c.task.BaseURL != "https://"+c.opts.ProviderID[len("prv-"):]+".test" {
			t.Fatalf("call %d base url = %q for provider %q", i, c.task.BaseURL, c.opts.ProviderID)
		}
		seen[c.opts.ProviderID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("batch reached %d providers, want 2", len(seen))
	}
	// The batch result must report the provider of each request too.
	providers := map[string]bool{}
	for _, r := range done.Results {
		providers[r.ProviderID] = true
		if r.ProviderName == "" {
			t.Fatalf("result %q has no provider name", r.RequestID)
		}
	}
	if len(providers) != 2 {
		t.Fatalf("results mention %d providers, want 2", len(providers))
	}
}

// TestService_ProvenanceReachesTaskConfig pins that the probe sees the
// provider's coordinates and the request's prompts — this is what makes a
// manual request equivalent to a monitoring probe.
func TestService_ProvenanceReachesTaskConfig(t *testing.T) {
	svc, _, prober, _, ids := seedService(t, 1)

	batch, err := svc.RunRequests(context.Background(), ids[:1], RunOptions{})
	if err != nil {
		t.Fatalf("run requests: %v", err)
	}
	waitBatch(t, svc, batch.ID)

	calls, _ := prober.snapshot()
	if len(calls) != 1 {
		t.Fatalf("prober calls = %d, want 1", len(calls))
	}
	tc := calls[0].task
	if tc.BaseURL != "https://alpha.test" {
		t.Fatalf("base url = %q, want the provider's base url", tc.BaseURL)
	}
	if tc.APIKey != "sk-alpha" {
		t.Fatalf("api key = %q, want the provider's key", tc.APIKey)
	}
	if tc.Model != "model-alpha" {
		t.Fatalf("model = %q, want the request's selected model", tc.Model)
	}
	if tc.UserPrompt != "hello" {
		t.Fatalf("user prompt = %q, want the request's prompt", tc.UserPrompt)
	}
	if tc.Protocol != config.ProtocolOpenAI {
		t.Fatalf("protocol = %q, want openai", tc.Protocol)
	}
	if tc.ID != SyntheticTaskID(ids[0]) {
		t.Fatalf("synthetic task id = %q, want %q", tc.ID, SyntheticTaskID(ids[0]))
	}
	// Only the matching protocol block may travel with the config —
	// TaskConfig.ValidateProtocolSegments rejects a config that sets more
	// than one.
	if tc.Anthropic != nil || tc.Gemini != nil {
		t.Fatalf("non-matching protocol blocks leaked into the config: %+v", tc)
	}
}

// TestService_SkipsMissingProvider pins that a request pointing at a
// deleted provider is reported as skipped rather than taking down the
// whole batch.
func TestService_SkipsMissingProvider(t *testing.T) {
	svc, store, prober, _, ids := seedService(t, 2)

	// Re-point just one request at a provider that does not exist.
	r, err := store.GetRequest(context.Background(), ids[1])
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	r.ProviderID = "prv-gone"
	if err := store.UpdateRequest(context.Background(), r); err != nil {
		t.Fatalf("update request: %v", err)
	}

	batch, err := svc.RunRequests(context.Background(), ids, RunOptions{})
	if err != nil {
		t.Fatalf("run requests: %v", err)
	}
	done := waitBatch(t, svc, batch.ID)
	if done.Total != 1 || done.Succeeded != 1 {
		t.Fatalf("total=%d succeeded=%d, want 1/1", done.Total, done.Succeeded)
	}
	if len(done.Skipped) != 1 || done.Skipped[0].ID != ids[1] ||
		done.Skipped[0].Reason != SkipReasonProviderMissing {
		t.Fatalf("skipped = %+v, want %s as provider_missing", done.Skipped, ids[1])
	}
	calls, _ := prober.snapshot()
	if len(calls) != 1 {
		t.Fatalf("prober calls = %d, want 1", len(calls))
	}
}

// TestService_SkipsProviderWithBrokenSecret pins the master-key change
// case: the provider is still listed but its key cannot be opened. The
// other request points at a healthy provider and must still run — a bad
// upstream must not take down the whole batch.
func TestService_SkipsProviderWithBrokenSecret(t *testing.T) {
	svc, _, prober, providers, ids := seedService(t, 2)

	p, err := providers.GetProvider(context.Background(), "prv-alpha")
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	p.APIKey = ""
	p.SecretError = "cipher: authentication failed"
	providers.add(p)

	batch, err := svc.RunRequests(context.Background(), ids, RunOptions{})
	if err != nil {
		t.Fatalf("run requests: %v", err)
	}
	done := waitBatch(t, svc, batch.ID)
	if done.Total != 1 || done.Succeeded != 1 {
		t.Fatalf("total=%d succeeded=%d, want 1/1", done.Total, done.Succeeded)
	}
	if len(done.Skipped) != 1 || done.Skipped[0].ID != ids[0] ||
		done.Skipped[0].Reason != SkipReasonSecretError {
		t.Fatalf("skipped = %+v, want %s as secret_error", done.Skipped, ids[0])
	}
	calls, _ := prober.snapshot()
	if len(calls) != 1 || calls[0].opts.ProviderID != "prv-beta" {
		t.Fatalf("prober calls = %+v, want exactly one call to the healthy provider", calls)
	}
}

// TestService_AllProvidersBroken pins that a batch where nothing can be
// prepared is rejected up front instead of returning an empty 202.
func TestService_AllProvidersBroken(t *testing.T) {
	svc, _, prober, providers, ids := seedService(t, 1)

	p, err := providers.GetProvider(context.Background(), "prv-alpha")
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	p.APIKey = ""
	p.SecretError = "cipher: authentication failed"
	providers.add(p)

	if _, err := svc.RunRequests(context.Background(), ids[:1], RunOptions{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig (nothing could be prepared)", err)
	}
	calls, _ := prober.snapshot()
	if len(calls) != 0 {
		t.Fatalf("prober was called %d times for an undecryptable provider; want 0", len(calls))
	}
}

// TestService_SkipsUnknownRequest pins that a stale selection doesn't
// cost the operator the requests that are fine.
func TestService_SkipsUnknownRequest(t *testing.T) {
	svc, _, prober, _, ids := seedService(t, 2)

	batch, err := svc.RunRequests(context.Background(), append(ids, "ghost"), RunOptions{})
	if err != nil {
		t.Fatalf("run requests: %v", err)
	}
	done := waitBatch(t, svc, batch.ID)
	if done.Total != 2 {
		t.Fatalf("total = %d, want 2 (the ghost must not count)", done.Total)
	}
	if len(done.Skipped) != 1 {
		t.Fatalf("skipped = %d, want 1", len(done.Skipped))
	}
	if done.Skipped[0].ID != "ghost" || done.Skipped[0].Reason != SkipReasonNotFound {
		t.Fatalf("skipped = %+v, want the ghost reported as not found", done.Skipped[0])
	}
	calls, _ := prober.snapshot()
	if len(calls) != 2 {
		t.Fatalf("prober calls = %d, want 2", len(calls))
	}
}

// TestService_ConcurrencyLimit pins that a batch never fires more
// requests at once than it was told to.
func TestService_ConcurrencyLimit(t *testing.T) {
	svc, _, prober, _, ids := seedService(t, 6)
	prober.delay = 20 * time.Millisecond

	batch, err := svc.RunRequests(context.Background(), ids, RunOptions{Concurrency: 2})
	if err != nil {
		t.Fatalf("run requests: %v", err)
	}
	done := waitBatch(t, svc, batch.ID)
	if done.Succeeded != 6 {
		t.Fatalf("succeeded = %d, want 6", done.Succeeded)
	}
	_, peak := prober.snapshot()
	if peak > 2 {
		t.Fatalf("peak concurrency = %d, want <= 2", peak)
	}
	if peak < 2 {
		t.Fatalf("peak concurrency = %d, want the limit to actually be used", peak)
	}
}

// TestService_FailureIsReported pins that a failing request shows up as a
// failed result rather than vanishing.
func TestService_FailureIsReported(t *testing.T) {
	svc, _, prober, _, ids := seedService(t, 1)
	prober.err = errors.New("upstream exploded")

	batch, err := svc.RunRequests(context.Background(), ids[:1], RunOptions{})
	if err != nil {
		t.Fatalf("run requests: %v", err)
	}
	done := waitBatch(t, svc, batch.ID)
	if done.Failed != 1 || done.Succeeded != 0 {
		t.Fatalf("failed=%d succeeded=%d, want 1/0", done.Failed, done.Succeeded)
	}
	if done.Results[0].Error == "" {
		t.Fatalf("failure has no error message")
	}
}

// TestService_EmptySelection pins the input validation.
func TestService_EmptySelection(t *testing.T) {
	svc, _, _, _, _ := seedService(t, 1)
	if _, err := svc.RunRequests(context.Background(), nil, RunOptions{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

// TestService_NoProber pins that a build without a probe runner reports
// the feature as unavailable instead of failing obscurely.
func TestService_NoProber(t *testing.T) {
	store := newTestStore(t)
	svc := NewService(store, nil, ServiceOptions{})
	if _, err := svc.RunRequests(context.Background(), []string{"x"}, RunOptions{}); !errors.Is(err, ErrNoProber) {
		t.Fatalf("err = %v, want ErrNoProber", err)
	}
}
