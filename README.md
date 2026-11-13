# AI Monitor

> Single-instance Go service that probes multi-protocol AI endpoints (OpenAI-compatible / Anthropic / Gemini), records streaming metrics (TTFT / ITL / session duration) and forwards Prometheus pull, alerting to WeCom when health degrades.

Built per `docs/design.md` (kept in this repo).

## Architecture at a glance

```text
                                   ┌─────────────────────────┐
                                   │  Prometheus (pull)      │
                                   │  /metrics               │
                                   └────────────▲────────────┘
                                                │
┌────────────┐ cron ticks ┌──────────────┐     │
│ ai-monitor ├───────────▶│ scheduler    │     │
│            │            │ + semaphore  │     │
│            │            └──────┬───────┘     │
│            │                   │             │
│            │            ┌──────▼───────┐     │
│            │            │ probe.runner │     │
│            │            └──────┬───────┘     │
│            │                   │             │
│            │   ┌───────────────┼─────────────┴─ adapter (openai / anthropic / gemini)
│            │   │               │
│            │ ┌─▼────┐    ┌─────▼────────┐
│            │ │ sink │    │ dedup+notify │─── WeCom
│            │ └──────┘    └──────────────┘
└────────────┘
```

## Quick start

```bash
# 一键启动（最简；自动生成 master + jwt；输出登录信息）
./scripts/run-local.sh             # 前台；Ctrl+C 退出
./scripts/run-local.sh --bg        # 后台，日志 /tmp/ai-monitor.log
./scripts/run-local.sh --clean     # 启动前清空 DB（首次启动用）
./scripts/run-local.sh --rebuild   # 强制重新编译二进制后再启动

# 浏览器：
#   URL     : http://localhost:9090
#   默认账号 : admin / LocalAdmin2026!Q
#   启动日志会一行 INFO: "seed_admin_created"（DB 已有 admin 时是 "seed_admin_exists"）

# 或者手动（更细的控制）：
make build
export AI_MONITOR_MASTER_KEY=$(openssl rand -hex 16)
export AI_MONITOR_JWT_SECRET=$(openssl rand -hex 16)
export AI_MONITOR_ADMIN_PASSWORD="LocalAdmin2026!Q"
export WECOM_WEBHOOK="https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=test"
./ai-monitor -config deploy/example.config.yaml
```

脚本会在源码变更后自动重新编译（比较源文件与二进制的 mtime），不必手动 `go build`。

未显式设置 `AI_MONITOR_MASTER_KEY` 时，脚本把随机生成的密钥写入仓库根的 `.local-secrets`（已 gitignore，权限 600），后续启动复用同一份。**这份文件决定 DB 里已加密的 api_key 能否解开**——删掉它等于换了主密钥，存量任务的 api_key 将无法解密（服务仍会启动，但那些任务需要在管理端重新填写 API Key）。

服务可用后：

```bash
curl http://localhost:9090/healthz                       # 健康检查
curl http://localhost:9090/metrics                       # Prometheus 拉取目标
curl http://localhost:9090/admin/tasks -H "auth:..."     # 任务列表（需 token）
```

`POST /auth/login` 拿 admin token 后即可调所有 `/admin/*` 写接口。详细 RBAC 矩阵见 `docs/design.md` §18.8。

## Configuration

The loader merges defaults → `config.yaml` → `AI_MONITOR_*` env vars → flags.
A starter config is in `deploy/example.config.yaml`. See `internal/config/loader.go`
for the full key list.

## Monitoring tasks and prompts

A monitoring task **names the AI service provider it probes**
(`provider_id`) and owns only what makes it a monitoring task: the user
prompt, the `${var}` substitutions, the cron schedule, the canary group and
the notification switch. The upstream coordinates — protocol, base URL,
encrypted API key, model, timeout, TLS and thinking switches, the
protocol-specific options — live on the provider. One edit there reaches
every task that uses it, and two tasks pointing at the same endpoint no
longer carry two copies of the same credentials.

The provider is resolved **when the probe runs**, not when the task is
loaded: editing a provider takes effect on the very next tick, without a
scheduler reload.

A scheduled monitoring task sends only its `user_prompt` (with `${var}`
substitutions). **Monitoring tasks have no system prompt**: the field is gone
from the admin UI and from `/admin/tasks` responses, and `probe.Runner.Run`
clears it before a request is built. This also covers legacy rows and YAML
tasks — editing an old task clears the stored column, and the column itself is
kept so existing databases still open. Old clients that still send
`system_prompt` to `/admin/tasks` are accepted but the value is ignored.

System prompts still belong to **AI request manager**: request groups store
fully authored requests, and the run dialog lets an operator customise system
/ user prompts for one execution. Those one-shot runs are not monitoring
samples.

### Upgrading an existing database

A pre-provider installation carried the upstream coordinates on every task
row. On startup the service migrates them:

- every task's upstream becomes a provider (or is matched to an already
  registered provider that describes the same endpoint);
- the `tasks` table is rebuilt so it only holds task-owned fields plus the
  provider reference;
- the sealed API key is copied verbatim — both sides use the same instance
  master key and the same AES-GCM layout, so no credential is re-encrypted
  and none is lost.

The migration is idempotent and runs before GORM's auto-migration (SQLite
cannot add a `NOT NULL` column to an existing table, so the table has to be
rebuilt rather than altered).

| Endpoint | Purpose |
|---|---|
| `GET /admin/tasks` | List every task, with its provider nested |
| `GET /admin/tasks/{id}` | Read one task |
| `POST /admin/tasks` | Create a task (rejects an unknown `provider_id`) |
| `PATCH /admin/tasks/{id}` | Update a task; may re-point it at another provider |
| `DELETE /admin/tasks/{id}` | Delete a task |
| `POST /admin/tasks/{id}/start` | Enable the task and register its cron entry |
| `POST /admin/tasks/{id}/stop` | Disable the task and retract its cron entry |

Deleting a provider that is still referenced by a task is refused with
`409 provider_in_use` and the names of the tasks that need re-pointing.

`scripts/e2e_task_providers.py` exercises the whole path against a fake SSE
upstream: register a provider → create a task that references it → the task
response nests the provider → an unknown provider is rejected → a provider
that is still in use cannot be deleted → the scheduled probe really reaches
the provider's upstream.

```bash
make build && python3 scripts/e2e_task_providers.py ./ai-monitor
```

## TLS certificate verification

Some internal endpoints serve a self-signed certificate or one signed by a
private CA that the host doesn't trust. Probing them fails with:

```text
probe: openai: do: Post "https://20.20.100.94:18089/v1/chat/completions":
tls: failed to verify certificate: x509: certificate signed by unknown authority:
protocol: upstream unavailable.
```

There are two ways to turn verification off:

| Scope | Where | How |
|---|---|---|
| Per provider | provider config | `insecure_skip_verify: true` on the provider, or `PATCH /admin/providers/{id}` with `{"insecure_skip_verify": true}` |
| Instance-wide | runtime config | `insecure_skip_verify: true` in `config.yaml`, `AI_MONITOR_INSECURE_SKIP_VERIFY=true`, or `--insecure-skip-verify` |

The two are OR'ed — the instance-wide switch is an explicit "this deployment
does not verify certificates" decision, so it can't be undone for a single
provider. Prefer the per-provider switch: skipping verification removes MITM
protection, so keep it scoped to endpoints on a trusted network path.

The per-provider setting persists in SQLite (the `insecure_skip_verify`
column is added automatically on startup) and is editable from the admin UI
under 「TLS 证书校验」. A monitoring task inherits it from the provider it
probes.

## Protocols

Three adapters ship in `internal/protocol/*`:

- **OpenAI** — POST `/v1/chat/completions`, parses SSE `data:` lines, terminates on `[DONE]`.
- **Anthropic** — POST `/v1/messages`, parses `event: content_block_delta`, terminates on `message_stop`.
- **Gemini** — POST `/v1beta/models/<model>:streamGenerateContent`, parses the JSON array stream.

Every adapter supports custom `extra_headers` for canary traffic tests
(see `deploy/canary-scenarios.yaml`).

## Run history

`GET /admin/tasks/{id}/runs` returns one row per probe execution. Each row
carries both the raw wire bytes (§18.13) and the parsed semantic content
(§18.14):

| Field | What it holds |
|---|---|
| `request_id` | The `X-Req-Id` sent upstream — **kept even on 401/403/5xx**, so a failed probe can be correlated with the provider's own logs |
| `input_content` | What was actually sent (system + user, prompt vars already expanded) |
| `thinking_content` | The model's reasoning trace (DeepSeek/Qwen `reasoning_content`, OpenRouter `reasoning`, Anthropic `thinking_delta`, Gemini thought parts) |
| `output_content` | The model's answer text |
| `response_error_body` | The upstream's error body on a failed probe — usually where the actionable detail lives |

Reasoning is captured in the adapter while the stream is live, because it
cannot be recovered from the bounded head/tail byte capture afterwards.
Each content field is capped at 16 KiB (`content_truncated` flags
overflow), truncated on a UTF-8 boundary.

In the UI the X-Req-Id is shown on every row (click to copy), and the
「内容解析」 section expands into input / thinking / output.

## AI service provider registry

A **provider** is the reusable description of one upstream AI endpoint:
protocol, base URL, encrypted API key, an ordered list of available
**models**, timeout and the protocol-specific options (OpenAI
`stream_path` / extra headers, Anthropic `max_tokens`, Gemini `alt`). See
the 「AI 服务商」 page in the admin SPA.

Every monitoring task and every AI request references a provider by id and
**selects exactly one** of its models, storing that model name on its own
row. Removing a model that is still selected is refused with
`409 model_in_use` (the error names the affected requests and tasks), and
saving a task / request with a model the provider does not offer is
rejected with `400 unknown_model`.

**Connectivity is an on-demand, per-model check, not a gate on saving.**
Creating or updating a provider never contacts the upstream: with several
models, probing all of them would turn every edit into a long request and
one dead model would block the whole save. The form's 「测试连通性」 button
instead checks one model at a time; the verdict and the model that was
tested are returned, and the result is recorded on the provider row.

The verdict of the check classifies the failure so the operator knows
what to fix:

| `kind` | Meaning | Typical cause |
|---|---|---|
| `unauthorized` | 401 / 403 | wrong or expired API key |
| `invalid_request` | 4xx (404 / 400) | wrong base URL, path or model |
| `upstream_unavailable` | 5xx | the upstream itself is broken |
| `timeout` | no answer in time | the upstream is hanging |
| `network_error` | never reached the upstream | DNS failure, refused connection, TLS error |
| `config_error` | nothing was sent | a required field is missing |

The upstream's own error message is carried along (`Incorrect API key
provided: sk-…`), together with the `X-Req-Id` so the attempt can be
grepped in the upstream's logs.

Two check routes exist, both explicit and both probing **one** model:

- `POST /admin/providers/check` verifies a payload **without saving it** —
  this is what the form's per-model 「测试连通性」 button calls. An optional
  `model` names the candidate to probe; when omitted the first one is used.
- `POST /admin/providers/{id}/check` re-verifies a saved provider with its
  stored key, optionally with `{"model":"..."}`.

The legacy `"force"` field is still accepted (so older clients do not trip
the unknown-field guard) but ignored: there is no save-time check to
override any more.

| Endpoint | Purpose |
|---|---|
| `GET /admin/providers` | List every provider |
| `GET /admin/providers/{id}` | Read one provider |
| `POST /admin/providers` | Create a provider (no upstream call) |
| `PATCH /admin/providers/{id}` | Update a provider (no upstream call) |
| `DELETE /admin/providers/{id}` | Delete a provider |
| `POST /admin/providers/check` | Verify a payload without saving it |
| `POST /admin/providers/{id}/check` | Re-verify a saved provider with its stored key |

Reads need a session; writes are admin-only. The API key is write-only —
the API reports `has_api_key` and never returns the key itself. A row
whose stored key cannot be decrypted with the current master key is still
listed, marked `secret_error`, so the operator can repair it.

The check deadline is `provider_check_timeout` (default `10s`, also
`AI_MONITOR_PROVIDER_CHECK_TIMEOUT`). It bounds one on-demand check; the
HTTP server's write timeout is raised to match, so a check is never cut
off mid-flight.

`scripts/e2e_providers.py` exercises the whole path against a fake SSE
upstream: saving an unreachable provider, per-model checks (including a
wrong key), multi-model update, the `model_in_use` guard, and delete.

```bash
make build && python3 scripts/e2e_providers.py ./ai-monitor
```

A provider is shared by monitoring tasks and AI requests alike. Both
reference it by id, both select one of its models, and both refuse to
point at a provider that does not exist (`400 unknown_provider`).

## AI request manager (one-shot batches)

Monitoring tasks are cron-driven: one task is one probe, fired on a
schedule. The **AI request manager** is the operator-driven counterpart —
see the 「AI 请求管理」 page in the admin SPA.

There is **no request-group layer**. Each request is a flat row holding its
own system / user prompt, `${var}` substitutions, and one `provider_id`
pointing at an entry in the AI service provider registry. Different
requests can therefore target different upstreams, and one batch can span
several of them. The provider owns the URL, credentials, model, timeout and
thinking switch, so editing it once reaches every request that uses it.

You can:

- select **one request** and execute just that one;
- select **several requests**, possibly across providers, and execute them
  together in one batch;
- **customise the content of a single run**: the dialog shown before every
  execution is prefilled with the saved prompts, and lets you edit the user
  prompt (plus the system prompt and `${var}` values) per request, or apply
  one piece of content to every request in the batch.

| Endpoint | Purpose |
|---|---|
| `GET /admin/requests` | Flat list of every request, with its provider nested |
| `GET /admin/requests/{id}` | Read one request |
| `POST /admin/requests` | Create a request (rejects an unknown `provider_id`) |
| `PATCH /admin/requests/{id}` | Update a request; may re-point it at another provider |
| `DELETE /admin/requests/{id}` | Delete a request |
| `POST /admin/requests/run` | Fire a selected set of request ids |
| `GET /admin/request-batches/{id}` | Poll the progress and per-request results |

`POST /admin/requests/run` accepts an optional `overrides` object keyed by
request id:

```json
{
  "request_ids": ["<request-id>", "<another-request-id>"],
  "concurrency": 4,
  "overrides": {
    "<request-id>": {
      "system_prompt": "answer briefly",
      "user_prompt": "explain ${topic}",
      "prompt_vars": { "topic": "SSE" }
    }
  }
}
```

An override is **one-shot**: it is applied to an in-memory copy of the
request, is never persisted and does not rewrite the predefined request.
Omitted fields keep the saved content; `prompt_vars: {}` clears the
variables for this run only; a blank `user_prompt` is rejected with `400`
before anything is sent. Overrides for ids outside the batch are ignored,
so a stale selection cannot affect another run. The UI's "Run predefined"
button simply sends no overrides at all.

All write and run endpoints require the `admin` role; viewers can browse
requests but cannot fire them. Deleting a provider that still has requests
attached is refused with `409 provider_in_use` and the names of the
requests that need re-pointing.

Execution is **asynchronous**: the HTTP server's write timeout is short
while an AI request can run for tens of seconds, so a run returns `202`
with a `batch_id` and the UI polls the batch endpoint. Within one batch,
requests run concurrently with a bounded limit (default 4, overridable per
call) and each gets its own deadline. A request whose provider is missing,
has no key, or whose key cannot be decrypted is reported as `skipped`
rather than failing the whole batch.

**Manual runs do not pollute the monitoring signal.** They are recorded in
`probe_runs` (`source = manual`, plus `batch_id` / `provider_id` /
`ai_request_id`) so the result is traceable, but they never feed the
Prometheus metrics, the success-rate sliding window, the
consecutive-failure counter or the WeCom alert channel. Scheduled runs
keep `source = schedule` and behave exactly as before.

### Upgrading from the request-group model

An existing database is migrated once at startup, by
`request.MigrateLegacyGroups`:

- every legacy `request_groups` row becomes a provider (its sealed API key
  is copied verbatim — both tables use the same master key and AES-GCM
  layout, so no re-encryption is needed);
- a legacy group whose name matches an **already registered** provider
  reuses that provider instead of creating a duplicate — this is the
  normal case when the operator registered the upstream by hand first;
- every `request_items` row becomes an `ai_requests` row with the same id,
  prompts and variables, pointing at its group's provider;
- the two legacy tables are then dropped.

Nothing is lost and the migration is idempotent (it drops its input on
success). `scripts/e2e_request_migration.py` copies a real database —
including its WAL — and verifies the whole upgrade plus a second restart.

Batch state is in-memory and lost on restart; the durable record is the
`probe_runs` row. The two e2e scripts exercise the full path against a fake
SSE upstream:

```bash
make build
python3 scripts/e2e_request_manager.py ./ai-monitor   # flat CRUD + cross-provider batch
python3 scripts/e2e_request_migration.py ./ai-monitor ./ai-monitor.db
```

## Metrics

Exposed at `/metrics`. Names follow design.md §11.6:

| Metric | Type | Labels |
|---|---|---|
| `ai_probe_request_duration_seconds` | Histogram | task_id, protocol, model, status |
| `ai_probe_ttft_seconds` | Histogram | task_id, protocol, model |
| `ai_probe_itl_seconds` | Histogram | task_id, protocol, model |
| `ai_probe_request_total` | Counter | task_id, protocol, model, status |
| `ai_probe_failure_total` | Counter | task_id, protocol, model, error_type |
| `ai_probe_chunk_total` | Counter | task_id, protocol, model |
| `ai_probe_task_last_success_timestamp_seconds` | Gauge | task_id |
| `ai_probe_scheduler_lag_seconds` | Gauge | task_id |

Sample PromQL lives in `deploy/llm-promql-examples.md`; a Grafana dashboard
in `deploy/grafana-llm-dashboard.json`.

## Alerts

Four rules (see `internal/probe/runner.go`):

| Rule | Severity | Trigger |
|---|---|---|
| `unauthorized_immediate` | critical | 401 / 403 |
| `consecutive_failures`   | critical | N consecutive failures (default N=3) |
| `slow_request`           | warning  | TTFT > SlowThreshold (10s) |
| `success_rate_below_80`  | warning  | success-rate < 80% over a sliding window of 200 samples |

Deduper collapses repeats to one POST per task_id+rule within 5 minutes.

`deploy/prometheus-alerts.yaml` ships Alertmanager rules for the same
conditions in the Prometheus-native DSL.

## Development

```bash
make build      # binary with the SPA embedded (builds the SPA first if needed)
make build-dev  # backend-only binary, no SPA embedded (no Node.js required)
make test      # -race unit tests
make test-int  # -race -tags=integration end-to-end tests
make lint      # go vet
make docker    # multi-stage scratch image
```

### Build tags

The admin SPA is embedded into the binary with `//go:embed`, so the bundle in
`internal/webui/dist/` must exist at compile time. That tree is a gitignored
build artifact, so it is produced on demand:

| Build | Command | SPA source | Needs Node.js |
| --- | --- | --- | --- |
| production | `make build` | embedded in the binary | yes (only if the bundle is missing) |
| development | `make build-dev` | `web/dist` on disk, served at runtime | no |
| tests | `make test` | none | no |

`make build` runs `make web-build` automatically when `internal/webui/dist/` is
missing, so a fresh checkout builds without extra steps. A plain `go build` or
`go test ./...` also works on a fresh checkout — those compile without the
embed and serve the SPA from disk.


## Directory layout (per design.md §10)

```
cmd/ai-monitor/          # wiring entry-point only
internal/
  config/                # TaskConfig, loader, AES-GCM, SQLite store
  protocol/              # ChatProtocolAdapter interface + sentinel errors
  protocol/openai/       # OpenAI /v1/chat/completions streaming
  protocol/anthropic/    # Anthropic /v1/messages event-stream
  protocol/gemini/       # Gemini streamGenerateContent
  metrics/               # MetricSink interface + PromSink (8 metrics)
  notifier/              # Notifier interface + MultiNotifier + Deduper
  notifier/wecom/        # WeCom webhook backend
  probe/                 # probe.Runner + sliding window + alert rules
  request/               # request groups, predefined requests, one-shot batches
  scheduler/             # cron v3 + semaphore + jitter
  server/                # /metrics + /healthz + /admin
  logging/               # slog JSON handler
  integration/           # -tags=integration e2e tests
deploy/
  Dockerfile             # multi-stage scratch image
  example.config.yaml    # starter config
  prometheus-alerts.yaml # sample Alertmanager rules
  grafana-llm-dashboard.json
  llm-promql-examples.md
  canary-scenarios.yaml  # canary traffic examples
```

## Version

Latest tag is `v0.1.0-P1`. See `docs/design.md` for the full design history.

## Maintainer

- **yangchun** — [825660677@qq.com](mailto:825660677@qq.com)

Issues, security reports, and feature requests are welcome. For security
vulnerabilities please email the maintainer directly rather than opening a
public issue.

## License

[MIT](./LICENSE) © 2026 yangchun. Anyone may use, modify, and redistribute
this software (including for commercial purposes), provided the copyright
notice and permission notice in `LICENSE` are preserved.
