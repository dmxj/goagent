package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmxj/goagent/agent"
	"github.com/dmxj/goagent/app"
	"github.com/dmxj/goagent/config"
	"github.com/dmxj/goagent/llm"
	"github.com/dmxj/goagent/trace"
)

func TestRunDispatch(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, args := range [][]string{{"version"}, {"help"}, {"--help"}} {
		if err := run(args); err != nil {
			t.Errorf("run(%v) 不应出错：%v", args, err)
		}
	}
	if err := run([]string{"nope"}); err == nil || !strings.Contains(err.Error(), "未知命令") {
		t.Errorf("未知命令应报错：%v", err)
	}
}

func TestHelpers(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	tests := []struct {
		input string
		want  string
	}{
		{`{"command":"go test ./..."}`, "(go test ./...)"},
		{`{"path":"a.go","offset":1}`, "(a.go)"},
		{`{"n":1}`, `{"n":1}`},
		{`not json`, ""},
	}
	for _, tt := range tests {
		if got := summarizeInput(json.RawMessage(tt.input)); got != tt.want {
			t.Errorf("summarizeInput(%s) = %q，期望 %q", tt.input, got, tt.want)
		}
	}
	if got := firstLines("a\nb\nc\nd", 2); !strings.HasPrefix(got, "a\nb") || !strings.Contains(got, "共 4 行") {
		t.Errorf("firstLines 错误：%q", got)
	}
	if got := clip("第一行\n第二行很长很长", 5); got != "第一行 第…" {
		t.Errorf("clip 错误：%q", got)
	}
	if red("x") != "x" {
		t.Errorf("NO_COLOR 时不应输出颜色码")
	}
}

func TestPrinterHandlesAllEvents(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	p := &printer{}
	events := []agent.Event{
		{Type: agent.EventThinking, Agent: "main", Text: "想"},
		{Type: agent.EventText, Agent: "main", Text: "答\n复"},
		{Type: agent.EventText, Agent: "main>explore#1", Text: "子 Agent 文本（非 verbose 隐藏）"},
		{Type: agent.EventToolCall, Agent: "main", Tool: &agent.ToolEvent{Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}},
		{Type: agent.EventToolResult, Agent: "main>explore#1", Tool: &agent.ToolEvent{Name: "bash", Output: "a\nb", IsError: true}},
		{Type: agent.EventCompact, Data: map[string]any{"tokens_before": 10, "tokens_after": 2}},
		{Type: agent.EventLog, Text: "日志"},
		{Type: agent.EventError, Error: "错误"},
		{Type: agent.EventRunEnd, Agent: "main>x", Usage: &llm.Usage{}},
		{Type: agent.EventRunEnd, Agent: "main", Usage: &llm.Usage{InputTokens: 1}, Data: map[string]any{"turns": 1}},
	}
	for _, ev := range events {
		p.handle(ev)
	}
	if p.inText || p.inThink {
		t.Errorf("run_end 后应结束当前行")
	}
}

func TestCmdTrace(t *testing.T) {
	dir := t.TempDir()
	if err := cmdTrace([]string{"-dir", dir}); err == nil {
		t.Errorf("没有 trace 文件时应报错")
	}
	path := filepath.Join(dir, ".goagent", "traces", "20261027.jsonl")
	f, err := trace.NewFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Export(trace.SpanData{TraceID: "t", SpanID: "s", Name: "agent.run", Kind: trace.KindAgent, Status: "ok"})
	_ = f.Close()
	if err := cmdTrace([]string{"-dir", dir}); err != nil {
		t.Errorf("读取最新 trace 文件失败：%v", err)
	}
	if err := cmdTrace([]string{filepath.Join(dir, "missing.jsonl")}); err == nil {
		t.Errorf("指定不存在的文件应报错")
	}
}

func TestCmdConfigAndSessions(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("GOAGENT_NO_CLAUDE_SETTINGS", "1")
	t.Setenv("GOAGENT_API_KEY", "")
	work := filepath.Join(root, "work")
	_ = os.MkdirAll(work, 0o755)
	if err := cmdConfig([]string{"-dir", work, "-model", "m[1m]", "-mode", "plan"}); err != nil {
		t.Errorf("config 命令失败：%v", err)
	}
	if err := cmdSessions([]string{"-dir", work}); err != nil {
		t.Errorf("sessions 命令失败：%v", err)
	}
	if err := cmdConfig([]string{"-dir", work, "-profile", "missing"}); err == nil {
		t.Errorf("不存在的 profile 应报错")
	}
}

// mockEnv 准备一个使用 mock provider 的隔离环境，返回工作目录。
func mockEnv(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("NO_COLOR", "1")
	t.Setenv("GOAGENT_NO_CLAUDE_SETTINGS", "1")
	t.Setenv("GOAGENT_PROVIDER", "mock")
	t.Setenv("GOAGENT_PERMISSION_MODE", "")
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	return work
}

func TestCmdRunWithMock(t *testing.T) {
	work := mockEnv(t)
	if err := run([]string{"run", "-dir", work, "-p", "你好"}); err != nil {
		t.Fatalf("run 命令失败：%v", err)
	}
	if err := run([]string{"run", "-dir", work, "-json", "以 JSON 输出"}); err != nil {
		t.Fatalf("run -json 失败：%v", err)
	}
	files, _ := filepath.Glob(filepath.Join(work, ".goagent", "sessions", "*.json"))
	if len(files) != 2 {
		t.Errorf("每次 run 应保存一个会话，实际 %d", len(files))
	}
	if err := run([]string{"run", "-dir", work, "-resume", "missing", "-p", "x"}); err == nil {
		t.Errorf("恢复不存在的会话应报错")
	}
}

func TestSlashCommands(t *testing.T) {
	work := mockEnv(t)
	cfg, err := (&common{dir: work}).load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Profiles = map[string]config.ModelProfile{"fast": {Model: "fast-model"}}
	a, err := app.New(context.Background(), cfg, app.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	sess := a.NewSession()
	if _, err := a.Run(context.Background(), sess, "第一句", nil); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tests := []struct {
		line    string
		wantErr bool
		quit    bool
	}{
		{"/help", false, false},
		{"/tools", false, false},
		{"/skills", false, false},
		{"/agents", false, false},
		{"/memory", false, false},
		{"/trace", false, false},
		{"/files", false, false},
		{"/usage", false, false},
		{"/sessions", false, false},
		{"/model", false, false},
		{"/model fast", false, false},
		{"/model other-model", false, false},
		{"/mode", false, false},
		{"/mode plan", false, false},
		{"/mode yolo", true, false},
		{"/config", false, false},
		{"/reload", false, false},
		{"/compact", true, false}, // 历史太短
		{"/resume", true, false},
		{"/resume " + sess.ID, false, false},
		{"/new", false, false},
		{"/unknown", true, false},
		{"/exit", false, true},
	}
	for _, tt := range tests {
		quit, err := slash(ctx, a, &sess, tt.line)
		if (err != nil) != tt.wantErr || quit != tt.quit {
			t.Errorf("%s：quit=%v err=%v，期望 quit=%v 出错=%v", tt.line, quit, err, tt.quit, tt.wantErr)
		}
	}
	if a.Agent().Model != "other-model" || a.Agent().Permissions.Mode() != "plan" {
		t.Errorf("/model 与 /mode 应生效：%s %s", a.Agent().Model, a.Agent().Permissions.Mode())
	}
}

// withStdin 在测试期间把 os.Stdin 替换为给定内容。
func withStdin(t *testing.T, input string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(p, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = old
		f.Close()
	})
}

func TestCmdChatREPL(t *testing.T) {
	work := mockEnv(t)
	withStdin(t, "\n/help\n你好\n/unknown\n/exit\n")
	if err := run([]string{"-dir", work}); err != nil {
		t.Fatalf("chat 失败：%v", err)
	}
	files, _ := filepath.Glob(filepath.Join(work, ".goagent", "sessions", "*.json"))
	if len(files) != 1 {
		t.Errorf("对话后应保存会话，实际 %d 个", len(files))
	}
	// 输入结束（EOF）时正常退出
	withStdin(t, "")
	if err := run([]string{"chat", "-dir", work}); err != nil {
		t.Errorf("EOF 时应正常退出：%v", err)
	}
}

func TestCmdRunFromStdin(t *testing.T) {
	work := mockEnv(t)
	withStdin(t, "从标准输入读取的任务")
	if err := run([]string{"run", "-dir", work}); err != nil {
		t.Fatalf("应从 stdin 读取任务：%v", err)
	}
	withStdin(t, "")
	if err := run([]string{"run", "-dir", work}); err == nil {
		t.Errorf("没有任务时应报错")
	}
}

func TestInterrupter(t *testing.T) {
	exited := 0
	now := time.Unix(1000, 0)
	i := newInterrupter(func() { exited++ })
	i.now = func() time.Time { return now }
	// 空闲时按一次只提示，不影响之后的任务
	if got := i.handle(); got != "hint" {
		t.Fatalf("空闲时第一次 Ctrl+C 应只提示，得到 %s", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	i.begin(cancel)
	if ctx.Err() != nil {
		t.Fatal("空闲时按下的 Ctrl+C 不应中断下一个任务")
	}
	if got := i.handle(); got != "cancelled" || ctx.Err() == nil {
		t.Errorf("运行中 Ctrl+C 应取消当前任务：%s", got)
	}
	i.end()
	// 空闲时 2 秒内连按两次退出；间隔超过 2 秒重新计时
	i.handle()
	now = now.Add(3 * time.Second)
	if i.handle() != "hint" || exited != 0 {
		t.Errorf("间隔超过 2 秒不应退出")
	}
	now = now.Add(time.Second)
	if i.handle() != "exit" || exited != 1 {
		t.Errorf("2 秒内连按两次应退出")
	}
}

func TestRunJSONDoesNotEscapeAgentPath(t *testing.T) {
	var buf strings.Builder
	jsonlSink(&buf)(agent.Event{Type: agent.EventText, Agent: "main>explore#1", Text: "<b>"})
	if !strings.Contains(buf.String(), `"agent":"main>explore#1"`) || !strings.Contains(buf.String(), "<b>") {
		t.Errorf("JSONL 输出中的 Agent 路径不应被转义：%s", buf.String())
	}
}
