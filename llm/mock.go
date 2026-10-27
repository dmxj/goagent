package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// MockProvider 是用于测试与离线演示的假模型。
//
// 每次调用按顺序返回 Responses 中的下一条；也可以设置 Handler 根据请求动态生成。
// 它会像真实模型一样逐段发出流式事件，方便测试 Agent Loop。
type MockProvider struct {
	mu        sync.Mutex
	Responses []Message
	Handler   func(req *Request) (Message, error)
	Requests  []*Request // 记录收到的请求，便于断言
	calls     int
}

// NewMock 用固定的回复序列创建 MockProvider。
func NewMock(responses ...Message) *MockProvider {
	return &MockProvider{Responses: responses}
}

func (m *MockProvider) Name() string { return "mock" }

// Calls 返回被调用次数。
func (m *MockProvider) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *MockProvider) Stream(ctx context.Context, req *Request, onEvent func(StreamEvent)) (*Response, error) {
	m.mu.Lock()
	m.Requests = append(m.Requests, req)
	idx := m.calls
	m.calls++
	m.mu.Unlock()

	var msg Message
	switch {
	case m.Handler != nil:
		var err error
		if msg, err = m.Handler(req); err != nil {
			return nil, err
		}
	case idx < len(m.Responses):
		msg = m.Responses[idx]
	default:
		msg = AssistantText("（mock：没有更多预设回复）")
	}
	msg.Role = RoleAssistant
	emit := func(ev StreamEvent) {
		if onEvent != nil {
			onEvent(ev)
		}
	}
	stop := StopEndTurn
	for i, b := range msg.Content {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch b.Type {
		case BlockText:
			for _, chunk := range chunks(b.Text, 8) {
				emit(StreamEvent{Type: EventTextDelta, Index: i, Text: chunk})
			}
		case BlockThinking:
			emit(StreamEvent{Type: EventThinkingDelta, Index: i, Text: b.Thinking})
		case BlockToolUse:
			stop = StopToolUse
			if b.ID == "" {
				msg.Content[i].ID = fmt.Sprintf("mock_call_%d_%d", idx, i)
				b.ID = msg.Content[i].ID
			}
			emit(StreamEvent{Type: EventToolUseStart, Index: i, Block: &ContentBlock{Type: BlockToolUse, ID: b.ID, Name: b.Name}})
			emit(StreamEvent{Type: EventToolUseDelta, Index: i, Text: string(b.Input)})
			cp := b
			emit(StreamEvent{Type: EventToolUseEnd, Index: i, Block: &cp})
		}
	}
	in := 0
	for _, mm := range req.Messages {
		for _, b := range mm.Content {
			in += len(b.Text) + len(b.Content) + len(b.Input)
		}
	}
	usage := Usage{InputTokens: in/4 + len(req.System)/4, OutputTokens: len(msg.Text())/4 + 1}
	emit(StreamEvent{Type: EventMessageEnd, Usage: &usage})
	return &Response{Message: msg, StopReason: stop, Usage: usage, Model: "mock"}, nil
}

// ToolCall 构造一个工具调用内容块（测试辅助）。
func ToolCall(id, name string, input any) ContentBlock {
	b, _ := json.Marshal(input)
	return ContentBlock{Type: BlockToolUse, ID: id, Name: name, Input: b}
}

func chunks(s string, n int) []string {
	r := []rune(s)
	var out []string
	for len(r) > 0 {
		k := min(n, len(r))
		out = append(out, string(r[:k]))
		r = r[k:]
	}
	if len(out) == 0 && s == "" {
		return nil
	}
	return out
}

// EchoHandler 是一个简单的 Handler：把最后一条用户消息原样复述。
func EchoHandler(req *Request) (Message, error) {
	last := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == RoleUser {
			if t := strings.TrimSpace(req.Messages[i].Text()); t != "" {
				last = t
				break
			}
		}
	}
	return AssistantText("你说的是：" + last), nil
}
