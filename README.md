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
# 1. Build
make build

# 2. Provide a master key (32 bytes) and start
export AI_MONITOR_MASTER_KEY=$(openssl rand -hex 16)
./ai-monitor --config deploy/example.config.yaml

# 3. Send traffic & scrape
curl http://localhost:9090/healthz
curl http://localhost:9090/metrics   # Prometheus pull target
curl http://localhost:9090/admin/tasks
curl -X POST http://localhost:9090/admin/reload
```

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
