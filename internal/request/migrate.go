// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package request

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/angryang/ai-monitor/internal/provider"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// MigrationResult summarizes what the legacy migration did.
type MigrationResult struct {
	// GroupsMigrated is the number of legacy request groups that were
	// turned into providers.
	GroupsMigrated int
	// ProvidersReused is the number of legacy groups that matched an
	// already existing provider by name.
	ProvidersReused int
	// RequestsMigrated is the number of legacy request items that were
	// carried over.
	RequestsMigrated int
	// Orphaned is the number of legacy items whose group was missing.
	// They are dropped: without the group there is no upstream to send
	// them to.
	Orphaned int
}

// legacyGroup mirrors the columns of the retired `request_groups` table.
//
// It is declared here rather than reused from the old model so the
// request package carries no trace of the group concept: the struct
// exists only to read rows that are about to be migrated away.
type legacyGroup struct {
	ID          string
	Name        string
	Description string
	Protocol    config.Protocol
	BaseURL     string
	APIKeyEnc   []byte
	Model       string
	TimeoutMS   int64

	InsecureSkipVerify bool
	ThinkingEnabled    bool

	StreamPath     string
	OpenAIExtra    string
	MaxTokens      int
	AnthropicExtra string
	Alt            string
	GeminiExtra    string
}

// legacyItem mirrors the columns of the retired `request_items` table.
type legacyItem struct {
	ID               string
	GroupID          string
	Name             string
	Description      string
	SystemPrompt     string
	UserPrompt       string
	PromptVars       string
	SortOrder        int
	ThinkingOverride *bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// MigrateLegacyGroups converts the retired request-group model into the
// provider registry.
//
// The old model bundled the upstream coordinates (protocol, URL, key,
// model) into a "request group" and hung the requests off it. The new
// model splits those two apart: the coordinates live in the provider
// registry, each request points at one provider. This function performs
// that split on an existing database:
//
//   - every legacy group becomes a provider (or is matched to an existing
//     provider of the same name, which is what happens when the operator
//     already registered the upstream by hand);
//   - every legacy item becomes a request pointing at its group's
//     provider;
//   - the two legacy tables are dropped.
//
// The api key needs no re-encryption on the way: the legacy group and the
// provider registry seal their keys with the same instance master key and
// the same AES-GCM layout, so the ciphertext is copied verbatim.
//
// The function is idempotent — running it twice is a no-op — because it
// drops the legacy tables when it is done.
func MigrateLegacyGroups(ctx context.Context, db *gorm.DB, pstore provider.Store, log *slog.Logger) (MigrationResult, error) {
	var res MigrationResult
	if db == nil {
		return res, fmt.Errorf("request: migrate: nil db handle")
	}
	if log == nil {
		log = slog.Default()
	}
	if !tableExists(ctx, db, "request_groups") {
		return res, nil
	}

	groups, err := readLegacyGroups(ctx, db)
	if err != nil {
		return res, err
	}
	items, err := readLegacyItems(ctx, db)
	if err != nil {
		return res, err
	}

	// group id -> provider id, and the group's single legacy model
	providerForGroup := make(map[string]string, len(groups))
	modelForGroup := make(map[string]string, len(groups))
	for i := range groups {
		g := groups[i]
		pid, reused, err := ensureProvider(ctx, pstore, &g)
		if err != nil {
			return res, err
		}
		providerForGroup[g.ID] = pid
		modelForGroup[g.ID] = g.Model
		if reused {
			res.ProvidersReused++
		} else {
			res.GroupsMigrated++
		}
	}

	for i := range items {
		it := items[i]
		pid, ok := providerForGroup[it.GroupID]
		if !ok {
			res.Orphaned++
			continue
		}
		if err := migrateItem(ctx, db, &it, pid, modelForGroup[it.GroupID]); err != nil {
			return res, err
		}
		res.RequestsMigrated++
	}

	if err := dropLegacyTables(ctx, db); err != nil {
		return res, err
	}

	if res.GroupsMigrated+res.ProvidersReused+res.RequestsMigrated > 0 {
		log.Info("request_groups_migrated",
			slog.Int("groups_to_providers", res.GroupsMigrated),
			slog.Int("providers_reused", res.ProvidersReused),
			slog.Int("requests_migrated", res.RequestsMigrated),
			slog.Int("orphaned_dropped", res.Orphaned),
		)
	}
	if res.Orphaned > 0 {
		log.Warn("legacy requests without a group were dropped",
			slog.Int("count", res.Orphaned))
	}
	return res, nil
}

// ensureProvider finds or creates the provider that replaces one legacy
// group.
func ensureProvider(ctx context.Context, pstore provider.Store, g *legacyGroup) (string, bool, error) {
	// The operator may already have registered the same upstream by
	// hand — reuse it instead of failing on the unique name.
	existing, err := pstore.ListProviders(ctx)
	if err == nil {
		for i := range existing {
			if existing[i].Name == g.Name {
				return existing[i].ID, true, nil
			}
		}
	} else {
		return "", false, fmt.Errorf("request: migrate: list providers: %w", err)
	}

	p := &provider.Provider{
		ID:                 uuid.NewString(),
		Name:               g.Name,
		Description:        g.Description,
		Enabled:            true,
		Protocol:           g.Protocol,
		BaseURL:            g.BaseURL,
		APIKeyEnc:          g.APIKeyEnc,
		Models:             []string{g.Model},
		TimeoutMS:          g.TimeoutMS,
		InsecureSkipVerify: g.InsecureSkipVerify,
		ThinkingEnabled:    g.ThinkingEnabled,
	}
	if g.StreamPath != "" || g.OpenAIExtra != "" {
		p.OpenAI = &config.OpenAIConfig{StreamPath: g.StreamPath, ExtraHeadersJSON: g.OpenAIExtra}
	}
	if g.MaxTokens != 0 || g.AnthropicExtra != "" {
		p.Anthropic = &config.AnthropicConfig{MaxTokens: g.MaxTokens, ExtraHeadersJSON: g.AnthropicExtra}
	}
	if g.Alt != "" || g.GeminiExtra != "" {
		p.Gemini = &config.GeminiConfig{Alt: g.Alt, ExtraHeadersJSON: g.GeminiExtra}
	}
	if err := pstore.CreateProvider(ctx, p); err != nil {
		return "", false, fmt.Errorf("request: migrate: create provider %q: %w", g.Name, err)
	}
	return p.ID, false, nil
}

// migrateItem carries one legacy item over as a request.
func migrateItem(ctx context.Context, db *gorm.DB, it *legacyItem, providerID, model string) error {
	// Find (not First) so an absent row is an empty slice rather than an
	// error — GORM logs every ErrRecordNotFound, which would flood the
	// startup log on a database with hundreds of legacy requests.
	var existing []Request
	if err := db.WithContext(ctx).Where("id = ?", it.ID).Limit(1).Find(&existing).Error; err != nil {
		return fmt.Errorf("request: migrate: read request %q: %w", it.ID, err)
	}
	if len(existing) > 0 {
		// Already carried over (a previous, partial migration): make sure
		// it points at the right provider and has its model selected.
		updates := map[string]any{"provider_id": providerID}
		if existing[0].Model == "" && model != "" {
			updates["model"] = model
		}
		return db.WithContext(ctx).Model(&Request{}).Where("id = ?", it.ID).
			Updates(updates).Error
	}
	r := &Request{
		ID:               it.ID,
		ProviderID:       providerID,
		Model:            model,
		Name:             it.Name,
		Description:      it.Description,
		SystemPrompt:     it.SystemPrompt,
		UserPrompt:       it.UserPrompt,
		PromptVarsJSON:   it.PromptVars,
		Order:            it.SortOrder,
		ThinkingOverride: it.ThinkingOverride,
	}
	if !it.CreatedAt.IsZero() {
		r.CreatedAt = it.CreatedAt
	}
	if !it.UpdatedAt.IsZero() {
		r.UpdatedAt = it.UpdatedAt
	}
	if err := db.WithContext(ctx).Create(r).Error; err != nil {
		return fmt.Errorf("request: migrate: create request %q: %w", it.ID, err)
	}
	return nil
}

// readLegacyGroups loads the retired request_groups rows.
func readLegacyGroups(ctx context.Context, db *gorm.DB) ([]legacyGroup, error) {
	var out []legacyGroup
	err := db.WithContext(ctx).Raw(`
		SELECT id, name, description, protocol, base_url, api_key_enc, model, timeout_ms,
		       insecure_skip_verify, thinking_enabled, stream_path, openai_extra_headers,
		       max_tokens, anthropic_extra_headers, alt, gemini_extra_headers
		FROM request_groups`).Scan(&out).Error
	if err != nil {
		return nil, fmt.Errorf("request: migrate: read request_groups: %w", err)
	}
	return out, nil
}

// readLegacyItems loads the retired request_items rows.
func readLegacyItems(ctx context.Context, db *gorm.DB) ([]legacyItem, error) {
	var out []legacyItem
	err := db.WithContext(ctx).Raw(`
		SELECT id, group_id, name, description, system_prompt, user_prompt,
		       prompt_vars, sort_order, thinking_override, created_at, updated_at
		FROM request_items`).Scan(&out).Error
	if err != nil {
		return nil, fmt.Errorf("request: migrate: read request_items: %w", err)
	}
	return out, nil
}

// dropLegacyTables removes the two retired tables.
func dropLegacyTables(ctx context.Context, db *gorm.DB) error {
	for _, name := range []string{"request_items", "request_groups"} {
		if !tableExists(ctx, db, name) {
			continue
		}
		if err := db.WithContext(ctx).Exec("DROP TABLE IF EXISTS " + name).Error; err != nil {
			return fmt.Errorf("request: migrate: drop %s: %w", name, err)
		}
	}
	return nil
}

// tableExists reports whether a table is present in the database.
func tableExists(ctx context.Context, db *gorm.DB, name string) bool {
	var count int64
	err := db.WithContext(ctx).
		Raw("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).
		Scan(&count).Error
	if err != nil {
		return false
	}
	return count > 0
}
