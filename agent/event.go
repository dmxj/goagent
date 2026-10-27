package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/dmxj/goagent/llm"
	"github.com/dmxj/goagent/trace"
)

// EventType 是 Agent 运行过程中对外发出的事件类型。
// UI（CLI、Web）通过订阅这些事件实现流式输出、过程展示与调试面板。
type EventType string

const (
	EventRunStart       EventType = "run_start"
	EventTurnStart      EventType = "turn_start"
	EventThinking       EventType = "thinking_delta"
	EventText           EventType = "text_delta"
	EventToolStart      EventType = "tool_start"       // 模型开始生成某个工具调用
	EventToolInputDelta EventType = "tool_input_delta" // 工具参数 JSON 增量
	EventToolCall       EventType = "tool_call"        // 参数完整，即将执行
	EventToolResult     EventType = "tool_result"
	EventPermission     EventType = "permission_request"
	EventMessage        EventType = "message" // 一条完整的助手消息
	EventCompact        EventType = "compact"
	EventUsage          EventType = "usage"
	EventLog            EventType = "log"
	EventSpanStart      EventType = "span_start"
	EventSpanEnd        EventType = "span_end"
	EventRetry          EventType = "retry"
	EventError          EventType = "error"
	EventRunEnd         EventType = "run_end"
)

// ToolEvent 携带工具调用信息。
type ToolEvent struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Input      json.RawMessage `json:"input,omitempty"`
	Output     string          `json:"output,omitempty"`
	IsError    bool            `json:"is_error,omitempty"`
	Meta       map[string]any  `json:"meta,omitempty"`
	DurationMS int64           `json:"duration_ms,omitempty"`
}

// PermissionEvent 是待审批的工具调用。
type PermissionEvent struct {
	ID     string          `json:"id"`
	Tool   string          `json:"tool"`
	Input  json.RawMessage `json:"input"`
	Reason string          `json:"reason"`
}

// Event 是对外发出的一个事件，可直接序列化为 JSON 通过 SSE 推送。
type Event struct {
	Type       EventType        `json:"type"`
	Agent      string           `json:"agent"` // 发出事件的 Agent 路径，如 main 或 main>explore
	SessionID  string           `json:"session_id,omitempty"`
	Turn       int              `json:"turn,omitempty"`
	Text       string           `json:"text,omitempty"`
	Tool       *ToolEvent       `json:"tool,omitempty"`
	Permission *PermissionEvent `json:"permission,omitempty"`
	Usage      *llm.Usage       `json:"usage,omitempty"`
	Span       *trace.SpanData  `json:"span,omitempty"`
	Message    *llm.Message     `json:"message,omitempty"`
	Error      string           `json:"error,omitempty"`
	Data       map[string]any   `json:"data,omitempty"`
	Time       time.Time        `json:"time"`
}

// Sink 接收事件。实现必须是并发安全的（并行工具会同时发事件）。
type Sink func(Event)

type sinkKey struct{}
type pathKey struct{}

// WithSink 把事件接收器放进 ctx，使深层组件（审批器、子 Agent）也能发事件。
func WithSink(ctx context.Context, s Sink) context.Context {
	return context.WithValue(ctx, sinkKey{}, s)
}

// SinkFrom 取出 ctx 中的事件接收器（没有时返回空实现）。
func SinkFrom(ctx context.Context) Sink {
	if s, ok := ctx.Value(sinkKey{}).(Sink); ok && s != nil {
		return s
	}
	return func(Event) {}
}

// AgentPath 返回 ctx 中记录的 Agent 路径。
func AgentPath(ctx context.Context) string {
	p, _ := ctx.Value(pathKey{}).(string)
	return p
}

func withPath(ctx context.Context, name string) context.Context {
	if parent := AgentPath(ctx); parent != "" {
		name = parent + ">" + name
	}
	return context.WithValue(ctx, pathKey{}, name)
}
