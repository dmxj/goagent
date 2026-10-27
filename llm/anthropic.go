package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// AnthropicProvider 实现 Anthropic Messages API（/v1/messages）。
// 国内的很多中转网关（如 one-api / new-api）也兼容这个协议。
type AnthropicProvider struct {
	BaseURL string // 例如 https://api.anthropic.com
	APIKey  string
	Version string // anthropic-version 头，默认 2023-06-01
	Client  *http.Client
	Headers map[string]string
}

// NewAnthropic 创建 Anthropic Provider。
func NewAnthropic(baseURL, apiKey string) *AnthropicProvider {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	return &AnthropicProvider{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Version: "2023-06-01",
		Client:  &http.Client{Timeout: 10 * time.Minute},
	}
}

func (p *AnthropicProvider) Name() string { return "anthropic" }

type anthropicReq struct {
	Model         string          `json:"model"`
	System        string          `json:"system,omitempty"`
	Messages      []Message       `json:"messages"`
	Tools         []ToolSpec      `json:"tools,omitempty"`
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream"`
	Thinking      *anthropicThink `json:"thinking,omitempty"`
}

type anthropicThink struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func (u anthropicUsage) toUsage() Usage {
	return Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadInputTokens, CacheWriteTokens: u.CacheCreationInputTokens}
}

type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Model string         `json:"model"`
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *ContentBlock `json:"content_block"`
	Delta        *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anthropicUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// sanitizeMessages 去掉空文本块；Anthropic 不接受空 text。
func sanitizeMessages(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		nm := Message{Role: m.Role}
		for _, b := range m.Content {
			if b.Type == BlockText && b.Text == "" {
				continue
			}
			if b.Type == BlockToolUse && len(b.Input) == 0 {
				b.Input = json.RawMessage("{}")
			}
			nm.Content = append(nm.Content, b)
		}
		if len(nm.Content) == 0 {
			nm.Content = []ContentBlock{{Type: BlockText, Text: "(空)"}}
		}
		out = append(out, nm)
	}
	return out
}

func (p *AnthropicProvider) Stream(ctx context.Context, req *Request, onEvent func(StreamEvent)) (*Response, error) {
	body := anthropicReq{
		Model:         req.Model,
		System:        req.System,
		Messages:      sanitizeMessages(req.Messages),
		Tools:         req.Tools,
		MaxTokens:     req.MaxTokens,
		Temperature:   req.Temperature,
		StopSequences: req.StopSequences,
		Stream:        true,
	}
	if body.MaxTokens == 0 {
		body.MaxTokens = 8192
	}
	if req.ThinkingBudget > 0 {
		body.Thinking = &anthropicThink{Type: "enabled", BudgetTokens: req.ThinkingBudget}
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/v1/messages", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("content-type", "application/json")
	hreq.Header.Set("accept", "text/event-stream")
	hreq.Header.Set("x-api-key", p.APIKey)
	hreq.Header.Set("authorization", "Bearer "+p.APIKey)
	hreq.Header.Set("anthropic-version", p.Version)
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
	var blocks []*ContentBlock
	var jsonBufs []*strings.Builder
	var streamErr error

	err = ReadSSE(resp.Body, func(e SSEEvent) error {
		if e.Data == "" || e.Data == "[DONE]" {
			return nil
		}
		var ev anthropicStreamEvent
		if err := json.Unmarshal([]byte(e.Data), &ev); err != nil {
			return fmt.Errorf("解析流式事件失败: %w", err)
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				out.Model = ev.Message.Model
				out.Usage.Add(ev.Message.Usage.toUsage())
			}
		case "content_block_start":
			if ev.ContentBlock == nil || ev.Index < 0 {
				return nil // 不完整的事件，忽略而不是 panic
			}
			for len(blocks) <= ev.Index {
				blocks = append(blocks, nil)
				jsonBufs = append(jsonBufs, &strings.Builder{})
			}
			b := *ev.ContentBlock
			if b.Type == BlockToolUse {
				b.Input = nil
			}
			blocks[ev.Index] = &b
			if b.Type == BlockToolUse {
				emit(StreamEvent{Type: EventToolUseStart, Index: ev.Index, Block: &ContentBlock{Type: BlockToolUse, ID: b.ID, Name: b.Name}})
			}
		case "content_block_delta":
			if ev.Index >= len(blocks) || blocks[ev.Index] == nil || ev.Delta == nil {
				return nil
			}
			b := blocks[ev.Index]
			switch ev.Delta.Type {
			case "text_delta":
				b.Text += ev.Delta.Text
				emit(StreamEvent{Type: EventTextDelta, Index: ev.Index, Text: ev.Delta.Text})
			case "thinking_delta":
				b.Thinking += ev.Delta.Thinking
				emit(StreamEvent{Type: EventThinkingDelta, Index: ev.Index, Text: ev.Delta.Thinking})
			case "signature_delta":
				b.Signature += ev.Delta.Signature
			case "input_json_delta":
				jsonBufs[ev.Index].WriteString(ev.Delta.PartialJSON)
				emit(StreamEvent{Type: EventToolUseDelta, Index: ev.Index, Text: ev.Delta.PartialJSON})
			}
		case "content_block_stop":
			if ev.Index < len(blocks) && blocks[ev.Index] != nil && blocks[ev.Index].Type == BlockToolUse {
				b := blocks[ev.Index]
				raw := strings.TrimSpace(jsonBufs[ev.Index].String())
				if raw == "" {
					raw = "{}"
				}
				b.Input = json.RawMessage(raw)
				cp := *b
				emit(StreamEvent{Type: EventToolUseEnd, Index: ev.Index, Block: &cp})
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				out.StopReason = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				// message_delta 中的 output_tokens 是累计值
				out.Usage.OutputTokens = ev.Usage.OutputTokens
				if ev.Usage.InputTokens > 0 {
					out.Usage.InputTokens = ev.Usage.InputTokens
				}
			}
		case "error":
			if ev.Error != nil {
				streamErr = &APIError{Provider: p.Name(), StatusCode: 500, Body: ev.Error.Type + ": " + ev.Error.Message}
				if ev.Error.Type == "overloaded_error" {
					streamErr.(*APIError).StatusCode = 529
				}
			}
		}
		return nil
	})
	if streamErr != nil {
		return nil, streamErr
	}
	if err != nil {
		return nil, err
	}
	for _, b := range blocks {
		if b != nil {
			out.Message.Content = append(out.Message.Content, *b)
		}
	}
	if out.StopReason == "" {
		out.StopReason = StopEndTurn
	}
	u := out.Usage
	emit(StreamEvent{Type: EventMessageEnd, Usage: &u})
	return out, nil
}
