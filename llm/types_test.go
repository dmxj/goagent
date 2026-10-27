package llm

import (
	"context"
	"errors"
	"testing"
)

func TestMessageHelpers(t *testing.T) {
	m := Message{Role: RoleAssistant, Content: []ContentBlock{
		{Type: BlockText, Text: "a"},
		ToolCall("1", "x", map[string]int{"n": 1}),
		{Type: BlockText, Text: "b"},
	}}
	if m.Text() != "ab" {
		t.Errorf("Text() = %q，期望 ab", m.Text())
	}
	if uses := m.ToolUses(); len(uses) != 1 || uses[0].Name != "x" || string(uses[0].Input) != `{"n":1}` {
		t.Errorf("ToolUses() 错误：%+v", uses)
	}
	if UserText("q").Role != RoleUser || AssistantText("r").Role != RoleAssistant {
		t.Errorf("构造函数角色错误")
	}
	u := Usage{InputTokens: 1, OutputTokens: 2}
	u.Add(Usage{InputTokens: 3, OutputTokens: 4, CacheReadTokens: 5, CacheWriteTokens: 6})
	if u.Total() != 10 || u.CacheReadTokens != 5 || u.CacheWriteTokens != 6 {
		t.Errorf("Usage 累加错误：%+v", u)
	}
}

func TestMockProvider(t *testing.T) {
	m := NewMock(
		Message{Content: []ContentBlock{{Type: BlockThinking, Thinking: "嗯"}, {Type: BlockText, Text: "一二三四五六七八九十"}, {Type: BlockToolUse, Name: "t"}}},
		AssistantText("第二条"),
	)
	var evs []StreamEvent
	resp, err := m.Stream(context.Background(), &Request{System: "s", Messages: []Message{UserText("hi")}}, collect(&evs))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != StopToolUse || resp.Message.Role != RoleAssistant {
		t.Errorf("含工具调用时 StopReason 应为 tool_use：%+v", resp)
	}
	if id := resp.Message.ToolUses()[0].ID; id == "" {
		t.Errorf("未指定 ID 的工具调用应自动生成 ID")
	}
	if countType(evs, EventTextDelta) != 2 { // 10 个字符按 8 个一段切分
		t.Errorf("文本应分段输出：%+v", evs)
	}
	resp, _ = Complete(context.Background(), m, &Request{})
	if resp.Message.Text() != "第二条" {
		t.Errorf("第二次调用应返回第二条预设回复")
	}
	resp, _ = Complete(context.Background(), m, &Request{})
	if resp.Message.Text() == "" || m.Calls() != 3 || len(m.Requests) != 3 {
		t.Errorf("预设回复用尽后应返回占位文本，并记录请求")
	}

	h := &MockProvider{Handler: func(*Request) (Message, error) { return Message{}, errors.New("boom") }}
	if _, err := h.Stream(context.Background(), &Request{}, nil); err == nil {
		t.Errorf("Handler 返回的错误应透传")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewMock(AssistantText("x")).Stream(ctx, &Request{}, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("ctx 取消时应返回 context.Canceled，实际 %v", err)
	}
}

func TestEchoHandler(t *testing.T) {
	msg, _ := EchoHandler(&Request{Messages: []Message{UserText("第一句"), AssistantText("x"), UserText("  最后一句 ")}})
	if msg.Text() != "你说的是：最后一句" {
		t.Errorf("EchoHandler = %q", msg.Text())
	}
	if chunks("", 8) != nil {
		t.Errorf("空字符串不应产生分段")
	}
}
