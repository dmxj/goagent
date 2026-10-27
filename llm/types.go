// Package llm 定义与大模型交互的统一抽象：消息、工具描述、流式事件，
// 以及 Anthropic、OpenAI 兼容等多种 Provider 实现。
//
// 设计要点：上层（agent 包）只依赖本包的类型，不感知具体厂商协议，
// 新增一个模型厂商只需要实现 Provider 接口。
package llm

import (
	"context"
	"encoding/json"
	"strings"
)

// Role 是消息角色。系统提示词单独放在 Request.System 中。
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// BlockType 是内容块类型。
type BlockType string

const (
	BlockText       BlockType = "text"
	BlockThinking   BlockType = "thinking"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
)

// ContentBlock 是消息中的一个内容块。
// 一条消息可以同时包含文字、思考过程和多个工具调用。
type ContentBlock struct {
	Type BlockType `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// Message 是一轮对话中的一条消息。
type Message struct {
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content"`
}

// UserText 构造一条纯文本用户消息。
func UserText(text string) Message {
	return Message{Role: RoleUser, Content: []ContentBlock{{Type: BlockText, Text: text}}}
}

// AssistantText 构造一条纯文本助手消息。
func AssistantText(text string) Message {
	return Message{Role: RoleAssistant, Content: []ContentBlock{{Type: BlockText, Text: text}}}
}

// Text 返回消息中所有文本块拼接后的内容。
func (m Message) Text() string {
	var sb strings.Builder
	for _, b := range m.Content {
		if b.Type == BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// ToolUses 返回消息中的所有工具调用块。
func (m Message) ToolUses() []ContentBlock {
	var out []ContentBlock
	for _, b := range m.Content {
		if b.Type == BlockToolUse {
			out = append(out, b)
		}
	}
	return out
}

// ToolSpec 描述一个可供模型调用的工具（JSON Schema 形式的参数）。
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Request 是一次模型调用的请求。
type Request struct {
	Model       string     `json:"model"`
	System      string     `json:"system,omitempty"`
	Messages    []Message  `json:"messages"`
	Tools       []ToolSpec `json:"tools,omitempty"`
	MaxTokens   int        `json:"max_tokens,omitempty"`
	Temperature *float64   `json:"temperature,omitempty"`
	// ThinkingBudget > 0 时开启扩展思考（仅部分模型支持）。
	ThinkingBudget int      `json:"thinking_budget,omitempty"`
	StopSequences  []string `json:"stop_sequences,omitempty"`
}

// Usage 记录 token 用量。
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// Add 累加用量。
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
}

// Total 返回输入输出 token 总数。
func (u Usage) Total() int { return u.InputTokens + u.OutputTokens }

// StopReason 表示模型停止生成的原因。
const (
	StopEndTurn   = "end_turn"
	StopToolUse   = "tool_use"
	StopMaxTokens = "max_tokens"
	StopSequence  = "stop_sequence"
)

// Response 是一次完整调用的结果（流式调用结束后组装而成）。
type Response struct {
	Message    Message `json:"message"`
	StopReason string  `json:"stop_reason"`
	Usage      Usage   `json:"usage"`
	Model      string  `json:"model"`
}

// StreamEventType 是流式事件类型。
type StreamEventType string

const (
	EventTextDelta     StreamEventType = "text_delta"
	EventThinkingDelta StreamEventType = "thinking_delta"
	EventToolUseStart  StreamEventType = "tool_use_start"
	EventToolUseDelta  StreamEventType = "tool_use_delta"
	EventToolUseEnd    StreamEventType = "tool_use_end"
	EventMessageEnd    StreamEventType = "message_end"
)

// StreamEvent 是模型流式输出的增量事件。
type StreamEvent struct {
	Type  StreamEventType `json:"type"`
	Index int             `json:"index"`
	// Text 是文本/思考/工具参数 JSON 的增量片段。
	Text string `json:"text,omitempty"`
	// Block 在 tool_use_start / tool_use_end 时携带工具调用信息。
	Block *ContentBlock `json:"block,omitempty"`
	Usage *Usage        `json:"usage,omitempty"`
}

// Provider 是模型提供方的统一接口。
//
// Stream 发起一次流式调用，每收到一个增量事件就回调 onEvent（可为 nil），
// 结束后返回组装好的完整响应。实现必须响应 ctx 取消。
type Provider interface {
	Name() string
	Stream(ctx context.Context, req *Request, onEvent func(StreamEvent)) (*Response, error)
}

// Complete 是非流式调用的便捷封装。
func Complete(ctx context.Context, p Provider, req *Request) (*Response, error) {
	return p.Stream(ctx, req, nil)
}
