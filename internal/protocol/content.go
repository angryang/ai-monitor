// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

// §18.14 — 语义化内容捕获（输入 / 思考 / 输出）。
//
// §18.13 落库的是"原始字节"（request_body / response_head / response_tail），
// 取证够用但不好读：管理员真正想看的是三段语义内容——
//  1. 输入(input)：这次探测实际发给模型的到底是什么；
//  2. 思考(thinking)：模型回答前的推理过程（DeepSeek / Qwen 的 reasoning、
//     Anthropic 的 thinking_delta、Gemini 的 thought part）；
//  3. 输出(output)：模型最终的回答正文。
//
// 关键约束：thinking / output **不能**事后从 response_head / response_tail
// 反解——那是 HAR 风格的首尾各 4 KiB，中间字节被丢弃，跨帧的推理流必然断裂。
// 所以必须在 adapter 消费流式响应的当下按帧提取（见 Chunk.Kind）。
//
// 本文件提供四样东西：
//   - ExpandVars / BuildInput：由配置还原"真正发出去"的输入内容；
//   - TextAccumulator：带上限的文本累加器（截断到合法 UTF-8 边界）；
//   - ContentCapture：ProbeStream 的附加出口；
//   - RequestError：让失败路径也能带上 X-Req-Id。
package protocol

import (
	"io"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/angryang/ai-monitor/internal/config"
)

// DefaultContentCapture 是 input / thinking / output 三段内容各自的上限。
//
// 16 KiB 足够放下绝大多数 system prompt + 多轮示例 + 一段完整回答，同时把
// probe_runs 单行体积控制在 ~48 KiB 量级（每任务保留 100 行 ≈ 5 MiB）。
const DefaultContentCapture = 16 * 1024

// DefaultErrorBodyCapture 是失败响应体被保留的最大字节数。
//
// 4xx / 5xx 的响应体往往就是最有价值的线索（"Incorrect API key provided"、
// "model not found"），所以失败路径上要把它留下来；但同样要设上限，
// 免得上游返回一张 base64 大图把数据库撑爆。
const DefaultErrorBodyCapture = 4 * 1024

// ExpandVars 把 ${var} 占位符替换成 prompt_vars 里的值。未知变量原样保留。
//
// §18.14 — 三个适配器此前各自抄了一份同名同实现的 expandVars。收敛到一处
// 是为了让 BuildInput 渲染出的输入内容与适配器真正发出的请求体**逐字一致**：
// 一旦两边分叉，数据库里的"输入内容"就会变成谎话。
func ExpandVars(s string, vars map[string]string) string {
	if len(vars) == 0 || s == "" {
		return s
	}
	out := s
	for k, v := range vars {
		out = strings.ReplaceAll(out, "${"+k+"}", v)
	}
	return out
}

// InputBlock 是输入内容里的一个 role 分块。
type InputBlock struct {
	Role    string
	Content string
}

// BuildInput 把任务配置还原成"这次探测实际发给模型的输入内容"。
//
// 渲染格式是若干个 `[role]\n<内容>` 块，块之间以空行分隔：
//
//	[system]
//	You are a helpful assistant.
//
//	[user]
//	你好
//
// system prompt 为空时不产出 system 块。三个协议共用同一份渲染逻辑，
// 差别只在"模型看到的内容先后"上——那属于各协议自身的 wire 语义，
// 不影响这里"人类可读"的定位。
func BuildInput(cfg *config.TaskConfig) string {
	if cfg == nil {
		return ""
	}
	userPrompt := ExpandVars(cfg.UserPrompt, cfg.PromptVars)
	sysPrompt := ExpandVars(cfg.SystemPrompt, cfg.PromptVars)

	var blocks []string
	if sysPrompt != "" {
		blocks = append(blocks, renderRole("system", sysPrompt))
	}
	if userPrompt != "" {
		blocks = append(blocks, renderRole("user", userPrompt))
	}
	return strings.Join(blocks, "\n\n")
}

// renderRole 把 role + text 渲染成统一的 `[role]\n<text>` 块。文本为空时返回空串。
func renderRole(role, text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	role = strings.TrimSpace(role)
	if role == "" {
		role = "user"
	}
	return "[" + role + "]\n" + text
}

// TextAccumulator 是带上限的文本累加器，线程安全。
//
// 上限是硬性的：截断时按字节截断并回退到合法的 UTF-8 字符边界，避免切出
// 半个汉字导致 SQLite / JSON 出现非法字节序列。
type TextAccumulator struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
}

// NewTextAccumulator 返回一个上限为 limit 字节的累加器。limit <= 0 时
// 退化为 DefaultContentCapture。
func NewTextAccumulator(limit int) *TextAccumulator {
	if limit <= 0 {
		limit = DefaultContentCapture
	}
	return &TextAccumulator{limit: limit}
}

// WriteString 追加一段文本。超过上限的部分被丢弃并置位 truncated 标志。
func (a *TextAccumulator) WriteString(s string) {
	if a == nil || s == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.buf) >= a.limit {
		a.truncated = true
		return
	}
	if room := a.limit - len(a.buf); len(s) > room {
		a.buf = append(a.buf, s[:room]...)
		a.truncated = true
		return
	}
	a.buf = append(a.buf, s...)
}

// String 返回已累积的文本，尾部可能被裁到合法 UTF-8 边界。
func (a *TextAccumulator) String() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	b := a.buf
	// 截断可能切在多字节字符中间：逐字节回退直到末尾是一个完整 rune。
	for len(b) > 0 {
		r, size := utf8.DecodeLastRune(b)
		if r != utf8.RuneError || size > 1 {
			break
		}
		b = b[:len(b)-1]
	}
	return string(b)
}

// Truncated 报告累积过程中是否发生过截断。
func (a *TextAccumulator) Truncated() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.truncated
}

// ContentCapture 承载一次探测中捕获的语义内容（§18.14）。
//
// 它是 ProbeStream 的附加出口：adapter 在消费流式响应的当下把推理内容攒
// 起来，runner 在 chunk 循环结束后统一读取。相比"事后从原始字节反解"，
// 这是唯一可靠的做法——HAR 风格的首尾各 4 KiB 捕获会丢掉中段，跨帧的推理
// 流必然断裂。
type ContentCapture struct {
	// ThinkingContent 是模型的推理过程文本。模型不产出推理时为空串。
	ThinkingContent string
	// Truncated 表示 ThinkingContent 触达上限被截断。
	Truncated bool
}

// FirstNonEmpty 返回第一个非空字符串，全空则返回 ""。
//
// §18.14 — 不同厂商把推理内容放在不同字段名上（DeepSeek / Qwen 用
// reasoning_content，OpenRouter 用 reasoning，Kimi 用 thinking），
// 适配器统一用本函数挑出第一个有值的。
func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// RequestError 包装"请求已经发出、但没拿到成功响应"的错误。
//
// 它携带 X-Req-Id、请求体与（截断后的）错误响应体，原因很实际：
// 401 / 403 / 5xx 恰恰是最需要排查的场景，而在此之前这条路径上 RequestID
// 整个丢失——管理员看到一条 "unauthorized" 记录，却拿不到任何能拿去上游
// 日志里 grep 的线索。错误响应体同理：它往往直接写着 "Incorrect API key"。
//
// Unwrap 保证 errors.Is(err, protocol.ErrUnauthorized) 之类的判定仍然成立。
type RequestError struct {
	// RequestID 是本次请求使用的 X-Req-Id。
	RequestID string
	// RequestBody 是实际发出去的请求体（已按 DefaultRequestCapture 截断）。
	RequestBody []byte
	// ResponseBody 是失败响应体（已按 DefaultErrorBodyCapture 截断）。
	// 非 4xx / 5xx 时为空。
	ResponseBody string
	// Err 是底层错误。
	Err error
}

// Error 实现 error 接口。
func (e *RequestError) Error() string {
	if e == nil || e.Err == nil {
		return "protocol: request failed"
	}
	return e.Err.Error()
}

// Unwrap 返回底层错误，使 errors.Is / errors.As 可以穿透。
func (e *RequestError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// NewRequestError 构造一个携带请求上下文的错误。
func NewRequestError(requestID string, reqBody []byte, respBody string, err error) *RequestError {
	return &RequestError{
		RequestID:    requestID,
		RequestBody:  reqBody,
		ResponseBody: respBody,
		Err:          err,
	}
}

// ReadErrorBody 从失败响应体里读出最多 max 字节的可读文本。
//
// 读到的内容会被裁到合法 UTF-8 边界；读取失败返回空串（不能因为拿不到
// 错误响应体就掩盖掉真正的错误）。
func ReadErrorBody(r io.Reader, max int) string {
	if r == nil {
		return ""
	}
	if max <= 0 {
		max = DefaultErrorBodyCapture
	}
	buf, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil && len(buf) == 0 {
		return ""
	}
	truncated := len(buf) > max
	if truncated {
		buf = buf[:max]
	}
	// 裁到合法 UTF-8 边界，避免切出半个字符。
	for len(buf) > 0 {
		rn, size := utf8.DecodeLastRune(buf)
		if rn != utf8.RuneError || size > 1 {
			break
		}
		buf = buf[:len(buf)-1]
	}
	return strings.TrimSpace(string(buf))
}
