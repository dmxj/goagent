// Package app 把各个模块组装成一个可运行的 Agent 应用，并负责配置热更新。
//
// 这是"依赖注入"发生的地方：读取配置 → 创建 Provider → 注册工具（内置 + 记忆 + 技能 + 子 Agent + MCP）
// → 权限、钩子、Trace、会话存储 → 组装 Agent。CLI 与 HTTP 服务都通过它启动。
//
// # 热更新
//
// 组装结果是一个不可变的 Runtime 快照（配置 + Agent + 技能 + 子 Agent）。Watch 打开时，
// 后台每秒检查一次配置文件、提示词文件、技能 / 子 Agent 目录与 AGENTS.md 的修改时间，
// 有变化就重新组装一个新快照并原子替换：
//
//   - 正在运行的对话继续使用开始时拿到的旧快照，直到这一轮结束；
//   - 下一条消息使用新快照（新模型、新提示词、新工具、新技能立即可用）；
//   - 新配置有错误时保留上一份可用配置，错误记录在 Status().LastError 中并通知订阅者；
//   - 跨快照共享的状态（会话运行标志、文件访问记录、"本会话总是允许"、长期记忆、Trace、会话存储）不会丢失。
package app

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	// Override 在每次从磁盘重新加载配置后调用，用于叠加命令行参数（-model、-mode、-profile）。
	Override func(*config.Config) error
	// Watch 为 true 时启动热更新监视（chat / serve 使用；run 这类一次性任务不需要）。
	Watch bool
	// Logger 用于输出热更新日志，默认 slog.Default()。
	Logger *slog.Logger
}

// Runtime 是某一版本配置组装出的不可变快照。
type Runtime struct {
	Version   int
	LoadedAt  time.Time
	Config    *config.Config
	Agent     *agent.Agent
	Skills    []skill.Skill
	Subagents []subagent.Definition
	Prompt    *prompt.Builder
	Warnings  []string

	refs    int // 正在使用它的 Run 数量（受 App.rtMu 保护）
	retired bool
	clients []*mcp.Client
}

// Status 是热更新状态（GET /v1/config 返回）。
type Status struct {
	Version       int       `json:"version"`
	LoadedAt      time.Time `json:"loaded_at"`
	LastCheckedAt time.Time `json:"last_checked_at,omitzero"`
	LastError     string    `json:"last_error,omitempty"`
	LastErrorAt   time.Time `json:"last_error_at,omitzero"`
	Sources       []string  `json:"sources"`
	Watched       []string  `json:"watched"`
	Watching      bool      `json:"watching"`
	Warnings      []string  `json:"warnings,omitempty"`
}

// Event 是热更新事件。
type Event struct {
	Type    string    `json:"type"` // config_reloaded | config_error
	Version int       `json:"version"`
	Error   string    `json:"error,omitempty"`
	Time    time.Time `json:"time"`
	Changed []string  `json:"changed,omitempty"`
}

// App 是组装完成的应用。
type App struct {
	Store  session.Store
	MCP    *mcp.Manager
	Traces *trace.Memory
	Tracer *trace.Tracer
	// Grants 保存"本会话总是允许"的决定，所有配置版本共享。
	Grants *permission.Grants
	// States 保存会话级运行时状态，所有配置版本共享。
	States *agent.States

	opts      Options
	workDir   string
	log       *slog.Logger
	traceFile *trace.DailyFile

	memMu  sync.Mutex
	memory *memory.Store

	rtMu      sync.Mutex
	cur       *Runtime
	clientUse map[*mcp.Client]int

	reloadMu sync.Mutex
	retryMCP bool // 受 snapMu / reloadMu 保护：强制重新加载时重试失败的 MCP 服务器
	snapMu   sync.Mutex
	snap     snapshot
	stMu     sync.Mutex
	status   Status
	override struct{ model, mode string }

	subMu sync.Mutex
	subs  map[chan Event]struct{}

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// New 根据配置组装应用。cfg 通常来自 config.Load；热更新时会从 cfg.WorkDir 重新加载。
func New(ctx context.Context, cfg *config.Config, opts Options) (*App, error) {
	a := &App{
		opts: opts, workDir: cfg.WorkDir, log: opts.Logger,
		Grants: permission.NewGrants(), States: agent.NewStates(),
		MCP: mcp.NewManager(), Traces: trace.NewMemory(20000),
		clientUse: map[*mcp.Client]int{}, subs: map[chan Event]struct{}{}, stop: make(chan struct{}),
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	if err := a.validate(cfg); err != nil {
		return nil, err
	}
	a.Tracer = trace.New(a.Traces)
	if f, err := trace.NewDailyFile(filepath.Join(cfg.ProjectDir, "traces")); err == nil {
		a.traceFile = f
		a.Tracer.AddExporter(f)
	}
	store, err := session.NewFileStore(filepath.Join(cfg.ProjectDir, "sessions"))
	if err != nil {
		return nil, err
	}
	a.Store = store
	rt, err := a.build(ctx, cfg, 1)
	if err != nil {
		a.Close()
		return nil, err
	}
	if a.traceFile == nil && cfg.Trace.Enabled {
		rt.Warnings = append(rt.Warnings, "无法写入 trace 文件")
	}
	a.swap(rt)
	a.stMu.Lock()
	a.status = Status{Version: 1, LoadedAt: rt.LoadedAt, Sources: cfg.Sources, Warnings: rt.Warnings, Watched: a.watchPaths(cfg)}
	a.stMu.Unlock()
	if opts.Watch {
		a.startWatch(cfg.ReloadEnabled())
	}
	return a, nil
}

// validate 检查配置能否使用。
func (a *App) validate(cfg *config.Config) error {
	if a.opts.Provider == nil {
		if err := cfg.Validate(); err != nil {
			return err
		}
		_, err := llm.NewProvider(cfg.Provider)
		return err
	}
	return cfg.ValidateStatic()
}

// load 从磁盘重新加载配置并叠加命令行参数与运行时覆盖（/model、/mode）。
func (a *App) load() (*config.Config, error) {
	cfg, err := config.Load(a.workDir)
	if err != nil {
		return nil, err
	}
	if a.opts.Override != nil {
		if err := a.opts.Override(cfg); err != nil {
			return nil, err
		}
	}
	a.stMu.Lock()
	ov := a.override
	a.stMu.Unlock()
	if ov.model != "" {
		if _, ok := cfg.Profiles[ov.model]; ok {
			_ = cfg.UseProfile(ov.model)
		} else {
			cfg.Model = config.CleanModel(ov.model)
		}
	}
	if ov.mode != "" {
		cfg.Permissions.Mode = permission.Mode(ov.mode)
	}
	return cfg, a.validate(cfg)
}

// build 按配置组装一个新的 Runtime。
func (a *App) build(ctx context.Context, cfg *config.Config, version int) (*Runtime, error) {
	rt := &Runtime{Version: version, LoadedAt: time.Now(), Config: cfg}

	// 1. 模型
	provider := a.opts.Provider
	if provider == nil {
		var err error
		if provider, err = llm.NewProvider(cfg.Provider); err != nil {
			return nil, err
		}
	}

	// 2. 内置工具
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
	if cfg.Tools.MaxOutput > 0 {
		bopts.MaxOutput = cfg.Tools.MaxOutput
	}
	if cfg.Tools.WebFetchTimeoutSec > 0 {
		bopts.FetchTimeout = time.Duration(cfg.Tools.WebFetchTimeoutSec) * time.Second
	}
	bopts.Shell = cfg.Tools.BashShell
	bopts.Env = cfg.Tools.BashEnv
	reg := tool.NewRegistry(builtin.All(bopts)...)

	// 3. 长期记忆（存储跨版本共享）
	var mem *memory.Store
	if cfg.Memory.Enabled {
		var err error
		if mem, err = a.memoryStore(cfg); err != nil {
			return nil, err
		}
		for _, t := range mem.Tools() {
			reg.Register(t)
		}
	}

	// 4. 技能：按 skills.dirs 扫描，再按 enabled / disabled 过滤
	var dirs []skill.Dir
	for _, d := range cfg.SkillDirs() {
		dirs = append(dirs, skill.Dir{Path: d, Source: skillSource(cfg, d)})
	}
	all, errs := skill.Discover(dirs...)
	for _, e := range errs {
		rt.Warnings = append(rt.Warnings, e.Error())
	}
	for _, s := range all {
		if cfg.Skills.Allowed(s.Name) {
			rt.Skills = append(rt.Skills, s)
		}
	}
	if len(rt.Skills) > 0 {
		reg.Register(skill.Tool(rt.Skills))
	}

	// 5. MCP：配置没变的服务器复用连接
	notes := map[string]string{}
	if !a.opts.SkipMCP {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		retired := a.MCP.Sync(cctx, cfg.MCPServers, a.retryMCP)
		a.retryMCP = false
		cancel()
		a.retireClients(retired)
		for name, err := range a.MCP.ErrorsSnapshot() {
			rt.Warnings = append(rt.Warnings, fmt.Sprintf("MCP 服务器 %s 连接失败：%v", name, err))
		}
		for _, t := range a.MCP.Tools() {
			reg.Register(t)
		}
		notes = a.MCP.Instructions()
		rt.clients = a.MCP.Clients()
	}
	for _, t := range a.opts.ExtraTools {
		reg.Register(t)
	}

	// 6. 权限：规则来自配置，"本会话总是允许"跨版本共享
	approver := a.opts.Approver
	if approver == nil {
		approver = permission.AutoDeny
	}
	perm, err := permission.NewChecker(cfg.Permissions, approver)
	if err != nil {
		return nil, fmt.Errorf("权限配置错误: %w", err)
	}
	perm.UseGrants(a.Grants)

	// 7. 钩子（命令钩子与 bash 使用同一套环境变量策略）
	hooks := hook.NewManager()
	hooks.Env = cfg.Tools.BashEnv
	if err := hooks.LoadConfig(cfg.Hooks, cfg.WorkDir); err != nil {
		return nil, err
	}

	// 8. Trace：大小上限、密钥脱敏、文件导出开关随配置变化
	a.Tracer.SetMaxAttrBytes(cfg.Trace.MaxFieldBytes)
	a.Tracer.SetSecrets(cfg.Secrets()...)
	if a.traceFile != nil {
		a.traceFile.SetEnabled(cfg.Trace.Enabled)
	}

	// 9. 上下文压缩
	var compactor *memory.Compactor
	if cfg.Compact.Enabled {
		compactor = &memory.Compactor{Provider: provider, Model: cfg.Model, ContextWindow: cfg.ContextWindow, Threshold: cfg.Compact.Threshold, KeepRecent: cfg.Compact.KeepRecent}
	}

	// 10. 系统提示词：身份（system_prompt / system_prompt_file）+ 内置章节 + 自定义章节 + 追加内容
	pb, err := buildPrompt(cfg)
	if err != nil {
		return nil, err
	}
	pb.Memory, pb.Skills, pb.MCPNotes = mem, rt.Skills, notes
	rt.Prompt = pb

	params := cfg.EffectiveParams()
	ag := &agent.Agent{
		Name:             "main",
		Provider:         provider,
		Model:            cfg.Model,
		MaxTokens:        params.MaxTokens,
		Temperature:      params.Temperature,
		TopP:             params.TopP,
		TopK:             params.TopK,
		ThinkingBudget:   params.ThinkingBudget,
		StopSequences:    params.Stop,
		NoStream:         params.Stream != nil && !*params.Stream,
		ExtraHeaders:     params.ExtraHeaders,
		ExtraBody:        params.ExtraBody,
		Version:          strconv.Itoa(version),
		Prompt:           pb,
		Tools:            reg,
		Permissions:      perm,
		Hooks:            hooks,
		Tracer:           a.Tracer,
		Compactor:        compactor,
		Store:            a.Store,
		States:           a.States,
		WorkDir:          cfg.WorkDir,
		MaxTurns:         cfg.MaxTurns,
		ToolTimeout:      time.Duration(cfg.Tools.TimeoutSec) * time.Second,
		MaxParallelTools: cfg.Tools.MaxParallel,
		MaxToolOutput:    cfg.Tools.MaxResultBytes,
		KeepThinking:     cfg.KeepThinking,
	}

	// 11. 子 Agent：内置 + 配置目录，按 agents.enabled / disabled 过滤
	custom, errs := subagent.Load(cfg.AgentDirs()...)
	for _, e := range errs {
		rt.Warnings = append(rt.Warnings, e.Error())
	}
	for _, d := range subagent.Merge(subagent.Builtin(), custom) {
		if cfg.Agents.Allowed(d.Name) {
			rt.Subagents = append(rt.Subagents, d)
		}
	}
	pb.Subagents = rt.Subagents
	if len(rt.Subagents) > 0 {
		reg.Register(subagent.TaskToolWith(ag, rt.Subagents, subagent.Options{Resolve: a.profileResolver(cfg)}))
	}

	// 12. 工具绑定：单个工具的覆盖选项 + enabled / disabled 名单
	for name, o := range cfg.Tools.Options {
		if t, ok := reg.Get(name); ok {
			reg.Register(tool.Configure(t, o))
		}
	}
	filter := config.ListConfig{Enabled: cfg.Tools.Enabled, Disabled: cfg.Tools.Disabled}
	for _, name := range reg.Names() {
		if !filter.Allowed(name) {
			reg.Unregister(name)
		}
	}
	for _, name := range append(append([]string(nil), cfg.Tools.Enabled...), keys(cfg.Tools.Options)...) {
		if !strings.Contains(name, "*") {
			if _, ok := reg.Get(name); !ok && filter.Allowed(name) {
				rt.Warnings = append(rt.Warnings, fmt.Sprintf("配置中引用的工具 %s 不存在", name))
			}
		}
	}
	rt.Agent = ag
	return rt, nil
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func skillSource(cfg *config.Config, dir string) string {
	switch {
	case dir == filepath.Join(cfg.ProjectDir, "skills"):
		return "project"
	case dir == filepath.Join(cfg.UserDir, "skills"):
		return "user"
	}
	return dir
}

// profileResolver 为指定了 profile 的子 Agent 创建独立的 Provider。
func (a *App) profileResolver(cfg *config.Config) subagent.ProviderResolver {
	var mu sync.Mutex
	cache := map[string]llm.Provider{}
	return func(name string) (llm.Provider, string, error) {
		p, ok := cfg.Profiles[name]
		if !ok {
			return nil, "", fmt.Errorf("profiles 中没有 %q", name)
		}
		model := config.CleanModel(p.Model)
		if model == "" {
			model = cfg.Model
		}
		if a.opts.Provider != nil {
			return a.opts.Provider, model, nil
		}
		mu.Lock()
		defer mu.Unlock()
		if prov, ok := cache[name]; ok {
			return prov, model, nil
		}
		prov, err := llm.NewProvider(p.Provider)
		if err != nil {
			return nil, "", err
		}
		cache[name] = prov
		return prov, model, nil
	}
}

func (a *App) memoryStore(cfg *config.Config) (*memory.Store, error) {
	a.memMu.Lock()
	defer a.memMu.Unlock()
	if a.memory == nil {
		ms, err := memory.NewStore(filepath.Join(cfg.ProjectDir, "memory"))
		if err != nil {
			return nil, err
		}
		a.memory = ms
	}
	return a.memory, nil
}

// buildPrompt 根据配置构造提示词构建器（读取提示词文件、项目指令）。
func buildPrompt(cfg *config.Config) (*prompt.Builder, error) {
	b := &prompt.Builder{
		WorkDir:      cfg.WorkDir,
		Identity:     strings.TrimSpace(cfg.SystemPrompt),
		Instructions: memory.LoadInstructions(cfg.WorkDir, cfg.UserDir),
		Sections:     cfg.Prompt.Sections,
	}
	if cfg.SystemPromptFile != "" {
		text, err := readText(cfg.ResolvePath(cfg.SystemPromptFile))
		if err != nil {
			return nil, fmt.Errorf("读取 system_prompt_file 失败: %w", err)
		}
		b.Identity = text
	}
	var extra []string
	for _, s := range []string{cfg.SystemPromptAppend, cfg.Prompt.Append} {
		if strings.TrimSpace(s) != "" {
			extra = append(extra, strings.TrimSpace(s))
		}
	}
	if cfg.Prompt.AppendFile != "" {
		text, err := readText(cfg.ResolvePath(cfg.Prompt.AppendFile))
		if err != nil {
			return nil, fmt.Errorf("读取 prompt.append_file 失败: %w", err)
		}
		extra = append(extra, text)
	}
	b.Extra = strings.Join(extra, "\n\n")
	for _, s := range cfg.Prompt.Extra {
		content := s.Content
		if s.File != "" {
			text, err := readText(cfg.ResolvePath(s.File))
			if err != nil {
				return nil, fmt.Errorf("读取提示词章节 %q 的文件失败: %w", s.Title, err)
			}
			content = text
		}
		b.ExtraSections = append(b.ExtraSections, prompt.Section{Title: s.Title, Content: content})
	}
	return b, nil
}

// ---------- 快照管理 ----------

// swap 把 rt 设为当前快照，旧快照在没有运行中的对话后释放。
func (a *App) swap(rt *Runtime) {
	a.rtMu.Lock()
	defer a.rtMu.Unlock()
	for _, c := range rt.clients {
		a.clientUse[c]++
	}
	old := a.cur
	a.cur = rt
	if old != nil {
		old.retired = true
		if old.refs == 0 {
			a.releaseLocked(old)
		}
	}
}

// acquire 取得当前快照并登记一次使用。
func (a *App) acquire() *Runtime {
	a.rtMu.Lock()
	defer a.rtMu.Unlock()
	a.cur.refs++
	return a.cur
}

// release 结束一次使用；已被替换且没人使用的快照释放其 MCP 连接引用。
func (a *App) release(rt *Runtime) {
	a.rtMu.Lock()
	defer a.rtMu.Unlock()
	rt.refs--
	if rt.retired && rt.refs == 0 {
		a.releaseLocked(rt)
	}
}

func (a *App) releaseLocked(rt *Runtime) {
	for _, c := range rt.clients {
		a.clientUse[c]--
		if a.clientUse[c] <= 0 {
			delete(a.clientUse, c)
			if !a.MCP.Owns(c) {
				go c.Close()
			}
		}
	}
	rt.clients = nil
}

// retireClients 处理被 MCP.Sync 换下的连接：没有快照在用时立即关闭，否则等最后一个使用者释放。
func (a *App) retireClients(cs []*mcp.Client) {
	a.rtMu.Lock()
	defer a.rtMu.Unlock()
	for _, c := range cs {
		if a.clientUse[c] == 0 {
			go c.Close()
		}
	}
}

// Current 返回当前配置快照。
func (a *App) Current() *Runtime {
	a.rtMu.Lock()
	defer a.rtMu.Unlock()
	return a.cur
}

// Agent 返回当前版本的 Agent。
func (a *App) Agent() *agent.Agent { return a.Current().Agent }

// Config 返回当前生效的配置。
func (a *App) Config() *config.Config { return a.Current().Config }

// Skills 返回当前启用的技能。
func (a *App) Skills() []skill.Skill { return a.Current().Skills }

// Subagents 返回当前可用的子 Agent。
func (a *App) Subagents() []subagent.Definition { return a.Current().Subagents }

// Warnings 返回当前配置的警告。
func (a *App) Warnings() []string { return a.Current().Warnings }

// Memory 返回长期记忆存储（未启用时为 nil）。
func (a *App) Memory() *memory.Store {
	if !a.Config().Memory.Enabled {
		return nil
	}
	a.memMu.Lock()
	defer a.memMu.Unlock()
	return a.memory
}

// Run 用当前配置快照运行一轮对话：运行期间即使配置被热更新，本轮也始终使用开始时的快照。
//
// 开启监视时，Run 开始前会同步检查一次文件变化，保证"刚保存的配置 / 刚新建的技能"
// 在紧接着的这条消息里就能用上，而不必等下一次轮询。
func (a *App) Run(ctx context.Context, sess *session.Session, input string, sink agent.Sink) (*agent.Result, error) {
	if a.opts.Watch {
		a.checkChanges(ctx, false)
	}
	rt := a.acquire()
	defer a.release(rt)
	return rt.Agent.Run(ctx, sess, input, sink)
}

// Compact 手动压缩会话上下文。
func (a *App) Compact(ctx context.Context, sess *session.Session, sink agent.Sink) error {
	rt := a.acquire()
	defer a.release(rt)
	return rt.Agent.Compact(ctx, sess, sink)
}

// SetModel 在运行时切换模型（名称或 profiles 中的配置名），立即重新组装。
func (a *App) SetModel(name string) error {
	a.stMu.Lock()
	prev := a.override.model
	a.override.model = name
	a.stMu.Unlock()
	if err := a.Reload(context.Background()); err != nil {
		a.stMu.Lock()
		a.override.model = prev
		a.stMu.Unlock()
		return err
	}
	return nil
}

// SetMode 在运行时切换权限模式（对之后的所有配置版本生效）。
func (a *App) SetMode(mode permission.Mode) error {
	switch mode {
	case permission.ModeDefault, permission.ModeAcceptEdits, permission.ModePlan, permission.ModeBypass:
	default:
		return fmt.Errorf("未知的权限模式 %q", mode)
	}
	a.stMu.Lock()
	a.override.mode = string(mode)
	a.stMu.Unlock()
	a.Agent().Permissions.SetMode(mode)
	return nil
}

// ForgetSession 清除某会话的运行时状态与授权（会话被删除时调用）。
func (a *App) ForgetSession(id string) {
	a.States.Forget(id)
	a.Grants.Clear(id)
}

// ---------- 热更新 ----------

// Reload 立即重新加载配置并组装新快照（也会重试上次连接失败的 MCP 服务器）。
// 失败时保留当前快照并返回错误。
func (a *App) Reload(ctx context.Context) error {
	a.snapMu.Lock()
	defer a.snapMu.Unlock()
	a.retryMCP = true
	err := a.reload(ctx, nil)
	if a.opts.Watch {
		a.snap = fingerprint(a.Status().Watched)
	}
	return err
}

func (a *App) reload(ctx context.Context, changed []string) error {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	now := time.Now()
	cfg, err := a.load()
	var rt *Runtime
	if err == nil {
		rt, err = a.build(ctx, cfg, a.Current().Version+1)
	}
	a.stMu.Lock()
	a.status.LastCheckedAt = now
	if err != nil {
		a.status.LastError, a.status.LastErrorAt = err.Error(), now
		ver := a.status.Version
		a.stMu.Unlock()
		a.log.Warn("config reload failed，继续使用上一份可用配置", "version", ver, "err", err)
		a.publish(Event{Type: "config_error", Version: ver, Error: err.Error(), Time: now, Changed: changed})
		return err
	}
	a.swap(rt)
	a.status.Version, a.status.LoadedAt, a.status.LastError, a.status.LastErrorAt = rt.Version, rt.LoadedAt, "", time.Time{}
	a.status.Sources, a.status.Warnings, a.status.Watched = cfg.Sources, rt.Warnings, a.watchPaths(cfg)
	a.status.Watching = a.opts.Watch && cfg.ReloadEnabled()
	a.stMu.Unlock()
	a.log.Info("config reloaded", "version", rt.Version, "model", cfg.Model, "tools", len(rt.Agent.Tools.Names()), "skills", len(rt.Skills), "changed", changed)
	a.publish(Event{Type: "config_reloaded", Version: rt.Version, Time: now, Changed: changed})
	return nil
}

// Status 返回热更新状态。
func (a *App) Status() Status {
	a.stMu.Lock()
	defer a.stMu.Unlock()
	st := a.status
	st.Sources = append([]string(nil), st.Sources...)
	st.Watched = append([]string(nil), st.Watched...)
	return st
}

// Subscribe 订阅热更新事件；返回的函数用于取消订阅。
func (a *App) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 16)
	a.subMu.Lock()
	a.subs[ch] = struct{}{}
	a.subMu.Unlock()
	return ch, func() {
		a.subMu.Lock()
		if _, ok := a.subs[ch]; ok {
			delete(a.subs, ch)
			close(ch)
		}
		a.subMu.Unlock()
	}
}

func (a *App) publish(ev Event) {
	a.subMu.Lock()
	defer a.subMu.Unlock()
	for ch := range a.subs {
		select {
		case ch <- ev:
		default: // 订阅者太慢时丢弃，不能阻塞热更新
		}
	}
}

// watchPaths 返回需要监视的全部路径。
func (a *App) watchPaths(cfg *config.Config) []string {
	paths := cfg.ConfigFiles()
	paths = append(paths, cfg.PromptFiles()...)
	paths = append(paths, cfg.SkillDirs()...)
	paths = append(paths, cfg.AgentDirs()...)
	paths = append(paths, memory.InstructionPaths(cfg.WorkDir, cfg.UserDir)...)
	seen := map[string]bool{}
	out := paths[:0]
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// startWatch 启动后台监视。reload.enabled 为 false 时监视仍在运行，但只留意配置文件本身：
// 这样在运行中把它改回 true 也能生效，而提示词、技能等其他文件的修改不会触发重新加载。
func (a *App) startWatch(enabled bool) {
	a.stMu.Lock()
	a.status.Watching = enabled
	a.stMu.Unlock()
	a.snapMu.Lock()
	a.snap = fingerprint(a.Status().Watched)
	a.snapMu.Unlock()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		for {
			select {
			case <-a.stop:
				return
			case <-time.After(a.Config().ReloadInterval()):
			}
			a.checkChanges(context.Background(), true)
		}
	}()
}

// checkChanges 比较监视路径的指纹，有变化时重新加载。settle 为 true 时稍等片刻再加载：
// 编辑器保存文件常常分几步写入（先截断再写），立即读取可能读到半个文件。
func (a *App) checkChanges(ctx context.Context, settle bool) {
	a.snapMu.Lock()
	defer a.snapMu.Unlock()
	paths := a.Status().Watched
	enabled := a.Config().ReloadEnabled()
	if !enabled {
		paths = a.Config().ConfigFiles() // 热更新已关闭：只看配置文件，以便重新打开
	}
	cur := fingerprint(paths)
	if !cur.changedFrom(a.snap, enabled) {
		return
	}
	if settle {
		time.Sleep(150 * time.Millisecond)
		cur = fingerprint(paths)
	}
	changed := cur.diffFrom(a.snap, enabled)
	a.snap = cur
	_ = a.reload(ctx, changed)
	a.snap = fingerprint(a.Status().Watched) // 新配置可能引入新的监视路径
}

// Close 停止监视并释放资源（MCP 子进程、trace 文件）。
func (a *App) Close() {
	a.stopOnce.Do(func() { close(a.stop) })
	a.wg.Wait()
	if a.MCP != nil {
		a.MCP.Close()
	}
	if a.traceFile != nil {
		_ = a.traceFile.Close()
	}
	a.subMu.Lock()
	for ch := range a.subs {
		close(ch)
		delete(a.subs, ch)
	}
	a.subMu.Unlock()
}

// NewSession 创建新会话。
func (a *App) NewSession() *session.Session {
	s := session.New()
	s.Model = a.Agent().Model
	return s
}

// LoadSession 加载会话；id 为空时新建。
func (a *App) LoadSession(id string) (*session.Session, error) {
	if id == "" {
		return a.NewSession(), nil
	}
	return a.Store.Load(id)
}
