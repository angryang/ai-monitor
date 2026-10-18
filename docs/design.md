# AI 服务监控与指标统计系统 · 设计方案

> **状态**：设计方案（待用户审阅）
> **阶段**：第一阶段交付（实现前）
> **作者**：许清楚（PRD） + 高见远（架构设计 + 任务分解）
> **汇编**：齐活林（Qi）· 主理人
> **项目根**：`/Users/yangchun/code/gitlab/aibench/ai-monitor`
> **下一步**：用户确认设计后启动寇豆码（工程师）实现 → 严过关（QA）验证

---

## TL;DR

为多协议 AI 端点（OpenAI 兼容 / Anthropic / Gemini）提供**周期性拨测 + 流式时延统计（TTFT / ITL / 会话总时长）+ Prometheus 拉取指标 + 企业微信告警**的单实例 Go 服务。

---

## 目录

- [Part 1 · PRD（许清楚）](#part-1--prd许清楚)
  - [1. 默认技术栈与关键决策](#1-默认技术栈与关键决策)
  - [2. 任务配置字段表](#2-任务配置字段表)
  - [3. 指标 metric 清单](#3-指标-metric-清单)
  - [4. 接口契约（PRD 草签）](#4-接口契约prd-草签)
  - [5. 通知触发规则](#5-通知触发规则)
  - [6. 假设清单（⚠️ 审阅重点）](#6-假设清单-审阅重点)
  - [7. 非目标](#7-非目标)
  - [8. 需求池（P0/P1/P2）](#8-需求池p0p1p2)
- [Part 2 · 架构设计（高见远）](#part-2--架构设计高见远)
  - [9. 最终技术选型（含 go.mod）](#9-最终技术选型含-gomod)
  - [10. 目录结构](#10-目录结构)
  - [11. 包与接口定义（Go 签名）](#11-包与接口定义go-签名)
  - [12. DB Schema](#12-db-schema)
  - [13. 关键流程时序图](#13-关键流程时序图)
  - [14. 模块依赖图](#14-模块依赖图)
  - [15. 实现任务列表](#15-实现任务列表)
  - [16. 跨文件共享约定](#16-跨文件共享约定)
  - [17. 待明确事项 + 风险](#17-待明确事项--风险)
- [附录 A · Mermaid 文件独立产物](#附录-a--mermaid-文件独立产物)

---

# Part 1 · PRD（许清楚）

## 1. 默认技术栈与关键决策

| # | 决策点 | 推荐方案 | 理由 | 替代项 |
|---|---|---|---|---|
| 1 | 配置存储 | **SQLite + GORM** | 任务需要增删改查 + 状态字段 + 审计；单实例零运维；GORM 提供 AutoMigrate 与类型安全 | YAML（静态、轻量、缺事务与运行时变更）；MySQL（多实例协作才需要） |
| 2 | 任务调度 | **robfig/cron v3 + goroutine + 信号量** | 主流库、秒级 cron、内置日志钩子；Go 生态首选 | cron v3 官方包（API 较新）；自研 ticker（需自实现解析与持久化） |
| 3 | 指标采集与上报 | 流式 chunk 回调 → **prometheus/client_golang pull 模式** | 流式天然支持增量；pull 是 Prometheus 标准做法、进程常驻场景契合 | pushgateway（短生命周期任务适用）；OTel exporter（本期不必要） |
| 4 | 多协议可扩展 | **ProtocolAdapter interface + 注册表** | Go 接口天然支持多实现；注册表新增协议零侵入；schema 与协议解耦 | switch on type（紧耦合）；策略模式 + 工厂（样板代码多） |
| 5 | 通知触发 | 连续失败 + 单次慢 + 滑窗成功率三组合 + 去抖 | 单一阈值易抖动；三组合覆盖三类异常 | 仅 HTTP 错误码（漏掉劣化）；纯成功率（漏掉单次严重延迟） |

> 实现语言：**Go 1.25.x**（默认）；请求模式：**强制流式**（ITL 必须从流式 chunk 计算）。

---

## 2. 任务配置字段表

**标记说明**：🌐 公共字段 ｜ 🔌 协议相关字段

| 字段名 | 类型 | 必填 | 默认值 | 说明 | 示例 |
|---|---|---|---|---|---|
| 🌐 id | string (uuid) | 自动生成 | — | 任务唯一标识 | `task-7f3a` |
| 🌐 name | string | 是 | — | 任务显示名，唯一索引 | `prod-gpt4o-probe` |
| 🌐 enabled | bool | 否 | true | 是否启用 | `true` |
| 🌐 protocol | enum | 是 | — | `openai` / `anthropic` / `gemini` | `openai` |
| 🌐 base_url | string | 是 | — | AI 服务 base URL（`url` 校验） | `https://api.openai.com` |
| 🌐 apikey | string (secret) | 是 | — | 鉴权密钥（DB 加密） | `sk-...` |
| 🌐 model | string | 是 | — | 模型名 | `gpt-4o` |
| 🌐 system_prompt | string | 否 | "" | 系统提示，支持 `${var}` | `You are a helpful assistant.` |
| 🌐 user_prompt | string | 是 | — | 用户提示，支持 `${timestamp}` 等变量 | `Say hi at ${timestamp}` |
| 🌐 prompt_vars | map[string]string | 否 | {} | 变量注入映射 | `{"timestamp": "{{.NowRFC3339}}"}` |
| 🌐 schedule | string | 是 | — | cron 表达式（秒级 6 位） | `*/30 * * * * *` |
| 🌐 timeout | duration | 否 | 30s | 单次探测总超时（1s~10m） | `30s` |
| 🌐 notify.enabled | bool | 否 | true | 是否启用通知 | `true` |
| 🌐 notify.channel | enum | 否 | wecom | 通知渠道（本期仅 wecom） | `wecom` |
| 🌐 notify.wecom_webhook | string (secret) | 条件 | — | 企业微信群机器人 webhook（DB 加密） | `https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx` |
| 🔌 openai.stream_path | string | 否 | `/v1/chat/completions` | OpenAI 流式端点 | `/v1/chat/completions` |
| 🔌 openai.extra_headers | map[string]string | 否 | {} | 自定义请求头 | `{"X-Trace": "1"}` |
| 🔌 anthropic.max_tokens | int | 否 | 1024 | Anthropic 必填 max_tokens（1~200000） | `64` |
| 🔌 gemini.alt | string | 否 | `sse` | Gemini 流式参数（`sse` / `ndjson`） | `sse` |

> 协议段说明：openai/anthropic/gemini 三段根据 `protocol` 字段**互斥启用**；ProtocolAdapter 负责 schema 校验。
> apikey + webhook 用 AES-GCM 加密落 DB，主密钥由 env / flag 注入。

---

## 3. 指标 metric 清单

| Metric 名 | 类型 | 单位 | Labels | Bucket / 用途 |
|---|---|---|---|---|
| `ai_probe_request_duration_seconds` | Histogram | s | `task_id, protocol, model, status` | buckets `[.1, .25, .5, 1, 2, 5, 10, 20, 30, 60]` 会话总时长分布 |
| `ai_probe_ttft_seconds` | Histogram | s | `task_id, protocol, model` | `[.05, .1, .25, .5, 1, 2, 5, 10]` 首 token 延迟分布 |
| `ai_probe_itl_seconds` | Histogram | s | `task_id, protocol, model` | `[.01, .025, .05, .1, .25, .5, 1]` 相邻 token 间隔 |
| `ai_probe_request_total` | Counter | — | `task_id, protocol, model, status` | 探测总次数 |
| `ai_probe_failure_total` | Counter | — | `task_id, protocol, model, error_type` | 失败细分（timeout / 4xx / 5xx / protocol_error） |
| `ai_probe_chunk_total` | Counter | — | `task_id, protocol, model` | 累计接收 chunk 数 |
| `ai_probe_task_last_success_timestamp_seconds` | Gauge | s | `task_id` | 最近一次成功时间（探活 / 看板） |
| `ai_probe_scheduler_lag_seconds` | Gauge | s | `task_id` | 调度延迟（实际执行时间 - 计划时间） |

> **Label cardinality 建议**：`task_id` ≤ 100 量级；`error_type` 用枚举非原始错误文本。
> **Layout 选型**：单实例单任务维度安全；可后续拆分 `task_id` 为 `<protocol>.<model>` 哈希以减小 series。

---

## 4. 接口契约（PRD 草签）

```text
ChatProtocolAdapter
  Name() string
  Validate(cfg TaskConfig) error
  Probe(ctx, cfg) ProbeStream

ProbeStream
  Chunks() <-chan Chunk    // 每个 chunk 带 ArrivedAt + payload
  Err() error              // 流结束时返回最终错误
  Close() error            // 释放连接

ConfigStore
  ListTasks / GetTask / CreateTask / UpdateTask / DeleteTask
  Watch(ctx) <-chan Event  // P1：变更通知

Notifier
  Name() string
  Send(ctx, AlertEvent) error

MetricSink
  ObserveTTFT / ObserveITL / ObserveDuration
  IncFailure / IncChunk
```

> 详细方法签名见 Part 2 §11。

---

## 5. 通知触发规则

| 规则 | 默认阈值 | 理由 |
|---|---|---|
| 连续失败 | 同一任务连续 N=3 次失败 | 容忍偶发抖动；3 次足以确认非随机 |
| 单次严重慢请求 | 会话总时长 > 20s 或 TTFT > 10s | 远超 P99 即视为劣化 |
| 滑窗成功率 | 5 分钟窗口成功率 < 80%（样本 ≥ 20） | 持续劣化但未完全失败 |
| 鉴权失败 | 401 / 403 立即告警（不经连续阈值） | 凭据问题不容缓冲 |
| 通知去抖 | 同一 task_id+rule 5 分钟内仅触发一次 | 避免告警风暴 |
| 恢复通知 | 触发过的任务恢复后发一次 OK（P1） | 闭环 |

> 所有阈值均落地为配置项，**默认配置见 §9 选型表**，用户审阅时可调。

---

## 6. 假设清单（⚠️ 审阅重点）

| # | 假设 | 备选 / 推翻路径 |
|---|---|---|
| A1 | 配置落 SQLite（非 YAML / MySQL） | 改 YAML / MySQL |
| A2 | Prometheus **pull** 模式（非 pushgateway） | 改 pushgateway |
| A3 | 调度最小粒度到**秒**（cron 6 位） | 改 5 位标准 cron |
| A4 | 最大并发探测数 = **16** | 按需调整 |
| A5 | apikey / webhook 在 DB 内 **AES-GCM** 加密 | 可接 KMS |
| A6 | 本期只做**企业微信**一种通知 | 钉钉 / Slack 走 P2 |
| A7 | 历史探测结果不长期存储 | 加 ring buffer |
| A8 | **单实例**部署，不考虑 HA / leader election | 拆调度与执行 |
| A9 | 二进制 + Docker 发布，不打包 Helm | — |
| A10 | ITL 强制要求流式；若协议仅非流式则**降级**——只统计 duration | 转告用户该协议不可用 |

---

## 7. 非目标

- ❌ 不做告警规则引擎（交给 Prometheus Alertmanager）
- ❌ 不做长链路 trace 关联（不接 OTel trace）
- ❌ 不做 dashboard / 可视化（交给 Grafana）
- ❌ 不做多租户 / RBAC
- ❌ 不做云原生自动扩缩
- ❌ 不做协议层代理（只拨测，不转发业务流量）
- ❌ 不做对话式评测 / 答案质量打分

---

## 8. 需求池（P0/P1/P2）

### P0（必须有）
- 三种协议（OpenAI / Anthropic / Gemini）流式探测
- 任务配置 CRUD（SQLite 持久化）
- cron 秒级调度 + 多任务并发 + 并发上限保护
- TTFT / ITL / 会话总时长采集 + Prometheus pull
- 异常通知（企业微信）
- `/metrics` + `/healthz`

### P1（强烈建议）
- 任务启用 / 禁用热切换（无需重启）
- 协议 adapter 配置校验（启动 + 保存双重校验）
- 通知去抖（5min）
- 优雅关闭（drain in-flight）
- 结构化日志（task_id / duration / status 全链路）

### P2（可选）
- **P2-A · Web UI 任务管理**：`deploy/admin-ui.html`（单文件 HTML + Vue 3 CDN）已提供；零后端改动可读 + reload。要"新建/删除"需补 POST / PATCH / DELETE `/admin/tasks` 路由（apikey/webhook 在后端做 AES-GCM 加密后入库）。
- 配置文件导入 / 导出
- 多通知渠道（钉钉 / Slack / Email）
- Pushgateway 模式
- OTel exporter

---

# Part 2 · 架构设计（高见远）

## 9. 最终技术选型（含 go.mod）

| 技术项 | 选型 | 版本 | 理由 / 风险 |
|---|---|---|---|
| 语言 | Go | 1.25.x | slog/slices/maps 标准库齐全；单二进制发布 |
| HTTP 路由 | 标准库 `net/http` + `http.ServeMux` (Go 1.22+ pattern) | stdlib | 固定 3 路由无需框架；零依赖 |
| SQLite 驱动 | `glebarez/sqlite` | v1.11.0 | pure-Go、无 CGO；交叉编译友好 |
| ORM | `gorm.io/gorm` | v1.25.10 | AutoMigrate 省迁移脚本 |
| Cron | `robfig/cron/v3` | v3.0.1 | 秒级支持 + SkipIfStillRunning |
| 并发控制 | `golang.org/x/sync/semaphore` | v0.7.0 | 容量 16；轻量 |
| Prometheus | `prometheus/client_golang` | v1.19.1 | HistogramVec + 自定义 registry |
| 日志 | stdlib `log/slog`（JSON handler） | Go 1.22+ | 零依赖；结构化字段易采集 |
| YAML 解析 | `gopkg.in/yaml.v3` | v3.0.1 | 启动合并 DB 任务配置 |
| 配置校验 | `go-playground/validator/v10` | v10.22.0 | struct tag 校验 |
| UUID | `google/uuid` | v1.6.0 | task_id |
| 加密 | stdlib `crypto/aes` + `cipher.AEAD` | stdlib | AES-256-GCM |
| HTTP 客户端 | stdlib `net/http` | stdlib | SSE 友好 |
| 测试 | stdlib + `stretchr/testify` | v1.9.0 | 断言 |
| Mock | 优先 `httptest.Server`；可备 `go.uber.org/mock` | v0.5.0 | 真实 mock 优先 |

### 完整 `go.mod`

```go
module github.com/aibench/aibench/ai-monitor

go 1.25

require (
    github.com/glebarez/sqlite v1.11.0
    github.com/go-playground/validator/v10 v10.22.0
    github.com/google/uuid v1.6.0
    github.com/prometheus/client_golang v1.19.1
    github.com/robfig/cron/v3 v3.0.1
    github.com/stretchr/testify v1.9.0
    go.uber.org/mock v0.5.0
    golang.org/x/sync v0.7.0
    gopkg.in/yaml.v3 v3.0.1
    gorm.io/gorm v1.25.10
)
```

**未引入**：chi/echo/gin（路由太轻）、zap/logrus（slog 够）、migrate 库（GORM AutoMigrate 够）、viper（yaml+env+flag 三层手动合并）。

**默认参数**：`MaxConcurrent=16`、`SlowThreshold=10s`、`ConsecFailureN=3`、`SuccessRateWindow=200` 样本、`SuccessRateThreshold=0.8`、`DedupTTL=5min`、`Jitter=250ms`、`HTTPAddr=:9090`。

---

## 10. 目录结构

```
ai-monitor/
├── cmd/
│   └── ai-monitor/
│       └── main.go                  # 装配入口：唯一装配点
├── internal/
│   ├── config/
│   │   ├── task.go                  # TaskConfig + 子结构 + 校验
│   │   ├── store.go                 # ConfigStore 接口、Event、Watch
│   │   ├── store_sqlite.go          # GORM 实现 + AutoMigrate
│   │   ├── crypto.go                # AES-GCM 加解密
│   │   └── loader.go                # yaml < env < flag 合并
│   ├── protocol/
│   │   ├── adapter.go               # ChatProtocolAdapter / ProbeStream / Chunk / AlertEvent
│   │   ├── registry.go              # Register / MustGet / List
│   │   ├── openai/                  # OpenAI SSE 流式
│   │   ├── anthropic/               # Anthropic event-stream
│   │   └── gemini/                  # Gemini streamGenerateContent
│   ├── metrics/
│   │   ├── sink.go                  # MetricSink 接口
│   │   └── prom.go                  # PromSink + 自定义 *prometheus.Registry
│   ├── notifier/
│   │   ├── notifier.go              # Notifier 接口 + AlertEvent + MultiNotifier
│   │   ├── dedup.go                 # 5min 去抖
│   │   └── wecom/                   # 企业微信实现
│   ├── probe/
│   │   ├── runner.go                # 编排：adapter→chunk→sink→notifier
│   │   ├── sliding.go               # 滑动窗口成功率
│   │   └── rules.go                 # 4 条告警规则判定
│   ├── scheduler/
│   │   ├── scheduler.go             # Scheduler 接口
│   │   ├── cron.go                  # cron v3 + semaphore + jitter
│   │   └── reload.go                # 热加载（P1）
│   ├── server/
│   │   └── http.go                  # /metrics / /healthz / /admin
│   └── logging/
│       └── slog.go                  # 初始化 + 字段中间件
├── deploy/
│   ├── example.config.yaml
│   ├── prometheus-alerts.yaml
│   └── Dockerfile                   # 多阶段 CGO_ENABLED=0 scratch 镜像
├── Makefile                         # build / test / lint / run / docker
├── go.mod
├── go.sum
└── README.md
```

**职责约束**：
- `cmd/ai-monitor/main.go` 唯一装配入口，禁业务逻辑
- `internal/` 不可被外部 import
- 包名小写无下划线；文件名小写下划线
- 测试文件与被测文件同包同目录

---

## 11. 包与接口定义（Go 签名）

### 11.1 `internal/protocol/adapter.go`

```go
package protocol

// X-Req-Id 是 ai-monitor 在每个 outbound 请求上携带的唯一标识，便于
// 出问题时按 request id 在 AI 服务器侧日志定位。详见 §11.6.3。
const RequestIDHeader = "X-Req-Id"

// GenerateRequestID 给 req 注入 X-Req-Id（若已存在则保留）。generator
// 可注入测试场景；默认走 uuid.NewString。生成结果返回便于调用方落日志。
func GenerateRequestID(req *http.Request, generator func() string) string { ... }

// Chunk 单个流式片段。
type Chunk struct {
    Delta     string    // 增量文本
    ArrivedAt time.Time // 到达本进程的时刻（用于 ITL/TTFT 计算）
    Index     int       // 自增，从 0
    IsFinal   bool      // 是否为终止帧
}

type ProbeStream interface {
    Chunks() <-chan Chunk   // Adapter 在内部 goroutine 写入；Close 或 ctx 取消后必须关闭
    Err() error             // 流结束后返回首个非 nil 错误
    Close() error           // 释放连接；幂等
}

type ChatProtocolAdapter interface {
    Name() string                                   // "openai" / "anthropic" / "gemini"
    Validate(cfg *config.TaskConfig) error           // 校验协议段配置
    Probe(ctx context.Context, cfg *config.TaskConfig) (ProbeStream, error)
}

var (
    ErrUnauthorized = errors.New("protocol: unauthorized")          // 401/403，即时告警
    ErrProtocolCfg  = errors.New("protocol: config invalid")        // 配置错误
    ErrUpstreamDown = errors.New("protocol: upstream unavailable")  // 网络/DNS/超时
)
```

### 11.2 `internal/protocol/registry.go`

```go
type AdapterFactory func(deps AdapterDeps) ChatProtocolAdapter

type AdapterDeps struct {
    HTTPClient *http.Client
    UserAgent  string
}

func Register(name string, f AdapterFactory)   // 同名重复 panic
func MustGet(name string, deps AdapterDeps) ChatProtocolAdapter
func List() []string
```

### 11.3 `internal/config/task.go`

```go
type Protocol string
const (
    ProtocolOpenAI    Protocol = "openai"
    ProtocolAnthropic Protocol = "anthropic"
    ProtocolGemini    Protocol = "gemini"
)

type NotifyChannel string
const NotifyWeCom NotifyChannel = "wecom"

type TaskConfig struct {
    ID           string            `gorm:"primaryKey;size:64"`
    Name         string            `gorm:"uniqueIndex;size:128;not null" validate:"required,min=1,max=128"`
    Enabled      bool              `gorm:"not null;default:true"`
    Protocol     Protocol          `gorm:"size:32;not null" validate:"required,oneof=openai anthropic gemini"`
    BaseURL      string            `gorm:"size:512;not null" validate:"required,url"`
    APIKey       string            `gorm:"-"`            // 运行时
    APIKeyEnc    []byte            `gorm:"column:api_key_enc;type:blob"`
    Model        string            `gorm:"size:128;not null" validate:"required"`
    SystemPrompt string            `gorm:"type:text"`
    UserPrompt   string            `gorm:"type:text" validate:"required"`
    PromptVars   map[string]string `gorm:"type:text"`     // JSON
    Schedule     string            `gorm:"size:128;not null" validate:"required"` // cron 6 位
    Timeout      time.Duration     `gorm:"not null;default:30s" validate:"min=1s,max=10m"`
    Notify       NotifyConfig      `gorm:"embedded"`
    OpenAI       *OpenAIConfig     `gorm:"embedded" yaml:"openai,omitempty"`
    Anthropic    *AnthropicConfig  `gorm:"embedded" yaml:"anthropic,omitempty"`
    Gemini       *GeminiConfig     `gorm:"embedded" yaml:"gemini,omitempty"`
    CreatedAt    time.Time         `gorm:"autoCreateTime"`
    UpdatedAt    time.Time         `gorm:"autoUpdateTime"`
}

type NotifyConfig struct {
    Enabled         bool          `gorm:"not null;default:false"`
    Channel         NotifyChannel `gorm:"size:32;not null;default:wecom" validate:"omitempty,oneof=wecom"`
    WeComWebhook    string        `gorm:"-"`
    WeComWebhookEnc []byte        `gorm:"column:wecom_webhook_enc;type:blob"`
}

type OpenAIConfig struct {
    StreamPath        string `gorm:"size:256" validate:"omitempty,startswith=/"`
    ExtraHeadersJSON  string `gorm:"column:openai_extra_headers;type:text"` // JSON 序列化
}

type AnthropicConfig struct {
    MaxTokens        int    `gorm:"not null;default:1024" validate:"min=1,max=200000"`
    ExtraHeadersJSON string `gorm:"column:anthropic_extra_headers;type:text"` // JSON 序列化；用于灰度/Canary 路由头
}

type GeminiConfig struct {
    Alt              string `gorm:"size:32;not null;default:sse" validate:"omitempty,oneof=sse ndjson"`
    ExtraHeadersJSON string `gorm:"column:gemini_extra_headers;type:text"`   // JSON 序列化；用于灰度/Canary 路由头
}
```

### 11.4 `internal/config/store.go`

```go
type EventType string
const (
    EventUpsert EventType = "upsert"
    EventDelete EventType = "delete"
)

type Event struct {
    Type   EventType
    TaskID string
    At     time.Time
}

type ConfigStore interface {
    ListTasks(ctx context.Context) ([]TaskConfig, error)
    GetTask(ctx context.Context, id string) (*TaskConfig, error)
    CreateTask(ctx context.Context, t *TaskConfig) error
    UpdateTask(ctx context.Context, t *TaskConfig) error
    DeleteTask(ctx context.Context, id string) error
    Watch(ctx context.Context) (<-chan Event, error)  // P1：变更订阅
    Close() error
}

var (
    ErrNotFound      = errors.New("config: not found")
    ErrAlreadyExists = errors.New("config: already exists")
    ErrInvalidConfig = errors.New("config: invalid")
)
```

### 11.5 `internal/config/crypto.go`

```go
type Cipher interface {
    Encrypt(plaintext []byte) ([]byte, error)
    Decrypt(ciphertext []byte) ([]byte, error)
}

type AESCipher struct{ aead cipher.AEAD }

func NewAESCipher(masterKey []byte) (*AESCipher, error)  // masterKey 必须 32 字节
func (c *AESCipher) Encrypt(plaintext []byte) ([]byte, error)
func (c *AESCipher) Decrypt(ciphertext []byte) ([]byte, error)
// 落盘格式：nonce(12) || ciphertext || tag(16)
```

### 11.6 `internal/metrics/sink.go`

```go
type MetricSink interface {
    ObserveTTFT(taskID, protocol, model string, seconds float64)
    ObserveITL(taskID, protocol, model string, seconds float64)
    ObserveDuration(taskID, protocol, model, status string, seconds float64)
    IncChunk(taskID, protocol, model string, n int)
    IncFailure(taskID, protocol, model, errorType string)
    SetTaskLastSuccess(taskID string, ts time.Time)
    SetSchedulerLag(seconds float64)
    Handler() http.Handler
}
```

**PromSink 内部 8 指标**（独立 `*prometheus.Registry`，避免重复注册）：
- `ai_probe_request_duration_seconds`（HistogramVec {task_id, protocol, model, status}）
- `ai_probe_ttft_seconds`（HistogramVec {task_id, protocol, model}）
- `ai_probe_itl_seconds`（HistogramVec {task_id, protocol, model}）
- `ai_probe_request_total`（CounterVec {task_id, protocol, model, status}）
- `ai_probe_failure_total`（CounterVec {task_id, protocol, model, error_type}）
- `ai_probe_chunk_total`（CounterVec {task_id, protocol, model}）
- `ai_probe_task_last_success_timestamp_seconds`（GaugeVec {task_id}）
- `ai_probe_scheduler_lag_seconds`（Gauge）

默认 Histogram buckets：`{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}`。

**11.6.1（用户问题触发的设计补丁）— 分位数计算与 buckets 优化**

Histogram 类型原生支持 PromQL 的 `histogram_quantile(quantile, sum by (le) (rate(...[5m])))` 计算 P50/P90/P95/P99 趋势图，配套 Grafana Dashboard 见 `deploy/grafana-llm-dashboard.json`。

但默认单套 buckets 在以下场景精度不足，建议在 P1-D 拆分：

| 指标 | 推荐 buckets | 理由 |
|---|---|---|
| `ai_probe_ttft_seconds` | `{.01, .025, .05, .1, .2, .35, .5, .75, 1, 2, 5}` | TTFT P99 通常 ≤ 1s，应在 1s 附近加密 |
| `ai_probe_itl_seconds` | `{.005, .01, .025, .05, .1, .25, .5}` | ITL 期望 ≤ 50ms |
| `ai_probe_request_duration_seconds` | `{.1, .25, .5, 1, 2.5, 5, 10, 20, 30, 60, 120}` | 会话总时长在长上下文下 P99 可达 30s+ |
| `ai_probe_scheduler_lag_seconds` | 升级为 Histogram（沿用 `{.01, .05, .1, .5, 1, 5, 30, 60}`） | 便于 P95 调度延迟计算 |

**11.6.2 — 可选 label：`canary_group`**（与 §17.1 第 1 项联动）

若启用灰度流量测试，建议给所有 `*Vec` 指标加 label `canary_group string`，默认 `"default"`。灰度任务配置时设定该值（如 `"gray-2024q3"` / `"prod"`），PromQL 可直接 split 出对比曲线：

```promql
histogram_quantile(0.95,
  sum by (le, protocol, model, canary_group) (
    rate(ai_probe_ttft_seconds_bucket[5m])
  )
)
```

> Cardinality 影响：每个任务增加 1 个 label 维度，若 task_id < 100 量级可控。

### 11.6.3 — X-Req-Id 自动注入（用户需求触发的设计补丁）

**目标**：每次 probe 触发时携带唯一 `X-Req-Id` HTTP header，方便出问题时根据 request id 在 AI 服务器侧日志定位对应的拨测记录。

**实现**：
- `protocol.RequestIDHeader = "X-Req-Id"` 常量（`internal/protocol/adapter.go`）
- `protocol.GenerateRequestID(req, generator func() string) string` helper：
  - 若 req 已含 `X-Req-Id`（test 注入 / 用户 extras）→ 原样返回
  - 否则用 generator（默认 `uuid.NewString`，RFC 4122 v4）生成一个，set 到 req.Header，返回
  - generator 返回空字符串时会兜底用 uuid，不会让 header 在线上"空着"
- `AdapterDeps.RequestIDGenerator func() string`（`internal/protocol/registry.go`）— 测试可注入确定性 generator
- 三协议适配器（OpenAI / Anthropic / Gemini）在 `http.NewRequestWithContext` 后立刻、**在 `ApplyExtraHeaders` 之前**调用 `GenerateRequestID`

**优先级约定**：
1. 用户通过 `extra_headers` 显式给 `X-Req-Id` → 用户值生效（让上游 trace id 直接贯穿）
2. 否则 ai-monitor 自动注入 uuid v4
3. 用户只想用 ai-monitor 的 uuid，不传 extras 即可

**未来可加**（不属于本期）：
- 把 `req_id` 加进 slog logger context（与 `task_id` 同级），客户端日志可一行 grep
- alert `Meta["req_id"]` 让告警消息也带上
- 与 OpenTelemetry traceparent 协同（传递 `traceparent` 而非自造 `X-Req-Id`）

### 11.7 `internal/notifier/notifier.go`

```go
type AlertSeverity string
const (
    SeverityInfo     AlertSeverity = "info"
    SeverityWarning  AlertSeverity = "warning"
    SeverityCritical AlertSeverity = "critical"
)

type AlertRule string
const (
    RuleConsecFailure  AlertRule = "consecutive_failures"
    RuleSlowRequest    AlertRule = "slow_request"
    RuleSuccessRateLow AlertRule = "success_rate_below_80"
    RuleUnauthorized   AlertRule = "unauthorized_immediate"
)

type AlertEvent struct {
    TaskID     string
    TaskName   string
    Protocol   string
    Rule       AlertRule
    Severity   AlertSeverity
    Message    string
    OccurredAt time.Time
    Meta       map[string]string
}

type Notifier interface {
    Name() string
    Send(ctx context.Context, evt AlertEvent) error
}

type MultiNotifier struct{ notifiers []Notifier }  // P1 预留
```

### 11.8 `internal/notifier/dedup.go`

```go
type Deduper struct {
    ttl time.Duration
    mu  sync.Mutex
    m   map[string]time.Time
}

func NewDeduper(ttl time.Duration) *Deduper
func (d *Deduper) ShouldFire(key string, now time.Time) bool
// Dedup key = task_id + ":" + rule；5min 内仅触发一次
```

### 11.9 `internal/scheduler/scheduler.go`

```go
type Scheduler interface {
    Start(ctx context.Context) error
    Reload(ctx context.Context, tasks []config.TaskConfig) error  // P1
    Add(task config.TaskConfig) error
    Remove(taskID string) error
    Stop(ctx context.Context) error
}
```

### 11.10 `internal/scheduler/cron.go` 关键结构

```go
type cronScheduler struct {
    cron     *cron.Cron
    sem      *semaphore.Weighted
    runner   *probe.Runner
    sink     metrics.MetricSink
    logger   *slog.Logger
    jitter   time.Duration
    inflight sync.WaitGroup
}
```

### 11.11 `internal/probe/probe.go`

```go
type Result struct {
    TaskID    string
    TaskName  string
    Protocol  string
    Model     string
    StartedAt time.Time
    EndedAt   time.Time
    TTFT      time.Duration     // 0 表示无 chunk
    ITLSum    time.Duration
    ITLCount  int
    Chunks    int
    Status    string            // "success" / "failure" / "unauthorized"
    Err       error
}

type Runner struct {
    adapters                protocol.Registry
    sink                    metrics.MetricSink
    notifier                notifier.Notifier
    dedup                   *notifier.Deduper
    sliders                 sync.Map           // task_id -> *slidingWindow
    slowThreshold           time.Duration      // 默认 10s
    consecFailureN          int                // 默认 3
    successRateWindow       int                // 默认 200
    successRateThreshold    float64            // 默认 0.8
}

func (r *Runner) Run(ctx context.Context, t config.TaskConfig) Result
// 步骤：
//   1. registry.MustGet(t.Protocol)
//   2. adapter.Probe(ctx, &t) → ProbeStream
//   3. 读 Chunks：记录 TTFT / ITL
//   4. Close → 记录 Duration
//   5. sink.Observe* 上报
//   6. evaluateAlerts → notifier.Send
```

### 11.12 `internal/server/http.go`

```go
type Server struct { /* cfg + store + scheduler + sink + logger + http.Server */ }

func (s *Server) Start(ctx context.Context) error
func (s *Server) Shutdown(ctx context.Context) error

// 路由：
//   GET  /metrics         → sink.Handler()
//   GET  /healthz         → 200 / 503（DB ping + 最近 cron 触发时间）
//   GET  /admin/tasks     → store.ListTasks（dev bind）
//   POST /admin/reload    → scheduler.Reload（dev bind）
// 默认监听：:9090
```

### 11.13 `internal/probe/store.go` + `store_sqlite.go`（**§18.11 增量**）

> **背景**：P50/P90/P99 趋势图需要从聚合 Histogram 推算，但单点失败、上游 5xx 风暴、模型撤回等**「离散事件」**会淹没在 average 里。`ai_probe_request_total{status="failure"}` 只给累计数，看不到「5 分钟前哪一条 404 因为什么」。本节把每次探测的**实际结果**（status / error / TTFT / ITL / chunks / 时间戳）落 DB，让 admin 能按任务看历史。

```go
// Run 后立即写一行；Prune 在 Save 之后跑（crash-safe："略多" 比 "略少" 安全）。
type RunStore interface {
    Save(ctx context.Context, run *ProbeRun) error
    ListByTask(ctx context.Context, taskID string, limit int) ([]ProbeRun, error)
    Prune(ctx context.Context, keep int) error
    Close() error
}

// 一个 ProbeRun 就是一次探测的"行式"画像。
type ProbeRun struct {
    ID         int64     `gorm:"primaryKey;autoIncrement" json:"id"`
    TaskID     string    `gorm:"size:64;not null;index"        json:"task_id"`
    TaskName   string    `gorm:"size:128;not null"             json:"task_name"`  // 反范式：任务改名/删后历史仍可读
    Protocol   string    `gorm:"size:32;not null"              json:"protocol"`
    Model      string    `gorm:"size:128;not null"             json:"model"`
    Status     string    `gorm:"size:32;not null"              json:"status"`    // success / failure / unauthorized
    Err        string    `gorm:"type:text"                      json:"error,omitempty"`
    StartedAt  time.Time `gorm:"not null;index"                 json:"started_at"`
    EndedAt    time.Time `gorm:"not null"                       json:"ended_at"`
    DurationMS int64     `gorm:"not null"                       json:"duration_ms"`
    TTFTMS     int64                                          json:"ttft_ms"`
    ITLAvgMS   int64                                          json:"itl_avg_ms"`
    ITLCount   int                                            json:"itl_count"`
    Chunks     int      `gorm:"not null"                       json:"chunks"`
}

// 关键设计：probe 包用 FromResult(Result) ProbeRun 做适配，
// 不让 probe 包 import GORM 类型，runner 也不持有 DB handle。
```

**SqliteRunStore**（`internal/probe/store_sqlite.go`）：

- 共享 `config.SqliteStore.DB()` 的 `*gorm.DB` handle —— 不重开文件，避免 WAL / lock 冲突
- `AutoMigrate` 在 main 启动时跑一次，GORM 只在 schema 不一致时写 `CREATE TABLE`
- `ListByTask` 默认 `limit=20`，上限 `100`，SQL: `ORDER BY started_at DESC, id DESC LIMIT N`（id 是同毫秒平局兜底）
- `Prune` 用 window function 一次性扫描所有任务，per-task 保留最新 N 条：
  ```sql
  DELETE FROM probe_runs
  WHERE id NOT IN (
    SELECT id FROM (
      SELECT id, ROW_NUMBER() OVER (
        PARTITION BY task_id
        ORDER BY started_at DESC, id DESC
      ) AS rn
      FROM probe_runs
    ) WHERE rn <= :keep
  )
  ```
  默认 `keep=100`（30s schedule × 100 ≈ 50 分钟窗口）

**与 runner 的接线**：

```go
func (r *Runner) Run(ctx context.Context, t config.TaskConfig) Result {
    res := Result{...}
    // 关键：defer **传指针**——`defer f(args)` 在 defer 语句处按值求值，
    // 传 res 值会拿到零值 Result。传 &res，deferred 调用能看到全部字段。
    defer r.persistRun(&res)
    // ... 探测逻辑 ...
    return res
}

func (r *Runner) persistRun(res *Result) {
    if r.RunStore == nil { return }
    run := FromResult(*res)
    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
    defer cancel()
    r.RunStore.Save(ctx, &run)         // 错误仅 log，不影响 cron 下一次 tick
    r.RunStore.Prune(ctx, r.RunRetention)
}
```

> **错误裁剪**：`r.Err.Error()` 截到 4096 字符。网络错误可能含整个 HTML body（几 KB），避免 SQLite text 列无限增长。

**新端点**：

```
GET /admin/tasks/{id}/runs?limit=20
→ 200 {"runs": [ProbeRun, ProbeRun, ...]}      # 新→旧
→ 404 task_not_found                          # id 不存在
→ 400 bad_limit                               # limit 不是正整数
→ 503 run_store_disabled                      # RunStore=nil（防误关）
```

`limit` 默认 20、上限 100。RBAC 同其它 `GET /admin/*`：viewer / admin 都可读（只读不写）。

---

## 12. DB Schema

```go
// GORM AutoMigrate，启用 WAL + busy_timeout
db, _ := gorm.Open(sqlite.Open(
    "file:ai-monitor.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)",
    &gorm.Config{},
))
db.AutoMigrate(&TaskConfig{})  // 单表，含 embedded Notify / OpenAI / Anthropic / Gemini
```

**生成的 `tasks` 表关键字段**：

```sql
CREATE TABLE tasks (
    id              VARCHAR(64)  PRIMARY KEY,
    name            VARCHAR(128) NOT NULL UNIQUE,
    enabled         BOOLEAN      NOT NULL DEFAULT 1,
    protocol        VARCHAR(32)  NOT NULL,
    base_url        VARCHAR(512) NOT NULL,
    api_key_enc     BLOB,
    model           VARCHAR(128) NOT NULL,
    system_prompt   TEXT,
    user_prompt     TEXT,
    prompt_vars     TEXT,                                    -- JSON
    schedule        VARCHAR(128) NOT NULL,
    timeout_seconds INTEGER      NOT NULL DEFAULT 30,
    notify_enabled  BOOLEAN      NOT NULL DEFAULT 0,
    notify_channel  VARCHAR(32)  NOT NULL DEFAULT 'wecom',
    wecom_webhook_enc BLOB,
    openai_stream_path      VARCHAR(256),
    openai_extra_headers    TEXT,                            -- JSON
    anthropic_max_tokens    INTEGER      DEFAULT 1024,
    gemini_alt              VARCHAR(32)  DEFAULT 'sse',
    created_at  DATETIME,
    updated_at  DATETIME
);
```

**`probe_runs` 表（**§18.11 增量**）** —— 每次探测一行，admin 看历史趋势/失败溯源：

```sql
CREATE TABLE probe_runs (
    id          INTEGER      PRIMARY KEY AUTOINCREMENT,
    task_id     VARCHAR(64)  NOT NULL,                      -- 关联 tasks.id
    task_name   VARCHAR(128) NOT NULL,                      -- 反范式：删/改名后历史仍可读
    protocol    VARCHAR(32)  NOT NULL,
    model       VARCHAR(128) NOT NULL,
    status      VARCHAR(32)  NOT NULL,                      -- success / failure / unauthorized
    err         TEXT,                                        -- 截到 4096 字符
    started_at  DATETIME     NOT NULL,
    ended_at    DATETIME     NOT NULL,
    duration_ms INTEGER      NOT NULL,
    ttft_ms     INTEGER      NOT NULL DEFAULT 0,
    itl_avg_ms  INTEGER      NOT NULL DEFAULT 0,
    itl_count   INTEGER      NOT NULL DEFAULT 0,
    chunks      INTEGER      NOT NULL DEFAULT 0
);
CREATE INDEX idx_probe_runs_task_id    ON probe_runs(task_id);
CREATE INDEX idx_probe_runs_started_at ON probe_runs(started_at);
```

> **设计要点**：
> - **共享 `*gorm.DB`**：`probe_runs` 与 `tasks` 共用 `config.SqliteStore.DB()` 句柄，不重开文件，避免 WAL/lock 冲突。
> - **GORM AutoMigrate**：`main` 启动时跑一次，schema 不一致时才发 `CREATE TABLE / INDEX`，幂等。
> - **Prune 用窗口函数**：见 §11.13 的 SQL，单条 DELETE 把所有任务的历史裁到 `keep` 条（默认 100）。
> - **RBAC**：`GET /admin/tasks/{id}/runs` 与其它 GET 同权限（viewer / admin 都可读）。

---

## 13. 关键流程时序图

### 13.1 启动加载

```mermaid
sequenceDiagram
    autonumber
    participant Main as cmd/main
    participant Cfg as config.Loader
    participant DB as ConfigStore(SQLite)
    participant Sink as PromSink
    participant Notif as wecom.Notifier
    participant Reg as protocol.Registry
    participant Sch as cronScheduler
    participant Run as http_server

    Main->>Cfg: Load(yaml, env, flag)
    Cfg-->>Main: *Config
    Main->>DB: Open + AutoMigrate
    Main->>Sink: New(cfg.Registry)
    Main->>Notif: New(wecomCfg, dedup)
    Main->>Reg: MustGet("openai"/"anthropic"/"gemini")
    Main->>Sch: New(Options{Runner, Sink, ...})
    Main->>DB: ListTasks(ctx)
    DB-->>Main: []TaskConfig
    Main->>Sch: Start(ctx)
    Sch->>Sch: for t in tasks: cron.AddFunc(t.Schedule, entry)
    Main->>Run: Start(:9090)
```

### 13.2 单次探测

```mermaid
sequenceDiagram
    autonumber
    participant Cron as cron.v3
    participant Sch as scheduler
    participant Sem as semaphore
    participant Run as probe.Runner
    participant Reg as Registry
    participant Adp as ChatProtocolAdapter
    participant HTTP as Upstream AI
    participant Sink as PromSink
    participant Notif as Notifier
    participant Dedup as Deduper

    Cron->>Sch: entry fires (with jitter)
    Sch->>Sem: Acquire(ctx, 1)
    Sem-->>Sch: ok
    Sch->>Run: Run(ctx, task)
    Run->>Reg: MustGet(task.Protocol)
    Reg-->>Run: adapter
    Run->>Adp: Probe(ctx, &task)
    Adp->>HTTP: POST + auth
    HTTP-->>Adp: 200 chunked
    Adp-->>Run: ProbeStream
    loop 读 Chunks
        Run->>Run: first chunk? → TTFT = now - startedAt
        Run->>Run: prev != nil? → ITL = now - prev
        Run->>Sink: ObserveTTFT / ObserveITL / IncChunk
    end
    Run->>Adp: Close()
    Run->>Run: Duration; Status = success/failure/unauthorized
    Run->>Sink: ObserveDuration + SetTaskLastSuccess
    Run->>Run: evaluateAlerts(task, result)
    alt 鉴权失败
        Run->>Dedup: ShouldFire(RuleUnauthorized)
        Dedup-->>Run: true
        Run->>Notif: Send(AlertEvent)
    else 连续失败 / 慢请求 / 低成功率
        Run->>Dedup: ShouldFire(rule)
        Run->>Notif: Send(AlertEvent) if true
    end
    Run->>Sem: Release(1)
    Run->>Sink: SetSchedulerLag

    Note over Run: === §18.11 增量 · 探测结果落库 ===
    Run->>RunStore: Save(probe_run)         # defer(persistRun) 触发
    Run->>RunStore: Prune(keep=100)          # 一次窗口函数裁剪所有任务
    alt Save 失败（log only）
        RunStore-->>Run: error
        Run->>Run: log warn，不影响下次 tick
    end
```

### 13.3 热更新（P1）

```mermaid
sequenceDiagram
    autonumber
    participant Admin as POST /admin/reload
    participant DB as ConfigStore
    participant Watch as Watch goroutine
    participant Sch as scheduler

    DB->>Watch: emit Event{Upsert|Delete, TaskID}
    Watch->>Sch: Reload(ctx, ListTasks)
    Sch->>Sch: diff(old,new) → cron.AddFunc / Remove
    Note over Sch: 运行中探测不打断
```

---

## 14. 模块依赖图

```mermaid
graph TD
    main[cmd/ai-monitor]
    server[internal/server]
    scheduler[internal/scheduler]
    probe[internal/probe]
    config[internal/config]
    metrics[internal/metrics]
    notifier[internal/notifier]
    protocol[internal/protocol]
    logging[internal/logging]

    openai[protocol/openai]
    anthropic[protocol/anthropic]
    gemini[protocol/gemini]
    wecom[notifier/wecom]

    main --> logging
    main --> config
    main --> metrics
    main --> notifier
    main --> protocol
    main --> scheduler
    main --> server

    openai --> protocol
    openai --> config
    anthropic --> protocol
    anthropic --> config
    gemini --> protocol
    gemini --> config

    wecom --> notifier
    wecom --> config

    protocol --> config
    notifier --> config
    probe --> protocol
    probe --> config
    probe --> metrics
    probe --> notifier
    scheduler --> probe
    scheduler --> config
    scheduler --> metrics
    server --> config
    server --> metrics
    server --> scheduler
```

**Import 方向约束**（写入 Makefile lint 阶段 + depguard）：
- `protocol/*` → `protocol` → `config`（最底层）
- `metrics` 仅依赖 stdlib + prom client
- `notifier` 仅依赖 `config` + `notifier/wecom`
- `probe` 可依赖所有下层
- `scheduler` 可依赖 `probe + metrics + config`
- `server` 可依赖 `config + metrics + scheduler`
- `main` 是唯一装配入口

**禁止反向依赖**（lint 强制）。

---

## 15. 实现任务列表

> **强约束**：19 个任务（P0=14 + P1=5），远低于 30 上限；每个任务独立可提交；TDD 流程（红→绿→重构）。
> **首任务必为项目骨架**；最后任务为收尾。
> **新增**：
> - P1-C「协议级 ExtraHeaders 收口」用于支持 AI 端点的**灰度流量测试**（Canary 路由 / 租户路由 / A/B 对比）。
> - P1-D「Histogram 指标分桶拆分 + scheduler_lag 升级 Histogram」用于让 P50/P90/P95/P99 分位数趋势图精度达标。

### 15.1 任务总览

| ID | 标题 | 分区 | 依赖 | 优先级 |
|---|---|---|---|---|
| A1 | 项目骨架（go.mod/Makefile/Hello HTTP） | A | — | P0 |
| B1 | 配置加载与合并（yaml+env+flag） | B | A1 | P0 |
| B2 | SQLite + GORM 迁移 + TaskConfig 模型 | B | B1 | P0 |
| B3 | AES-GCM 加密 + ConfigStore CRUD + Watch | B | B2 | P0 |
| C1 | adapter interface + registry + sentinel errors | C | B2 | P0 |
| C2 | OpenAI 适配器（SSE） | C | C1 | P0 |
| C3 | Anthropic 适配器（event-stream） | C | C1 | P0 |
| C4 | Gemini 适配器（streamGenerateContent） | C | C1 | P0 |
| D1 | MetricSink interface + PromSink（8 指标） | D | A1 | P0 |
| E1 | scheduler cron + semaphore + jitter | E | C2,C3,C4,D1 | P0 |
| F1 | Notifier 接口 + wecom 实现 + Deduper | F | B3 | P0 |
| G1 | probe.runner + 滑动窗口 + 规则评估 | G | C1,D1,F1 | P0 |
| G2 | HTTP 服务（/metrics + /healthz + /admin） | G | E1,G1,D1 | P0 |
| H1 | 端到端集成测试 | H | G2 | P0 |
| I1 | 收尾（README/示例/告警规则/Dockerfile） | I | H1 | P0 |
| P1-A | 热加载（Watch → Reload） | P1 | G2 | P1 |
| P1-B | 协议校验 + 结构化日志 + 优雅关闭 | P1 | G2 | P1 |
| P1-C | 协议级 ExtraHeaders 收口（OpenAI/Anthropic/Gemini 全量） + 灰度用例示例 | P1 | C2,C3,C4 | P1 |
| P1-D | Histogram 分桶拆分 + scheduler_lag 升 Histogram + canary_group label（精度支撑 P50/P90/P95/P99） | P1 | D1 | P1 |
| J1 | 探测结果落库（RunStore + `probe_runs` 表 + `GET /admin/tasks/{id}/runs` + Prune 策略） | J | G1,B2 | P1 |

### 15.2 详细任务卡（关键 P0）

#### **A1 · 项目骨架**（2-5 分钟）
- **文件**：`go.mod`、`go.sum`、`Makefile`、`internal/logging/slog.go`、`cmd/ai-monitor/main.go`、`internal/server/http.go`（最小版仅 `/healthz`）
- **步骤**：
  1. `go mod init` + 写入 §9 全部依赖 + `go mod tidy`
  2. `Makefile`：build / test / lint / run / docker
  3. `internal/logging/slog.go` 初始化 JSON handler
  4. `cmd/main.go` 启动 `:9090` 返回 "ok"
- **测试**：
  - ❌ `TestHealthz_Ok`：`/healthz` 期待 200 + body "ok"

#### **B1 · 配置加载与合并**（2-5 分钟）
- **文件**：`internal/config/loader.go` + `loader_test.go` + `deploy/example.config.yaml`
- **步骤**：定义 `Config{MasterKey, SQLitePath, MaxConcurrent, Jitter, HTTPAddr, SlowThreshold, ConsecFailureN, SuccessRateWindow, SuccessRateThreshold}`；`Load(yamlPath)`：yaml < env < flag 覆盖
- **测试**：
  - ❌ `TestLoad_YAMLOnly` / `TestLoad_EnvBeatsYAML` / `TestLoad_FlagBeatsAll` / `TestLoad_MissingMasterKey`

#### **B2 · SQLite + GORM**（2-5 分钟）
- **文件**：`internal/config/task.go` + `store_sqlite.go` + tests
- **步骤**：§11.3 struct + gorm tag；`Open(path)` 启用 WAL+busy_timeout；`AutoMigrate`
- **测试**：
  - ❌ `TestAutoMigrate_CreatesTasksTable`
  - ❌ `TestTaskConfig_RoundTrip`

#### **B3 · AES-GCM + ConfigStore CRUD + Watch**（5 分钟）
- **文件**：`crypto.go` + `store.go` + `store_sqlite.go` 补全 + tests
- **步骤**：`AESCipher` nonce(12)||ct||tag；CRUD 6 方法；`Watch` poll 5s 去重
- **测试**：
  - ❌ `TestAESCipher_RoundTrip` / `TestAESCipher_WrongKeyFail`
  - ❌ `TestConfigStore_CRUD` / `TestConfigStore_UniqueName` / `TestConfigStore_Watch`

#### **C1 · adapter interface + registry + sentinel**（5 分钟）
- **文件**：`adapter.go` + `registry.go` + `adapter_test.go`
- **步骤**：§11.1 / §11.2；sentinel errors
- **测试**：
  - ❌ `TestRegistry_RegisterOverwrite`（panic）
  - ❌ `TestRegistry_MustGetUnknown`（panic）
  - ❌ `TestRegistry_Concurrent`（`-race` 通过）

#### **C2 · OpenAI 适配器**（5-10 分钟）
- **步骤**：SSE 解析（`event:` + `data:` 行；`data: [DONE]` 终止）；401/403 → `ErrUnauthorized`
- **测试**：
  - ❌ `TestOpenAI_Probe_Success`（5 chunk + IsFinal + chan close）
  - ❌ `TestOpenAI_Probe_Unauthorized`
  - ❌ `TestOpenAI_Probe_Timeout`（ctx 取消不阻塞）
  - ❌ `TestOpenAI_Validate_InvalidPath`

#### **C3 · Anthropic 适配器**（5-10 分钟）
- **步骤**：解析 `event: content_block_delta` 的 `data.delta.text`；`message_stop` 终止
- **测试**：3 条同 C2 形态

#### **C4 · Gemini 适配器**（5-10 分钟）
- **步骤**：`streamGenerateContent` 解析 JSON 数组流；`finishReason != ""` 终止
- **测试**：3 条同 C2 形态

#### **D1 · MetricSink + PromSink**（5 分钟）
- **步骤**：§11.6 接口 + 8 指标 + 独立 `*prometheus.Registry`；buckets 默认值
- **测试**：
  - ❌ `TestPromSink_ObserveTTFT_IncrementsBuckets`
  - ❌ `TestPromSink_AllMetricsRegistered`（8 families）
  - ❌ `TestPromSink_HandlerReturnsValid`

#### **E1 · scheduler cron + semaphore + jitter**（5-10 分钟）
- **步骤**：`cron.New(cron.WithSeconds(), cron.WithChain(cron.SkipIfStillRunning))`；`semaphore.Weighted`；jitter 0~Jitter；Start/Stop/Add/Remove/Reload
- **测试**：
  - ❌ `TestScheduler_FiresEntry`（fake clock）
  - ❌ `TestScheduler_MaxConcurrent`（第 17 个 fail-fast）
  - ❌ `TestScheduler_Remove`

#### **F1 · Notifier + wecom + Deduper**（5 分钟）
- **步骤**：Notifier + AlertEvent + MultiNotifier；wecom POST markdown；Deduper 5min TTL
- **测试**：
  - ❌ `TestWeCom_Send_Success`（POST body 含 `task_id`）
  - ❌ `TestWeCom_Send_RetryableError`（500 → error）
  - ❌ `TestDeduper_FirstFireReturnsTrue` / `TestDeduper_DedupWithinTTL`

#### **G1 · probe runner + 滑动窗口 + 规则评估**（5-10 分钟）
- **步骤**：§11.11 Runner；Run 步骤 1-6；slidingWindow + SuccessRate；evaluateAlerts 4 条规则 + 去抖 + notify.Enabled
- **测试**：
  - ❌ `TestRunner_TTFTItChDuration`（mock 3 chunk 全调 Observe*）
  - ❌ `TestRunner_UnauthorizedImmediate`
  - ❌ `TestRunner_ConsecutiveFailures`（连续 3 + 第 4 次 5min 内抑制）
  - ❌ `TestRunner_SlowRequest` / `TestRunner_SlidingWindow_LowSuccessRate`
  - ❌ `TestRunner_NotifyDisabled`

#### **G2 · HTTP 服务**（5 分钟）
- **步骤**：3 路由 + 优雅 Shutdown（5s 超时）
- **测试**：
  - ❌ `TestHTTP_Metrics`（含 `ai_probe_request_duration_seconds`）
  - ❌ `TestHTTP_Healthz_Ok` / `TestHTTP_Healthz_DBDown`
  - ❌ `TestHTTP_AdminTasks`

#### **H1 · 端到端集成**（5-10 分钟）
- **步骤**：启子进程 + httptest mock + 1 周期断言 `/metrics`
- **测试**：
  - ❌ `TestE2E_ProbeFiresAndMetricsExposed`（`-tags=integration`）
  - ❌ `TestE2E_AlertSent`（连续失败 3 次 → wecom mock 收到 POST）

#### **I1 · 收尾**（5 分钟）
- **步骤**：README + `example.config.yaml` + `prometheus-alerts.yaml`（5 条规则样例）+ `Dockerfile`（多阶段 CGO_ENABLED=0 + scratch）
- **测试**：
  - ❌ `TestDockerfile_Builds` / `TestPromAlerts_YAMLLint`

#### **P1-A · 热加载**（5 分钟）
- **步骤**：Watch goroutine → diff(old,new) → cron.AddFunc/Remove
- **测试**：`TestReload_AddsNewTask` / `TestReload_RemovesDeletedTask`

#### **P1-B · 协议校验 + 结构化日志 + 优雅关闭**（5 分钟）
- **步骤**：slog task_id 注入；server.Shutdown 等 in-flight；main 优雅退出；协议段 Validate 互斥强制
- **测试**：`TestLogging_TaskIDInContext` / `TestGracefulShutdown_StopsWithinTimeout` / `TestValidate_MutuallyExclusiveProtocols`

#### **P1-C · 协议级 ExtraHeaders 收口（灰度流量测试）**（5 分钟）
- **目的**：让 OpenAI / Anthropic / Gemini 三种协议全部支持自定义请求头，支撑**灰度（Canary）流量测试**场景
- **文件**：`internal/config/task.go`（扩展 AnthropicConfig / GeminiConfig 字段）、`internal/protocol/openai/openai.go`（注入 ExtraHeaders）、`internal/protocol/anthropic/anthropic.go`（同）、`internal/protocol/gemini/gemini.go`（同）、`deploy/canary-scenarios.yaml`（示例）、各适配器测试
- **步骤**：
  1. 在三协议的 config 结构体补 `ExtraHeadersJSON`（OpenAI 已有，Anthropic / Gemini 新增）
  2. `AdapterDeps` 增加 `ExtraHeaders map[string]string` 注入（启动时由 Runner 从 cfg 解码 JSON 后传入）
  3. 三协议 Probe 实现：在 `http.NewRequestWithContext` 之后遍历 ExtraHeaders 调用 `req.Header.Set`
  4. Header 注入顺序（防被覆盖）：先 `Authorization` 等鉴权头 → 再 `User-Agent` → 最后用户 ExtraHeaders（允许覆盖）
  5. 在 `deploy/canary-scenarios.yaml` 写入典型示例（同域名灰度 / 跨实例对比 / 租户级）
- **测试**（TDD）：
  - ❌ `TestOpenAI_Probe_SendsExtraHeaders`：httptest.Server 断言收到 `X-Canary: gray`
  - ❌ `TestOpenAI_Probe_AuthHeaderNotOverridden`：ExtraHeaders 含 `Authorization` 时与 apikey 冲突报错（避免凭据泄漏）
  - ❌ `TestAnthropic_Probe_SendsExtraHeaders`
  - ❌ `TestGemini_Probe_SendsExtraHeaders`
  - ❌ `TestExtraHeaders_BlackList`：拒绝对 `Host` / `Content-Length` 等系统头设置
- **依赖**：C2, C3, C4

#### **P1-D · Histogram 分桶拆分 + scheduler_lag 升 Histogram + canary_group label**（5 分钟）
- **目的**：让 P50/P90/P95/P99 趋势图的精度满足生产要求（解决默认值下 duration P99 边界饱和、TTFT 桶太粗、scheduler_lag 不可分位数）
- **文件**：`internal/metrics/prom.go`（每指标独立 buckets）、`internal/config/task.go`（TaskConfig 加 `CanaryGroup`）、Prometheus 注册表改造、各 label 维度同步测试
- **步骤**：
  1. `Config` 加 `BucketsTTFT/BucketsITL/BucketsDuration/BucketsLag` 4 个 `[]float64`，默认按 §11.6.1
  2. `PromSink` 构造时按指标分别传入各自 buckets（不再共用）
  3. `scheduler_lag_seconds` 升级为 HistogramVec
  4. 所有 HistogramVec/CounterVec/GaugeVec 的 Labels 增加 `canary_group`，默认 `"default"`
  5. `Sink.Observe*`/`Inc*`/`Set*` 入参加 canary_group 透传
- **测试**（TDD）：
  - ❌ `TestPromSink_DurationP99At60s`：构造样本 100% 在 60s，断言 histogram_quantile(0.99) ≈ 60 不饱和
  - ❌ `TestPromSink_TTFTBuckets`：断言 bucket 顺序与 §11.6.1 一致
  - ❌ `TestPromSink_CanaryGroupLabel`：canary_group=gray 时分裂为独立 series
  - ❌ `TestPromSink_SchedulerLagHistogram`：断言 PromSink 注册了 lag 的 HistogramVec
- **依赖**：D1
- **配套产出**：`deploy/grafana-llm-dashboard.json`（已带 P50/P90/P95/P99 趋势面板）+ `deploy/llm-promql-examples.md`（已提供全部 PromQL 模板）

---

## 16. 跨文件共享约定

| 类别 | 约定 |
|---|---|
| **错误处理** | sentinel error 集中在包顶部；一律 `fmt.Errorf("op ctx: %w", err)`；用 `errors.Is/As`；禁字符串比较 |
| **日志** | `log/slog` + JSON；字段固定 `task_id / task_name / protocol / model / status / duration_ms / chunk_count / ttft_ms / itl_ms / error`；ctx 注入 task_id |
| **上下文** | ctx 必须携带 task_id（自定义 key 类型防冲突）；丢时在 runner 入口 upsert；所有 IO 必须接收 ctx |
| **配置注入** | `default < config.yaml < env (AI_MONITOR_*) < flag`；flag 仅启动期；DB 任务配置走热加载 |
| **时区** | DB 写 UTC；cron 解析用本地时区（`TZ` env 控制）；日志 RFC3339Nano + 时区标识 |
| **命名** | 包名小写无下划线；接口 `er / Store / Adapter` 结尾；文件小写下划线；变量驼峰；常量全大写下划线分词 |
| **并发** | 共享状态用 `sync.RWMutex` 或 `atomic`；测试一律 `-race` |
| **加密** | master key 仅 env / flag，绝不落盘 / 进日志；含 api_key 的错误日志 redact；启动后清零内存变量 |
| **版本** | 依赖锁 minor 不锁 patch；go 1.25 toolchain 锁定 |
| **Makefile 钩子** | lint = `go vet` + `golint` + `depguard`（强制 import 方向） |

---

## 17. 待明确事项 + 风险

### 17.1 待用户拍板（≥5）

1. **Web UI 任务管理**（⚠️ **新增项，用户问题触发的 P2-A 落地评估**）：是否落地？落地要补 3 个后端路由（POST / PATCH / DELETE `/admin/tasks`，apikey/webhook 在后端做 AES-GCM 加密后入库），约 30 分钟工作量；是否同时把 `deploy/admin-ui.html` 用 `//go:embed` 嵌入二进制，新增 `GET /admin/` 路由（让 UI 与 API 同源，跨域更顺）？否则只能 nginx/caddy 反代或跨域。
2. **灰度（Canary）流量测试支持范围**（⚠️ **新增项，用户原始需求触发的设计补丁**）：是否启用 P1-C「协议级 ExtraHeaders 收口」？主要用途是同域名 Header 触发网关路由、跨实例 A/B 对比、租户级灰度，还是别的场景？是否需要在 metrics label 上新增 `canary_group` 维度方便对比？
2. **Master Key 来源**：env `AI_MONITOR_MASTER_KEY` 还是 K8s Secret 挂载？是否需要密钥轮转？
3. **HTTP 服务端口与绑定**：默认 `:9090` 是否绑 `0.0.0.0`？`/admin` 是否需要 basic auth / token？
4. **SQLite 驱动**：是否接受 pure-Go `glebarez/sqlite`？是否需 CGO 版追求极致写入？
5. **DB 加密字段**：仅加密 api_key + webhook？还是扩到 user_prompt（防提示词泄漏）？
6. **告警扩展性**：预留 Slack / Email / generic webhook（当前仅 MultiNotifier 骨架）？
7. **任务并发**：默认 16 是否需要按 task 单独限流？
8. **历史结果**：是否需要本地落盘最近 N 次用于事后排查？
9. **部署形态**：Docker 镜像 + systemd unit 示例是否一起发布？
10. **TLS**：HTTP 客户端到 AI 上游是否需要 mTLS / 自签证书跳过？
11. **协议扩展**：Azure / Bedrock / 国产模型是否预留可插拔 HTTP 模板层？

### 17.2 技术风险（≥5）

| 风险 | 兜底 |
|---|---|
| SQLite 写并发瓶颈 | WAL + busy_timeout；不行则串行化写入队列 + 单 goroutine 提交 |
| 流式响应阻塞 | Chunks chan buffered 256；超过即丢弃（chunk_count 仍计） |
| Cron 雪崩 | jitter 0~250ms；撞信号量 fail-fast + scheduler_lag 上报 |
| 企微 webhook 失败 | 当前不重试；P1 落 `failed_alerts.db` + 日志告警 |
| apikey 泄漏 | master key 启动后立即清零内存变量 |
| Prom label cardinality | 启动校验 task_id 总量，超阈值 fail-fast |
| scheduler_lag 漂移 | lag 用 monotonic clock；上报换 wall clock |
| 优雅停机超时 | 默认 5s；超时后强制退出 + 落未完成任务日志 |
| HTTP/2 上游 | transport 强制配置 h2 |
| 热加载一致 | Reload 用 mutex 串行化 + 内部 version 号防 race |

---

## 18. 增量 · 前端 + Auth/RBAC（用户要求：登录页 + admin 增删 / viewer 只读）

> 状态：架构已敲定，工程师一次性实现 T01–T05
> 本章为对已有 PRD + 架构 §9–§17 的**追加**；不复述既有结论

### 18.1 角色与边界

| 角色 | 权限 | 入口 |
|---|---|---|
| `admin` | 任务 CRUD + 用户管理 + 改自己密码 | 全部路径 |
| `viewer` | GET /admin/tasks、GET /metrics、看仪表盘、看 profile | 仅 read-only |

关系：**viewer ⊂ admin**。防御纵深：前端隐藏 + 后端强制 `auth.Middleware.RequireRole(admin)`。

### 18.2 端点（新增与调整）

| Method | Path | 鉴权 | 角色 |
|---|---|---|---|
| POST | `/auth/login` | 否 | — |
| POST | `/auth/logout` | 是 | 任意 |
| GET | `/auth/me` | 是 | 任意 |
| POST | `/auth/change-password` | 是 | 任意 |
| GET / POST / PATCH / DELETE | `/admin/users[/{id}]` | 是 | admin |
| POST | `/auth/users/seed` | **启动期内部** | — |
| GET | `/admin/tasks/{id}` | 是 | 任意 |
| POST / PATCH / DELETE | `/admin/tasks[/{id}]` | 是 | admin |
| GET / POST | 其余保持设计 §11.12 | — | — |

### 18.3 用户表（新增）

```sql
CREATE TABLE users (
    id            VARCHAR(64)  PRIMARY KEY,
    username      VARCHAR(64)  NOT NULL UNIQUE,
    password_hash VARCHAR(255) NOT NULL,        -- bcrypt cost=12
    role          VARCHAR(16)  NOT NULL,
    display_name  VARCHAR(128),
    enabled       BOOLEAN      NOT NULL DEFAULT 1,
    created_at    DATETIME,
    updated_at    DATETIME
);
```

`tasks` 表 schema 不变。

### 18.4 视觉定调（frontend-design skill 已锁）

- 目的：SRE / 运维后台；登录、管理 AI 探测任务、查看实时指标
- 调性：**Industrial**——等宽数字、状态色体系、发丝边框、高密度
- 字体：JetBrains Mono（数字/展示）+ IBM Plex Sans（正文）；**禁 Inter/Roboto**
- 配色（暗色基调）：bg `#0e0d0c` / surface `#171614` / text `#f5f1ea` / accent **琥珀 `#ff6b2c`**；状态色 success/warn/danger 分别 `#14a06d` / `#d6a514` / `#d8443f`
- 构成：280px 侧栏 + 56px 顶栏 + 12-grid；表格 + 行 hover 信息抽屉
- 记忆点：登录页背景 = 实时 latency 折线心跳；仪表盘顶部 = P95 呼吸灯随健康度变色

### 18.5 完整 tokens.css 与组件骨架

完整 tokens.css（字面复制到 `web/src/styles/tokens.css`）：

```css
:root {
  --bg: #0e0d0c; --surface: #171614; --surface-2: #211e1b; --surface-3: #2b2723;
  --text: #f5f1ea; --muted: #a09a90; --muted-2: #6e6962;
  --border: rgba(245,241,234,.12); --border-strong: rgba(245,241,234,.22);
  --accent: #ff6b2c; --accent-2: #ff8a5b; --accent-soft: rgba(255,107,44,.16);
  --ok: #14a06d; --warn: #d6a514; --danger: #d8443f; --info: #5b8def;
  --font-display: "JetBrains Mono", "SF Mono", ui-monospace, Menlo, Consolas, monospace;
  --font-body: "IBM Plex Sans", "PingFang SC", -apple-system, "Helvetica Neue", sans-serif;
  --font-mono: "JetBrains Mono", "SF Mono", ui-monospace, monospace;
  --fs-11: 11px; --fs-12: 12px; --fs-13: 13px; --fs-14: 14px;
  --fs-16: 16px; --fs-20: 20px; --fs-24: 24px; --fs-32: 32px;
  --fs-48: 48px; --fs-72: 72px;
  --fs-display: clamp(48px, 6vw, 72px);
  --lh-tight: 1.15; --lh-base: 1.5; --lh-loose: 1.7;
  --fw-regular: 400; --fw-medium: 500; --fw-semibold: 600; --fw-bold: 700;
  --space-1: 4px; --space-2: 8px; --space-3: 12px; --space-4: 16px;
  --space-5: 24px; --space-6: 32px; --space-7: 48px; --space-8: 64px;
  --radius-sm: 4px; --radius-md: 8px; --radius-lg: 14px; --radius-pill: 999px;
  --shadow-1: 0 2px 6px rgba(0,0,0,.32);
  --shadow-2: 0 12px 32px rgba(0,0,0,.48);
  --shadow-glow: 0 0 0 1px var(--accent), 0 0 24px rgba(255,107,44,.25);
  --ease-out: cubic-bezier(.16, 1, .3, 1);
  --ease-spring: cubic-bezier(.34, 1.56, .64, 1);
  --dur-fast: 160ms; --dur-base: 320ms; --dur-slow: 640ms;
  --sidebar-w: 280px; --topbar-h: 56px; --container: 1200px; --gutter: 16px;
  font-variant-numeric: tabular-nums;
}
```

详见架构产出的 §1.3.2（含 `[data-theme="light"]` 与 base.css / animations.css 摘要）。

### 18.6 嵌入到二进制的方案

- 前端产物 `web/dist/` 通过 `//go:embed` 进 `internal/webui/embed.go`
- `internal/server/static.go` 提供 SPA fallback：API 路由优先命中，其它路径 → `index.html`
- `mux.Handle("GET /", staticHandler)` + `mux.Handle("GET /assets/", staticHandler)`
- 单二进制交付；零 CORS；零跨域

### 18.7 任务清单（5 个集成任务 · 工程师一次性实现）

| ID | 标题 | 涉及主要文件 |
|---|---|---|
| T01 | 项目基础设施（依赖 + embed 骨架 + Vite 起步） | go.mod、internal/webui/embed.go、cmd/main.go、Web/package.json 等 |
| T02 | 后端 auth + RBAC（users 表 / JWT / bcrypt / 中间件 / seed） | internal/auth/*、main.go |
| T03 | 后端 handlers（auth + admin user + admin task CRUD）+ SPA fallback | internal/server/* |
| T04 | 前端骨架 + 视图（tokens.css / 9 视图 / RBAC 双层防御） | web/src/* |
| T05 | 特色组件（LatencyHeartbeat + BreathingLight）+ 集成 + e2e + 收尾 | web/src/components/fx/*、e2e、README、design.md §18 |

完整步骤与失败测试用例集合见架构 §1.6（每个 T 内含 5–10 步与 4–10 个 TDD 失败用例）。

### 18.8 端到端 RBAC 矩阵（必含）

| Route × Method | no-token | viewer | admin |
|---|---|---|---|
| POST `/auth/login` | 200 | 200 | 200 |
| GET `/auth/me` | 401 | 200 | 200 |
| GET `/admin/tasks[/id]` | 401 | 200 | 200 |
| POST/PATCH/DELETE `/admin/tasks[/id]` | 401 | **403** | 200 |
| GET/POST/PATCH/DELETE `/admin/users[/id]` | 401 | **403** | 200 |
| GET `/metrics` | 200 | 200 | 200 |
| GET `/healthz` | 200 | 200 | 200 |
| GET `/` | 200 (SPA) | 200 | 200 |

### 18.9 环境变量（新增）

| 变量 | 默认 | 说明 |
|---|---|---|
| `AI_MONITOR_JWT_SECRET` | **必填** ≥ 32 字节 | JWT HS256 密钥；未设 → 启动失败 |
| `AI_MONITOR_ADMIN_USERNAME` | `admin` | 启动期 seed 用户名 |
| `AI_MONITOR_ADMIN_PASSWORD` | 强随机 + WARN 一次性 | 启动期 seed 密码 |

`AI_MONITOR_MASTER_KEY` / `AI_MONITOR_HTTP_ADDR` / `AI_MONITOR_SQLITE_PATH` 沿用既有。

### 18.10 反 AI 味自检清单（frontend-design skill §7 对齐）

- 展示字避开 Inter/Roboto/Arial/system-ui → 强制 JetBrains Mono + IBM Plex Sans
- 强调色单焦点（琥珀）→ 状态色独立体系
- 非对称 + 编辑密度 → 280 侧栏 + 56 顶栏 + 12-grid
- 编排动效有意义 → LatencyHeartbeat 与 BreathingLight 直接对应产品语境
- 氛围层（contextual）→ 心跳折线 = 监控系统的"监控"主题
- 8 种状态全覆盖 → loading/empty/error/success/warning/info/disabled/focused
- 视觉唯一性 → 不引 chart.js / echarts；8 metric 卡全部手写 + tabular-nums
- 降级动效 → `prefers-reduced-motion: reduce` 全局关

CI 强制：`grep -RE '(Inter|Roboto|Arial|tailwind|ant-design|element-plus)' web/src` 必须空。

---

### 18.11 增量 · 探测结果落库（`probe_runs` + `RunStore`）

> **触发场景**：指标 Histogram 能撑出 P50/P90/P99 趋势，但「单点失败」「上游 5xx 风暴」「模型撤回」这类**离散事件**会被 average 抹平；`ai_probe_request_total{status="failure"}` 只给累计数，admin 想看「5 分钟前哪一条 404 因为什么」必须落 DB。

#### 18.11.1 目标

- 每次探测写一行 `probe_runs`（status / err / TTFT / ITL / chunks / 时间戳）
- 新增 `GET /admin/tasks/{id}/runs?limit=20` 让 admin 在 UI 上点任务 → 看历史
- 保留最近 N 条/任务（默认 100），老数据自动 Prune，避免 SQLite 无限膨胀
- 不破坏现有 P0：新增独立包 + 独立端点 + 独立表，老的 metric / notifier / scheduler 路径零改动

#### 18.11.2 关键设计决策

| 决策 | 选 A | 选 B | 选 A 的理由 |
|---|---|---|---|
| 落谁 | **Runner 自己 defer 落** | 中间件统一 hook | defer 拿得到最终 status（success / failure / unauthorized），middleware 只能拿 HTTP-level |
| 接口位置 | **新包 `internal/probe/store.go`** | 塞 `internal/config/store.go` | `config` 包管的是「任务配置」，不掺「执行结果」；新包职责单一 |
| DB 连接 | **共享 `config.SqliteStore.DB()`** | probe 包自己开 | WAL / lock 不冲突，少一个 file handle；AutoMigrate 一次跑完两张表 |
| Prune 策略 | **per-task 保留最新 N 条（window function）** | 定期全部清 | 「30s schedule × 100 ≈ 50 分钟窗口」对调试够用，且每任务都有最近窗口 |
| 写失败处理 | **log warn，不影响 cron 下一次 tick** | panic / retry | cron 调度优先于审计；落库是 best-effort |
| 反范式 `task_name` | **写进 `probe_runs`** | join `tasks` | 任务改名/删除后历史仍可读；join 多一次 IO |
| Err 长度 | **截到 4096 字符** | 全文存 | 网络错误可能含几 KB HTML body，截断避免 text 列爆炸 |
| 写时延保护 | **`context.WithTimeout(2s)`** | 无超时 | Save 卡住会让后续探测累积；2s 兜底 |

#### 18.11.3 数据模型与代码

```go
// internal/probe/store.go
type RunStore interface {
    Save(ctx context.Context, run *ProbeRun) error
    ListByTask(ctx context.Context, taskID string, limit int) ([]ProbeRun, error)
    Prune(ctx context.Context, keep int) error
    Close() error
}

type ProbeRun struct {
    ID         int64     `gorm:"primaryKey;autoIncrement" json:"id"`
    TaskID     string    `gorm:"size:64;not null;index"        json:"task_id"`
    TaskName   string    `gorm:"size:128;not null"             json:"task_name"`
    Protocol   string    `gorm:"size:32;not null"              json:"protocol"`
    Model      string    `gorm:"size:128;not null"             json:"model"`
    Status     string    `gorm:"size:32;not null"              json:"status"`
    Err        string    `gorm:"type:text"                      json:"error,omitempty"`
    StartedAt  time.Time `gorm:"not null;index"                 json:"started_at"`
    EndedAt    time.Time `gorm:"not null"                       json:"ended_at"`
    DurationMS int64     `gorm:"not null"                       json:"duration_ms"`
    TTFTMS     int64                                          json:"ttft_ms"`
    ITLAvgMS   int64                                          json:"itl_avg_ms"`
    ITLCount   int                                            json:"itl_count"`
    Chunks     int      `gorm:"not null"                       json:"chunks"`
}
```

**Prune SQL**（一次 DELETE 扫所有任务，per-task 保留最新 N）：

```sql
DELETE FROM probe_runs
WHERE id NOT IN (
  SELECT id FROM (
    SELECT id, ROW_NUMBER() OVER (
      PARTITION BY task_id ORDER BY started_at DESC, id DESC
    ) AS rn
    FROM probe_runs
  ) WHERE rn <= :keep
)
```

#### 18.11.4 Runner 接线（**defer 传指针的关键修复**）

```go
func (r *Runner) Run(ctx context.Context, t config.TaskConfig) Result {
    res := Result{...}
    // 关键：defer f(args) 按值求值。传 res 值 → deferred 拿到零值 Result，
    // 全部 started_at=0001-01-01、duration_ms=0；传 &res → 看到最终字段。
    defer r.persistRun(&res)
    // ... 探测逻辑 ...
    return res
}

func (r *Runner) persistRun(res *Result) {
    if r.RunStore == nil { return }            // 防误关：nil RunStore 不阻塞 cron
    run := FromResult(*res)
    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
    defer cancel()
    if err := r.RunStore.Save(ctx, &run); err != nil {
        log.Warn("probe_run save failed", "err", err)   // best-effort
    }
    _ = r.RunStore.Prune(ctx, r.RunRetention)
}
```

#### 18.11.5 新端点

```
GET /admin/tasks/{id}/runs?limit=20
→ 200 {"runs": [ProbeRun, ProbeRun, ...]}    # 新→旧
→ 400 bad_limit                             # limit 非正整数 / 超 100
→ 404 task_not_found                         # id 不存在
→ 503 run_store_disabled                     # RunStore=nil（防误关）
```

`limit` 默认 20、上限 100。RBAC 同其它 GET：viewer / admin 都可读。

#### 18.11.6 接线图

```
cmd/ai-monitor/main.go
 ├─ config.NewSqliteStore(...)  ──┐
 ├─ probe.NewSqliteRunStore(store.DB()) ──┐  // 共享 *gorm.DB
 ├─ runner.RunStore  = runStore            │
 └─ server.Options.RunStore = runStore      │
                                          │
internal/probe/runner.go                  │
 └─ defer r.persistRun(&res)               │
     ├─ RunStore.Save(ctx, run)            │
     └─ RunStore.Prune(ctx, keep=100)      │
                                          │
internal/server/admin_task_handlers.go    │
 └─ mux.Handle("GET /admin/tasks/{id}/runs", listRunsHandler)
```

#### 18.11.7 验收（实施时勾）

- [ ] 创建任务 A → 立刻看到 1 条 run
- [ ] 等 30s → 看到第 2 条
- [ ] `GET /admin/tasks/{id}/runs?limit=10` 返回新→旧数组
- [ ] 手工改 keep=5，等一轮 Prune → 历史裁到 5 条
- [ ] 故意填超长 err（>4 KB）→ DB 里只存 4096 字符
- [ ] Save 抛错（关掉 DB 文件）→ cron 下一次 tick 不受影响
- [ ] defer 拿到的 started_at 等于实际 run 开始时间（不是零值）
- [ ] RBAC：viewer 也能看 runs

#### 18.11.8 不在范围内（留 P2）

- 按时间段过滤（`?from=&to=`）
- 按 status 过滤（`?status=failure`）
- 按 error 模糊搜索（`?err_like=timeout`）
- CSV 导出
- 单独 `GET /admin/runs` 全局最近 N 条（不分任务）

---

### 18.12 增量 · 监测任务启动 / 停止（默认不自动开启）

> **触发场景**：上一版 POST 出来的任务立刻被 cron 拉起来，导致「刚配、参数还没填好、误触发」三种姿势接连发生。任务应该「先建好再启」，start / stop 是显式动作。

#### 18.12.1 目标

- 新建任务默认 `Enabled=false`（POST 不传 `enabled` 字段时）
- 提供 `POST /admin/tasks/{id}/start` 与 `POST /admin/tasks/{id}/stop` 两个专用端点
- start / stop 同时改 SQLite `enabled` 字段 + 调度器的 cron 注册；两边保持一致
- 已 enabled 的任务再 `start` 或已 disabled 的任务再 `stop`：幂等返回 200，不重复操作
- 旧有存量的 `enabled=true` 任务保留原状（不写 migration），由管理员按需手动 `stop`

#### 18.12.2 关键设计决策

| 决策 | 选项 | 选择 | 理由 |
|---|---|---|---|
| 新建任务默认 `enabled` | `true`（旧）/ `false`（新） | **false** | 「先建好再启」减少误触；显式 start 是单一开关 |
| 默认值影响范围 | 仅新建 / 含存量 migration | **仅新建** | 存量任务保留原状，admin 主动 stop；不偷偷改变行为 |
| start / stop 接口形态 | 复用 PATCH / 专用端点 / 二者皆有 | **专用端点** | 语义清晰；admin 一次只切一个动作；不与 PATCH 的「整体配置更新」混用 |
| scheduler `Add` / `Reload` 是否读 `enabled` | 否（旧）/ 是（新） | **是（纵深防御）** | 任何绕开 HTTP 的路径（reload、watcher）都不会让 disabled 任务被调度 |
| 写顺序 | 先 DB 后 sched / 先 sched 后 DB | **start: DB→sched；stop: sched→DB** | start 失败能「回滚 enabled=false」；stop 失败 cron 已停不会幽灵 fire |
| RBAC | viewer / admin | **admin** | §18.8 矩阵：写类端点一律 admin |

#### 18.12.3 数据流（start 路径）

```
admin → POST /admin/tasks/{id}/start
        │
        ▼
 AdminTaskHandlers.start()
        │
   ┌────┴─────────────────────────────────┐
   ▼                                      │
 Store.GetTask(id)            ←─ 同 §18 已有   │
   │ existing.Enabled = false ?            │
   ├─ 是 ──→ Store.UpdateTask(existing{Enabled:true})
   │            │                          │
   │            ├─ 失败 → 500 persist_failed
   │            ▼
   │        Scheduler.Add(*existing)
   │            │  (scheduler.Add 守卫：若 Enabled=false 返回 nil)
   │            ├─ 失败 → Store.UpdateTask 回滚 enabled=false → 500 schedule_failed
   │            ▼
   │        respondStoredTask(200)
   │
   └─ 否（已 enabled）→ 直接返回 200（幂等）
```

stop 路径镜像上面的流程，但次序反转：先 `Scheduler.Remove(id)`（idempotent），再 `Store.UpdateTask(... enabled:false)`。

#### 18.12.4 关键代码（节选）

**`internal/scheduler/scheduler.go`：**

```go
// Add registers a single task. Disabled tasks are silently skipped so the
// scheduler's effective schedule always equals "the subset of tasks whose
// Enabled flag is true at the moment of call".
func (s *cronScheduler) Add(task config.TaskConfig) error {
    if !task.Enabled {
        return s.Remove(task.ID) // retract any lingering cron entry
    }
    if _, ok := s.entries[task.ID]; ok {
        return s.Remove(task.ID)
    }
    id, err := s.cron.AddFunc(task.Schedule, func() { s.fire(task) })
    if err != nil {
        return fmt.Errorf("scheduler: invalid cron expression %q: %w", task.Schedule, err)
    }
    s.mu.Lock()
    s.entries[task.ID] = id
    s.mu.Unlock()
    return nil
}
```

`Reload()` 同样：不读 `enabled` 的任务不进 `entries`，已经是 enabled 后被改 disabled 的会先 `Remove` 再跳过 Add。

**`internal/server/admin_task_handlers.go`：**

```go
func (h *AdminTaskHandlers) start(w http.ResponseWriter, r *http.Request) {
    if h.cfg.Store == nil || h.cfg.Scheduler == nil { /* 503 */ }
    id := r.PathValue("id")
    existing, err := h.cfg.Store.GetTask(r.Context(), id)
    if err != nil { /* 404 task_not_found */ }
    if existing.Enabled { /* idempotent 200 */ }
    existing.Enabled = true
    if err := h.cfg.Store.UpdateTask(...); err != nil { /* 500 start_persist_failed */ }
    if err := h.cfg.Scheduler.Add(*existing); err != nil {
        existing.Enabled = false
        _ = h.cfg.Store.UpdateTask(...)  // 回滚 DB
        /* 500 start_schedule_failed */
    }
    respondStoredTask(w, 200, existing)
}
```

`stop` 函数镜像：`Scheduler.Remove(id)` 先；DB 后；幂等。

#### 18.12.5 新端点 / 默认值

| 项 | 旧 | 新 |
|---|---|---|
| `POST /admin/tasks` 默认 `enabled` | `true` | **`false`** |
| `POST /admin/tasks/{id}/start` | — | **新增** |
| `POST /admin/tasks/{id}/stop` | — | **新增** |

两个新端点的 RBAC：同 `POST/PATCH/DELETE /admin/tasks` —— **401 无 token / 403 viewer / 200 admin**（§18.8）。

响应统一为 `taskResponse`（与 GET / POST / PATCH 同 shape），secret 字段按 `redactTask` 规则清除。

#### 18.12.6 接线图（main → server）

```
cmd/ai-monitor/main.go
├── store (config.SqliteStore) ────┐
│                                  ├──► server.Options
├── scheduler (cronScheduler) ──────┤        │
├── runStore (probe.SqliteRunStore) ┤        ├──► NewAdminTaskHandlers
└── auth.Middleware + UserStore  ───┘        │       │
                                              ▼       │
                                       AdminTaskHandlersConfig{
                                         Store:     …,
                                         Scheduler: …,   ← 新增
                                         RunStore:  …,
                                         Auth:      …,
                                         …
                                       }
                                              │
                                              ▼
                                       Mux:  POST /admin/tasks/{id}/start  (admin)
                                       Mux:  POST /admin/tasks/{id}/stop   (admin)
                                       Mux:  POST /admin/tasks               (admin, 现在默认 enabled=false)
```

#### 18.12.7 验收（实施时勾）

- [ ] POST 不带 `enabled` 的 task → DB 行 `enabled=0`，cron 没注册
- [ ] 立即 `curl /admin/tasks/{id}/runs` → 空数组（不 fire）
- [ ] POST `/admin/tasks/{id}/start` → 200，response `enabled=true`
- [ ] cron 立即按 schedule fire（用一个 `@every 1m` 等待 60s 也行，或人工 `POST /admin/tasks/{id}/run-now` P2 不做，靠时间）
- [ ] 再 POST `/start` → 仍 200，DB / scheduler 都无副作用（idempotent）
- [ ] POST `/admin/tasks/{id}/stop` → 200，response `enabled=false`，cron 立即停
- [ ] start 期间 scheduler.Add 抛错 → 响应 500 + `start_schedule_failed`；DB `enabled` 被回滚为 false
- [ ] stop 期间 store.UpdateTask 抛错 → 响应 500；cron 已停，下一次 reload 才会恢复
- [ ] viewer 调用 `/start` 或 `/stop` → 403（§18.8 RBAC）
- [ ] 旧存量任务 `enabled=true` 启动后被 `sched.Reload(tasks)` 加载 → 仍按原 schedule fire（不被新默认值影响）
- [ ] 单元测试：`TestScheduler_Add_DisabledIsNoOp` / `TestScheduler_Reload_DisabledSkipped` / `TestScheduler_Reload_DisablingActiveRetracts` 全 PASS
- [ ] 单元测试：`TestStartTask_*` × 5 + `TestStopTask_*` × 3（8 个 handler 测试）全 PASS

#### 18.12.8 不在范围内（留 P2）

- POST `/admin/tasks/{id}/run-now`（立即触发一次 probe，不依赖 cron）
- 批量 start / stop：`POST /admin/tasks/batch/start` `{"ids":[...]}`
- 任务调度健康度面板（已 start 但调度器未 fire 的告警）
- 占用磁盘 / 调度器 entry 上限的告警（P1 `task_count` fail-fast 已有了）

---

### 18.13 增量 · 监控记录增加请求内容 / 返回内容 / x-req-id

> **触发场景**：每条 probe 失败时，admin 想知道「究竟是什么请求发出去的，服务器究竟回了什么字节」。原有 `probe_runs` 只留 `Err` 摘要（首 4 KiB），无法定位 SSE 中段 / OpenAI 截断响应 / Anthropic 异常的尾部 frame。

#### 18.13.1 目标

- `probe_runs` 新增 5 个字段：`request_id`、`request_body`、`response_head`、`response_tail`、两个 `*_truncated` 标志位
- 数据来源全复用：协议 adapter 已在 `internal/protocol/adapter.go:81` 自动注入 `X-Req-Id: <uuid v4>` 到 outbound header —— 这部分信号之前被丢弃，本次接住
- 请求 body 用 32 KiB 硬上限；响应截 HAR 风格「head + tail」各 4 KiB，丢弃中段
- `GET /admin/tasks/{id}/runs` 自动带新字段（无 schema 变更 break）

#### 18.13.2 关键设计决策

| 决策 | 选项 | 选择 | 理由 |
|---|---|---|---|
| 响应 body 抓策略 | 全部 / 仅首 N / 首 N + 尾 N（HAR） | **HAR：head 4 KiB + tail 4 KiB + truncated 标记** | 调试能看首消息 + 错误 + 结束标志；disk 可控；与 /admin/tasks/{id}/runs limit 配合 |
| 请求 body 上限 | 64 KiB / 32 KiB / 不限 | **32 KiB（截断 + truncated 标志位）** | OpenAI / Anthropic 实际请求体常 2-8 KiB；32 KiB 覆盖长 system prompt + 多轮示例；超出时前端可显示提示 |
| X-Req-Id 字段名 / 大小 | VARCHAR(64) / VARCHAR(128) | **VARCHAR(128)，加索引** | UUID v4 = 36 字符；用户可注入长 trace id（阿里云 / 字节链式 trace），128 是覆盖上行 |
| API 形状变化 | 增字段 / 拆子端点 | **增字段（admin-only 取数无 RBAC 变化）** | viewer 看 runs 也是只读，加字段无破坏 |
| 旧 row 兼容 | 加列 NULL / 加列默认值 | **NULL / `false`** | GORM AutoMigrate 自动加列 + `gorm:"not null;default:false"` |
| capture 位置 | adapter 内 / runner 内 | **adapter 内 `ProbeStream` 暴露，runner 汇总** | adapter 知道 protocol 特定的字节拼装；runner 只做"读 3 个方法 + 1 个截断判定" |

#### 18.13.3 ProbeStream 扩展 + capture 路径

`internal/protocol/adapter.go` 的 `ProbeStream` interface 加 3 个方法：

```go
type ProbeStream interface {
    Chunks() <-chan Chunk
    Err() error
    Close() error
    // §18.13 wire capture
    RequestID() string                      // X-Req-Id used on outbound request
    RequestBody() []byte                    // raw JSON body POSTed to upstream
    ResponseSnapshot() (head, tail []byte, truncated bool)
}
```

每个 adapter (`openai.go` / `anthropic.go` / `gemini.go`) 在 `stream` struct 上加私有字段，在 `Probe()` 里：

1. `protocol.TruncateRequest(body, 32 KiB)` 拿到截断的 body —— 同时 set `stream.requestBody`
2. `protocol.GenerateRequestID(req, a.rid)` 拿到返回值（之前丢弃）—— set `stream.requestID`
3. `protocol.NewRespCapture(4 KiB, 4 KiB)` 拿到 capture —— 用 `io.TeeReader(resp.Body, capture)` 喂给 scanner/reader

`consume()` 跑完关闭 channel 后，runner 从 stream 读取 3 个 accessor 写入 `Result`，并按 `len(body) ≥ 32 KiB` 标 `RequestTruncated`。

#### 18.13.4 数据模型与代码

**`internal/probe/store.go：ProbeRun`**：

```go
type ProbeRun struct {
    // ... 原有 14 列 ...
    // §18.13 新增 5 列
    RequestID        string `gorm:"size:128;index" json:"request_id,omitempty"`
    RequestBody      string `gorm:"type:text" json:"request_body,omitempty"`
    ResponseHead     string `gorm:"type:text" json:"response_head,omitempty"`
    ResponseTail     string `gorm:"type:text" json:"response_tail,omitempty"`
    RequestTruncated bool   `gorm:"not null;default:false" json:"request_truncated"`
    ResponseTruncated bool  `gorm:"not null;default:false" json:"response_truncated"`
}
```

GORM AutoMigrate 加列；旧 row 新列为 NULL / `false`。

**`internal/probe/runner.go：Result`**：

```go
type Result struct {
    // ... 原有 13 字段 ...
    RequestID         string
    RequestBody       []byte
    ResponseHead      []byte
    ResponseTail      []byte
    ResponseTruncated bool
    RequestTruncated  bool
}
```

`Run()` 在 chunk 循环 `for chunk := range stream.Chunks()` 退出后：

```go
res.RequestID   = stream.RequestID()
res.RequestBody = stream.RequestBody()
res.ResponseHead, res.ResponseTail, res.ResponseTruncated = stream.ResponseSnapshot()
// Request 已经在 adapter 端按 DefaultRequestCapture 截断；这里只标 flag
res.RequestTruncated = len(res.RequestBody) >= protocol.DefaultRequestCapture
```

#### 18.13.5 新增 helper：`internal/protocol/respcap.go`

HAR 风格响应 capture。`io.TeeReader` 把 HTTP body 同时写到 capture 和 scanner：

```go
type RespCapture struct {
    headCap, tailCap int
    head  []byte           // first N bytes verbatim
    tail  []byte           // last M bytes via sliding-window slice
    total int              // total bytes observed
}

func (r *RespCapture) Write(p []byte) (int, error) // implements io.Writer
func (r *RespCapture) Snapshot() (head, tail []byte, truncated bool)

func TruncateRequest(body []byte, max int) (out []byte, truncated bool)
```

`DefaultRespHeadCapture = 4096`、`DefaultRespTailCapture = 4096`、`DefaultRequestCapture = 32*1024`。

#### 18.13.6 前端：`RunRow.vue` + `api/runs.ts`

- `web/src/api/runs.ts`：`ProbeRun` 接口加 6 字段（含 `request_truncated` / `response_truncated`）
- `web/src/components/runs/RunRow.vue`：error 行下方新增「Wire capture」折叠 section
  - 顶部 toggle 按钮带 X-Req-Id 前 8 字符作为摘要；点开显示 X-Req-Id、request body、response head、response tail 共 4 行
  - 每行带 ⌘C 复制按钮
  - 截断时琥珀色 note：「请求体超过 32 KiB，已截断」 / 「响应中部字节被丢弃（保留 head + tail 各 4 KiB）」
  - 沿用现有 Industrial Utilitarian 风格：JetBrains Mono + 等宽数字 + 1px dashed border

#### 18.13.7 接线图（main → proto）

```
cmd/ai-monitor/main.go
└── scheduler.Handler: runner.Run(ctx, t) ─► 协议 adapter.Probe(ctx, cfg) ─► stream
                                                                              ├─ stream.RequestID()   ─► Result.RequestID
                                                                              ├─ stream.RequestBody() ─► Result.RequestBody
                                                                              └─ stream.ResponseSnapshot() ─► Result.Response{Head,Tail,Truncated}
                                                                                  ▲
                                                                                  │ tee via io.TeeReader
                                                                       protocol.NewRespCapture(4K, 4K)
                                                                            ▲
                                                                    bytes written to cap
                                                                            │
                                                          adapter.consume(s, tapReader, ctx)
                                                                            └─ tapReader = io.TeeReader(resp.Body, cap)
```

#### 18.13.8 验收（实施时勾）

- [ ] POST /admin/tasks 创建的任务跑一次，curl `GET /admin/tasks/{id}/runs` → `request_id` 是 uuid v4 格式
- [ ] 同一 row 的 `response_head` 是 SSE 第一个 `data:` frame，`response_tail` 是 `data: [DONE]` 或最后一行
- [ ] 故意构造一个 100 KiB request（system_prompt 灌字）→ `request_body` 是首 32 KiB，`request_truncated=true`
- [ ] 故意构造一个 50 KiB response（streaming mock）→ `response_head` 是首 4 KiB，`response_tail` 是后 4 KiB，`response_truncated=true`
- [ ] 故意把 X-Req-Id 通过 `openai.extra_headers` 设成自己的格式 → `probe_runs.request_id` 跟 extra_headers 一致（不覆盖失败）
- [ ] 在浏览器打开 `tasks/{id}/runs`，点开任意一行 → 「Wire capture」section 显示 X-Req-Id + JSON，复制按钮可一键 copy
- [ ] AutoMigrate 启动后，老 row 的新字段都是 NULL / false（无破坏）
- [ ] 单测：`TestRunner_ProbeCapturesWireBytes` + `TestRunner_ProbeCaptureMarksTruncation` PASS
- [ ] `go vet ./...` 干净
- [ ] 前端 type check / build 干净

#### 18.13.9 不在范围内（留 P2）

- response body 完整存储（不管多大）
- 请求 body / 响应 body 解码后结构化展示（突出 message / tool_calls 等 JSON path）
- 按 X-Req-Id 跨任务关联追踪
- 「按上游 trace id 反查 ai-monitor run」端点：`GET /admin/runs/by-request-id/{id}`
- 自动 mask：response body 含疑似 secret 字符串（如长 base64 / 数字 API key shape）时部分打码
- 抓 raw HTTP headers（含 OpenAI 路由 metadata）

---


## A.1 sequence-diagram.mermaid

```mermaid
sequenceDiagram
    autonumber
    participant Main as cmd/ai-monitor/main
    participant Cfg as config.Loader
    participant DB as ConfigStore(SQLite)
    participant Sink as PromSink
    participant Notif as wecom.Notifier
    participant Reg as protocol.Registry
    participant Sch as cronScheduler
    participant Run as http_server
    participant Cron as cron.v3
    participant Adp as ChatProtocolAdapter
    participant HTTP as Upstream AI
    participant Dedup as Deduper

    Note over Main,Run: === 启动 ===
    Main->>Cfg: Load(yaml, env, flag)
    Cfg-->>Main: *Config
    Main->>DB: Open + AutoMigrate
    Main->>Sink: New(cfg.Registry)
    Main->>Notif: New + Deduper
    Main->>Reg: MustGet(protocols)
    Main->>Sch: New(Options)
    Main->>DB: ListTasks(ctx)
    DB-->>Main: []TaskConfig
    Main->>Sch: Start(ctx)
    Sch->>Sch: cron.AddFunc(t.Schedule, entry)
    Main->>Run: Start(:9090)

    Note over Cron,HTTP: === 单次探测 ===
    Cron->>Sch: entry fires (with jitter)
    Sch->>Sch: try acquire semaphore
    Sch->>Reg: MustGet(task.Protocol)
    Reg-->>Sch: adapter
    Sch->>Adp: Probe(ctx, &task)
    Adp->>HTTP: POST + auth
    HTTP-->>Adp: 200 chunked
    Adp-->>Sch: ProbeStream{Chunks, Err, Close}
    loop chunks
        Sch->>Sch: record TTFT / ITL
        Sch->>Sink: Observe* / IncChunk
    end
    Sch->>Adp: Close()
    Sch->>Sink: ObserveDuration + SetTaskLastSuccess
    Sch->>Sch: evaluateAlerts(result)
    alt 鉴权失败
        Sch->>Dedup: ShouldFire(RuleUnauthorized)
        Dedup-->>Sch: true
        Sch->>Notif: Send(evt)
    else 连续失败 / 慢请求 / 低成功率
        Sch->>Dedup: ShouldFire(rule)
        Sch->>Notif: Send(evt) (if true)
    end
    Sch->>Sink: SetSchedulerLag

    Note over Main,Run: === 关闭 ===
    Main->>Run: Shutdown(ctx)
    Main->>Sch: Stop(ctx)
```

## A.2 class-diagram.mermaid

```mermaid
classDiagram
    class ChatProtocolAdapter {<<interface>> +Name() string +Validate() error +Probe() (ProbeStream, error)}
    class ProbeStream {<<interface>> +Chunks() ~Chunk +Err() error +Close() error}
    class Chunk {+Delta string +ArrivedAt time.Time +Index int +IsFinal bool}
    class OpenAIAdapter
    class AnthropicAdapter
    class GeminiAdapter
    class Registry {+Register() +MustGet() +List()}

    class ConfigStore {<<interface>> +ListTasks() +GetTask() +CreateTask() +UpdateTask() +DeleteTask() +Watch() +Close()}
    class SqliteStore
    class TaskConfig {+ID +Name +Enabled +Protocol +BaseURL +APIKey +Model +Schedule +Timeout +Notify}
    class NotifyConfig {+Enabled +WeComWebhook}
    class AESCipher {+Encrypt() +Decrypt()}

    class MetricSink {<<interface>> +ObserveTTFT() +ObserveITL() +ObserveDuration() +IncChunk() +IncFailure() +SetTaskLastSuccess() +SetSchedulerLag() +Handler()}
    class PromSink

    class Notifier {<<interface>> +Name() +Send()}
    class WeComNotifier
    class Deduper {+ShouldFire()}
    class AlertEvent {+TaskID +Rule +Severity +Message +OccurredAt}

    class Scheduler {<<interface>> +Start() +Reload() +Add() +Remove() +Stop()}
    class CronScheduler
    class Runner {+Run() -evaluateAlerts()}
    class SlidingWindow {+Add() +SuccessRate()}

    ChatProtocolAdapter <|.. OpenAIAdapter
    ChatProtocolAdapter <|.. AnthropicAdapter
    ChatProtocolAdapter <|.. GeminiAdapter
    ProbeStream ..> Chunk : yields
    Registry o-- ChatProtocolAdapter : manages

    ConfigStore <|.. SqliteStore
    SqliteStore ..> AESCipher : uses
    ConfigStore ..> TaskConfig : CRUD

    MetricSink <|.. PromSink

    Notifier <|.. WeComNotifier
    Notifier ..> AlertEvent : sends
    Deduper ..> AlertEvent : dedups

    Scheduler <|.. CronScheduler
    CronScheduler ..> Runner : invokes
    CronScheduler ..> MetricSink : reports lag

    Runner ..> ChatProtocolAdapter : resolves
    Runner ..> ProbeStream : reads
    Runner ..> MetricSink : observes
    Runner ..> Notifier : dispatches
    Runner ..> Deduper : dedups
    Runner ..> SlidingWindow : per-task
```

---

> **设计方案到此结束 — 等用户审阅并明确 §17.1 中的待拍板事项后，将启动第二阶段（寇豆码·工程师实现）。**
