// hooks 演示 Go 函数钩子：
//
//  1. PreToolUse：阻止危险的 rm -rf 命令（模型会收到阻止原因并改用其它做法）
//  2. PreToolUse：改写参数——给所有 go test 命令自动加上 -count=1
//  3. PostToolUse：审计日志，记录每一次工具调用的耗时与结果
//  4. UserPromptSubmit：给每条用户输入附加上下文
//  5. Stop：回答里没有"测试通过"时要求 Agent 继续工作
//
// 运行：go run ./examples/hooks （使用 Mock 模型，无需 API Key；bash 工具会在临时目录中执行）
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dmxj/goagent/agent"
	"github.com/dmxj/goagent/hook"
	"github.com/dmxj/goagent/llm"
	"github.com/dmxj/goagent/permission"
	"github.com/dmxj/goagent/session"
	"github.com/dmxj/goagent/tool"
	"github.com/dmxj/goagent/tool/builtin"
)

// dangerous 匹配 rm -rf / rm -fr 等递归强制删除。
var dangerous = regexp.MustCompile(`\brm\s+-[a-zA-Z]*(rf|fr)[a-zA-Z]*\b`)

// auditLog 是一个简单的审计日志（真实项目可以写文件或发送到日志系统）。
type auditLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *auditLog) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	line := time.Now().Format("15:04:05.000 ") + fmt.Sprintf(format, args...)
	l.entries = append(l.entries, line)
	fmt.Fprintln(os.Stderr, "[audit]", line)
}

func commandOf(input json.RawMessage) string {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &in)
	return in.Command
}

func registerHooks(h *hook.Manager, audit *auditLog) {
	must := func(err error) {
		if err != nil {
			log.Fatal(err)
		}
	}
	// matcher 是工具名正则；"" 或 "*" 表示所有工具
	must(h.On(hook.PreToolUse, "bash", func(ctx context.Context, in *hook.Input) (*hook.Output, error) {
		if cmd := commandOf(in.ToolInput); dangerous.MatchString(cmd) {
			audit.add("阻止危险命令：%s", cmd)
			return &hook.Output{Block: true, Reason: "禁止执行 rm -rf。如需清理，请列出具体文件并征得用户同意。"}, nil
		}
		return nil, nil
	}))
	must(h.On(hook.PreToolUse, "bash", func(ctx context.Context, in *hook.Input) (*hook.Output, error) {
		cmd := commandOf(in.ToolInput)
		if strings.HasPrefix(cmd, "go test") && !strings.Contains(cmd, "-count") {
			updated, _ := json.Marshal(map[string]string{"command": strings.Replace(cmd, "go test", "go test -count=1", 1)})
			return &hook.Output{UpdatedInput: updated}, nil
		}
		return nil, nil
	}))
	must(h.On(hook.PostToolUse, "", func(ctx context.Context, in *hook.Input) (*hook.Output, error) {
		audit.add("%s %s → error=%v，输出 %d 字节", in.ToolName, in.ToolInput, in.ToolError, len(in.ToolOutput))
		return nil, nil
	}))
	must(h.On(hook.UserPromptSubmit, "", func(ctx context.Context, in *hook.Input) (*hook.Output, error) {
		return &hook.Output{AdditionalContext: "当前时间：" + time.Now().Format(time.RFC3339)}, nil
	}))
	stops := 0
	must(h.On(hook.Stop, "", func(ctx context.Context, in *hook.Input) (*hook.Output, error) {
		stops++
		if !strings.Contains(in.LastReply, "测试通过") && stops <= 1 {
			return &hook.Output{Block: true, Reason: "请先运行测试并确认通过后再结束"}, nil
		}
		return nil, nil
	}))
}

func main() {
	dir, err := os.MkdirTemp("", "goagent-hooks-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	audit := &auditLog{}
	hooks := hook.NewManager()
	registerHooks(hooks, audit)

	// Mock 模型的剧本：先尝试 rm -rf（会被钩子阻止）→ 改为 echo → 结束 →
	// 被 Stop 钩子要求继续 → 运行"测试" → 回答测试通过
	mock := llm.NewMock(
		toolUse("c1", `{"command":"rm -rf ./build"}`),
		toolUse("c2", `{"command":"echo 清理完成"}`),
		llm.AssistantText("清理好了。"),
		toolUse("c3", `{"command":"echo go test 模拟输出: ok"}`),
		llm.AssistantText("测试通过，任务完成。"),
	)
	perms, _ := permission.NewChecker(permission.Config{Mode: permission.ModeBypass}, nil)
	a := &agent.Agent{
		Provider:    mock,
		Model:       "mock",
		Tools:       tool.NewRegistry(builtin.Bash(builtin.DefaultOptions())),
		Permissions: perms,
		Hooks:       hooks,
		WorkDir:     dir,
	}
	res, err := a.Run(context.Background(), session.New(), "清理构建产物并确认测试通过", func(ev agent.Event) {
		switch ev.Type {
		case agent.EventToolResult:
			fmt.Printf("⏺ bash %s\n  ⎿ %s\n", commandOf(ev.Tool.Input), firstLine(ev.Tool.Output))
		case agent.EventLog:
			fmt.Println("·", ev.Text)
		}
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n最终回答：%s（%d 轮）\n审计日志共 %d 条\n", res.Text, res.Turns, len(audit.entries))
}

func toolUse(id, input string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockToolUse, ID: id, Name: "bash", Input: json.RawMessage(input)},
	}}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
