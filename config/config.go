// Package config 负责加载 GoAgent 的配置。
//
// 配置分层合并（后者覆盖前者）：
//
//	内置默认值 → ~/.claude/settings.json（导入模型配置）→ ~/.goagent/config.json（用户级）
//	→ <项目>/.goagent/config.json（项目级）→ 环境变量
//
// 模型、参数、系统提示词、工具绑定、技能、子 Agent、MCP、权限、钩子全部来自配置；
// app 包监视这些文件（以及提示词文件、技能目录、AGENTS.md），修改后下一轮对话即生效（热更新）。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dmxj/goagent/hook"
	"github.com/dmxj/goagent/llm"
	"github.com/dmxj/goagent/mcp"
	"github.com/dmxj/goagent/permission"
	"github.com/dmxj/goagent/procenv"
	"github.com/dmxj/goagent/tool"
)

// ModelProfile 是一个命名的模型配置，可通过 --profile 或 /model 切换。
type ModelProfile struct {
	Provider      llm.ProviderConfig `json:"provider"`
	Model         string             `json:"model"`
	ContextWindow int                `json:"context_window,omitempty"`
	MaxTokens     int                `json:"max_tokens,omitempty"`
	// Params 非空时覆盖顶层 params 中出现的字段。
	Params *ModelParams `json:"params,omitempty"`
}

// ModelParams 是模型调用参数（对应配置 params）。
type ModelParams struct {
	MaxTokens      int      `json:"max_tokens,omitempty"`
	Temperature    *float64 `json:"temperature,omitempty"`
	TopP           *float64 `json:"top_p,omitempty"`
	TopK           int      `json:"top_k,omitempty"`
	ThinkingBudget int      `json:"thinking_budget,omitempty"`
	Stop           []string `json:"stop,omitempty"`
	// Stream 为 false 时使用非流式接口；默认 true。
	Stream *bool `json:"stream,omitempty"`
	// ExtraHeaders 是每次模型请求附加的 HTTP 头，值支持 ${VAR}。
	ExtraHeaders map[string]string `json:"extra_headers,omitempty"`
	// ExtraBody 合并进厂商原生请求体，用于厂商特有参数。
	ExtraBody map[string]any `json:"extra_body,omitempty"`
}

// merge 用 o 中出现的字段覆盖 p。
func (p *ModelParams) merge(o *ModelParams) {
	if o == nil {
		return
	}
	if o.MaxTokens > 0 {
		p.MaxTokens = o.MaxTokens
	}
	if o.Temperature != nil {
		p.Temperature = o.Temperature
	}
	if o.TopP != nil {
		p.TopP = o.TopP
	}
	if o.TopK > 0 {
		p.TopK = o.TopK
	}
	if o.ThinkingBudget > 0 {
		p.ThinkingBudget = o.ThinkingBudget
	}
	if o.Stop != nil {
		p.Stop = o.Stop
	}
	if o.Stream != nil {
		p.Stream = o.Stream
	}
	if o.ExtraHeaders != nil {
		p.ExtraHeaders = o.ExtraHeaders
	}
	if o.ExtraBody != nil {
		p.ExtraBody = o.ExtraBody
	}
}

// PromptSection 是追加到系统提示词中的一个自定义章节。
type PromptSection struct {
	Title   string `json:"title,omitempty"`
	Content string `json:"content,omitempty"`
	// File 非空时从文件读取内容（相对路径基于工作目录）。
	File string `json:"file,omitempty"`
}

// PromptConfig 控制系统提示词的组装（对应配置 prompt）。
type PromptConfig struct {
	// Append 追加到系统提示词末尾的文本。
	Append string `json:"append,omitempty"`
	// AppendFile 追加文件内容（相对路径基于工作目录）。
	AppendFile string `json:"append_file,omitempty"`
	// Sections 开关内置章节：environment、tools、skills、subagents、mcp、instructions、memory，false 表示不输出。
	Sections map[string]bool `json:"sections,omitempty"`
	// Extra 是额外的自定义章节，按顺序输出在内置章节之后。
	Extra []PromptSection `json:"extra,omitempty"`
}

// ListConfig 是"目录 + 启用 / 禁用名单"形式的配置（技能、子 Agent）。
type ListConfig struct {
	// Dirs 是扫描目录（支持 ~ 与相对工作目录的路径）；为空时使用默认目录。
	Dirs []string `json:"dirs,omitempty"`
	// Enabled 非空时只启用名单中的项（支持 * 通配）。
	Enabled []string `json:"enabled,omitempty"`
	// Disabled 中的项不启用（支持 * 通配）。
	Disabled []string `json:"disabled,omitempty"`
}

// Allowed 判断名称是否启用。
func (l ListConfig) Allowed(name string) bool {
	if matchName(l.Disabled, name) {
		return false
	}
	return len(l.Enabled) == 0 || matchName(l.Enabled, name)
}

// matchName 判断 name 是否匹配名单中的任意一项（精确或 * 通配）。
func matchName(patterns []string, name string) bool {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == name || p == "*" {
			return true
		}
		if ok, err := filepath.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}

// ToolsConfig 是工具配置（对应配置 tools）。
type ToolsConfig struct {
	// Enabled 非空时只启用名单中的工具（支持 * 通配，例如 "mcp__github__*"）。
	Enabled []string `json:"enabled,omitempty"`
	// Disabled 中的工具不启用。
	Disabled []string `json:"disabled,omitempty"`
	// Options 是单个工具的覆盖选项：描述、超时、输出上限、是否并行。
	Options map[string]tool.Options `json:"options,omitempty"`

	RestrictToWorkDir     *bool `json:"restrict_to_workdir,omitempty"`
	RequireReadBeforeEdit *bool `json:"require_read_before_edit,omitempty"`
	BashTimeoutSec        int   `json:"bash_timeout_sec,omitempty"`
	// BashShell 指定 bash 工具使用的 shell（默认 /bin/bash，不存在时 /bin/sh）。
	BashShell string `json:"bash_shell,omitempty"`
	// BashEnv 是 bash 工具与命令钩子的环境变量策略（默认只继承白名单变量并剔除密钥）。
	BashEnv procenv.Policy `json:"bash_env"`
	// WebFetchTimeoutSec 是 web_fetch 的超时。
	WebFetchTimeoutSec int `json:"web_fetch_timeout_sec,omitempty"`
	// MaxOutput 是内置工具自身的输出上限（字节）。
	MaxOutput int `json:"max_output,omitempty"`
	// TimeoutSec 是任意工具调用的总超时（Agent 级，默认 600 秒）。
	TimeoutSec int `json:"timeout_sec,omitempty"`
	// MaxParallel 是并行执行工具的最大并发数。
	MaxParallel int `json:"max_parallel,omitempty"`
	// MaxResultBytes 是回填给模型的工具结果最大字节数。
	MaxResultBytes int `json:"max_result_bytes,omitempty"`
}

// Config 是完整配置。
type Config struct {
	Provider       llm.ProviderConfig      `json:"provider"`
	Model          string                  `json:"model"`
	Profiles       map[string]ModelProfile `json:"profiles,omitempty"`
	Params         ModelParams             `json:"params"`
	MaxTokens      int                     `json:"max_tokens,omitempty"`      // 兼容旧版，等价于 params.max_tokens
	Temperature    *float64                `json:"temperature,omitempty"`     // 兼容旧版，等价于 params.temperature
	ThinkingBudget int                     `json:"thinking_budget,omitempty"` // 兼容旧版，等价于 params.thinking_budget
	ContextWindow  int                     `json:"context_window,omitempty"`
	MaxTurns       int                     `json:"max_turns,omitempty"`
	KeepThinking   bool                    `json:"keep_thinking,omitempty"`

	// SystemPrompt 替换默认的身份与行为准则（系统提示词的第一部分）。
	SystemPrompt string `json:"system_prompt,omitempty"`
	// SystemPromptFile 从文件读取身份与行为准则（相对路径基于工作目录，例如 .goagent/prompts/system.md）。
	// 同时设置时文件优先。
	SystemPromptFile string       `json:"system_prompt_file,omitempty"`
	Prompt           PromptConfig `json:"prompt"`
	// SystemPromptAppend 兼容旧版，等价于 prompt.append。
	SystemPromptAppend string `json:"system_prompt_append,omitempty"`

	Permissions permission.Config           `json:"permissions"`
	Hooks       map[string][]hook.Config    `json:"hooks,omitempty"`
	MCPServers  map[string]mcp.ServerConfig `json:"mcp_servers,omitempty"`

	Tools  ToolsConfig `json:"tools"`
	Skills ListConfig  `json:"skills"`
	Agents ListConfig  `json:"agents"`

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
		// MaxFieldBytes 是单个原始字段（请求体、原始流、工具输出）的上限，默认 4MB。
		MaxFieldBytes int `json:"max_field_bytes,omitempty"`
	} `json:"trace"`
	// Reload 控制配置热更新。
	Reload struct {
		// Enabled 为 false 时关闭热更新（默认开启）。
		Enabled *bool `json:"enabled,omitempty"`
		// IntervalMS 是轮询间隔，默认 1000。
		IntervalMS int `json:"interval_ms,omitempty"`
	} `json:"reload"`

	// 以下字段在运行时确定，不来自配置文件
	WorkDir    string   `json:"-"`
	ProjectDir string   `json:"-"` // <workdir>/.goagent
	UserDir    string   `json:"-"` // ~/.goagent
	Sources    []string `json:"-"` // 实际生效的配置来源，便于排查
}

// EffectiveParams 返回最终生效的模型参数：旧版顶层字段 → params。
func (c *Config) EffectiveParams() ModelParams {
	p := ModelParams{MaxTokens: c.MaxTokens, Temperature: c.Temperature, ThinkingBudget: c.ThinkingBudget}
	params := c.Params
	p.merge(&params)
	return p
}

// ReloadEnabled 报告是否开启热更新。
func (c *Config) ReloadEnabled() bool { return c.Reload.Enabled == nil || *c.Reload.Enabled }

// ReloadInterval 返回热更新轮询间隔。
func (c *Config) ReloadInterval() time.Duration {
	if c.Reload.IntervalMS <= 0 {
		return time.Second
	}
	return time.Duration(max(c.Reload.IntervalMS, 100)) * time.Millisecond
}

// ResolvePath 把配置中的路径解析为绝对路径：支持 ~/ 前缀，相对路径基于工作目录。
func (c *Config) ResolvePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[1:])
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(c.WorkDir, p)
	}
	return filepath.Clean(p)
}

// SkillDirs 返回技能扫描目录（后面的目录同名技能覆盖前面的）。
func (c *Config) SkillDirs() []string {
	if len(c.Skills.Dirs) == 0 {
		return []string{filepath.Join(c.UserDir, "skills"), filepath.Join(c.ProjectDir, "skills")}
	}
	out := make([]string, 0, len(c.Skills.Dirs))
	for _, d := range c.Skills.Dirs {
		out = append(out, c.ResolvePath(d))
	}
	return out
}

// AgentDirs 返回自定义子 Agent 扫描目录。
func (c *Config) AgentDirs() []string {
	if len(c.Agents.Dirs) == 0 {
		return []string{filepath.Join(c.UserDir, "agents"), filepath.Join(c.ProjectDir, "agents")}
	}
	out := make([]string, 0, len(c.Agents.Dirs))
	for _, d := range c.Agents.Dirs {
		out = append(out, c.ResolvePath(d))
	}
	return out
}

// ConfigFiles 返回参与合并的配置文件路径（包括尚不存在的）。
func (c *Config) ConfigFiles() []string {
	files := []string{filepath.Join(c.UserDir, "config.json"), filepath.Join(c.ProjectDir, "config.json")}
	if os.Getenv("GOAGENT_NO_CLAUDE_SETTINGS") == "" {
		if home, err := os.UserHomeDir(); err == nil {
			files = append([]string{filepath.Join(home, ".claude", "settings.json")}, files...)
		}
	}
	return files
}

// PromptFiles 返回配置引用的提示词文件（system_prompt_file、prompt.append_file、prompt.extra[].file）。
func (c *Config) PromptFiles() []string {
	var out []string
	for _, p := range []string{c.SystemPromptFile, c.Prompt.AppendFile} {
		if p != "" {
			out = append(out, c.ResolvePath(p))
		}
	}
	for _, s := range c.Prompt.Extra {
		if s.File != "" {
			out = append(out, c.ResolvePath(s.File))
		}
	}
	return out
}

// Secrets 返回配置中的全部密钥原文（用于从 Trace 中抹掉）。
func (c *Config) Secrets() []string {
	var out []string
	add := func(p llm.ProviderConfig) {
		out = append(out, p.APIKey)
		for _, v := range p.Headers {
			out = append(out, v)
		}
	}
	add(c.Provider)
	for _, p := range c.Profiles {
		add(p.Provider)
	}
	for _, s := range c.MCPServers {
		for _, v := range s.Env {
			out = append(out, os.ExpandEnv(v))
		}
		for _, v := range s.Headers {
			out = append(out, os.ExpandEnv(v))
		}
	}
	for k, v := range c.EffectiveParams().ExtraHeaders {
		if llm.SensitiveHeader(k) {
			out = append(out, v)
		}
	}
	for _, v := range c.Tools.BashEnv.Extra {
		out = append(out, os.ExpandEnv(v))
	}
	var kept []string
	for _, s := range out {
		s = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(s, "Bearer "), "bearer "))
		if s != "" {
			kept = append(kept, s)
		}
	}
	return kept
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
	if len(c.Params.ExtraHeaders) > 0 {
		h := make(map[string]string, len(c.Params.ExtraHeaders))
		for k, v := range c.Params.ExtraHeaders {
			h[k] = os.ExpandEnv(v)
		}
		c.Params.ExtraHeaders = h
	}
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
		c.Params.MaxTokens = p.MaxTokens
	}
	c.Params.merge(p.Params)
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
	return c.ValidateStatic()
}

// ValidateStatic 检查与密钥无关的配置错误：权限规则、钩子正则、引用的文件是否存在。
// 热更新时任何一项出错都会保留上一份可用配置。
func (c *Config) ValidateStatic() error {
	if _, err := permission.NewChecker(c.Permissions, nil); err != nil {
		return fmt.Errorf("权限配置错误: %w", err)
	}
	if err := hook.NewManager().LoadConfig(c.Hooks, c.WorkDir); err != nil {
		return fmt.Errorf("钩子配置错误: %w", err)
	}
	for _, f := range c.PromptFiles() {
		if _, err := os.Stat(f); err != nil {
			return fmt.Errorf("提示词文件 %s 不存在", f)
		}
	}
	if p := c.EffectiveParams(); p.Temperature != nil && (*p.Temperature < 0 || *p.Temperature > 2) {
		return fmt.Errorf("params.temperature 应在 0~2 之间，当前 %v", *p.Temperature)
	} else if p.TopP != nil && (*p.TopP <= 0 || *p.TopP > 1) {
		return fmt.Errorf("params.top_p 应在 (0, 1] 之间，当前 %v", *p.TopP)
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
	cp.Params.ExtraHeaders = maskValues(c.Params.ExtraHeaders)
	cp.Tools.BashEnv.Extra = maskValues(c.Tools.BashEnv.Extra)
	b, _ := json.MarshalIndent(cp, "", "  ")
	return string(b)
}

// RedactedJSON 返回隐藏密钥后的配置（JSON 对象），用于 /v1/config 接口。
func (c *Config) RedactedJSON() json.RawMessage { return json.RawMessage(c.Redacted()) }

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
