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

## Protocols

Three adapters ship in `internal/protocol/*`:

- **OpenAI** — POST `/v1/chat/completions`, parses SSE `data:` lines, terminates on `[DONE]`.
- **Anthropic** — POST `/v1/messages`, parses `event: content_block_delta`, terminates on `message_stop`.
- **Gemini** — POST `/v1beta/models/<model>:streamGenerateContent`, parses the JSON array stream.

Every adapter supports custom `extra_headers` for canary traffic tests
(see `deploy/canary-scenarios.yaml`).

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
