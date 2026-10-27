package subagent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/dmxj/goagent/agent"
	"github.com/dmxj/goagent/hook"
	"github.com/dmxj/goagent/llm"
	"github.com/dmxj/goagent/permission"
	"github.com/dmxj/goagent/session"
	"github.com/dmxj/goagent/tool"
	"github.com/dmxj/goagent/trace"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"reviewer.md":   "---\nname: code-reviewer\ndescription: 审查代码\ntools: [read_file, grep]\nmodel: deepseek-chat\nmax_turns: 12\n---\n你是严格的审查者。\n",
		"tester.md":     "---\ndescription: 跑测试\ntools:\n  - bash\n  - read_file\n---\n运行 go test",
		"broken.md":     "---\nname: broken\n---\n缺少描述",
		"notes.txt":     "不是 md",
		"bad-turns.md":  "---\ndescription: d\nmax_turns: abc\n---\n",
		"inherit.md":    "---\ndescription: 继承模型\nmodel: inherit\n---\n",
		"empty-tool.md": "---\ndescription: 全部工具\n---\n正文",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Mkdir(filepath.Join(dir, "sub.md"), 0o755) // 目录应被跳过

	defs, errs := Load(dir, filepath.Join(dir, "missing"))
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "broken.md") {
		t.Errorf("缺少 description 的定义应报错：%v", errs)
	}
	byName := map[string]Definition{}
	for _, d := range defs {
		byName[d.Name] = d
	}
	if len(defs) != 5 {
		t.Fatalf("应加载 5 个定义，实际 %d：%v", len(defs), defs)
	}
	r := byName["code-reviewer"]
	if r.Description != "审查代码" || strings.Join(r.Tools, ",") != "read_file,grep" || r.Model != "deepseek-chat" || r.MaxTurns != 12 || r.Prompt != "你是严格的审查者。" || !strings.HasSuffix(r.Source, "reviewer.md") {
		t.Errorf("frontmatter 解析错误：%+v", r)
	}
	if tt := byName["tester"]; strings.Join(tt.Tools, ",") != "bash,read_file" {
		t.Errorf("文件名应作为默认名称，多行列表应解析：%+v", tt)
	}
	if byName["bad-turns"].MaxTurns != 0 {
		t.Errorf("非法 max_turns 应忽略")
	}

	merged := Merge(Builtin(), []Definition{{Name: "explore", Description: "自定义 explore"}}, defs)
	names := []string{}
	for _, d := range merged {
		names = append(names, d.Name)
		if d.Name == "explore" && d.Description != "自定义 explore" {
			t.Errorf("后面的定义应覆盖内置同名定义")
		}
	}
	if !strings.HasPrefix(strings.Join(names, ","), "bad-turns,code-reviewer") {
		t.Errorf("Merge 结果应按名称排序：%v", names)
	}
	sec := PromptSection(merged)
	if !strings.Contains(sec, "**code-reviewer**: 审查代码") || !strings.Contains(sec, "task 工具") || PromptSection(nil) != "" {
		t.Errorf("提示词片段错误：%s", sec)
	}
}

type readArgs struct {
	Path string `json:"path"`
}

func readTool() tool.Tool {
	return tool.Func("read_file", "读文件", true, func(ctx context.Context, in readArgs, c *tool.Call) (*tool.Result, error) {
		return tool.Text("内容:" + in.Path), nil
	})
}

func writeTool() tool.Tool {
	return tool.Func("write_file", "写文件", false, func(ctx context.Context, in readArgs, c *tool.Call) (*tool.Result, error) {
		return tool.Text("ok"), nil
	})
}

// scriptedProvider 根据"是父 Agent 还是子 Agent 的请求"返回不同回复，并记录子 Agent 收到的请求。
type scriptedProvider struct {
	mu        sync.Mutex
	childReqs []*llm.Request
}

func (s *scriptedProvider) handler(req *llm.Request) (llm.Message, error) {
	first := req.Messages[0].Text()
	last := req.Messages[len(req.Messages)-1]
	if strings.HasPrefix(first, "子任务") { // 子 Agent
		s.mu.Lock()
		s.childReqs = append(s.childReqs, req)
		s.mu.Unlock()
		if len(req.Messages) == 1 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ToolCall("", "read_file", readArgs{"go.mod"})}}, nil
		}
		return llm.AssistantText("子 Agent 汇报：" + first), nil
	}
	// 父 Agent：第一轮委派两个并行子任务，之后汇总
	if len(req.Messages) == 1 {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolCall("t1", "task", map[string]string{"description": "探索", "prompt": "子任务 A", "agent": "explore"}),
			llm.ToolCall("t2", "task", map[string]string{"description": "探索", "prompt": "子任务 B", "agent": "explore"}),
			llm.ToolCall("t3", "task", map[string]string{"description": "未知", "prompt": "子任务 C", "agent": "nope"}),
		}}, nil
	}
	var parts []string
	for _, b := range last.Content {
		parts = append(parts, b.Content)
	}
	return llm.AssistantText("汇总：" + strings.Join(parts, " | ")), nil
}

func TestTaskTool(t *testing.T) {
	sp := &scriptedProvider{}
	mock := &llm.MockProvider{Handler: sp.handler}
	parent := &agent.Agent{
		Name: "main", Provider: mock, Model: "parent-model", WorkDir: t.TempDir(),
		Tools:  tool.NewRegistry(readTool(), writeTool()),
		System: func() string { return "父提示词" },
		Hooks:  hook.NewManager(),
	}
	var stops []string
	var mu sync.Mutex
	_ = parent.Hooks.On(hook.SubagentStop, "", func(ctx context.Context, in *hook.Input) (*hook.Output, error) {
		mu.Lock()
		stops = append(stops, in.AgentName)
		mu.Unlock()
		return nil, nil
	})
	defs := Builtin()
	task := TaskTool(parent, defs)
	if info := task.Info(); info.Name != "task" || info.ReadOnly || !info.Concurrent || !strings.Contains(info.Description, "explore") {
		t.Fatalf("task 工具定义错误：%+v", info)
	}
	parent.Tools.Register(task)

	var evMu sync.Mutex
	var events []agent.Event
	sink := func(ev agent.Event) {
		evMu.Lock()
		events = append(events, ev)
		evMu.Unlock()
	}
	res, err := parent.Run(context.Background(), session.New(), "帮我调研", sink)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "子 Agent 汇报：子任务 A") || !strings.Contains(res.Text, "子 Agent 汇报：子任务 B") || !strings.Contains(res.Text, `"nope" 不存在`) {
		t.Errorf("父 Agent 应拿到子 Agent 的汇报：%s", res.Text)
	}

	if len(sp.childReqs) != 4 {
		t.Fatalf("两个子 Agent 各应调用模型 2 次，实际 %d", len(sp.childReqs))
	}
	for _, req := range sp.childReqs {
		var names []string
		for _, ts := range req.Tools {
			names = append(names, ts.Name)
		}
		if strings.Join(names, ",") != "read_file" {
			t.Errorf("explore 子 Agent 只应拥有过滤后的只读工具（且没有 task），实际 %v", names)
		}
		if !strings.Contains(req.System, "父提示词") || !strings.Contains(req.System, "代码库探索专家") || req.Model != "parent-model" {
			t.Errorf("子 Agent 应继承父提示词并追加角色说明：%q", req.System)
		}
	}

	pathRe := regexp.MustCompile(`^main>explore#\d+$`)
	childPaths := map[string]bool{}
	for _, ev := range events {
		if ev.Agent != "main" {
			if !pathRe.MatchString(ev.Agent) {
				t.Errorf("子 Agent 事件路径格式错误：%q", ev.Agent)
			}
			childPaths[ev.Agent] = true
		}
	}
	if len(childPaths) != 2 {
		t.Errorf("应有两个不同的子 Agent 路径：%v", childPaths)
	}
	var childToolResults int
	for _, ev := range events {
		if ev.Type == agent.EventToolResult && ev.Agent != "main" {
			childToolResults++
		}
	}
	if childToolResults != 2 {
		t.Errorf("子 Agent 的工具事件应冒泡到父 Sink：%d", childToolResults)
	}
	if len(stops) != 2 {
		t.Errorf("每个子 Agent 结束时应触发 SubagentStop：%v", stops)
	}
}

func TestTaskToolChildFailure(t *testing.T) {
	mock := &llm.MockProvider{Handler: func(req *llm.Request) (llm.Message, error) {
		// 子 Agent 一直调用工具，直到超过 MaxTurns
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ToolCall("", "read_file", readArgs{"x"})}}, nil
	}}
	parent := &agent.Agent{Name: "main", Provider: mock, Tools: tool.NewRegistry(readTool())}
	task := TaskTool(parent, []Definition{{Name: "general-purpose", Description: "通用", MaxTurns: 2}})
	b, _ := json.Marshal(map[string]string{"prompt": "永远做不完"})
	res, err := task.Run(context.Background(), &tool.Call{Input: b, Env: &tool.Env{SessionID: "parent"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "子 Agent 执行失败") {
		t.Errorf("子 Agent 失败应返回错误结果：%+v", res)
	}
}

func TestSpawn(t *testing.T) {
	temp := 0.3
	parent := &agent.Agent{
		Name: "main", Model: "m1", MaxTokens: 1000, Temperature: &temp, WorkDir: "/w",
		Tools:  tool.NewRegistry(readTool(), writeTool(), tool.Func("task", "", false, func(context.Context, struct{}, *tool.Call) (*tool.Result, error) { return nil, nil })),
		Prompt: agent.PromptFunc(func(a *agent.Agent) string { return "工具：" + strings.Join(a.Tools.Names(), ",") }),
	}
	child := Spawn(parent, Definition{Name: "gp", Prompt: "角色"})
	if child.Model != "m1" || child.MaxTurns != 40 || child.MaxTokens != 1000 || child.Temperature != &temp || child.WorkDir != "/w" {
		t.Errorf("子 Agent 应继承父配置：%+v", child)
	}
	if strings.Join(child.Tools.Names(), ",") != "read_file,write_file" {
		t.Errorf("Tools 为空时应继承除 task 外的全部工具：%v", child.Tools.Names())
	}
	if sp := child.SystemPrompt(); sp != "工具：read_file,write_file\n\n# 子 Agent 角色\n\n角色" {
		t.Errorf("子 Agent 提示词应基于自身工具集构建：%q", sp)
	}
	if c := Spawn(parent, Definition{Name: "x", Model: "inherit", MaxTurns: 5}); c.Model != "m1" || c.MaxTurns != 5 {
		t.Errorf("model=inherit 应继承父模型")
	}
	if c := Spawn(parent, Definition{Name: "x", Model: "other"}); c.Model != "other" {
		t.Errorf("应使用定义中的模型")
	}
	bare := &agent.Agent{Tools: tool.NewRegistry()}
	if sp := Spawn(bare, Definition{Prompt: "只有角色"}).SystemPrompt(); sp != "只有角色" {
		t.Errorf("父 Agent 没有提示词时应只用角色提示：%q", sp)
	}
}

// TestRepoAgents 保证仓库自带的示例子 Agent 定义格式正确。
func TestRepoAgents(t *testing.T) {
	defs, errs := Load(filepath.Join("..", ".goagent", "agents"))
	if len(errs) != 0 || len(defs) != 1 {
		t.Fatalf("示例子 Agent 加载失败：%v %v", defs, errs)
	}
	d := defs[0]
	if d.Name != "test-runner" || d.MaxTurns != 25 || strings.Join(d.Tools, ",") != "bash,read_file,grep,glob" || d.Prompt == "" {
		t.Errorf("test-runner 定义解析错误：%+v", d)
	}
}

// TestPlanModeDelegatesToReadOnlySubagent：plan 模式允许委派只读的 explore，但拒绝 general-purpose；
// 子 Agent 自身的工具仍受权限约束。
func TestPlanModeDelegatesToReadOnlySubagent(t *testing.T) {
	perm, _ := permission.NewChecker(permission.Config{Mode: permission.ModePlan}, permission.AutoDeny)
	mock := &llm.MockProvider{Handler: func(req *llm.Request) (llm.Message, error) {
		first := req.Messages[0].Text()
		if strings.HasPrefix(first, "子任务") {
			if len(req.Messages) == 1 {
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ToolCall("", "read_file", readArgs{"a.go"})}}, nil
			}
			return llm.AssistantText("只读汇报"), nil
		}
		if len(req.Messages) == 1 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				llm.ToolCall("e", "task", map[string]string{"description": "探索", "prompt": "子任务 E", "agent": "explore"}),
				llm.ToolCall("g", "task", map[string]string{"description": "改代码", "prompt": "子任务 G", "agent": "general-purpose"}),
			}}, nil
		}
		var parts []string
		for _, b := range req.Messages[len(req.Messages)-1].Content {
			parts = append(parts, b.Content)
		}
		return llm.AssistantText(strings.Join(parts, " | ")), nil
	}}
	tracer := trace.New()
	parent := &agent.Agent{Name: "main", Provider: mock, Model: "m", Tools: tool.NewRegistry(readTool(), writeTool()), Permissions: perm, Tracer: tracer}
	task := TaskTool(parent, Builtin())
	parent.Tools.Register(task)

	if !tool.IsReadOnly(task, json.RawMessage(`{"agent":"explore","prompt":"x"}`)) {
		t.Errorf("委派给 explore 应视为只读")
	}
	if tool.IsReadOnly(task, json.RawMessage(`{"prompt":"x"}`)) || tool.IsReadOnly(task, json.RawMessage(`{"agent":"general-purpose"}`)) {
		t.Errorf("委派给 general-purpose 不是只读")
	}

	var mu sync.Mutex
	var spanStarts, spanEnds []string
	res, err := parent.Run(context.Background(), session.New(), "规划一下", func(ev agent.Event) {
		mu.Lock()
		defer mu.Unlock()
		if ev.Span != nil && ev.Span.Kind == trace.KindSubagent {
			if ev.Type == agent.EventSpanStart {
				spanStarts = append(spanStarts, ev.Span.Name)
			} else if ev.Type == agent.EventSpanEnd {
				spanEnds = append(spanEnds, ev.Span.Name)
				if ev.Span.Attrs["input_tokens"] == nil || ev.Span.Attrs["output_tokens"] == nil {
					t.Errorf("subagent Span 应记录 input/output tokens：%v", ev.Span.Attrs)
				}
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "只读汇报") || !strings.Contains(res.Text, "plan") {
		t.Errorf("explore 应在 plan 模式下执行，general-purpose 应被拒绝：%s", res.Text)
	}
	if len(spanStarts) != 1 || spanStarts[0] != "subagent.explore" || len(spanEnds) != 1 {
		t.Errorf("subagent Span 应实时发出 span_start / span_end：start=%v end=%v", spanStarts, spanEnds)
	}
}

func TestMergeInheritsAndProfile(t *testing.T) {
	merged := Merge(Builtin(), []Definition{{Name: "explore", Description: "只改描述"}})
	var ex Definition
	for _, d := range merged {
		if d.Name == "explore" {
			ex = d
		}
	}
	if ex.Description != "只改描述" || ex.MaxTurns != 30 || len(ex.Tools) != 4 || !strings.Contains(ex.Prompt, "代码库探索专家") {
		t.Errorf("覆盖时未写出的字段应继承内置定义：%+v", ex)
	}

	parentMock := llm.NewMock(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ToolCall("t", "task", map[string]string{"prompt": "子任务", "agent": "cheap"}),
	}}, llm.AssistantText("done"))
	childMock := llm.NewMock(llm.AssistantText("来自便宜模型"))
	parent := &agent.Agent{Name: "main", Provider: parentMock, Model: "big", Tools: tool.NewRegistry(readTool())}
	defs := []Definition{{Name: "cheap", Description: "d", Profile: "deepseek", Tools: []string{"read_file"}, Prompt: "p"}}
	parent.Tools.Register(TaskToolWith(parent, defs, Options{Resolve: func(profile string) (llm.Provider, string, error) {
		if profile != "deepseek" {
			t.Errorf("profile = %s", profile)
		}
		return childMock, "deepseek-chat", nil
	}}))
	if _, err := parent.Run(context.Background(), session.New(), "go", nil); err != nil {
		t.Fatal(err)
	}
	if childMock.Calls() != 1 || childMock.Requests[0].Model != "deepseek-chat" {
		t.Errorf("子 Agent 应使用 profile 解析出的 Provider 与模型：calls=%d", childMock.Calls())
	}
	// 没有 Resolve 时报工具错误
	parent2 := &agent.Agent{Name: "main", Provider: llm.NewMock(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ToolCall("t", "task", map[string]string{"prompt": "x", "agent": "cheap"})}}, llm.AssistantText("ok")), Model: "m", Tools: tool.NewRegistry(readTool())}
	parent2.Tools.Register(TaskTool(parent2, defs))
	rec := []agent.Event{}
	var mu sync.Mutex
	_, _ = parent2.Run(context.Background(), session.New(), "go", func(ev agent.Event) {
		mu.Lock()
		rec = append(rec, ev)
		mu.Unlock()
	})
	found := false
	for _, ev := range rec {
		if ev.Type == agent.EventToolResult && strings.Contains(ev.Tool.Output, "profile") {
			found = true
		}
	}
	if !found {
		t.Errorf("没有 Resolve 时应返回 profile 相关的工具错误")
	}
}
