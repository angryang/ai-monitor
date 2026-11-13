// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package request

import (
	"context"
	"errors"
	"testing"

	"github.com/angryang/ai-monitor/internal/config"
)

// newTestStore builds a request store on top of an in-memory task store,
// mirroring production: everything shares one GORM handle and one SQLite
// file. Requests carry no secrets of their own, so unlike the old group
// model there is no cipher to install here.
func newTestStore(t *testing.T) *SqliteStore {
	t.Helper()
	taskStore, err := config.NewSqliteStore(config.SqliteOptions{Path: ":memory:"})
	if err != nil {
		t.Fatalf("task store: %v", err)
	}
	t.Cleanup(func() { _ = taskStore.Close() })
	store, err := NewSqliteStore(taskStore.DB())
	if err != nil {
		t.Fatalf("request store: %v", err)
	}
	return store
}

func seedRequest(t *testing.T, store *SqliteStore, id, providerID string) *Request {
	t.Helper()
	r := &Request{
		ID:           id,
		ProviderID:   providerID,
		Name:         "greeting",
		UserPrompt:   "hello ${who}",
		PromptVars:   map[string]string{"who": "world"},
		SystemPrompt: "be brief",
		Model:        "model-a",
		Order:        2,
	}
	if err := store.CreateRequest(context.Background(), r); err != nil {
		t.Fatalf("create request: %v", err)
	}
	return r
}

// TestStore_RequestRoundTrip pins the create → read → update cycle and
// the prompt-variable serialisation.
func TestStore_RequestRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	r := seedRequest(t, store, "req-1", "prv-a")

	got, err := store.GetRequest(ctx, r.ID)
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	if got.ProviderID != "prv-a" {
		t.Fatalf("provider_id = %q, want prv-a", got.ProviderID)
	}
	if got.UserPrompt != "hello ${who}" {
		t.Fatalf("user prompt = %q", got.UserPrompt)
	}
	if got.PromptVars["who"] != "world" {
		t.Fatalf("prompt vars = %v, want who=world", got.PromptVars)
	}
	if got.Order != 2 {
		t.Fatalf("order = %d, want 2", got.Order)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not populated: created=%v updated=%v", got.CreatedAt, got.UpdatedAt)
	}

	// Update: a request may be re-pointed at a completely different
	// provider. Nothing else should change.
	got.Name = "greeting-2"
	got.ProviderID = "prv-b"
	if err := store.UpdateRequest(ctx, got); err != nil {
		t.Fatalf("update request: %v", err)
	}
	again, err := store.GetRequest(ctx, r.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if again.Name != "greeting-2" || again.ProviderID != "prv-b" {
		t.Fatalf("update lost: name=%q provider=%q", again.Name, again.ProviderID)
	}
	if again.PromptVars["who"] != "world" {
		t.Fatalf("prompt vars lost on update: %v", again.PromptVars)
	}
	if again.UpdatedAt.Before(got.CreatedAt) {
		t.Fatalf("updated_at went backwards: created=%v updated=%v", got.CreatedAt, again.UpdatedAt)
	}
}

// TestStore_ListIsFlatAndOrdered is the defining assertion of the new
// model: requests from different providers appear in ONE list, with no
// grouping layer in between.
func TestStore_ListIsFlatAndOrdered(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	store.CreateRequest(ctx, &Request{ID: "b", ProviderID: "prv-2", Name: "second", UserPrompt: "x", Order: 2, Model: "model-a"})
	store.CreateRequest(ctx, &Request{ID: "a", ProviderID: "prv-1", Name: "first", UserPrompt: "x", Order: 1, Model: "model-a"})
	store.CreateRequest(ctx, &Request{ID: "c", ProviderID: "prv-1", Name: "third", UserPrompt: "x", Order: 3, Model: "model-a"})

	all, err := store.ListRequests(ctx)
	if err != nil {
		t.Fatalf("list requests: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("requests = %d, want 3", len(all))
	}
	want := []string{"a", "b", "c"}
	providers := []string{"prv-1", "prv-2", "prv-1"}
	for i := range all {
		if all[i].ID != want[i] || all[i].ProviderID != providers[i] {
			t.Fatalf("row %d = %s/%s, want %s/%s", i, all[i].ID, all[i].ProviderID, want[i], providers[i])
		}
	}
}

// TestStore_ListRequestsByProvider backs the provider-delete guard.
func TestStore_ListRequestsByProvider(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	store.CreateRequest(ctx, &Request{ID: "a", ProviderID: "prv-1", Name: "a", UserPrompt: "x", Model: "model-a"})
	store.CreateRequest(ctx, &Request{ID: "b", ProviderID: "prv-2", Name: "b", UserPrompt: "x", Model: "model-a"})
	store.CreateRequest(ctx, &Request{ID: "c", ProviderID: "prv-1", Name: "c", UserPrompt: "x", Model: "model-a"})

	got, err := store.ListRequestsByProvider(ctx, "prv-1")
	if err != nil {
		t.Fatalf("list by provider: %v", err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Fatalf("requests = %v, want a and c", got)
	}
	none, err := store.ListRequestsByProvider(ctx, "prv-3")
	if err != nil || len(none) != 0 {
		t.Fatalf("unused provider = %v, %v; want empty", none, err)
	}
}

// TestStore_NotFound pins the not-found contract on both read and delete.
func TestStore_NotFound(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.GetRequest(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get err = %v, want ErrNotFound", err)
	}
	if err := store.DeleteRequest(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete err = %v, want ErrNotFound", err)
	}
	store.CreateRequest(context.Background(), &Request{ID: "a", ProviderID: "p", Name: "a", UserPrompt: "x", Model: "model-a"})
	if err := store.DeleteRequest(context.Background(), "a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.GetRequest(context.Background(), "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("request survived deletion: %v", err)
	}
}

// TestStore_RejectsInvalidRequests pins the validation contract. In
// particular a request must point at a provider — a request without an
// upstream is not a request.
func TestStore_RejectsInvalidRequests(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	cases := map[string]*Request{
		"no id":       {ProviderID: "p", Name: "n", UserPrompt: "x", Model: "m"},
		"no provider": {ID: "i2", Name: "n", UserPrompt: "x", Model: "m"},
		"no name":     {ID: "i3", ProviderID: "p", UserPrompt: "x", Model: "m"},
		"no prompt":   {ID: "i4", ProviderID: "p", Name: "n", Model: "m"},
		"no model":    {ID: "i5", ProviderID: "p", Name: "n", UserPrompt: "x"},
	}
	for name, r := range cases {
		if err := store.CreateRequest(ctx, r); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("%s: err = %v, want ErrInvalidConfig", name, err)
		}
	}
	if err := store.CreateRequest(ctx, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil request: err = %v, want ErrInvalidConfig", err)
	}
}
