package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
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

func (p *OpenAIProvider) Stream(ctx context.Context, req *Request, onEvent func(StreamEvent)) (*Response, error) {
	body := map[string]any{
		"model":          req.Model,
		"messages":       toOpenAIMessages(req.System, req.Messages),
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
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
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("content-type", "application/json")
	hreq.Header.Set("accept", "text/event-stream")
	if p.APIKey != "" {
		hreq.Header.Set("authorization", "Bearer "+p.APIKey)
	}
	for k, v := range p.Headers {
		hreq.Header.Set(k, v)
	}
	resp, err := p.Client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, newAPIError(p.Name(), resp)
	}

	emit := func(ev StreamEvent) {
		if onEvent != nil {
			onEvent(ev)
		}
	}
	out := &Response{Message: Message{Role: RoleAssistant}}
	var text, thinking strings.Builder
	type partial struct {
		id, name string
		args     strings.Builder
		started  bool
	}
	calls := map[int]*partial{}
	finish := ""

	err = ReadSSE(resp.Body, func(e SSEEvent) error {
		if e.Data == "" || e.Data == "[DONE]" {
			return nil
		}
		var c oaChunk
		if err := json.Unmarshal([]byte(e.Data), &c); err != nil {
			return fmt.Errorf("解析流式事件失败: %w", err)
		}
		if c.Model != "" {
			out.Model = c.Model
		}
		if c.Usage != nil {
			out.Usage = Usage{InputTokens: c.Usage.PromptTokens, OutputTokens: c.Usage.CompletionTokens, CacheReadTokens: c.Usage.PromptTokensDetails.CachedTokens}
		}
		for _, ch := range c.Choices {
			if ch.Delta.ReasoningContent != "" {
				thinking.WriteString(ch.Delta.ReasoningContent)
				emit(StreamEvent{Type: EventThinkingDelta, Text: ch.Delta.ReasoningContent})
			}
			if ch.Delta.Content != "" {
				text.WriteString(ch.Delta.Content)
				emit(StreamEvent{Type: EventTextDelta, Text: ch.Delta.Content})
			}
			for i, tc := range ch.Delta.ToolCalls {
				idx := i
				if tc.Index != nil {
					idx = *tc.Index
				}
				pc := calls[idx]
				if pc == nil {
					pc = &partial{}
					calls[idx] = pc
				}
				if tc.ID != "" {
					pc.id = tc.ID
				}
				if tc.Function.Name != "" {
					pc.name += tc.Function.Name
				}
				if !pc.started && pc.name != "" {
					pc.started = true
					emit(StreamEvent{Type: EventToolUseStart, Index: idx, Block: &ContentBlock{Type: BlockToolUse, ID: pc.id, Name: pc.name}})
				}
				if tc.Function.Arguments != "" {
					pc.args.WriteString(tc.Function.Arguments)
					emit(StreamEvent{Type: EventToolUseDelta, Index: idx, Text: tc.Function.Arguments})
				}
			}
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
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
	return out, nil
}
