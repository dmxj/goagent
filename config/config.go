// Package config 负责加载 GoAgent 的配置。
//
// 配置分层合并（后者覆盖前者）：
//
//	内置默认值 → ~/.goagent/config.json（用户级）→ <项目>/.goagent/config.json（项目级）→ 环境变量
//
// 此外支持从 Claude Code 的 ~/.claude/settings.json 导入模型配置，
// 方便已经配好 Claude Code 的用户零配置上手。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dmxj/goagent/hook"
	"github.com/dmxj/goagent/llm"
	"github.com/dmxj/goagent/mcp"
	"github.com/dmxj/goagent/permission"
)

// ModelProfile 是一个命名的模型配置，可通过 --profile 或 /model 切换。
type ModelProfile struct {
	Provider      llm.ProviderConfig `json:"provider"`
	Model         string             `json:"model"`
	ContextWindow int                `json:"context_window,omitempty"`
	MaxTokens     int                `json:"max_tokens,omitempty"`
}

// Config 是完整配置。
type Config struct {
	Provider       llm.ProviderConfig      `json:"provider"`
	Model          string                  `json:"model"`
	Profiles       map[string]ModelProfile `json:"profiles,omitempty"`
	MaxTokens      int                     `json:"max_tokens,omitempty"`
	Temperature    *float64                `json:"temperature,omitempty"`
	ThinkingBudget int                     `json:"thinking_budget,omitempty"`
	ContextWindow  int                     `json:"context_window,omitempty"`
	MaxTurns       int                     `json:"max_turns,omitempty"`

	Permissions permission.Config           `json:"permissions"`
	Hooks       map[string][]hook.Config    `json:"hooks,omitempty"`
	MCPServers  map[string]mcp.ServerConfig `json:"mcp_servers,omitempty"`

	Tools struct {
		Disabled              []string `json:"disabled,omitempty"`
		RestrictToWorkDir     *bool    `json:"restrict_to_workdir,omitempty"`
		RequireReadBeforeEdit *bool    `json:"require_read_before_edit,omitempty"`
		BashTimeoutSec        int      `json:"bash_timeout_sec,omitempty"`
	} `json:"tools"`

	Memory struct {
		Enabled bool `json:"enabled"`
	} `json:"memory"`
	Compact struct {
		Enabled    bool    `json:"enabled"`
		Threshold  float64 `json:"threshold,omitempty"`
		KeepRecent int     `json:"keep_recent,omitempty"`
	} `json:"compact"`
	Trace struct {
		Enabled bool `json:"enabled"`
	} `json:"trace"`

	SystemPromptAppend string `json:"system_prompt_append,omitempty"`

	// 以下字段在运行时确定，不来自配置文件
	WorkDir    string   `json:"-"`
	ProjectDir string   `json:"-"` // <workdir>/.goagent
	UserDir    string   `json:"-"` // ~/.goagent
	Sources    []string `json:"-"` // 实际生效的配置来源，便于排查
}

// Default 返回默认配置。
func Default() *Config {
	c := &Config{
		Provider:      llm.ProviderConfig{Type: "anthropic"},
		Model:         "claude-sonnet-4-5",
		MaxTokens:     8192,
		ContextWindow: 128000,
		MaxTurns:      60,
	}
	c.Permissions.Mode = permission.ModeDefault
	c.Memory.Enabled = true
	c.Compact.Enabled = true
	c.Compact.Threshold = 0.8
	c.Compact.KeepRecent = 2
	c.Trace.Enabled = true
	return c
}

// Load 加载配置。workDir 为空时使用当前目录。
func Load(workDir string) (*Config, error) {
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	workDir, _ = filepath.Abs(workDir)
	c := Default()
	c.WorkDir = workDir
	c.ProjectDir = filepath.Join(workDir, ".goagent")
	if home, err := os.UserHomeDir(); err == nil {
		c.UserDir = filepath.Join(home, ".goagent")
	}
	if os.Getenv("GOAGENT_NO_CLAUDE_SETTINGS") == "" {
		c.importClaudeSettings()
	}
	for _, p := range []string{filepath.Join(c.UserDir, "config.json"), filepath.Join(c.ProjectDir, "config.json")} {
		if err := c.mergeFile(p); err != nil {
			return nil, err
		}
	}
	c.expandEnv()
	c.applyEnv()
	c.Model = CleanModel(c.Model)
	return c, nil
}

// mergeFile 把 JSON 文件覆盖到当前配置上（文件中没有的字段保持不变）。
func (c *Config) mergeFile(path string) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return fmt.Errorf("配置文件 %s 格式错误: %w", path, err)
	}
	c.Sources = append(c.Sources, path)
	return nil
}

// expandEnv 展开连接类字段中的 ${VAR} 引用，让密钥可以留在环境变量里而不写进配置文件。
//
// 注意不能对整个 JSON 文本做 os.ExpandEnv：那样会把钩子命令里的 $GOAGENT_TOOL_NAME 等
// 运行时变量在加载阶段就替换成空串，而且变量值中的引号还会破坏 JSON。
// MCP 的 env 与 headers 由 mcp 包在连接时展开，这里不重复处理。
func (c *Config) expandEnv() {
	expandProvider := func(p *llm.ProviderConfig) {
		p.BaseURL = os.ExpandEnv(p.BaseURL)
		p.APIKey = os.ExpandEnv(p.APIKey)
		for k, v := range p.Headers {
			p.Headers[k] = os.ExpandEnv(v)
		}
	}
	expandProvider(&c.Provider)
	for name, p := range c.Profiles {
		expandProvider(&p.Provider)
		c.Profiles[name] = p
	}
	for name, s := range c.MCPServers {
		s.Command = os.ExpandEnv(s.Command)
		s.URL = os.ExpandEnv(s.URL)
		args := make([]string, len(s.Args))
		for i, a := range s.Args {
			args[i] = os.ExpandEnv(a)
		}
		s.Args = args
		c.MCPServers[name] = s
	}
}

func env(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func (c *Config) applyEnv() {
	before := c.Provider
	if v := env("GOAGENT_PROVIDER"); v != "" {
		c.Provider.Type = v
	}
	if v := env("GOAGENT_BASE_URL"); v != "" {
		c.Provider.BaseURL = v
	}
	if v := env("GOAGENT_API_KEY"); v != "" {
		c.Provider.APIKey = v
	}
	if v := env("GOAGENT_MODEL"); v != "" {
		c.Model = v
	}
	// 没有显式配置 GoAgent 时，回退到各厂商的通用环境变量
	if c.Provider.APIKey == "" {
		switch strings.ToLower(c.Provider.Type) {
		case "anthropic", "":
			c.Provider.APIKey = env("ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY")
			if c.Provider.BaseURL == "" {
				c.Provider.BaseURL = env("ANTHROPIC_BASE_URL")
			}
		case "openai":
			c.Provider.APIKey = env("OPENAI_API_KEY", "DEEPSEEK_API_KEY", "DASHSCOPE_API_KEY")
			if c.Provider.BaseURL == "" {
				c.Provider.BaseURL = env("OPENAI_BASE_URL")
			}
		}
	}
	if v := env("GOAGENT_PERMISSION_MODE"); v != "" {
		c.Permissions.Mode = permission.Mode(v)
	}
	if v := env("GOAGENT_MAX_TURNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxTurns = n
		}
	}
	if c.Provider.Type != before.Type || c.Provider.BaseURL != before.BaseURL || c.Provider.APIKey != before.APIKey {
		c.Sources = append(c.Sources, "env")
	}
}

// importClaudeSettings 读取 Claude Code 的全局配置作为模型默认值。
func (c *Config) importClaudeSettings() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	p := filepath.Join(home, ".claude", "settings.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var s struct {
		Env   map[string]string `json:"env"`
		Model string            `json:"model"`
	}
	if json.Unmarshal(b, &s) != nil {
		return
	}
	key := s.Env["ANTHROPIC_AUTH_TOKEN"]
	if key == "" {
		key = s.Env["ANTHROPIC_API_KEY"]
	}
	if key == "" {
		return
	}
	c.Provider = llm.ProviderConfig{Type: "anthropic", BaseURL: s.Env["ANTHROPIC_BASE_URL"], APIKey: key}
	for _, m := range []string{s.Model, s.Env["ANTHROPIC_MODEL"], s.Env["ANTHROPIC_DEFAULT_SONNET_MODEL"]} {
		if m != "" {
			c.Model = m
			break
		}
	}
	c.Sources = append(c.Sources, p)
}

var ctxSuffix = regexp.MustCompile(`\[[^\]]*\]$`)

// CleanModel 去掉 Claude Code 风格的上下文后缀，如 "deepseek-v4[1m]" → "deepseek-v4"。
func CleanModel(m string) string {
	return strings.TrimSpace(ctxSuffix.ReplaceAllString(strings.TrimSpace(m), ""))
}

// UseProfile 切换到命名模型配置。
func (c *Config) UseProfile(name string) error {
	p, ok := c.Profiles[name]
	if !ok {
		return fmt.Errorf("模型配置 %q 不存在", name)
	}
	if p.Provider.Type != "" {
		c.Provider = p.Provider
	}
	if p.Model != "" {
		c.Model = CleanModel(p.Model)
	}
	if p.ContextWindow > 0 {
		c.ContextWindow = p.ContextWindow
	}
	if p.MaxTokens > 0 {
		c.MaxTokens = p.MaxTokens
	}
	return nil
}

// Validate 检查必要配置。
func (c *Config) Validate() error {
	if c.Provider.Type != "mock" && c.Provider.APIKey == "" && !strings.Contains(c.Provider.BaseURL, "localhost") && !strings.Contains(c.Provider.BaseURL, "127.0.0.1") {
		return fmt.Errorf("未配置 API Key：请设置环境变量 GOAGENT_API_KEY，或在 %s 中配置 provider.api_key", filepath.Join(c.UserDir, "config.json"))
	}
	if c.Model == "" {
		return fmt.Errorf("未配置模型名：请设置 GOAGENT_MODEL")
	}
	return nil
}

// Redacted 返回隐藏密钥后的配置 JSON（用于 /config 展示与日志）。
func (c *Config) Redacted() string {
	cp := *c
	cp.Provider = redactProvider(c.Provider)
	cp.Profiles = map[string]ModelProfile{}
	for n, p := range c.Profiles {
		p.Provider = redactProvider(p.Provider)
		cp.Profiles[n] = p
	}
	// MCP 服务器的 env / headers 里通常放着访问令牌（如 GITHUB_TOKEN、Authorization）
	cp.MCPServers = map[string]mcp.ServerConfig{}
	for n, s := range c.MCPServers {
		s.Env = maskValues(s.Env)
		s.Headers = maskValues(s.Headers)
		cp.MCPServers[n] = s
	}
	b, _ := json.MarshalIndent(cp, "", "  ")
	return string(b)
}

// redactProvider 返回隐藏了密钥与请求头的副本（不修改原配置）。
func redactProvider(p llm.ProviderConfig) llm.ProviderConfig {
	if p.APIKey != "" {
		p.APIKey = mask(p.APIKey)
	}
	p.Headers = maskValues(p.Headers)
	return p
}

func maskValues(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = mask(v)
	}
	return out
}

// mask 隐藏密钥：短密钥全部隐藏，长密钥只保留首尾各 4 个字符便于辨认。
func mask(k string) string {
	if len(k) <= 16 {
		return strings.Repeat("*", len(k))
	}
	return k[:4] + strings.Repeat("*", 8) + k[len(k)-4:]
}
