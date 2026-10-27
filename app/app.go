// Package app 把各个模块组装成一个可运行的 Agent 应用。
//
// 这是"依赖注入"发生的地方：读取配置 → 创建 Provider → 注册工具（内置 + 记忆 + 技能 + 子 Agent + MCP）
// → 权限、钩子、Trace、会话存储 → 组装 Agent。CLI 与 HTTP 服务都通过它启动。
package app

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/dmxj/goagent/agent"
	"github.com/dmxj/goagent/config"
	"github.com/dmxj/goagent/hook"
	"github.com/dmxj/goagent/llm"
	"github.com/dmxj/goagent/mcp"
	"github.com/dmxj/goagent/memory"
	"github.com/dmxj/goagent/permission"
	"github.com/dmxj/goagent/prompt"
	"github.com/dmxj/goagent/session"
	"github.com/dmxj/goagent/skill"
	"github.com/dmxj/goagent/subagent"
	"github.com/dmxj/goagent/tool"
	"github.com/dmxj/goagent/tool/builtin"
	"github.com/dmxj/goagent/trace"
)

// Options 是组装时可注入的依赖。
type Options struct {
	// Approver 处理需要人工确认的工具调用；nil 时按 AutoDeny 处理。
	Approver permission.Approver
	// Provider 覆盖根据配置创建的 Provider（测试时注入 Mock）。
	Provider llm.Provider
	// ExtraTools 追加自定义工具。
	ExtraTools []tool.Tool
	// SkipMCP 为 true 时不连接 MCP 服务器。
	SkipMCP bool
}

// App 是组装完成的应用。
type App struct {
	Config    *config.Config
	Agent     *agent.Agent
	Store     session.Store
	Memory    *memory.Store
	Skills    []skill.Skill
	Subagents []subagent.Definition
	MCP       *mcp.Manager
	Traces    *trace.Memory
	Prompt    *prompt.Builder
	Warnings  []string

	traceFile *trace.File
}

// New 根据配置组装应用。
func New(ctx context.Context, cfg *config.Config, opts Options) (*App, error) {
	a := &App{Config: cfg}

	// 1. 模型
	provider := opts.Provider
	if provider == nil {
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
		var err error
		if provider, err = llm.NewProvider(cfg.Provider); err != nil {
			return nil, err
		}
	}

	// 2. 工具
	bopts := builtin.DefaultOptions()
	if cfg.Tools.RestrictToWorkDir != nil {
		bopts.RestrictToWorkDir = *cfg.Tools.RestrictToWorkDir
	}
	if cfg.Tools.RequireReadBeforeEdit != nil {
		bopts.RequireReadBeforeEdit = *cfg.Tools.RequireReadBeforeEdit
	}
	if cfg.Tools.BashTimeoutSec > 0 {
		bopts.BashTimeout = time.Duration(cfg.Tools.BashTimeoutSec) * time.Second
	}
	reg := tool.NewRegistry(builtin.All(bopts)...)

	// 3. 长期记忆
	if cfg.Memory.Enabled {
		ms, err := memory.NewStore(filepath.Join(cfg.ProjectDir, "memory"))
		if err != nil {
			return nil, err
		}
		a.Memory = ms
		for _, t := range ms.Tools() {
			reg.Register(t)
		}
	}

	// 4. 技能：用户级 + 项目级（项目级同名覆盖）
	skills, errs := skill.Discover(
		skill.Dir{Path: filepath.Join(cfg.UserDir, "skills"), Source: "user"},
		skill.Dir{Path: filepath.Join(cfg.ProjectDir, "skills"), Source: "project"},
	)
	for _, e := range errs {
		a.Warnings = append(a.Warnings, e.Error())
	}
	a.Skills = skills
	if len(skills) > 0 {
		reg.Register(skill.Tool(skills))
	}

	// 5. MCP
	a.MCP = mcp.NewManager()
	notes := map[string]string{}
	if !opts.SkipMCP && len(cfg.MCPServers) > 0 {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		a.MCP.ConnectAll(cctx, cfg.MCPServers)
		cancel()
		for name, err := range a.MCP.Errors {
			a.Warnings = append(a.Warnings, fmt.Sprintf("MCP 服务器 %s 连接失败：%v", name, err))
		}
		for _, t := range a.MCP.Tools() {
			reg.Register(t)
		}
		notes = a.MCP.Instructions()
	}
	for _, t := range opts.ExtraTools {
		reg.Register(t)
	}
	for _, name := range cfg.Tools.Disabled {
		reg.Unregister(name)
	}

	// 6. 权限
	approver := opts.Approver
	if approver == nil {
		approver = permission.AutoDeny
	}
	perm, err := permission.NewChecker(cfg.Permissions, approver)
	if err != nil {
		return nil, fmt.Errorf("权限配置错误: %w", err)
	}

	// 7. 钩子
	hooks := hook.NewManager()
	if err := hooks.LoadConfig(cfg.Hooks, cfg.WorkDir); err != nil {
		return nil, err
	}

	// 8. Trace：内存（供 HTTP 查询）+ JSONL 文件
	a.Traces = trace.NewMemory(20000)
	tracer := trace.New(a.Traces)
	if cfg.Trace.Enabled {
		f, err := trace.NewFile(filepath.Join(cfg.ProjectDir, "traces", time.Now().Format("20060102")+".jsonl"))
		if err == nil {
			a.traceFile = f
			tracer.AddExporter(f)
		} else {
			a.Warnings = append(a.Warnings, "无法写入 trace 文件："+err.Error())
		}
	}

	// 9. 会话存储
	store, err := session.NewFileStore(filepath.Join(cfg.ProjectDir, "sessions"))
	if err != nil {
		return nil, err
	}
	a.Store = store

	// 10. 上下文压缩
	var compactor *memory.Compactor
	if cfg.Compact.Enabled {
		compactor = &memory.Compactor{Provider: provider, Model: cfg.Model, ContextWindow: cfg.ContextWindow, Threshold: cfg.Compact.Threshold, KeepRecent: cfg.Compact.KeepRecent}
	}

	// 11. 系统提示词
	a.Prompt = &prompt.Builder{
		WorkDir:      cfg.WorkDir,
		Instructions: memory.LoadInstructions(cfg.WorkDir, cfg.UserDir),
		Memory:       a.Memory,
		Skills:       skills,
		MCPNotes:     notes,
		Extra:        cfg.SystemPromptAppend,
	}

	ag := &agent.Agent{
		Name:           "main",
		Provider:       provider,
		Model:          cfg.Model,
		MaxTokens:      cfg.MaxTokens,
		Temperature:    cfg.Temperature,
		ThinkingBudget: cfg.ThinkingBudget,
		Prompt:         a.Prompt,
		Tools:          reg,
		Permissions:    perm,
		Hooks:          hooks,
		Tracer:         tracer,
		Compactor:      compactor,
		Store:          store,
		WorkDir:        cfg.WorkDir,
		MaxTurns:       cfg.MaxTurns,
	}

	// 12. 子 Agent：内置 + 用户级 + 项目级
	custom, errs := subagent.Load(filepath.Join(cfg.UserDir, "agents"), filepath.Join(cfg.ProjectDir, "agents"))
	for _, e := range errs {
		a.Warnings = append(a.Warnings, e.Error())
	}
	a.Subagents = subagent.Merge(subagent.Builtin(), custom)
	a.Prompt.Subagents = a.Subagents
	reg.Register(subagent.TaskTool(ag, a.Subagents))

	a.Agent = ag
	return a, nil
}

// NewSession 创建新会话。
func (a *App) NewSession() *session.Session {
	s := session.New()
	s.Model = a.Agent.Model
	return s
}

// LoadSession 加载会话；id 为空时新建。
func (a *App) LoadSession(id string) (*session.Session, error) {
	if id == "" {
		return a.NewSession(), nil
	}
	return a.Store.Load(id)
}

// Close 释放资源（MCP 子进程、trace 文件）。
func (a *App) Close() {
	if a.MCP != nil {
		a.MCP.Close()
	}
	if a.traceFile != nil {
		_ = a.traceFile.Close()
	}
}
