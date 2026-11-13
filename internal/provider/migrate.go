// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/angryang/ai-monitor/internal/config"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// TaskMigrationResult summarizes what the legacy monitoring-task migration
// did.
type TaskMigrationResult struct {
	// TasksMigrated is the number of legacy tasks carried over.
	TasksMigrated int
	// ProvidersCreated is the number of new providers that were registered
	// for the migrated tasks.
	ProvidersCreated int
	// ProvidersReused is the number of tasks whose upstream already
	// existed as a provider.
	ProvidersReused int
}

// legacyTask mirrors the columns of the retired task schema, where every
// row carried a full copy of the upstream coordinates.
//
// The struct exists only to read rows that are about to be migrated away;
// the new task model has none of these columns.
type legacyTask struct {
	ID      string
	Name    string
	Enabled bool
	Protocol config.Protocol
	BaseURL  string
	APIKeyEnc []byte
	Model     string
	TimeoutMS int64

	InsecureSkipVerify bool
	ThinkingEnabled    bool

	StreamPath     string
	OpenAIExtra    string
	MaxTokens      int
	AnthropicExtra string
	Alt            string
	GeminiExtra    string

	SystemPrompt   string
	UserPrompt     string
	PromptVars     string
	Schedule       string
	CanaryGroup    string
	Channel        string
	WeComWebhookEnc []byte
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// MigrateLegacyTasks converts the retired "task owns the upstream" model
// into the provider-referencing one.
//
// The old task table carried a full copy of the upstream coordinates —
// protocol, base URL, api key, model, timeout, TLS / thinking switches and
// the protocol-specific block — on every row. Two tasks pointed at the
// same endpoint therefore had to be edited twice, and a credential change
// had to be replayed on every row.
//
// The new model splits those two apart: the coordinates live in the
// provider registry, each task names the provider it probes. This function
// performs that split on an existing database:
//
//   - every legacy task's upstream becomes a provider (or is matched to an
//     already existing provider that describes the same upstream, which is
//     what happens when the operator already registered it by hand);
//   - the `tasks` table is rebuilt so it only holds task-owned fields plus
//     the provider reference;
//   - the api key needs no re-encryption: the legacy task and the provider
//     registry seal their keys with the same instance master key and the
//     same AES-GCM layout, so the ciphertext is copied verbatim.
//
// The function is idempotent — running it twice is a no-op, because the
// migrated table no longer carries the `protocol` column the detection
// looks for.
func MigrateLegacyTasks(ctx context.Context, db *gorm.DB, pstore Store, log *slog.Logger) (TaskMigrationResult, error) {
	var res TaskMigrationResult
	if db == nil {
		return res, fmt.Errorf("provider: migrate tasks: nil db handle")
	}
	if log == nil {
		log = slog.Default()
	}
	if !tableExists(ctx, db, config.TaskConfigTableName) {
		return res, nil
	}
	// Already migrated? The new schema has provider_id and no protocol.
	if columnExists(ctx, db, config.TaskConfigTableName, "provider_id") {
		return res, nil
	}
	if !columnExists(ctx, db, config.TaskConfigTableName, "protocol") {
		return res, nil
	}

	legacy, err := readLegacyTasks(ctx, db)
	if err != nil {
		return res, err
	}
	if len(legacy) == 0 {
		// Nothing to carry over, but the table still has the old shape:
		// rebuild it so the schema matches the new model.
		if err := rebuildTasksTable(ctx, db, nil, nil); err != nil {
			return res, err
		}
		return res, nil
	}

	// provider id per task id
	providerForTask := make(map[string]string, len(legacy))
	for i := range legacy {
		t := legacy[i]
		pid, reused, err := ensureProviderForTask(ctx, pstore, &t)
		if err != nil {
			return res, err
		}
		providerForTask[t.ID] = pid
		if reused {
			res.ProvidersReused++
		} else {
			res.ProvidersCreated++
		}
	}

	if err := rebuildTasksTable(ctx, db, legacy, providerForTask); err != nil {
		return res, err
	}
	res.TasksMigrated = len(legacy)

	log.Info("legacy monitoring tasks migrated to providers",
		slog.Int("tasks_migrated", res.TasksMigrated),
		slog.Int("providers_created", res.ProvidersCreated),
		slog.Int("providers_reused", res.ProvidersReused),
	)
	return res, nil
}

// ensureProviderForTask finds or creates the provider that replaces one
// legacy task's upstream.
func ensureProviderForTask(ctx context.Context, pstore Store, t *legacyTask) (string, bool, error) {
	existing, err := pstore.ListProviders(ctx)
	if err != nil {
		return "", false, fmt.Errorf("provider: migrate tasks: list providers: %w", err)
	}
	// The operator may already have registered the same upstream by hand —
	// reuse it instead of creating a duplicate.
	for i := range existing {
		if sameEndpoint(&existing[i], t) {
			return existing[i].ID, true, nil
		}
	}
	name := uniqueProviderName(existing, t.Name)
	p := &Provider{
		ID:                 uuid.NewString(),
		Name:               name,
		Description:        fmt.Sprintf("migrated from monitoring task %q", t.Name),
		Enabled:            true,
		Protocol:           t.Protocol,
		BaseURL:            t.BaseURL,
		APIKeyEnc:          t.APIKeyEnc,
		Models:             []string{t.Model},
		TimeoutMS:          t.TimeoutMS,
		InsecureSkipVerify: t.InsecureSkipVerify,
		ThinkingEnabled:    t.ThinkingEnabled,
	}
	if t.StreamPath != "" || t.OpenAIExtra != "" {
		p.OpenAI = &config.OpenAIConfig{StreamPath: t.StreamPath, ExtraHeadersJSON: t.OpenAIExtra}
	}
	if t.MaxTokens != 0 || t.AnthropicExtra != "" {
		p.Anthropic = &config.AnthropicConfig{MaxTokens: t.MaxTokens, ExtraHeadersJSON: t.AnthropicExtra}
	}
	if t.Alt != "" || t.GeminiExtra != "" {
		p.Gemini = &config.GeminiConfig{Alt: t.Alt, ExtraHeadersJSON: t.GeminiExtra}
	}
	if err := pstore.CreateProvider(ctx, p); err != nil {
		return "", false, fmt.Errorf("provider: migrate tasks: create provider %q: %w", name, err)
	}
	return p.ID, false, nil
}

// sameEndpoint reports whether an existing provider describes the same
// upstream endpoint as a legacy task.
//
// The endpoint identity is the protocol + base URL. Model names are now a
// list on the provider, so a legacy task matches when its model is one of
// them — the migrated task will then select exactly that one. The
// credential is deliberately not compared: the api key is sealed with a
// random nonce, so two encryptions of the same plaintext differ byte for
// byte, and the provider's credential is the one that wins.
func sameEndpoint(p *Provider, t *legacyTask) bool {
	return p.Protocol == t.Protocol && p.BaseURL == t.BaseURL && (p.HasModel(t.Model) || len(p.Models) == 0)
}

// uniqueProviderName derives a provider name from the task name, avoiding
// the unique-name constraint of the registry.
func uniqueProviderName(existing []Provider, base string) string {
	taken := make(map[string]bool, len(existing))
	for i := range existing {
		taken[existing[i].Name] = true
	}
	if base == "" {
		base = "migrated upstream"
	}
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s #%d", base, i)
		if !taken[candidate] {
			return candidate
		}
	}
}

// rebuildTasksTable rewrites the `tasks` table into the new shape.
//
// The rows are written explicitly instead of with an INSERT ... SELECT
// because every row needs its own provider id.
//
// The new table is created by GORM itself (AutoMigrate against a
// `tasks_new` table override) instead of by hand-written DDL: the schema
// then matches config.TaskConfig exactly, so the AutoMigrate that runs
// right after the migration finds nothing to change. A hand-written
// CREATE TABLE drifts from the model in quoting and defaults, and GORM
// then "fixes" the table with a rebuild that drops columns.
func rebuildTasksTable(ctx context.Context, db *gorm.DB, legacy []legacyTask, providerForTask map[string]string) error {
	if err := db.WithContext(ctx).Exec("DROP TABLE IF EXISTS `tasks_new`").Error; err != nil {
		return fmt.Errorf("provider: migrate tasks: drop tasks_new: %w", err)
	}
	if err := db.WithContext(ctx).Table("tasks_new").AutoMigrate(&config.TaskConfig{}); err != nil {
		return fmt.Errorf("provider: migrate tasks: create tasks_new: %w", err)
	}
	insert := "INSERT INTO `tasks_new` " +
		"(`id`,`name`,`enabled`,`provider_id`,`model`,`system_prompt`,`user_prompt`,`prompt_vars`," +
		"`schedule`,`canary_group`,`notify_enabled`,`notify_channel`,`notify_wecom_webhook_enc`," +
		"`created_at`,`updated_at`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)"
	for i := range legacy {
		t := legacy[i]
		canary := t.CanaryGroup
		if canary == "" {
			canary = config.CanaryGroupLabel
		}
		channel := t.Channel
		if channel == "" {
			channel = string(config.NotifyWeCom)
		}
		// The legacy `enabled` column was shared between the task's own
		// switch and the notification switch, so only one of the two
		// survived in the row. The task's switch is the one that matters
		// for scheduling, and it is what the column actually holds (the
		// scheduler skips disabled tasks, and a disabled task produced
		// no runs). The notification switch therefore starts out off —
		// it was unusable anyway, because no legacy row carried a
		// webhook.
		if err := db.WithContext(ctx).Exec(insert,
			t.ID, t.Name, t.Enabled, providerForTask[t.ID], t.Model, t.SystemPrompt, t.UserPrompt, t.PromptVars,
			t.Schedule, canary, false, channel, t.WeComWebhookEnc,
			t.CreatedAt, t.UpdatedAt,
		).Error; err != nil {
			return fmt.Errorf("provider: migrate tasks: insert task %q: %w", t.ID, err)
		}
	}
	if err := db.WithContext(ctx).Exec("DROP TABLE `tasks`").Error; err != nil {
		return fmt.Errorf("provider: migrate tasks: drop tasks: %w", err)
	}
	if err := db.WithContext(ctx).Exec("ALTER TABLE `tasks_new` RENAME TO `tasks`").Error; err != nil {
		return fmt.Errorf("provider: migrate tasks: rename tasks_new: %w", err)
	}
	// GORM named the indexes after the temporary table. Drop them and
	// recreate them under the names the model declares, so the next
	// AutoMigrate does not have to touch anything.
	for _, stale := range []string{"idx_tasks_new_name", "idx_tasks_new_provider_id"} {
		if err := db.WithContext(ctx).Exec("DROP INDEX IF EXISTS `" + stale + "`").Error; err != nil {
			return fmt.Errorf("provider: migrate tasks: drop index %s: %w", stale, err)
		}
	}
	if err := db.WithContext(ctx).Exec("CREATE UNIQUE INDEX `idx_tasks_name` ON `tasks`(`name`)").Error; err != nil {
		return fmt.Errorf("provider: migrate tasks: create name index: %w", err)
	}
	if err := db.WithContext(ctx).Exec("CREATE INDEX `idx_tasks_provider_id` ON `tasks`(`provider_id`)").Error; err != nil {
		return fmt.Errorf("provider: migrate tasks: create provider index: %w", err)
	}
	return nil
}

// readLegacyTasks loads the retired task rows.
func readLegacyTasks(ctx context.Context, db *gorm.DB) ([]legacyTask, error) {
	var out []legacyTask
	err := db.WithContext(ctx).Raw(`
		SELECT id, name, enabled, protocol, base_url, api_key_enc, model, timeout_ms,
		       insecure_skip_verify, thinking_enabled, stream_path, openai_extra_headers,
		       max_tokens, anthropic_extra_headers, alt, gemini_extra_headers,
		       system_prompt, user_prompt, prompt_vars, schedule, canary_group,
		       channel, wecom_webhook_enc, created_at, updated_at
		FROM ` + "`tasks`").Scan(&out).Error
	if err != nil {
		return nil, fmt.Errorf("provider: migrate tasks: read tasks: %w", err)
	}
	return out, nil
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

// columnExists reports whether a column is present on a table.
func columnExists(ctx context.Context, db *gorm.DB, table, column string) bool {
	var count int64
	err := db.WithContext(ctx).
		Raw("SELECT count(*) FROM pragma_table_info(?) WHERE name = ?", table, column).
		Scan(&count).Error
	if err != nil {
		return false
	}
	return count > 0
}

// MultiModelMigrationResult summarises the one-time multi-model upgrade.
type MultiModelMigrationResult struct {
	ProvidersBackfilled int
	TasksBackfilled     int
	RequestsBackfilled  int
}

// MigrateMultiModels upgrades a database written by the single-model
// schema to the "provider has many models, each task/request picks one"
// schema.
//
// It must run BEFORE any AutoMigrate: SQLite cannot add a NOT NULL column
// without a default, and GORM would try to do exactly that for the new
// models / model columns on an existing table. Adding the columns here
// with an explicit DEFAULT keeps the subsequent AutoMigrate a no-op.
//
// Three things happen, all idempotent:
//
//  1. ai_providers gains `models` (a JSON array). Every legacy row's
//     scalar `model` becomes a one-element array.
//  2. tasks gains `model`; each row is backfilled with the primary model
//     of the provider it references.
//  3. ai_requests gains `model` and is backfilled the same way.
//
// Legacy-shaped `tasks` (still carrying `protocol` instead of
// `provider_id`) and a missing `ai_requests` table are skipped: the
// retired-schema migrations that run later create those rows with the
// model already set.
func MigrateMultiModels(ctx context.Context, db *gorm.DB, log *slog.Logger) (MultiModelMigrationResult, error) {
	var res MultiModelMigrationResult
	if db == nil {
		return res, fmt.Errorf("provider: migrate models: nil db handle")
	}
	if log == nil {
		log = slog.Default()
	}

	// provider id -> the model that was the provider's only one before
	// the upgrade; this is what existing tasks/requests were running.
	primary := make(map[string]string)

	if tableExists(ctx, db, TableName) {
		if !columnExists(ctx, db, TableName, "models") {
			if err := db.WithContext(ctx).Exec(
				"ALTER TABLE `ai_providers` ADD COLUMN `models` TEXT NOT NULL DEFAULT '[]'").Error; err != nil {
				return res, fmt.Errorf("provider: migrate models: add ai_providers.models: %w", err)
			}
		}
		var rows []struct {
			ID     string
			Model  string
			Models string
		}
		if err := db.WithContext(ctx).Raw(
			"SELECT id, model, models FROM `ai_providers`").Scan(&rows).Error; err != nil {
			return res, fmt.Errorf("provider: migrate models: read providers: %w", err)
		}
		for i := range rows {
			row := rows[i]
			models := row.Models
			if models == "" || models == "[]" {
				if strings.TrimSpace(row.Model) == "" {
					continue
				}
				raw, err := json.Marshal([]string{row.Model})
				if err != nil {
					return res, fmt.Errorf("provider: migrate models: encode models: %w", err)
				}
				models = string(raw)
				if err := db.WithContext(ctx).Exec(
					"UPDATE `ai_providers` SET `models` = ? WHERE id = ?", models, row.ID).Error; err != nil {
					return res, fmt.Errorf("provider: migrate models: update provider %q: %w", row.ID, err)
				}
				res.ProvidersBackfilled++
			}
			var list []string
			if err := json.Unmarshal([]byte(models), &list); err == nil && len(list) > 0 {
				primary[row.ID] = list[0]
			} else if row.Model != "" {
				primary[row.ID] = row.Model
			}
		}
	}

	if err := backfillModelColumn(ctx, db, config.TaskConfigTableName, primary, &res.TasksBackfilled); err != nil {
		return res, err
	}
	if err := backfillModelColumn(ctx, db, requestTableName, primary, &res.RequestsBackfilled); err != nil {
		return res, err
	}

	if res.ProvidersBackfilled > 0 || res.TasksBackfilled > 0 || res.RequestsBackfilled > 0 {
		log.Info("multi-model schema migration",
			slog.Int("providers_backfilled", res.ProvidersBackfilled),
			slog.Int("tasks_backfilled", res.TasksBackfilled),
			slog.Int("requests_backfilled", res.RequestsBackfilled),
		)
	}
	return res, nil
}

// requestTableName is spelled out here to avoid an import cycle: the
// request package already imports provider.
const requestTableName = "ai_requests"

// backfillModelColumn adds and fills the `model` column on one referencing
// table. Rows that already carry a model are never touched, which makes a
// restart after a partially completed migration safe.
func backfillModelColumn(ctx context.Context, db *gorm.DB, table string, primary map[string]string, count *int) error {
	if !tableExists(ctx, db, table) {
		return nil
	}
	// A legacy-shaped tasks table has `model` but no `provider_id`; the
	// retired-schema migration rebuilds it with the right value already.
	if !columnExists(ctx, db, table, "provider_id") {
		return nil
	}
	if !columnExists(ctx, db, table, "model") {
		if err := db.WithContext(ctx).Exec(
			"ALTER TABLE `" + table + "` ADD COLUMN `model` TEXT NOT NULL DEFAULT ''").Error; err != nil {
			return fmt.Errorf("provider: migrate models: add %s.model: %w", table, err)
		}
	}
	var rows []struct {
		ID         string
		ProviderID string `gorm:"column:provider_id"`
		Model      string
	}
	if err := db.WithContext(ctx).Raw(
		"SELECT id, provider_id, model FROM `" + table + "` WHERE model IS NULL OR model = ''").Scan(&rows).Error; err != nil {
		return fmt.Errorf("provider: migrate models: read %s: %w", table, err)
	}
	for i := range rows {
		model := primary[rows[i].ProviderID]
		if model == "" {
			// The provider no longer exists; leave the row blank rather
			// than guessing, so the operator can repair it in the UI.
			continue
		}
		if err := db.WithContext(ctx).Exec(
			"UPDATE `"+table+"` SET `model` = ? WHERE id = ?", model, rows[i].ID).Error; err != nil {
			return fmt.Errorf("provider: migrate models: update %s %q: %w", table, rows[i].ID, err)
		}
		*count = *count + 1
	}
	return nil
}
