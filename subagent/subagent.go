// Package subagent 实现子 Agent 与任务委派（对应 Claude Code 的 Task 工具）。
//
// 主 Agent 通过 task 工具把一个独立的子任务交给"子 Agent"：
//   - 子 Agent 拥有全新的、隔离的上下文（不会把大量探索过程塞进主对话）；
//   - 可以限定工具集（例如只读的 explore）、使用不同模型与系统提示词；
//   - 多个 task 调用可以并行执行；
//   - 子 Agent 的事件与 Trace 会挂在父 Agent 之下，调试时一目了然。
package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/dmxj/goagent/agent"
	"github.com/dmxj/goagent/hook"
	"github.com/dmxj/goagent/internal/frontmatter"
	"github.com/dmxj/goagent/llm"
	"github.com/dmxj/goagent/session"
	"github.com/dmxj/goagent/tool"
	"github.com/dmxj/goagent/trace"
)

// Definition 描述一种子 Agent。
type Definition struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tools       []string `json:"tools,omitempty"` // 为空表示继承父 Agent 的全部工具（task 除外）
	Model       string   `json:"model,omitempty"` // 为空表示与父 Agent 相同
	// Profile 引用配置 profiles 中的命名模型配置，让子 Agent 使用独立的 Provider（例如主 Agent 用 Claude、explore 用 DeepSeek）。
	Profile  string `json:"profile,omitempty"`
	Prompt   string `json:"-"`
	MaxTurns int    `json:"max_turns,omitempty"`
	Source   string `json:"source"`
}

// Builtin 返回内置的子 Agent 定义。
func Builtin() []Definition {
	return []Definition{
		{
			Name:        "general-purpose",
			Description: "通用子 Agent：适合需要多步搜索、阅读、修改的复杂子任务。",
			Prompt:      "你是一个被主 Agent 委派任务的子 Agent。独立完成任务，最后给出简洁、完整的结果汇报（包含关键文件路径与结论），汇报就是你的最终回复。",
			Source:      "builtin",
		},
		{
			Name:        "explore",
			Description: "只读探索子 Agent：快速在代码库中查找文件、搜索代码、回答'某功能在哪里实现'之类的问题，不会修改任何文件。",
			Tools:       []string{"read_file", "list_dir", "glob", "grep"},
			Prompt:      "你是代码库探索专家，只能使用只读工具。高效地搜索与阅读代码，最终用要点列出发现，并给出 文件路径:行号 引用。",
			MaxTurns:    30,
			Source:      "builtin",
		},
	}
}

// Load 从目录加载自定义子 Agent（*.md，frontmatter: name/description/tools/model/max_turns，正文为系统提示词）。
func Load(dirs ...string) ([]Definition, []error) {
	var defs []Definition
	var errs []error
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			p := filepath.Join(dir, e.Name())
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			meta, body := frontmatter.Parse(string(b))
			d := Definition{
				Name:        meta["name"],
				Description: meta["description"],
				Tools:       frontmatter.List(meta["tools"]),
				Model:       meta["model"],
				Profile:     meta["profile"],
				Prompt:      strings.TrimSpace(body),
				Source:      p,
			}
			if d.Name == "" {
				d.Name = strings.TrimSuffix(e.Name(), ".md")
			}
			if n, err := strconv.Atoi(meta["max_turns"]); err == nil {
				d.MaxTurns = n
			}
			if d.Description == "" {
				errs = append(errs, fmt.Errorf("子 Agent 定义 %s 缺少 description", p))
				continue
			}
			defs = append(defs, d)
		}
	}
	return defs, errs
}

// Merge 合并定义，后者覆盖前者同名项。覆盖时没有写出的字段（tools、model、profile、max_turns、正文）
// 继承被覆盖的定义：只想改 explore 的提示词时，不必把内置的工具列表与轮数上限再抄一遍。
func Merge(lists ...[]Definition) []Definition {
	m := map[string]Definition{}
	for _, l := range lists {
		for _, d := range l {
			if old, ok := m[d.Name]; ok {
				if d.Tools == nil {
					d.Tools = old.Tools
				}
				if d.Model == "" {
					d.Model = old.Model
				}
				if d.Profile == "" {
					d.Profile = old.Profile
				}
				if d.MaxTurns <= 0 {
					d.MaxTurns = old.MaxTurns
				}
				if strings.TrimSpace(d.Prompt) == "" {
					d.Prompt = old.Prompt
				}
			}
			m[d.Name] = d
		}
	}
	out := make([]Definition, 0, len(m))
	for _, d := range m {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// PromptSection 生成系统提示词中的子 Agent 说明。
func PromptSection(defs []Definition) string {
	if len(defs) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("你可以通过 task 工具把独立的子任务委派给子 Agent（它们拥有独立上下文，可并行）。适合：大范围代码搜索、可以独立完成的调研或修改。可用子 Agent：\n\n")
	for _, d := range defs {
		fmt.Fprintf(&sb, "- **%s**: %s\n", d.Name, d.Description)
	}
	return sb.String()
}

type taskArgs struct {
	Description string `json:"description" desc:"任务的简短描述（3~8 个字）"`
	Prompt      string `json:"prompt" desc:"交给子 Agent 的完整任务说明。子 Agent 看不到当前对话，必须写清背景、目标与期望的汇报格式"`
	Agent       string `json:"agent,omitempty" desc:"子 Agent 类型，默认 general-purpose"`
}

var seq atomic.Int64

// taskTool 是 task 工具。它实现 tool.InputReadOnly：委派给"工具全部只读"的子 Agent（如 explore）时，
// 这次调用本身就是只读的，plan 模式下也允许；子 Agent 内部的每次工具调用仍会单独经过权限检查。
type taskTool struct {
	*tool.FuncTool[taskArgs]
	parent *agent.Agent
	byName map[string]Definition
}

// ReadOnlyFor 判断委派给哪个子 Agent：该子 Agent 可用的工具全部只读时返回 true。
func (t *taskTool) ReadOnlyFor(input json.RawMessage) bool {
	var in taskArgs
	if json.Unmarshal(input, &in) != nil {
		return false
	}
	name := in.Agent
	if name == "" {
		name = "general-purpose"
	}
	def, ok := t.byName[name]
	if !ok {
		return true // 不存在的子 Agent 只会返回错误，不会产生副作用
	}
	return ReadOnly(t.parent, def)
}

// ReadOnly 报告子 Agent 是否只能使用只读工具（工具集为空时视为非只读：它会继承父 Agent 的全部工具）。
func ReadOnly(parent *agent.Agent, def Definition) bool {
	tools := parent.Tools.Filter(def.Tools, "task").List()
	if len(def.Tools) == 0 || len(tools) == 0 {
		return false
	}
	for _, t := range tools {
		if !t.Info().ReadOnly {
			return false
		}
	}
	return true
}

// ProviderResolver 根据 profile 名称返回子 Agent 使用的 Provider 与模型名。
type ProviderResolver func(profile string) (llm.Provider, string, error)

// Options 是 task 工具的可选配置。
type Options struct {
	// Resolve 用于解析子 Agent 定义中的 profile；为 nil 时定义了 profile 的子 Agent 会报错。
	Resolve ProviderResolver
}

// TaskTool 创建 task 工具。parent 是父 Agent，defs 为可用的子 Agent 定义。
func TaskTool(parent *agent.Agent, defs []Definition) tool.Tool {
	return TaskToolWith(parent, defs, Options{})
}

// TaskToolWith 与 TaskTool 相同，但可以提供 profile 解析等选项。
func TaskToolWith(parent *agent.Agent, defs []Definition, opts Options) tool.Tool {
	byName := map[string]Definition{}
	var names []string
	for _, d := range defs {
		byName[d.Name] = d
		names = append(names, d.Name)
	}
	t := tool.Func("task",
		"把一个独立的子任务委派给子 Agent 执行，返回它的最终汇报。可以在一次回复中发起多个 task 调用以并行执行。可用类型："+strings.Join(names, ", "),
		false,
		func(ctx context.Context, in taskArgs, c *tool.Call) (*tool.Result, error) {
			name := in.Agent
			if name == "" {
				name = "general-purpose"
			}
			def, ok := byName[name]
			if !ok {
				return tool.Errorf("子 Agent 类型 %q 不存在，可用：%s", name, strings.Join(names, ", ")), nil
			}
			child := Spawn(parent, def)
			if def.Profile != "" {
				if opts.Resolve == nil {
					return tool.Errorf("子 Agent %s 指定了 profile %q，但当前没有可用的模型配置", def.Name, def.Profile), nil
				}
				p, model, err := opts.Resolve(def.Profile)
				if err != nil {
					return tool.Errorf("子 Agent %s 的 profile %q 无效：%v", def.Name, def.Profile, err), nil
				}
				child.Provider = p
				if def.Model == "" || def.Model == "inherit" {
					child.Model = model
				}
			}
			sess := session.New()
			sess.Parent = c.Env.SessionID
			ctx, span := parent.Tracer.Start(ctx, "subagent."+def.Name, trace.KindSubagent, map[string]any{
				"agent": def.Name, "description": in.Description, "child_session": sess.ID, "read_only": ReadOnly(parent, def),
			})
			span.SetText("prompt", in.Prompt)
			// 子 Agent 的 Span 也要实时推送：前端 Trace 视图据此立即画出子树，无需手动刷新
			sink := agent.SinkFrom(ctx)
			snap := func() *trace.SpanData {
				d := span.Snapshot().Light(16<<10, 2<<10)
				return &d
			}
			sink(agent.Event{Type: agent.EventSpanStart, Span: snap()})
			defer func() {
				span.Finish()
				sink(agent.Event{Type: agent.EventSpanEnd, Span: snap()})
			}()
			child.Name = fmt.Sprintf("%s#%d", def.Name, seq.Add(1))
			res, err := child.Run(ctx, sess, in.Prompt, nil) // nil：事件沿用父 ctx 中的 Sink
			if res != nil {
				span.SetAttr("turns", res.Turns)
				span.SetAttr("input_tokens", res.Usage.InputTokens)
				span.SetAttr("output_tokens", res.Usage.OutputTokens)
				span.SetText("output", res.Text)
			}
			if err != nil {
				span.Fail(err)
				text := ""
				if res != nil {
					text = res.Text
				}
				return &tool.Result{Content: fmt.Sprintf("子 Agent 执行失败：%v\n%s", err, text), IsError: true}, nil
			}
			if parent.Hooks.Count(hook.SubagentStop) > 0 {
				_, _ = parent.Hooks.Run(ctx, &hook.Input{Event: hook.SubagentStop, SessionID: sess.ID, WorkDir: parent.WorkDir, LastReply: res.Text, AgentName: child.Name})
			}
			return &tool.Result{
				Content: res.Text,
				Meta:    map[string]any{"subagent": def.Name, "turns": res.Turns, "session": sess.ID, "usage": res.Usage},
			}, nil
		})
	// 子 Agent 之间互不干扰，可以并行
	return &taskTool{FuncTool: t.WithConcurrent(true), parent: parent, byName: byName}
}

// Spawn 基于父 Agent 与定义创建子 Agent（共享 Provider、权限、钩子、Tracer）。
func Spawn(parent *agent.Agent, def Definition) *agent.Agent {
	tools := parent.Tools.Filter(def.Tools, "task") // 子 Agent 不能再委派，避免无限递归
	model := def.Model
	if model == "" || model == "inherit" {
		model = parent.Model
	}
	maxTurns := def.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 40
	}
	role := def.Prompt
	base := parent.Prompt
	if base == nil && parent.System != nil {
		sys := parent.System
		base = agent.PromptFunc(func(*agent.Agent) string { return sys() })
	}
	return &agent.Agent{
		Name:             def.Name,
		Provider:         parent.Provider,
		Model:            model,
		MaxTokens:        parent.MaxTokens,
		Temperature:      parent.Temperature,
		TopP:             parent.TopP,
		TopK:             parent.TopK,
		StopSequences:    parent.StopSequences,
		NoStream:         parent.NoStream,
		ExtraHeaders:     parent.ExtraHeaders,
		ExtraBody:        parent.ExtraBody,
		Version:          parent.Version,
		Tools:            tools,
		Permissions:      parent.Permissions,
		Hooks:            parent.Hooks,
		Tracer:           parent.Tracer,
		Compactor:        parent.Compactor,
		WorkDir:          parent.WorkDir,
		MaxTurns:         maxTurns,
		ToolTimeout:      parent.ToolTimeout,
		MaxParallelTools: parent.MaxParallelTools,
		MaxToolOutput:    parent.MaxToolOutput,
		KeepThinking:     parent.KeepThinking,
		Prompt: agent.PromptFunc(func(a *agent.Agent) string {
			if base == nil {
				return role
			}
			// 复用父 Agent 的提示词构建逻辑（环境信息、项目指令），
			// 但传入子 Agent 自身，使工具/技能说明与子 Agent 的工具集一致
			return base.Build(a) + "\n\n# 子 Agent 角色\n\n" + role
		}),
	}
}
