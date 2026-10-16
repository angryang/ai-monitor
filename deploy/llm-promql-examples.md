# LLM 拨测 · PromQL 分位数计算模板

> 配套 `ai-monitor` 二进制暴露的 `/metrics` 端点。
> 详见 `../docs/design.md` §11.6（指标定义）与 §3（label 维度）。
>
> 所有公式均使用 `histogram_quantile()` 函数，这是 Prometheus 对 Histogram 类型计算 P50/P90/P95/P99 的标准做法。

---

## 1. 核心约定

- **rate window**：建议 5 分钟。短于 `schedule` 周期会导致样本过稀；长于 30 分钟则趋势图过于平滑。
- **histogram_quantile 的局限**：在 bucket 内部做线性插值，桶边界附近的分位数有偏差。**这正是为什么下方 §3 要优化 bucket**。
- **跨实例**：所有公式天然按 label 拆分（无需手动加 `instance`）。
- **过滤维度**：所有示例都可通过 `{protocol="...", model="..."}` 缩小范围。

---

## 2. PromQL 公式清单

### 2.1 TTFT（Time To First Token）分位数

```promql
# P50（50% 请求的首 token 时间 ≤ 该值）
histogram_quantile(0.50,
  sum by (le, protocol, model) (
    rate(ai_probe_ttft_seconds_bucket[5m])
  )
)

# P90
histogram_quantile(0.90,
  sum by (le, protocol, model) (
    rate(ai_probe_ttft_seconds_bucket[5m])
  )
)

# P95
histogram_quantile(0.95,
  sum by (le, protocol, model) (
    rate(ai_probe_ttft_seconds_bucket[5m])
  )
)

# P99
histogram_quantile(0.99,
  sum by (le, protocol, model) (
    rate(ai_probe_ttft_seconds_bucket[5m])
  )
)
```

**指定单一协议 + 单一模型**：
```promql
histogram_quantile(0.95,
  sum by (le) (
    rate(ai_probe_ttft_seconds_bucket{protocol="openai", model="gpt-4o"}[5m])
  )
)
```

**排除某协议**（如只看 Anthropic + Gemini）：
```promql
histogram_quantile(0.95,
  sum by (le, protocol, model) (
    rate(ai_probe_ttft_seconds_bucket{protocol!="openai"}[5m])
  )
)
```

**叠加 canary 维度**（若启用 design.md §17.1 第 1 项的 canary_group）：
```promql
histogram_quantile(0.95,
  sum by (le, protocol, model, canary_group) (
    rate(ai_probe_ttft_seconds_bucket[5m])
  )
)
```

---

### 2.2 会话总时长（Request Duration）分位数

```promql
# P50 / P90 / P95 / P99 四档，结构同上，仅替换 metric 名
histogram_quantile(0.50,
  sum by (le, protocol, model) (
    rate(ai_probe_request_duration_seconds_bucket[5m])
  )
)

# 仅看成功请求的会话时长（剔除 4xx/5xx/timeout）：
histogram_quantile(0.95,
  sum by (le, protocol, model) (
    rate(ai_probe_request_duration_seconds_bucket{status="success"}[5m])
  )
)
```

---

### 2.3 趋势图（Graph Panel · Time Series）

把上面任意公式贴入 Grafana/夜莺/自建 dashboard 的"Time series" panel 类型即出趋势图。

**推荐面板布局**：
- 一条线一个 (protocol, model) 组合 → 用 `legend` 标注 `{{protocol}} · {{model}}`
- 4 张图叠放（P50 / P90 / P95 / P99）或用 `query` 在同一图里画 4 条线
- Y 轴单位设 `s（秒）`；Y 轴下限 0，上限建议 `max(集群历史峰值) * 1.2`

**同图叠加 P50/P90/P95/P99（节省看版）**：

```promql
# 当作 "query A"；新 query B 同理改 quantile

# query A
histogram_quantile(0.50, sum by (le, protocol, model) (rate(ai_probe_ttft_seconds_bucket[5m])))

# query B
histogram_quantile(0.90, sum by (le, protocol, model) (rate(ai_probe_ttft_seconds_bucket[5m])))

# query C
histogram_quantile(0.95, sum by (le, protocol, model) (rate(ai_probe_ttft_seconds_bucket[5m])))

# query D
histogram_quantile(0.99, sum by (le, protocol, model) (rate(ai_probe_ttft_seconds_bucket[5m])))
```

---

### 2.4 ITL（Inter-Token Latency）辅助指标

```promql
histogram_quantile(0.95,
  sum by (le, protocol, model) (
    rate(ai_probe_itl_seconds_bucket[5m])
  )
)
```

> ITL 反映"生成流畅度"，与 TTFT（首字延迟）互补；通常 ITL 健康在 50ms 内。

---

### 2.5 配合的错误率 / 成功率（已在方案内）

```promql
# 整体成功率
sum(rate(ai_probe_request_total{status="success"}[5m]))
/
sum(rate(ai_probe_request_total[5m]))

# 按协议 + 模型拆分
sum by (protocol, model) (rate(ai_probe_request_total{status="success"}[5m]))
/
sum by (protocol, model) (rate(ai_probe_request_total[5m]))

# 错误细分（按 error_type）
sum by (protocol, model, error_type) (rate(ai_probe_failure_total[5m]))
```

---

### 2.6 调度延迟（运维关注）

```promql
# P95 调度延迟（实际跑 - 计划跑）
histogram_quantile(0.95, sum by (le, task_id) (rate(ai_probe_scheduler_lag_seconds_bucket[5m])))

# 当前 schema 中 lag 是 Gauge 不是 Histogram
# 若需要分位数，需另起一个 Histogram 指标（见 design.md §17 待优化点）
ai_probe_scheduler_lag_seconds  # Gauge，直接 plot
```

---

## 3. Bucket 设计优化建议

当前 `ai-monitor` §11.6 默认 buckets：`{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}`

**问题**：
- TTFT 与 duration 共用一套 bucket，分辨率不匹配
- 会话时长 P99 在 30s 边界处会偏紧；当上游响应超过 30s 时饱和到 `+Inf`

**建议拆分**（作为 P1 补丁与 P1-C 同期落地）：

| 指标 | 推荐 buckets（单位秒） | 适合 |
|---|---|---|
| `ai_probe_ttft_seconds` | `{.01, .025, .05, .1, .2, .35, .5, .75, 1, 2, 5}` | P99 通常在 1s 内 |
| `ai_probe_itl_seconds` | `{.005, .01, .025, .05, .1, .25, .5}` | 期望均值 ≤ 50ms |
| `ai_probe_request_duration_seconds` | `{.1, .25, .5, 1, 2.5, 5, 10, 20, 30, 60, 120}` | 长文本场景 P99 可能 30s+ |
| `ai_probe_scheduler_lag_seconds` | 升级为 Histogram | 让 P95/P99 可算 |

落地做法：在 `internal/metrics/prom.go` 中分别为每个 HistogramVec 传入独立 buckets 切片，由 `Config` 中 `BucketsTTFT/ITL/Duration/Lag` 字段可覆盖。

---

## 4. 看板组件建议

完整 Grafana Dashboard JSON 见 `grafana-llm-dashboard.json`，含：

1. **顶部 Variables**：`protocol`（multi-select）、`model`（multi-select）、`canary_group`（multi-select，可选）
2. **Row 1 · TTFT 分位数趋势**：P50 / P90 / P95 / P99 一图四线
3. **Row 2 · 会话时长分位数趋势**：同上
4. **Row 3 · ITL P95 + 成功率**：辅助
5. **Row 4 · 错误率与错误细分**：stack bar
6. **Row 5 · 调度延迟与 chunk 数量**：运维

导入方式：Grafana → Dashboards → Import → 上传该 JSON → 选择 Prometheus 数据源。

---

## 5. 常见 PromQL 报错与排查

| 报错 | 原因 | 处理 |
|---|---|---|
| `histogram_quantile() returns unexpected result` | bucket 不足 + 样本稀疏 | 拉长 `rate window` 至 15m 或增加 bucket 密度 |
| `no data` | 任务未触发 / schedule 周期未到 | `up{job="ai-monitor"}` 确认抓取；`ai_probe_request_total` 确认有数据 |
| P99 ≈ P50 | bucket 设计过粗，所有值落入同一桶 | 按 §3 优化 bucket |
| Grafana 曲线毛刺严重 | rate window 短 + chunk 抖动 | 改 5m → 10m；或叠加 `_over_time` 平滑 |

---

## 6. 跨实例 / 跨集群对比（可选）

当多地部署 ai-monitor 时，Prometheus 通常已带 `instance` label。如需按集群对比：

```yaml
# prometheus.yml scrape_configs
- job_name: ai-monitor
  static_configs:
    - targets: ['ai-monitor-bj:9090']
      labels: { cluster: 'bj' }
    - targets: ['ai-monitor-sh:9090']
      labels: { cluster: 'sh' }
```

```promql
histogram_quantile(0.95,
  sum by (le, cluster, protocol, model) (
    rate(ai_probe_ttft_seconds_bucket[5m])
  )
)
```

