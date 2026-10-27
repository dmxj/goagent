package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmxj/goagent/llm"
	"github.com/dmxj/goagent/mcp"
	"github.com/dmxj/goagent/permission"
)

// isolate 隔离本机配置：临时 HOME、禁止导入 Claude Code 配置、清空相关环境变量。
func isolate(t *testing.T) (home, work string) {
	t.Helper()
	root := t.TempDir()
	home = filepath.Join(root, "home")
	work = filepath.Join(root, "project")
	for _, d := range []string{home, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GOAGENT_NO_CLAUDE_SETTINGS", "1")
	for _, k := range []string{
		"GOAGENT_PROVIDER", "GOAGENT_BASE_URL", "GOAGENT_API_KEY", "GOAGENT_MODEL", "GOAGENT_PERMISSION_MODE", "GOAGENT_MAX_TURNS",
		"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "OPENAI_API_KEY", "OPENAI_BASE_URL", "DEEPSEEK_API_KEY", "DASHSCOPE_API_KEY",
	} {
		t.Setenv(k, "")
	}
	return home, work
}

func writeJSON(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDefaults(t *testing.T) {
	_, work := isolate(t)
	c, err := Load(work)
	if err != nil {
		t.Fatal(err)
	}
	if c.Provider.Type != "anthropic" || c.Model == "" || c.MaxTurns != 60 || !c.Memory.Enabled || !c.Compact.Enabled || !c.Trace.Enabled {
		t.Errorf("默认值错误：%+v", c)
	}
	if c.Permissions.Mode != permission.ModeDefault {
		t.Errorf("默认权限模式错误")
	}
	if c.WorkDir != work || c.ProjectDir != filepath.Join(work, ".goagent") || !strings.HasSuffix(c.UserDir, filepath.Join("home", ".goagent")) {
		t.Errorf("目录错误：%s %s %s", c.WorkDir, c.ProjectDir, c.UserDir)
	}
	if len(c.Sources) != 0 {
		t.Errorf("没有任何配置文件时 Sources 应为空：%v", c.Sources)
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "API Key") {
		t.Errorf("没有密钥时 Validate 应报错：%v", err)
	}
}

func TestLayeredMerge(t *testing.T) {
	home, work := isolate(t)
	writeJSON(t, filepath.Join(home, ".goagent", "config.json"), `{
		"provider": {"type": "openai", "base_url": "https://user.example/v1", "api_key": "user-key-0000000000"},
		"model": "user-model",
		"max_turns": 10,
		"permissions": {"mode": "accept_edits", "allow": ["bash(go test:*)"]},
		"tools": {"disabled": ["web_fetch"]}
	}`)
	writeJSON(t, filepath.Join(work, ".goagent", "config.json"), `{
		"model": "project-model[1m]",
		"permissions": {"deny": ["bash(rm -rf:*)"]},
		"memory": {"enabled": false},
		"hooks": {"PreToolUse": [{"matcher": "bash", "command": "echo $GOAGENT_TOOL_NAME"}]}
	}`)
	c, err := Load(work)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"用户级 provider 保留", c.Provider.Type, "openai"},
		{"用户级 base_url 保留", c.Provider.BaseURL, "https://user.example/v1"},
		{"项目级覆盖模型并去掉后缀", c.Model, "project-model"},
		{"用户级 max_turns", c.MaxTurns, 10},
		{"用户级权限模式", c.Permissions.Mode, permission.ModeAcceptEdits},
		{"用户级 allow 保留", strings.Join(c.Permissions.Allow, ","), "bash(go test:*)"},
		{"项目级 deny", strings.Join(c.Permissions.Deny, ","), "bash(rm -rf:*)"},
		{"项目级关闭记忆", c.Memory.Enabled, false},
		{"未设置的默认值保留", c.Compact.Enabled, true},
		{"禁用工具", strings.Join(c.Tools.Disabled, ","), "web_fetch"},
		{"钩子命令中的 $ 变量不应在加载时展开", c.Hooks["PreToolUse"][0].Command, "echo $GOAGENT_TOOL_NAME"},
		{"配置来源", len(c.Sources), 2},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s：得到 %v，期望 %v", tt.name, tt.got, tt.want)
		}
	}
	if err := c.Validate(); err != nil {
		t.Errorf("有密钥时 Validate 应通过：%v", err)
	}
}

func TestInvalidConfigFile(t *testing.T) {
	_, work := isolate(t)
	writeJSON(t, filepath.Join(work, ".goagent", "config.json"), `{"model": `)
	if _, err := Load(work); err == nil || !strings.Contains(err.Error(), "格式错误") {
		t.Errorf("非法 JSON 应报错：%v", err)
	}
}

func TestEnvExpansionInConfig(t *testing.T) {
	_, work := isolate(t)
	t.Setenv("MY_KEY", "sk-from-env-1234567890")
	t.Setenv("MCP_DIR", "/opt/mcp")
	writeJSON(t, filepath.Join(work, ".goagent", "config.json"), `{
		"provider": {"api_key": "${MY_KEY}", "headers": {"X-Key": "$MY_KEY"}},
		"profiles": {"p": {"provider": {"type": "openai", "api_key": "${MY_KEY}"}, "model": "m"}},
		"mcp_servers": {"fs": {"command": "${MCP_DIR}/server", "args": ["--root", "$MCP_DIR"], "env": {"TOKEN": "${MY_KEY}"}}}
	}`)
	c, err := Load(work)
	if err != nil {
		t.Fatal(err)
	}
	if c.Provider.APIKey != "sk-from-env-1234567890" || c.Provider.Headers["X-Key"] != "sk-from-env-1234567890" || c.Profiles["p"].Provider.APIKey != "sk-from-env-1234567890" {
		t.Errorf("provider 字段中的环境变量应展开：%+v", c.Provider)
	}
	fs := c.MCPServers["fs"]
	if fs.Command != "/opt/mcp/server" || fs.Args[1] != "/opt/mcp" {
		t.Errorf("MCP command/args 应展开：%+v", fs)
	}
	if fs.Env["TOKEN"] != "${MY_KEY}" {
		t.Errorf("MCP env 由 mcp 包在连接时展开，配置中应保持原样：%q", fs.Env["TOKEN"])
	}
}

func TestEnvOverride(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		check func(t *testing.T, c *Config)
	}{
		{"GOAGENT_* 覆盖", map[string]string{"GOAGENT_PROVIDER": "openai", "GOAGENT_BASE_URL": "http://localhost:11434/v1", "GOAGENT_API_KEY": "k", "GOAGENT_MODEL": "qwen[128k]", "GOAGENT_PERMISSION_MODE": "plan", "GOAGENT_MAX_TURNS": "7"},
			func(t *testing.T, c *Config) {
				if c.Provider.Type != "openai" || c.Provider.BaseURL != "http://localhost:11434/v1" || c.Provider.APIKey != "k" || c.Model != "qwen" || c.Permissions.Mode != permission.ModePlan || c.MaxTurns != 7 {
					t.Errorf("环境变量覆盖失败：%+v", c)
				}
				if c.Sources[len(c.Sources)-1] != "env" {
					t.Errorf("Sources 应记录 env")
				}
			}},
		{"回退到 ANTHROPIC_*", map[string]string{"ANTHROPIC_AUTH_TOKEN": "a-token", "ANTHROPIC_BASE_URL": "https://gw.example"},
			func(t *testing.T, c *Config) {
				if c.Provider.APIKey != "a-token" || c.Provider.BaseURL != "https://gw.example" {
					t.Errorf("应回退到 Anthropic 环境变量：%+v", c.Provider)
				}
			}},
		{"回退到 OPENAI_* / DEEPSEEK_*", map[string]string{"GOAGENT_PROVIDER": "openai", "DEEPSEEK_API_KEY": "ds-key", "OPENAI_BASE_URL": "https://api.deepseek.com/v1"},
			func(t *testing.T, c *Config) {
				if c.Provider.APIKey != "ds-key" || c.Provider.BaseURL != "https://api.deepseek.com/v1" {
					t.Errorf("应回退到 OpenAI 兼容环境变量：%+v", c.Provider)
				}
			}},
		{"非法 MAX_TURNS 忽略", map[string]string{"GOAGENT_MAX_TURNS": "abc"},
			func(t *testing.T, c *Config) {
				if c.MaxTurns != 60 {
					t.Errorf("非法值应忽略：%d", c.MaxTurns)
				}
			}},
		{"本地模型无需密钥", map[string]string{"GOAGENT_PROVIDER": "openai", "GOAGENT_BASE_URL": "http://127.0.0.1:8000/v1"},
			func(t *testing.T, c *Config) {
				if err := c.Validate(); err != nil {
					t.Errorf("本地地址不应要求密钥：%v", err)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, work := isolate(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			c, err := Load(work)
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, c)
		})
	}
}

func TestImportClaudeSettings(t *testing.T) {
	home, work := isolate(t)
	t.Setenv("GOAGENT_NO_CLAUDE_SETTINGS", "")
	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), `{"env": {"ANTHROPIC_AUTH_TOKEN": "fake-token-for-test-only", "ANTHROPIC_BASE_URL": "https://proxy.example", "ANTHROPIC_MODEL": "glm-4.6[1m]"}}`)
	c, err := Load(work)
	if err != nil {
		t.Fatal(err)
	}
	if c.Provider.APIKey != "fake-token-for-test-only" || c.Provider.BaseURL != "https://proxy.example" || c.Model != "glm-4.6" {
		t.Errorf("应从 Claude Code 配置导入：%+v model=%s", c.Provider, c.Model)
	}
	if len(c.Sources) != 1 || !strings.HasSuffix(c.Sources[0], "settings.json") {
		t.Errorf("Sources 应记录 settings.json：%v", c.Sources)
	}
	// 项目配置优先于导入的配置
	writeJSON(t, filepath.Join(work, ".goagent", "config.json"), `{"model": "project-model"}`)
	if c, _ := Load(work); c.Model != "project-model" {
		t.Errorf("项目配置应覆盖导入的模型")
	}
	// 没有密钥时不导入；格式错误时忽略
	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), `{"env": {"ANTHROPIC_MODEL": "x"}}`)
	if c, _ := Load(work); c.Provider.APIKey != "" {
		t.Errorf("没有密钥时不应导入")
	}
	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), `{broken`)
	if _, err := Load(work); err != nil {
		t.Errorf("settings.json 损坏时应忽略：%v", err)
	}
}

func TestCleanModel(t *testing.T) {
	tests := map[string]string{
		"deepseek-v4[1m]":     "deepseek-v4",
		" claude-sonnet-4-5 ": "claude-sonnet-4-5",
		"qwen[128k] ":         "qwen",
		"model[a][b]":         "model[a]",
		"plain":               "plain",
		"":                    "",
	}
	for in, want := range tests {
		if got := CleanModel(in); got != want {
			t.Errorf("CleanModel(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestUseProfile(t *testing.T) {
	c := Default()
	c.Profiles = map[string]ModelProfile{
		"deepseek":   {Provider: llm.ProviderConfig{Type: "openai", BaseURL: "https://api.deepseek.com/v1", APIKey: "k"}, Model: "deepseek-chat[64k]", ContextWindow: 64000, MaxTokens: 4096},
		"model-only": {Model: "claude-opus-4"},
	}
	if err := c.UseProfile("missing"); err == nil {
		t.Errorf("不存在的配置应报错")
	}
	if err := c.UseProfile("deepseek"); err != nil {
		t.Fatal(err)
	}
	if c.Provider.Type != "openai" || c.Model != "deepseek-chat" || c.ContextWindow != 64000 || c.MaxTokens != 4096 {
		t.Errorf("UseProfile 未生效：%+v", c)
	}
	if err := c.UseProfile("model-only"); err != nil {
		t.Fatal(err)
	}
	if c.Provider.Type != "openai" || c.Model != "claude-opus-4" || c.ContextWindow != 64000 {
		t.Errorf("只设置模型的配置不应改变 Provider：%+v", c)
	}
}

func TestRedacted(t *testing.T) {
	secret := "sk-ant-api03-very-secret-value-XYZ1"
	c := Default()
	c.Provider.APIKey = secret
	c.Provider.Headers = map[string]string{"X-Api-Key": "header-secret-value"}
	c.Profiles = map[string]ModelProfile{"p": {Provider: llm.ProviderConfig{APIKey: "profile-secret-value-123"}}}
	c.MCPServers = map[string]mcp.ServerConfig{"gh": {Command: "gh-mcp", Env: map[string]string{"GITHUB_TOKEN": "ghp_supersecrettoken"}, Headers: map[string]string{"Authorization": "Bearer abcdefgh"}}}
	out := c.Redacted()
	for _, s := range []string{secret, "header-secret-value", "profile-secret-value-123", "ghp_supersecrettoken", "Bearer abcdefgh", "very-secret"} {
		if strings.Contains(out, s) {
			t.Errorf("Redacted 输出泄露了密钥 %q：\n%s", s, out)
		}
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("Redacted 应输出合法 JSON：%v", err)
	}
	if !strings.Contains(out, "sk-a") || !strings.Contains(out, "gh-mcp") {
		t.Errorf("应保留可辨认的前缀与非敏感字段：\n%s", out)
	}
	// 不修改原配置
	if c.Provider.APIKey != secret || c.MCPServers["gh"].Env["GITHUB_TOKEN"] != "ghp_supersecrettoken" || c.Provider.Headers["X-Api-Key"] != "header-secret-value" {
		t.Errorf("Redacted 不应修改原配置")
	}
	if mask("short") != "*****" {
		t.Errorf("短密钥应完全隐藏")
	}
}

// TestExampleConfig 保证仓库中的示例配置始终能被正确解析。
func TestExampleConfig(t *testing.T) {
	_, work := isolate(t)
	b, err := os.ReadFile(filepath.Join("..", ".goagent", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(work, ".goagent", "config.json"), string(b))
	t.Setenv("ANTHROPIC_API_KEY", "")
	c, err := Load(work)
	if err != nil {
		t.Fatalf("示例配置无法解析：%v", err)
	}
	if len(c.Profiles) < 4 || len(c.Permissions.Allow) == 0 || len(c.Hooks["PreToolUse"]) != 1 || c.MCPServers["demo"].Command != "go" {
		t.Errorf("示例配置内容不完整：%+v", c)
	}
	if err := c.UseProfile("deepseek"); err != nil || c.Provider.Type != "openai" {
		t.Errorf("示例 profile 切换失败：%v", err)
	}
	if _, err := permission.NewChecker(c.Permissions, nil); err != nil {
		t.Errorf("示例权限规则无效：%v", err)
	}
}
