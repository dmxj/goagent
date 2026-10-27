package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// OpenAIProvider 实现 OpenAI Chat Completions 协议（/v1/chat/completions）。
// DeepSeek、通义千问、Kimi、智谱、Ollama、vLLM 等都兼容这个协议。
type OpenAIProvider struct {
	BaseURL string // 例如 https://api.deepseek.com/v1
	APIKey  string
	Client  *http.Client
	Headers map[string]string
}

// NewOpenAI 创建 OpenAI 兼容 Provider。baseURL 需包含 /v1 之类的版本前缀。
func NewOpenAI(baseURL, apiKey string) *OpenAIProvider {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAIProvider{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Client:  &http.Client{Timeout: 10 * time.Minute},
	}
}

func (p *OpenAIProvider) Name() string { return "openai" }

type oaMessage struct {
	Role       string       `json:"role"`
	Content    any          `json:"content"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
	// DeepSeek 等模型的思考内容
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type oaToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type oaChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content          string       `json:"content"`
			ReasoningContent string       `json:"reasoning_content"`
			ToolCalls        []oaToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaUsage `json:"usage"`
}

// toOpenAIMessages 把通用消息转换为 OpenAI 格式：
// tool_result 块变成独立的 role=tool 消息，tool_use 块变成 tool_calls。
func toOpenAIMessages(system string, msgs []Message) []oaMessage {
	var out []oaMessage
	if system != "" {
		out = append(out, oaMessage{Role: "system", Content: system})
	}
	for _, m := range msgs {
		switch m.Role {
		case RoleUser:
			var text strings.Builder
			for _, b := range m.Content {
				switch b.Type {
				case BlockToolResult:
					content := b.Content
					if b.IsError {
						content = "[错误] " + content
					}
					out = append(out, oaMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: content})
				case BlockText:
					text.WriteString(b.Text)
				}
			}
			if text.Len() > 0 {
				out = append(out, oaMessage{Role: "user", Content: text.String()})
			}
		case RoleAssistant:
			am := oaMessage{Role: "assistant"}
			var text, reasoning strings.Builder
			for _, b := range m.Content {
				switch b.Type {
				case BlockText:
					text.WriteString(b.Text)
				case BlockThinking:
					reasoning.WriteString(b.Thinking)
				case BlockToolUse:
					tc := oaToolCall{ID: b.ID, Type: "function"}
					tc.Function.Name = b.Name
					tc.Function.Arguments = string(b.Input)
					if tc.Function.Arguments == "" {
						tc.Function.Arguments = "{}"
					}
					am.ToolCalls = append(am.ToolCalls, tc)
				}
			}
			am.Content = text.String()
			am.ReasoningContent = reasoning.String()
			out = append(out, am)
		}
	}
	return out
}

// BuildRequestBody 返回发给 OpenAI 兼容服务的原生请求体（便于测试与调试）。
func (p *OpenAIProvider) BuildRequestBody(req *Request) ([]byte, error) {
	body := map[string]any{
		"model":    req.Model,
		"messages": toOpenAIMessages(req.System, req.Messages),
		"stream":   !req.NoStream,
	}
	if !req.NoStream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		body["stop"] = req.StopSequences
	}
	if len(req.Tools) > 0 {
		tools := make([]oaTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			var ot oaTool
			ot.Type = "function"
			ot.Function.Name = t.Name
			ot.Function.Description = t.Description
			ot.Function.Parameters = t.InputSchema
			tools = append(tools, ot)
		}
		body["tools"] = tools
	}
	for k, v := range req.ExtraBody {
		body[k] = v
	}
	return json.Marshal(body)
}

func (p *OpenAIProvider) Stream(ctx context.Context, req *Request, onEvent func(StreamEvent)) (resp *Response, err error) {
	buf, err := p.BuildRequestBody(req)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("content-type", "application/json")
	if req.NoStream {
		hreq.Header.Set("accept", "application/json")
	} else {
		hreq.Header.Set("accept", "text/event-stream")
	}
	if p.APIKey != "" {
		hreq.Header.Set("authorization", "Bearer "+p.APIKey)
	}
	for k, v := range p.Headers {
		hreq.Header.Set(k, v)
	}
	for k, v := range req.Headers {
		hreq.Header.Set(k, v)
	}
	rec := startExchange(ctx, p.Name(), hreq, buf, !req.NoStream)
	defer func() { rec.done(err) }()
	hresp, err := p.Client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer hresp.Body.Close()
	rec.response(hresp)
	if hresp.StatusCode/100 != 2 {
		return nil, newAPIError(p.Name(), hresp)
	}
	if req.NoStream {
		return p.decodeCompletion(rec.tee(hresp.Body, false), onEvent)
	}
	return p.readStream(rec.tee(hresp.Body, true), onEvent)
}

// oaCompletion 是非流式响应。
type oaCompletion struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      oaMessage `json:"message"`
		FinishReason string    `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaUsage `json:"usage"`
}

type oaUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u *oaUsage) toUsage() Usage {
	if u == nil {
		return Usage{}
	}
	return Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens, CacheReadTokens: u.PromptTokensDetails.CachedTokens}
}

// decodeCompletion 解析非流式响应，并按流式的顺序补发事件。
func (p *OpenAIProvider) decodeCompletion(r io.Reader, onEvent func(StreamEvent)) (*Response, error) {
	var c oaCompletion
	if err := json.NewDecoder(r).Decode(&c); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	a := newOAAssembler(onEvent)
	a.out.Model = c.Model
	a.out.Usage = c.Usage.toUsage()
	for _, ch := range c.Choices {
		text, _ := ch.Message.Content.(string)
		a.delta(ch.Message.ReasoningContent, text, ch.Message.ToolCalls, ch.FinishReason)
		break // 只取第一个候选
	}
	return a.finish(), nil
}

func (p *OpenAIProvider) readStream(r io.Reader, onEvent func(StreamEvent)) (*Response, error) {
	a := newOAAssembler(onEvent)
	err := ReadSSE(r, func(e SSEEvent) error {
		if e.Data == "" || e.Data == "[DONE]" {
			return nil
		}
		var c oaChunk
		if err := json.Unmarshal([]byte(e.Data), &c); err != nil {
			return fmt.Errorf("解析流式事件失败: %w", err)
		}
		if c.Model != "" {
			a.out.Model = c.Model
		}
		if c.Usage != nil {
			a.out.Usage = c.Usage.toUsage()
		}
		for _, ch := range c.Choices {
			a.delta(ch.Delta.ReasoningContent, ch.Delta.Content, ch.Delta.ToolCalls, ch.FinishReason)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return a.finish(), nil
}

// oaAssembler 把增量（或非流式的一次性结果）组装成完整消息。
type oaAssembler struct {
	emit           func(StreamEvent)
	out            *Response
	text, thinking strings.Builder
	calls          map[int]*oaPartial
	finishReason   string
}

type oaPartial struct {
	id, name string
	args     strings.Builder
	started  bool
}

func newOAAssembler(onEvent func(StreamEvent)) *oaAssembler {
	return &oaAssembler{
		emit: func(ev StreamEvent) {
			if onEvent != nil {
				onEvent(ev)
			}
		},
		out:   &Response{Message: Message{Role: RoleAssistant}},
		calls: map[int]*oaPartial{},
	}
}

func (a *oaAssembler) delta(reasoning, content string, toolCalls []oaToolCall, finish string) {
	if reasoning != "" {
		a.thinking.WriteString(reasoning)
		a.emit(StreamEvent{Type: EventThinkingDelta, Text: reasoning})
	}
	if content != "" {
		a.text.WriteString(content)
		a.emit(StreamEvent{Type: EventTextDelta, Text: content})
	}
	for i, tc := range toolCalls {
		idx := i
		if tc.Index != nil {
			idx = *tc.Index
		}
		pc := a.calls[idx]
		if pc == nil {
			pc = &oaPartial{}
			a.calls[idx] = pc
		}
		if tc.ID != "" {
			pc.id = tc.ID
		}
		if tc.Function.Name != "" {
			pc.name += tc.Function.Name
		}
		if !pc.started && pc.name != "" {
			pc.started = true
			a.emit(StreamEvent{Type: EventToolUseStart, Index: idx, Block: &ContentBlock{Type: BlockToolUse, ID: pc.id, Name: pc.name}})
		}
		if tc.Function.Arguments != "" {
			pc.args.WriteString(tc.Function.Arguments)
			a.emit(StreamEvent{Type: EventToolUseDelta, Index: idx, Text: tc.Function.Arguments})
		}
	}
	if finish != "" {
		a.finishReason = finish
	}
}

func (a *oaAssembler) finish() *Response {
	out := a.out
	thinking, text, calls, finish, emit := &a.thinking, &a.text, a.calls, a.finishReason, a.emit
	if thinking.Len() > 0 {
		out.Message.Content = append(out.Message.Content, ContentBlock{Type: BlockThinking, Thinking: thinking.String()})
	}
	if text.Len() > 0 {
		out.Message.Content = append(out.Message.Content, ContentBlock{Type: BlockText, Text: text.String()})
	}
	idxs := make([]int, 0, len(calls))
	for i := range calls {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for n, i := range idxs {
		pc := calls[i]
		args := strings.TrimSpace(pc.args.String())
		if args == "" {
			args = "{}"
		}
		if pc.id == "" {
			pc.id = fmt.Sprintf("call_%d", n)
		}
		b := ContentBlock{Type: BlockToolUse, ID: pc.id, Name: pc.name, Input: json.RawMessage(args)}
		out.Message.Content = append(out.Message.Content, b)
		cp := b
		emit(StreamEvent{Type: EventToolUseEnd, Index: i, Block: &cp})
	}
	switch finish {
	case "tool_calls", "function_call":
		out.StopReason = StopToolUse
	case "length":
		out.StopReason = StopMaxTokens
	default:
		out.StopReason = StopEndTurn
	}
	if len(idxs) > 0 {
		out.StopReason = StopToolUse
	}
	u := out.Usage
	emit(StreamEvent{Type: EventMessageEnd, Usage: &u})
	return out
}
