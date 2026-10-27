package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseServer 返回一个按给定事件序列输出 SSE 的测试服务器，并记录收到的请求体与请求头。
func sseServer(t *testing.T, path string, events []string, gotBody *map[string]any, gotHeader *http.Header) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			t.Errorf("请求路径 = %s，期望 %s", r.URL.Path, path)
		}
		if gotBody != nil {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, gotBody)
		}
		if gotHeader != nil {
			*gotHeader = r.Header.Clone()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			_, _ = io.WriteString(w, e+"\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func collect(events *[]StreamEvent) func(StreamEvent) {
	return func(ev StreamEvent) { *events = append(*events, ev) }
}

func countType(evs []StreamEvent, typ StreamEventType) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func TestAnthropicStream(t *testing.T) {
	events := []string{
		`event: message_start
data: {"type":"message_start","message":{"model":"claude-test","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":4}}}`,
		`: 注释行`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"想一想"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"你好"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"，世界"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}}`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"pa"}}`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"th\":\"a.go\"}"}}`,
		`data: {"type":"content_block_stop","index":2}`,
		`data: {"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_2","name":"list_dir","input":{}}}`,
		`data: {"type":"content_block_stop","index":3}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`,
		`data: {"type":"message_stop"}`,
	}
	var body map[string]any
	var header http.Header
	srv := sseServer(t, "/v1/messages", events, &body, &header)
	p := NewAnthropic(srv.URL+"/", "test-key")
	p.Headers = map[string]string{"X-Extra": "1"}
	var got []StreamEvent
	temp := 0.5
	resp, err := p.Stream(context.Background(), &Request{
		Model: "claude-test", System: "sys", ThinkingBudget: 1024, Temperature: &temp,
		Messages: []Message{UserText("hi"), {Role: RoleAssistant, Content: []ContentBlock{{Type: BlockText, Text: ""}}}},
	}, collect(&got))
	if err != nil {
		t.Fatalf("Stream 出错：%v", err)
	}
	// 请求校验
	if header.Get("x-api-key") != "test-key" || header.Get("anthropic-version") == "" || header.Get("X-Extra") != "1" {
		t.Errorf("请求头不正确：%v", header)
	}
	if body["stream"] != true || body["system"] != "sys" || body["max_tokens"].(float64) != 8192 {
		t.Errorf("请求体不正确：%v", body)
	}
	if th, _ := body["thinking"].(map[string]any); th["budget_tokens"].(float64) != 1024 {
		t.Errorf("thinking 参数不正确：%v", body["thinking"])
	}
	msgs := body["messages"].([]any)
	second := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if second["text"] != "(空)" {
		t.Errorf("空文本块应被替换为占位文本：%v", second)
	}
	// 响应校验
	if resp.Model != "claude-test" || resp.StopReason != StopToolUse {
		t.Errorf("Model/StopReason 错误：%s / %s", resp.Model, resp.StopReason)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 42 || resp.Usage.CacheReadTokens != 4 {
		t.Errorf("用量错误：%+v", resp.Usage)
	}
	c := resp.Message.Content
	if len(c) != 4 {
		t.Fatalf("应有 4 个内容块，实际 %d：%+v", len(c), c)
	}
	if c[0].Thinking != "想一想" || c[0].Signature != "sig" {
		t.Errorf("思考块错误：%+v", c[0])
	}
	if resp.Message.Text() != "你好，世界" {
		t.Errorf("文本拼接错误：%q", resp.Message.Text())
	}
	if c[2].ID != "toolu_1" || string(c[2].Input) != `{"path":"a.go"}` {
		t.Errorf("工具参数增量 JSON 拼接错误：%+v", c[2])
	}
	if string(c[3].Input) != "{}" {
		t.Errorf("没有参数的工具调用应得到 {}：%s", c[3].Input)
	}
	if countType(got, EventTextDelta) != 2 || countType(got, EventThinkingDelta) != 1 ||
		countType(got, EventToolUseStart) != 2 || countType(got, EventToolUseDelta) != 2 ||
		countType(got, EventToolUseEnd) != 2 || countType(got, EventMessageEnd) != 1 {
		t.Errorf("流式事件数量不符：%+v", got)
	}
	if last := got[len(got)-1]; last.Type != EventMessageEnd || last.Usage.OutputTokens != 42 {
		t.Errorf("最后一个事件应为带用量的 message_end：%+v", last)
	}
}

func TestAnthropicErrors(t *testing.T) {
	t.Run("HTTP 错误状态码转为 APIError", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":"rate limited"}`)
		}))
		defer srv.Close()
		_, err := NewAnthropic(srv.URL, "k").Stream(context.Background(), &Request{}, nil)
		var ae *APIError
		if !errors.As(err, &ae) || ae.StatusCode != 429 || !strings.Contains(ae.Body, "rate limited") || ae.RetryAfter == 0 {
			t.Fatalf("期望 429 APIError，实际：%#v", err)
		}
		if !Retryable(err) {
			t.Errorf("429 应可重试")
		}
	})
	t.Run("流中的 overloaded_error", func(t *testing.T) {
		srv := sseServer(t, "/v1/messages", []string{`data: {"type":"error","error":{"type":"overloaded_error","message":"busy"}}`}, nil, nil)
		_, err := NewAnthropic(srv.URL, "k").Stream(context.Background(), &Request{}, nil)
		var ae *APIError
		if !errors.As(err, &ae) || ae.StatusCode != 529 {
			t.Fatalf("期望 529 APIError，实际：%v", err)
		}
	})
	t.Run("非法 JSON 事件", func(t *testing.T) {
		srv := sseServer(t, "/v1/messages", []string{`data: {not json`}, nil, nil)
		if _, err := NewAnthropic(srv.URL, "k").Stream(context.Background(), &Request{}, nil); err == nil {
			t.Fatal("非法 JSON 应返回错误")
		}
	})
	t.Run("缺少 content_block 不应 panic", func(t *testing.T) {
		srv := sseServer(t, "/v1/messages", []string{`data: {"type":"content_block_start","index":0}`, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`}, nil, nil)
		resp, err := NewAnthropic(srv.URL, "k").Stream(context.Background(), &Request{}, nil)
		if err != nil || resp.StopReason != StopEndTurn {
			t.Fatalf("应忽略不完整事件：resp=%+v err=%v", resp, err)
		}
	})
}

func TestOpenAIStream(t *testing.T) {
	events := []string{
		`data: {"model":"deepseek-test","choices":[{"delta":{"reasoning_content":"先分析"}}]}`,
		`data: {"choices":[{"delta":{"content":"好的"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"grep","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"pattern\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"glob","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"main\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30}}}`,
		`data: [DONE]`,
	}
	var body map[string]any
	var header http.Header
	srv := sseServer(t, "/v1/chat/completions", events, &body, &header)
	p := NewOpenAI(srv.URL+"/v1", "sk-test")
	var got []StreamEvent
	resp, err := p.Stream(context.Background(), &Request{
		Model: "deepseek-test", System: "sys", MaxTokens: 100, StopSequences: []string{"END"},
		Messages: []Message{UserText("hi")},
		Tools:    []ToolSpec{{Name: "grep", Description: "搜索", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}, collect(&got))
	if err != nil {
		t.Fatalf("Stream 出错：%v", err)
	}
	if header.Get("Authorization") != "Bearer sk-test" {
		t.Errorf("Authorization 头错误：%q", header.Get("Authorization"))
	}
	if body["max_tokens"].(float64) != 100 || body["stop"] == nil || body["stream"] != true {
		t.Errorf("请求体不正确：%v", body)
	}
	tools := body["tools"].([]any)
	if fn := tools[0].(map[string]any)["function"].(map[string]any); fn["name"] != "grep" {
		t.Errorf("工具定义转换错误：%v", tools)
	}
	if resp.Model != "deepseek-test" || resp.StopReason != StopToolUse {
		t.Errorf("Model/StopReason 错误：%s / %s", resp.Model, resp.StopReason)
	}
	if resp.Usage.InputTokens != 100 || resp.Usage.OutputTokens != 20 || resp.Usage.CacheReadTokens != 30 {
		t.Errorf("用量错误：%+v", resp.Usage)
	}
	c := resp.Message.Content
	if len(c) != 4 || c[0].Type != BlockThinking || c[0].Thinking != "先分析" || c[1].Text != "好的" {
		t.Fatalf("内容块错误：%+v", c)
	}
	if c[2].ID != "call_a" || c[2].Name != "grep" || string(c[2].Input) != `{"pattern":"main"}` {
		t.Errorf("工具调用 0 参数拼接错误：%+v", c[2])
	}
	if c[3].ID != "call_b" || c[3].Name != "glob" {
		t.Errorf("工具调用 1 错误：%+v", c[3])
	}
	if countType(got, EventToolUseStart) != 2 || countType(got, EventToolUseEnd) != 2 || countType(got, EventMessageEnd) != 1 {
		t.Errorf("流式事件数量不符：%+v", got)
	}
}

func TestOpenAIFinishReasons(t *testing.T) {
	tests := []struct {
		finish string
		want   string
	}{
		{"stop", StopEndTurn},
		{"length", StopMaxTokens},
		{"", StopEndTurn},
	}
	for _, tt := range tests {
		t.Run(tt.finish, func(t *testing.T) {
			srv := sseServer(t, "/chat/completions", []string{
				`data: {"choices":[{"delta":{"content":"x"},"finish_reason":"` + tt.finish + `"}]}`, `data: [DONE]`,
			}, nil, nil)
			resp, err := NewOpenAI(srv.URL, "").Stream(context.Background(), &Request{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StopReason != tt.want {
				t.Errorf("finish_reason %q → %s，期望 %s", tt.finish, resp.StopReason, tt.want)
			}
		})
	}
}

func TestOpenAIErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("没有 API Key 时不应发送 Authorization 头")
		}
		http.Error(w, "bad request", 400)
	}))
	defer srv.Close()
	_, err := NewOpenAI(srv.URL, "").Stream(context.Background(), &Request{}, nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.StatusCode != 400 || ae.Provider != "openai" {
		t.Fatalf("期望 400 APIError，实际：%v", err)
	}
}

func TestToOpenAIMessages(t *testing.T) {
	msgs := []Message{
		UserText("你好"),
		{Role: RoleAssistant, Content: []ContentBlock{
			{Type: BlockThinking, Thinking: "思考"},
			{Type: BlockText, Text: "我来读文件"},
			{Type: BlockToolUse, ID: "c1", Name: "read_file", Input: json.RawMessage(`{"path":"a"}`)},
			{Type: BlockToolUse, ID: "c2", Name: "list_dir"},
		}},
		{Role: RoleUser, Content: []ContentBlock{
			{Type: BlockToolResult, ToolUseID: "c1", Content: "内容"},
			{Type: BlockToolResult, ToolUseID: "c2", Content: "失败", IsError: true},
			{Type: BlockText, Text: "补充说明"},
		}},
	}
	out := toOpenAIMessages("系统", msgs)
	want := []struct {
		role, content, toolCallID string
		toolCalls                 int
	}{
		{"system", "系统", "", 0},
		{"user", "你好", "", 0},
		{"assistant", "我来读文件", "", 2},
		{"tool", "内容", "c1", 0},
		{"tool", "[错误] 失败", "c2", 0},
		{"user", "补充说明", "", 0},
	}
	if len(out) != len(want) {
		t.Fatalf("消息数 = %d，期望 %d：%+v", len(out), len(want), out)
	}
	for i, w := range want {
		m := out[i]
		if m.Role != w.role || m.Content != w.content || m.ToolCallID != w.toolCallID || len(m.ToolCalls) != w.toolCalls {
			t.Errorf("第 %d 条消息 = %+v，期望 %+v", i, m, w)
		}
	}
	if out[2].ReasoningContent != "思考" {
		t.Errorf("思考内容应转为 reasoning_content")
	}
	if out[2].ToolCalls[0].Function.Arguments != `{"path":"a"}` || out[2].ToolCalls[1].Function.Arguments != "{}" {
		t.Errorf("tool_calls 参数转换错误：%+v", out[2].ToolCalls)
	}
	if out := toOpenAIMessages("", nil); len(out) != 0 {
		t.Errorf("空输入应得到空输出")
	}
}
