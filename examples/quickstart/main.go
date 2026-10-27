// quickstart 演示 goagent 的最小用法：直接组装一个 agent.Agent 并打印事件流。
//
// 运行：
//
//	go run ./examples/quickstart                       # 没有密钥时使用离线 Mock 模型演示
//	GOAGENT_API_KEY=sk-xxx go run ./examples/quickstart # 使用真实模型（默认 Anthropic 协议）
//
// 使用 OpenAI 兼容服务（DeepSeek、通义、Kimi、Ollama……）：
//
//	GOAGENT_PROVIDER=openai GOAGENT_BASE_URL=https://api.deepseek.com/v1 \
//	GOAGENT_MODEL=deepseek-chat GOAGENT_API_KEY=sk-xxx go run ./examples/quickstart
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/dmxj/goagent/agent"
	"github.com/dmxj/goagent/llm"
	"github.com/dmxj/goagent/permission"
	"github.com/dmxj/goagent/session"
	"github.com/dmxj/goagent/tool"
	"github.com/dmxj/goagent/tool/builtin"
)

func main() {
	provider, model := chooseProvider()

	// 只读工具 + 只读权限（plan 模式），示例程序不会修改你的文件
	opts := builtin.DefaultOptions()
	tools := tool.NewRegistry(builtin.ReadFile(opts), builtin.ListDir(opts), builtin.Glob(opts), builtin.Grep(opts))
	perms, err := permission.NewChecker(permission.Config{Mode: permission.ModePlan}, permission.AutoDeny)
	if err != nil {
		log.Fatal(err)
	}
	wd, _ := os.Getwd()

	a := &agent.Agent{
		Provider:    provider,
		Model:       model,
		Tools:       tools,
		Permissions: perms,
		WorkDir:     wd,
		MaxTurns:    10,
		System: func() string {
			return "你是一个简洁的 Go 助手，用中文回答。需要了解项目时先用工具查看文件。"
		},
	}

	sess := session.New()
	res, err := a.Run(context.Background(), sess, "列出当前目录的内容，然后用一句话介绍这个项目。", printEvent)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n\n== 完成：%d 轮，输入 %d / 输出 %d tokens，trace=%s ==\n",
		res.Turns, res.Usage.InputTokens, res.Usage.OutputTokens, res.TraceID)
}

// chooseProvider 有 GOAGENT_API_KEY 时连接真实模型，否则返回一个"会先调用工具再回答"的 Mock。
func chooseProvider() (llm.Provider, string) {
	if key := os.Getenv("GOAGENT_API_KEY"); key != "" {
		cfg := llm.ProviderConfig{Type: os.Getenv("GOAGENT_PROVIDER"), BaseURL: os.Getenv("GOAGENT_BASE_URL"), APIKey: key}
		p, err := llm.NewProvider(cfg)
		if err != nil {
			log.Fatal(err)
		}
		model := os.Getenv("GOAGENT_MODEL")
		if model == "" {
			model = "claude-sonnet-4-5"
		}
		return p, model
	}
	fmt.Println("（未设置 GOAGENT_API_KEY，使用离线 Mock 模型演示）")
	return llm.NewMock(
		llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: llm.BlockText, Text: "我先看看目录结构。"},
			llm.ToolCall("call_1", "list_dir", map[string]any{"path": ".", "depth": 1}),
		}},
		llm.AssistantText("这是一个用 Go 编写、零第三方依赖的 Agent 框架示例目录。"),
	), "mock"
}

// printEvent 把事件流渲染到终端：文本逐字输出，工具调用与结果单独成行。
func printEvent(ev agent.Event) {
	switch ev.Type {
	case agent.EventText:
		fmt.Print(ev.Text)
	case agent.EventToolCall:
		fmt.Printf("\n⏺ 调用 %s %s\n", ev.Tool.Name, ev.Tool.Input)
	case agent.EventToolResult:
		out := ev.Tool.Output
		if len([]rune(out)) > 200 {
			out = string([]rune(out)[:200]) + "…"
		}
		fmt.Printf("  ⎿ %s\n", out)
	case agent.EventError:
		fmt.Printf("\n✗ %s\n", ev.Error)
	}
}
