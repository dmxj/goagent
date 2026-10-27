package agent

import (
	"context"
	"sort"
	"sync"

	"github.com/dmxj/goagent/llm"
	"github.com/dmxj/goagent/trace"
)

// Trace 属性键。前端 Trace 查看器按这些键展示一次模型调用 / 工具调用的完整原始数据。
const (
	// LLM Span（name = "llm.call"，kind = "llm"）
	AttrLLMParams          = "llm.request.params"     // 对象：model、max_tokens、temperature、top_p、top_k、thinking_budget、stop_sequences、stream、system、tools、extra_headers、extra_body、message_count
	AttrLLMRequestURL      = "llm.request.url"        // 字符串
	AttrLLMRequestMethod   = "llm.request.method"     // 字符串
	AttrLLMRequestHeaders  = "llm.request.headers"    // 对象：实际发出的请求头（凭据已脱敏）
	AttrLLMRequestBody     = "llm.request.body"       // 字符串：发给厂商的原生请求体（与发送的字节一致）
	AttrLLMRequestBytes    = "llm.request.body_bytes" // 数字
	AttrLLMStatus          = "llm.response.status"    // 数字：HTTP 状态码
	AttrLLMResponseHeaders = "llm.response.headers"   // 对象
	AttrLLMResponseBody    = "llm.response.body"      // 字符串：非流式响应体或错误响应体
	AttrLLMRawStream       = "llm.response.raw_stream"
	AttrLLMResponseBytes   = "llm.response.bytes"   // 数字：响应体真实字节数
	AttrLLMResponseMessage = "llm.response.message" // 对象：{model, stop_reason, message}，组装后的完整助手消息
	AttrLLMUsage           = "llm.usage"            // 对象：input_tokens、output_tokens、cache_read_tokens、cache_write_tokens
	AttrLLMLatency         = "llm.latency_ms"       // 数字：含重试的总耗时
	AttrLLMAttemptLatency  = "llm.response.latency_ms"
	AttrLLMTTFB            = "llm.response.ttfb_ms"
	AttrLLMStream          = "llm.request.stream"
	AttrLLMRetries         = "llm.retries"  // 数字：重试次数
	AttrLLMAttempts        = "llm.attempts" // 数组：每次尝试的 {attempt, status, error, latency_ms, response_body}
	AttrLLMProvider        = "llm.provider"

	// 工具 Span（name = "tool.<工具名>"，kind = "tool"）
	AttrToolName           = "tool.name"
	AttrToolCallID         = "tool.call_id"
	AttrToolInput          = "tool.input"                      // 字符串：完整参数 JSON
	AttrToolOutput         = "tool.output"                     // 字符串：工具的完整输出（回填给模型前）
	AttrToolOutputBytes    = "tool.output_bytes"               // 数字
	AttrToolModelTruncated = "tool.output_truncated_for_model" // 布尔：回填给模型时是否被 MaxToolOutput 截断
	AttrToolIsError        = "tool.is_error"
	AttrToolDuration       = "tool.duration_ms"
)

// 实时事件里的 Span 只带轻量快照：大属性（原始请求体等）只保留预览，完整内容通过 /v1/traces 获取。
const (
	lightMax     = 16 << 10
	lightPreview = 2 << 10
)

func lightSpan(s *trace.Span) *trace.SpanData {
	d := s.Snapshot().Light(lightMax, lightPreview)
	return &d
}

// requestParams 返回一次请求的全部参数（用于 llm.request.params）。
func requestParams(req *llm.Request) map[string]any {
	headers := map[string]string{}
	for k, v := range req.Headers {
		if llm.SensitiveHeader(k) {
			v = "[REDACTED]"
		}
		headers[k] = v
	}
	tools := req.Tools
	if tools == nil {
		tools = []llm.ToolSpec{}
	}
	return map[string]any{
		"model":           req.Model,
		"max_tokens":      req.MaxTokens,
		"temperature":     req.Temperature,
		"top_p":           req.TopP,
		"top_k":           req.TopK,
		"thinking_budget": req.ThinkingBudget,
		"stop_sequences":  req.StopSequences,
		"stream":          !req.NoStream,
		"system":          req.System,
		"tools":           tools,
		"extra_headers":   headers,
		"extra_body":      req.ExtraBody,
		"message_count":   len(req.Messages),
	}
}

// recordExchanges 在 ctx 中放入记录器，返回读取全部往返记录的函数。
func (a *Agent) recordExchanges(ctx context.Context) (context.Context, func() []*llm.Exchange) {
	if a.Tracer == nil {
		return ctx, func() []*llm.Exchange { return nil }
	}
	var mu sync.Mutex
	var list []*llm.Exchange
	ctx = llm.WithRecorder(ctx, &llm.Recorder{MaxBytes: a.Tracer.MaxAttrBytes(), OnExchange: func(ex *llm.Exchange) {
		mu.Lock()
		list = append(list, ex)
		mu.Unlock()
	}})
	return ctx, func() []*llm.Exchange {
		mu.Lock()
		defer mu.Unlock()
		out := append([]*llm.Exchange(nil), list...)
		sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
		return out
	}
}

// recordExchangeAttrs 把原始往返写进 Span：最后一次尝试的完整数据 + 每次尝试的摘要。
func recordExchangeAttrs(span *trace.Span, exs []*llm.Exchange) {
	if len(exs) == 0 {
		return
	}
	last := exs[len(exs)-1]
	span.SetAttr(AttrLLMProvider, last.Provider)
	span.SetAttr(AttrLLMRequestMethod, last.Method)
	span.SetAttr(AttrLLMRequestURL, last.URL)
	span.SetJSON(AttrLLMRequestHeaders, last.RequestHeaders)
	span.SetText(AttrLLMRequestBody, last.RequestBody)
	span.SetAttr(AttrLLMRequestBytes, len(last.RequestBody))
	span.SetAttr(AttrLLMStream, last.Stream)
	span.SetAttr(AttrLLMStatus, last.Status)
	span.SetJSON(AttrLLMResponseHeaders, last.ResponseHeaders)
	if last.ResponseBody != "" {
		span.SetText(AttrLLMResponseBody, last.ResponseBody)
	}
	if last.RawStream != "" {
		span.SetText(AttrLLMRawStream, last.RawStream)
	}
	span.SetAttr(AttrLLMResponseBytes, last.ResponseBytes)
	span.SetAttr(AttrLLMAttemptLatency, last.LatencyMS)
	span.SetAttr(AttrLLMTTFB, last.TTFBMS)
	attempts := make([]map[string]any, 0, len(exs))
	for i, ex := range exs {
		a := map[string]any{"attempt": i + 1, "status": ex.Status, "latency_ms": ex.LatencyMS, "start": ex.Start}
		if ex.Error != "" {
			a["error"] = ex.Error
		}
		if i < len(exs)-1 && ex.ResponseBody != "" {
			a["response_body"] = ex.ResponseBody // 失败尝试的错误响应体
		}
		attempts = append(attempts, a)
	}
	span.SetJSON(AttrLLMAttempts, attempts)
}

type rootSessionKey struct{}

// permissionSession 返回用于"本会话总是允许"的会话 ID：子 Agent 使用顶层（用户）会话的 ID，
// 用户在子 Agent 的审批中选择"总是允许"，对整个对话生效，但不会影响其它用户的会话。
func permissionSession(ctx context.Context, id string) string {
	if root, ok := ctx.Value(rootSessionKey{}).(string); ok && root != "" {
		return root
	}
	return id
}

func withRootSession(ctx context.Context, id string) context.Context {
	if root, ok := ctx.Value(rootSessionKey{}).(string); ok && root != "" {
		return ctx
	}
	return context.WithValue(ctx, rootSessionKey{}, id)
}
