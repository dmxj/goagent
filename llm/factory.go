package llm

import (
	"fmt"
	"strings"
)

// ProviderConfig 描述如何连接一个模型服务。
type ProviderConfig struct {
	// Type: anthropic | openai | mock
	Type    string            `json:"type"`
	BaseURL string            `json:"base_url,omitempty"`
	APIKey  string            `json:"api_key,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Retries 为 0 时使用默认重试策略；为负数时不重试。
	Retries int `json:"retries,omitempty"`
}

// NewProvider 根据配置创建 Provider（默认带重试）。
func NewProvider(c ProviderConfig) (Provider, error) {
	var p Provider
	switch strings.ToLower(c.Type) {
	case "", "anthropic", "claude":
		ap := NewAnthropic(c.BaseURL, c.APIKey)
		ap.Headers = c.Headers
		p = ap
	case "openai", "openai-compatible", "deepseek", "qwen", "ollama":
		op := NewOpenAI(c.BaseURL, c.APIKey)
		op.Headers = c.Headers
		p = op
	case "mock":
		return &MockProvider{Handler: EchoHandler}, nil
	default:
		return nil, fmt.Errorf("未知的 provider 类型 %q（支持 anthropic / openai / mock）", c.Type)
	}
	if c.Retries < 0 {
		return p, nil
	}
	rp := WithRetry(p)
	if c.Retries > 0 {
		rp.MaxAttempts = c.Retries
	}
	return rp, nil
}
