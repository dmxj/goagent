package hook

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestManagerRunMerge(t *testing.T) {
	m := NewManager()
	var seenInput string
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(m.On(PreToolUse, "bash", func(ctx context.Context, in *Input) (*Output, error) {
		return &Output{AdditionalContext: "上下文一", UpdatedInput: json.RawMessage(`{"command":"ls -la"}`)}, nil
	}))
	must(m.On(PreToolUse, "bash|grep", func(ctx context.Context, in *Input) (*Output, error) {
		seenInput = string(in.ToolInput)
		return &Output{Block: true, Reason: "原因一", AdditionalContext: "上下文二"}, nil
	}))
	must(m.On(PreToolUse, "*", func(ctx context.Context, in *Input) (*Output, error) {
		return &Output{Block: true, Reason: "原因二"}, nil
	}))
	must(m.On(PreToolUse, "read_file", func(ctx context.Context, in *Input) (*Output, error) {
		t.Error("matcher 不匹配的钩子不应被执行")
		return nil, nil
	}))
	must(m.On(PreToolUse, "", func(ctx context.Context, in *Input) (*Output, error) { return nil, nil }))
	must(m.On(PreToolUse, "", func(ctx context.Context, in *Input) (*Output, error) { return nil, errors.New("钩子坏了") }))

	out, err := m.Run(context.Background(), &Input{Event: PreToolUse, ToolName: "bash", ToolInput: json.RawMessage(`{"command":"ls"}`)})
	if err == nil || !strings.Contains(err.Error(), "钩子坏了") {
		t.Errorf("出错的钩子应在 error 中报告：%v", err)
	}
	if !out.Block || out.Reason != "原因一\n原因二" {
		t.Errorf("Block/Reason 合并错误：%+v", out)
	}
	if out.AdditionalContext != "上下文一\n上下文二" {
		t.Errorf("AdditionalContext 合并错误：%q", out.AdditionalContext)
	}
	if string(out.UpdatedInput) != `{"command":"ls -la"}` {
		t.Errorf("UpdatedInput 错误：%s", out.UpdatedInput)
	}
	if seenInput != `{"command":"ls -la"}` {
		t.Errorf("后续钩子应看到更新后的参数，实际 %s", seenInput)
	}
	if m.Count(PreToolUse) != 6 || m.Count(Stop) != 0 {
		t.Errorf("Count 错误")
	}
	if err := m.On(PreToolUse, "(", nil); err == nil {
		t.Errorf("非法正则 matcher 应报错")
	}
}

func TestNilManager(t *testing.T) {
	var m *Manager
	out, err := m.Run(context.Background(), &Input{Event: Stop})
	if err != nil || out == nil || out.Block {
		t.Errorf("nil Manager 应返回空结果")
	}
	if m.Count(Stop) != 0 {
		t.Errorf("nil Manager Count 应为 0")
	}
}

func TestCommandHandler(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("命令钩子测试依赖 /bin/sh")
	}
	dir := t.TempDir()
	tests := []struct {
		name    string
		command string
		wantErr bool
		want    *Output
	}{
		{"exit 0 输出 JSON", `echo '{"block":true,"reason":"不行","additional_context":"ctx"}'`, false, &Output{Block: true, Reason: "不行", AdditionalContext: "ctx"}},
		{"exit 0 普通文本作为上下文", `echo "当前分支 main"`, false, &Output{AdditionalContext: "当前分支 main"}},
		{"exit 0 无输出", `true`, false, nil},
		{"exit 0 非法 JSON 当作文本", `echo '{oops'`, false, &Output{AdditionalContext: "{oops"}},
		{"exit 2 阻止并以 stderr 为原因", `echo "危险命令" 1>&2; exit 2`, false, &Output{Block: true, Reason: "危险命令"}},
		{"其它退出码视为钩子出错", `echo bad 1>&2; exit 1`, true, nil},
		{"从 stdin 读取事件 JSON", `grep -q '"tool_name":"bash"' && echo matched`, false, &Output{AdditionalContext: "matched"}},
		{"环境变量", `echo "$GOAGENT_HOOK_EVENT/$GOAGENT_TOOL_NAME"`, false, &Output{AdditionalContext: "PreToolUse/bash"}},
		{"在工作目录中执行", `basename "$(pwd)"`, false, &Output{AdditionalContext: filepath.Base(dir)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := CommandHandler(Config{Command: tt.command, Timeout: 10}, dir)
			out, err := h(context.Background(), &Input{Event: PreToolUse, ToolName: "bash", ToolInput: json.RawMessage(`{"command":"ls"}`)})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v，期望出错=%v", err, tt.wantErr)
			}
			if tt.want == nil {
				if out != nil {
					t.Errorf("期望 nil 输出，实际 %+v", out)
				}
				return
			}
			if out == nil || out.Block != tt.want.Block || out.Reason != tt.want.Reason || out.AdditionalContext != tt.want.AdditionalContext {
				t.Errorf("输出 = %+v，期望 %+v", out, tt.want)
			}
		})
	}
	t.Run("超时", func(t *testing.T) {
		h := CommandHandler(Config{Command: "sleep 10", Timeout: 1}, dir)
		if _, err := h(context.Background(), &Input{Event: Stop}); err == nil {
			t.Errorf("超时应返回错误")
		}
	})
}

func TestLoadConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("命令钩子测试依赖 /bin/sh")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "guard.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho blocked >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := NewManager()
	err := m.LoadConfig(map[string][]Config{
		"PreToolUse": {{Matcher: "bash", Command: script}, {Command: "  "}},
		"Stop":       {{Command: "echo done"}},
	}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Count(PreToolUse) != 1 || m.Count(Stop) != 1 {
		t.Fatalf("空命令应被忽略：Pre=%d Stop=%d", m.Count(PreToolUse), m.Count(Stop))
	}
	out, _ := m.Run(context.Background(), &Input{Event: PreToolUse, ToolName: "bash"})
	if !out.Block || out.Reason != "blocked" {
		t.Errorf("脚本钩子应阻止：%+v", out)
	}
	out, _ = m.Run(context.Background(), &Input{Event: PreToolUse, ToolName: "read_file"})
	if out.Block {
		t.Errorf("matcher 不匹配时不应阻止")
	}
	out, err = m.Run(context.Background(), &Input{Event: Stop})
	if err != nil || out.AdditionalContext != "done" {
		t.Errorf("Stop 钩子输出错误：%+v %v", out, err)
	}
	if err := NewManager().LoadConfig(map[string][]Config{"PreToolUse": {{Matcher: "[", Command: "true"}}}, dir); err == nil {
		t.Errorf("非法 matcher 应报错")
	}
	m2 := NewManager()
	_ = m2.LoadConfig(map[string][]Config{"Stop": {{Command: "exit 1"}}}, dir)
	if _, err := m2.Run(context.Background(), &Input{Event: Stop}); err == nil || !strings.Contains(err.Error(), "exit 1") {
		t.Errorf("错误信息应包含钩子命令：%v", err)
	}
}
