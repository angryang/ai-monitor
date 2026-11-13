// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/angryang/ai-monitor/internal/config"
)

// newTestStore builds a provider store on top of an in-memory task store,
// mirroring production: both share one GORM handle and one SQLite file.
func newTestStore(t *testing.T) (*SqliteStore, *config.AESCipher) {
	t.Helper()
	taskStore, err := config.NewSqliteStore(config.SqliteOptions{Path: ":memory:"})
	if err != nil {
		t.Fatalf("task store: %v", err)
	}
	t.Cleanup(func() { _ = taskStore.Close() })
	cipher, err := config.NewAESCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	store, err := NewSqliteStore(taskStore.DB())
	if err != nil {
		t.Fatalf("provider store: %v", err)
	}
	store.SetCipher(cipher)
	return store, cipher
}

func seedProvider(t *testing.T, store *SqliteStore, name string) *Provider {
	t.Helper()
	p := &Provider{
		ID:        "prov-" + name,
		Name:      name,
		Protocol:  config.ProtocolOpenAI,
		BaseURL:   "https://e.test",
		APIKey:    "sk-secret",
		Model:     "gpt-test",
		TimeoutMS: 30000,
	}
	if err := store.CreateProvider(context.Background(), p); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	return p
}

// TestStore_ProviderRoundTrip pins the create → read → update cycle,
// including the api key encryption at rest.
func TestStore_ProviderRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	p := seedProvider(t, store, "alpha")
	if p.ID == "" {
		t.Fatalf("provider id was not assigned")
	}

	got, err := store.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if got.Name != "alpha" {
		t.Fatalf("name = %q, want alpha", got.Name)
	}
	if got.APIKey != "sk-secret" {
		t.Fatalf("api key = %q, want the decrypted value", got.APIKey)
	}
	if len(got.APIKeyEnc) == 0 {
		t.Fatalf("api key was not sealed")
	}
	if string(got.APIKeyEnc) == "sk-secret" {
		t.Fatalf("api key was persisted in the clear")
	}
	if got.SecretError != "" {
		t.Fatalf("secret error = %q, want empty", got.SecretError)
	}
	if !got.Enabled {
		t.Fatalf("enabled = false, want true by default")
	}

	// Update: the api key is omitted, so the stored one must survive.
	got.Models = []string{"gpt-4o"}
	got.APIKey = ""
	if err := store.UpdateProvider(ctx, got); err != nil {
		t.Fatalf("update provider: %v", err)
	}
	again, err := store.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if again.PrimaryModel() != "gpt-4o" || len(again.Models) != 1 {
		t.Fatalf("models = %v, want [gpt-4o]", again.Models)
	}
	if again.APIKey != "sk-secret" {
		t.Fatalf("api key = %q, want the key to survive an update that omits it", again.APIKey)
	}
}

// TestStore_DuplicateProviderName pins the uniqueness contract.
func TestStore_DuplicateProviderName(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	seedProvider(t, store, "alpha")

	dup := &Provider{
		ID:       "prov-other",
		Name:     "alpha",
		Protocol: config.ProtocolOpenAI,
		BaseURL:  "https://e.test",
		Model:    "gpt-test",
	}
	if err := store.CreateProvider(ctx, dup); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("err = %v, want ErrAlreadyExists", err)
	}
}

// TestStore_ProviderNotFound pins the not-found contract.
func TestStore_ProviderNotFound(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := store.GetProvider(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if err := store.DeleteProvider(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete err = %v, want ErrNotFound", err)
	}
}

// TestStore_SecretErrorOnWrongKey is the important operational case: the
// master key changed under the operator's feet. The provider must still be
// listed — with the failure recorded — so they can repair it.
func TestStore_SecretErrorOnWrongKey(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	p := seedProvider(t, store, "alpha")

	other, err := config.NewAESCipher([]byte("fedcba9876543210fedcba9876543210"))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	store.SetCipher(other)

	got, err := store.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if got.SecretError == "" {
		t.Fatalf("secret error is empty; want the decryption failure")
	}
	if got.APIKey != "" {
		t.Fatalf("api key = %q, want empty when it cannot be decrypted", got.APIKey)
	}
	if got.Runnable() {
		t.Fatalf("provider with a broken secret must not be runnable")
	}
	// The ciphertext must survive: the operator may restore the right
	// master key, and an unrelated write must not destroy the secret.
	if len(got.APIKeyEnc) == 0 {
		t.Fatalf("ciphertext was dropped on a decryption failure")
	}
}

// TestStore_SecretSurvivesUnrelatedUpdate pins the counterpart of the
// case above: updating a provider whose key cannot be decrypted must not
// wipe the stored ciphertext.
func TestStore_SecretSurvivesUnrelatedUpdate(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	p := seedProvider(t, store, "alpha")

	other, err := config.NewAESCipher([]byte("fedcba9876543210fedcba9876543210"))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	store.SetCipher(other)

	got, err := store.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	got.Models = []string{"gpt-4o"}
	if err := store.UpdateProvider(ctx, got); err != nil {
		t.Fatalf("update provider: %v", err)
	}

	// Read the raw column back with the original cipher.
	store.SetCipher(mustCipher(t))
	again, err := store.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if again.APIKey != "sk-secret" {
		t.Fatalf("api key = %q, want the ciphertext to survive the update", again.APIKey)
	}
}

// mustCipher returns the shared test cipher.
func mustCipher(t *testing.T) *config.AESCipher {
	t.Helper()
	c, err := config.NewAESCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	return c
}

// TestStore_ListProviders pins the listing contract.
func TestStore_ListProviders(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	seedProvider(t, store, "alpha")
	seedProvider(t, store, "beta")

	list, err := store.ListProviders(ctx)
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("providers = %d, want 2", len(list))
	}
	if list[0].Name != "alpha" || list[1].Name != "beta" {
		t.Fatalf("providers not ordered by creation: %s, %s", list[0].Name, list[1].Name)
	}
}

// TestStore_DeleteProvider pins the delete contract.
func TestStore_DeleteProvider(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	p := seedProvider(t, store, "alpha")

	if err := store.DeleteProvider(ctx, p.ID); err != nil {
		t.Fatalf("delete provider: %v", err)
	}
	if _, err := store.GetProvider(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("provider survived the deletion: %v", err)
	}
}

// TestStore_RejectsInvalidProviders pins the validation contract.
func TestStore_RejectsInvalidProviders(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	cases := map[string]*Provider{
		"no id":      {Name: "n", Protocol: config.ProtocolOpenAI, BaseURL: "https://e.test", Model: "m"},
		"no name":    {ID: "p1", Protocol: config.ProtocolOpenAI, BaseURL: "https://e.test", Model: "m"},
		"no base":    {ID: "p2", Name: "n", Protocol: config.ProtocolOpenAI, Model: "m"},
		"no model":   {ID: "p3", Name: "n", Protocol: config.ProtocolOpenAI, BaseURL: "https://e.test"},
		"bad proto":  {ID: "p4", Name: "n", Protocol: "nope", BaseURL: "https://e.test", Model: "m"},
		"big tmount": {ID: "p5", Name: "n", Protocol: config.ProtocolOpenAI, BaseURL: "https://e.test", Model: "m", TimeoutMS: (11 * time.Minute).Milliseconds()},
	}
	for name, p := range cases {
		if err := store.CreateProvider(ctx, p); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("%s: err = %v, want ErrInvalidConfig", name, err)
		}
	}
}

// TestStore_ProtocolBlocksRoundTrip pins that the embedded protocol
// blocks survive a write/read cycle — the OpenAI stream path in
// particular decides which URL the probe hits.
func TestStore_ProtocolBlocksRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	p := &Provider{
		ID:       "prov-blocks",
		Name:     "blocks",
		Protocol: config.ProtocolOpenAI,
		BaseURL:  "https://e.test",
		APIKey:   "sk-secret",
		Model:    "gpt-test",
		OpenAI:   &config.OpenAIConfig{StreamPath: "/custom/chat"},
	}
	if err := store.CreateProvider(ctx, p); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	got, err := store.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if got.OpenAI == nil {
		t.Fatalf("openai block is nil")
	}
	if got.OpenAI.StreamPath != "/custom/chat" {
		t.Fatalf("stream path = %q, want /custom/chat", got.OpenAI.StreamPath)
	}
}

// TestStore_VerificationColumnsRoundTrip pins the columns that record
// the verdict of the connectivity check.
func TestStore_VerificationColumnsRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	p := &Provider{
		ID:                  "prov-verified",
		Name:                "verified",
		Protocol:            config.ProtocolOpenAI,
		BaseURL:             "https://e.test",
		APIKey:              "sk-secret",
		Model:               "gpt-test",
		LastVerifiedAt:      &now,
		LastVerifiedOK:      true,
		LastVerifiedMessage: "upstream accepted the request",
	}
	if err := store.CreateProvider(ctx, p); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	got, err := store.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if !got.LastVerifiedOK {
		t.Fatalf("last_verified_ok = false, want true")
	}
	if got.LastVerifiedAt == nil || !got.LastVerifiedAt.Equal(now) {
		t.Fatalf("last_verified_at = %v, want %v", got.LastVerifiedAt, now)
	}
	if got.LastVerifiedMessage != "upstream accepted the request" {
		t.Fatalf("last_verified_message = %q", got.LastVerifiedMessage)
	}
}
