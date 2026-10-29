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
| Per task | task config | `insecure_skip_verify: true` in the task, or `PATCH /admin/tasks/{id}` with `{"insecure_skip_verify": true}` |
| Instance-wide | runtime config | `insecure_skip_verify: true` in `config.yaml`, `AI_MONITOR_INSECURE_SKIP_VERIFY=true`, or `--insecure-skip-verify` |

The two are OR'ed — the instance-wide switch is an explicit "this deployment
does not verify certificates" decision, so it can't be undone for a single
task. Prefer the per-task switch: skipping verification removes MITM
protection, so keep it scoped to endpoints on a trusted network path.

The per-task setting persists in SQLite (the `insecure_skip_verify` column is
added automatically on startup) and is editable from the admin UI under
「TLS 证书校验」.

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
make test      # -race unit tests
make test-int  # -race -tags=integration end-to-end tests
make lint      # go vet
make docker    # multi-stage scratch image
```

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
