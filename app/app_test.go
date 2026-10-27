package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmxj/goagent/config"
	"github.com/dmxj/goagent/hook"
	"github.com/dmxj/goagent/llm"
	"github.com/dmxj/goagent/mcp"
	"github.com/dmxj/goagent/permission"
	"github.com/dmxj/goagent/tool"
)

// testConfig 在临时目录中构造隔离的配置（不读取本机 HOME 与 Claude Code 配置）。
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	work := filepath.Join(root, "work")
	for _, d := range []string{home, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("GOAGENT_NO_CLAUDE_SETTINGS", "1")
	for _, k := range []string{"GOAGENT_PROVIDER", "GOAGENT_API_KEY", "GOAGENT_MODEL", "GOAGENT_BASE_URL", "GOAGENT_PERMISSION_MODE", "GOAGENT_MAX_TURNS", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		t.Setenv(k, "")
	}
	cfg, err := config.Load(work)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNewAssemblesEverything(t *testing.T) {
	cfg := testConfig(t)
	write(t, filepath.Join(cfg.ProjectDir, "skills", "x", "SKILL.md"), "---\nname: x\ndescription: 示例技能\n---\n技能正文")
	write(t, filepath.Join(cfg.ProjectDir, "skills", "bad", "SKILL.md"), "---\nname: bad\n---\n")
	write(t, filepath.Join(cfg.ProjectDir, "agents", "y.md"), "---\nname: y\ndescription: 示例子 Agent\ntools: [read_file]\n---\n你是 y")
	write(t, filepath.Join(cfg.UserDir, "skills", "user-skill", "SKILL.md"), "---\ndescription: 用户级技能\n---\n")
	write(t, filepath.Join(cfg.WorkDir, "AGENTS.md"), "# 项目约定\n使用中文注释")
	cfg.Tools.Disabled = []string{"web_fetch"}
	cfg.SystemPromptAppend = "额外说明 XYZ"

	extra := tool.Func("my_tool", "自定义工具", true, func(context.Context, struct{}, *tool.Call) (*tool.Result, error) { return tool.Text("ok"), nil })
	mock := llm.NewMock(llm.AssistantText("你好"))
	a, err := New(context.Background(), cfg, Options{Provider: mock, ExtraTools: []tool.Tool{extra}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if len(a.Skills()) != 2 || a.Skills()[0].Name != "user-skill" || a.Skills()[1].Name != "x" {
		t.Errorf("应发现用户级与项目级技能：%+v", a.Skills())
	}
	if len(a.Warnings()) != 1 || !strings.Contains(a.Warnings()[0], "缺少 description") {
		t.Errorf("无效技能应产生警告：%v", a.Warnings())
	}
	subs := map[string]bool{}
	for _, d := range a.Subagents() {
		subs[d.Name] = true
	}
	if !subs["y"] || !subs["explore"] || !subs["general-purpose"] {
		t.Errorf("应合并内置与自定义子 Agent：%v", subs)
	}
	want := []string{"bash", "edit_file", "glob", "grep", "list_dir", "memory_delete", "memory_save", "memory_search", "my_tool", "read_file", "skill", "task", "todo_write", "write_file"}
	if got := a.Agent().Tools.Names(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("工具注册不全：\n得到 %v\n期望 %v", got, want)
	}
	if a.Memory() == nil || a.Store == nil || a.Traces == nil || a.MCP == nil || a.Agent().Compactor == nil {
		t.Errorf("组件未初始化：%+v", a)
	}
	if a.Agent().Permissions.Mode() != permission.ModeDefault {
		t.Errorf("权限模式错误")
	}

	sp := a.Agent().SystemPrompt()
	for _, s := range []string{"**x**: 示例技能", "**y**: 示例子 Agent", "使用中文注释", "额外说明 XYZ", "长期记忆", cfg.WorkDir} {
		if !strings.Contains(sp, s) {
			t.Errorf("系统提示词应包含 %q", s)
		}
	}

	// 端到端运行一次并持久化
	sess := a.NewSession()
	if sess.Model != cfg.Model {
		t.Errorf("新会话应记录模型")
	}
	res, err := a.Run(context.Background(), sess, "hi", nil)
	if err != nil || res.Text != "你好" {
		t.Fatalf("运行失败：%+v %v", res, err)
	}
	loaded, err := a.LoadSession(sess.ID)
	if err != nil || len(loaded.Messages) != 2 {
		t.Errorf("会话应被保存：%v", err)
	}
	if s, _ := a.LoadSession(""); s == nil || s.ID == sess.ID {
		t.Errorf("空 ID 应新建会话")
	}
	if len(a.Traces.Spans(res.TraceID)) == 0 {
		t.Errorf("应记录 trace")
	}
	if files, _ := filepath.Glob(filepath.Join(cfg.ProjectDir, "traces", "*.jsonl")); len(files) != 1 {
		t.Errorf("应写入 trace 文件：%v", files)
	}
}

func TestNewOptionalFeatures(t *testing.T) {
	cfg := testConfig(t)
	cfg.Memory.Enabled = false
	cfg.Compact.Enabled = false
	cfg.Trace.Enabled = false
	off := false
	cfg.Tools.RestrictToWorkDir = &off
	cfg.Tools.RequireReadBeforeEdit = &off
	cfg.Tools.BashTimeoutSec = 5
	cfg.MCPServers = map[string]mcp.ServerConfig{"broken": {Command: "/nonexistent/server"}}
	a, err := New(context.Background(), cfg, Options{Provider: llm.NewMock()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.Memory() != nil || a.Agent().Compactor != nil {
		t.Errorf("关闭的功能不应初始化")
	}
	if _, ok := a.Agent().Tools.Get("memory_save"); ok {
		t.Errorf("关闭记忆时不应注册记忆工具")
	}
	if _, ok := a.Agent().Tools.Get("skill"); ok {
		t.Errorf("没有技能时不应注册 skill 工具")
	}
	found := false
	for _, w := range a.Warnings() {
		if strings.Contains(w, "MCP 服务器 broken 连接失败") {
			found = true
		}
	}
	if !found {
		t.Errorf("MCP 连接失败应记录警告：%v", a.Warnings())
	}
	if files, _ := filepath.Glob(filepath.Join(cfg.ProjectDir, "traces", "*.jsonl")); len(files) != 0 {
		t.Errorf("关闭 trace 时不应写文件")
	}
}

func TestNewErrors(t *testing.T) {
	t.Run("没有密钥且未注入 Provider", func(t *testing.T) {
		cfg := testConfig(t)
		if _, err := New(context.Background(), cfg, Options{}); err == nil || !strings.Contains(err.Error(), "API Key") {
			t.Errorf("应报告缺少密钥：%v", err)
		}
	})
	t.Run("mock provider 无需密钥", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.Provider.Type = "mock"
		a, err := New(context.Background(), cfg, Options{SkipMCP: true})
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		res, _ := a.Run(context.Background(), a.NewSession(), "回声", nil)
		if res.Text != "你说的是：回声" {
			t.Errorf("mock provider 应复述输入：%q", res.Text)
		}
	})
	t.Run("未知 provider", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.Provider = llm.ProviderConfig{Type: "nope", APIKey: "k"}
		if _, err := New(context.Background(), cfg, Options{}); err == nil {
			t.Errorf("未知 provider 应报错")
		}
	})
	t.Run("非法权限规则", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.Permissions.Allow = []string{"bash("}
		if _, err := New(context.Background(), cfg, Options{Provider: llm.NewMock()}); err == nil || !strings.Contains(err.Error(), "权限配置错误") {
			t.Errorf("应报告权限配置错误：%v", err)
		}
	})
	t.Run("非法钩子 matcher", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.Hooks = map[string][]hook.Config{"PreToolUse": {{Matcher: "[", Command: "true"}}}
		if _, err := New(context.Background(), cfg, Options{Provider: llm.NewMock()}); err == nil {
			t.Errorf("非法钩子配置应报错")
		}
	})
}
