// goagent 命令行入口。
//
//	goagent                 交互式对话（REPL）
//	goagent run -p "任务"    非交互执行一次任务（适合脚本/CI）
//	goagent serve           启动 HTTP + SSE 服务（带内置 Web 界面）
//	goagent trace [文件]     以树形查看 trace
//	goagent sessions        列出历史会话
//	goagent config          查看生效的配置（密钥已脱敏）
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dmxj/goagent/agent"
	"github.com/dmxj/goagent/app"
	"github.com/dmxj/goagent/config"
	"github.com/dmxj/goagent/permission"
	"github.com/dmxj/goagent/server"
	"github.com/dmxj/goagent/session"
	"github.com/dmxj/goagent/trace"
)

var version = "dev"

func main() {
	if version == "dev" {
		// go install github.com/dmxj/goagent/cmd/goagent@v0.1.1 安装时没有 -ldflags，从模块信息中读取版本
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			version = bi.Main.Version
		}
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, red("错误：")+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "chat"
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "-help") {
		// 以 - 开头的参数默认交给 chat 子命令，帮助参数需要先单独识别，否则 switch 中的分支永远走不到
		cmd, args = "help", nil
	} else if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "chat":
		return cmdChat(args)
	case "run":
		return cmdRun(args)
	case "serve":
		return cmdServe(args)
	case "trace":
		return cmdTrace(args)
	case "sessions":
		return cmdSessions(args)
	case "config":
		return cmdConfig(args)
	case "version":
		fmt.Println("goagent", version)
		return nil
	case "help":
		fmt.Print(usage)
		return nil
	}
	return fmt.Errorf("未知命令 %q\n%s", cmd, usage)
}

const usage = `用法：
  goagent [chat]           交互式对话
  goagent run -p "任务"     执行一次任务后退出
  goagent serve            启动 HTTP 服务（默认 127.0.0.1:8787）
  goagent trace [文件]      查看 trace 树
  goagent sessions         列出会话
  goagent config           查看配置
通用参数：
  -dir 工作目录   -model 模型名   -profile 模型配置名   -mode 权限模式(default|accept_edits|plan|bypass)
`

type common struct {
	dir, model, profile, mode, resume string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.dir, "dir", "", "工作目录（默认当前目录）")
	fs.StringVar(&c.model, "model", "", "覆盖模型名")
	fs.StringVar(&c.profile, "profile", "", "使用 profiles 中的命名模型配置")
	fs.StringVar(&c.mode, "mode", "", "权限模式：default | accept_edits | plan | bypass")
	fs.StringVar(&c.resume, "resume", "", "恢复指定会话 ID")
}

func (c *common) load() (*config.Config, error) {
	cfg, err := config.Load(c.dir)
	if err != nil {
		return nil, err
	}
	return cfg, c.apply(cfg)
}

// apply 把命令行参数叠加到配置上。热更新重新加载配置后也会调用它，保证 -model / -mode 一直有效。
func (c *common) apply(cfg *config.Config) error {
	if c.profile != "" {
		if err := cfg.UseProfile(c.profile); err != nil {
			return err
		}
	}
	if c.model != "" {
		cfg.Model = config.CleanModel(c.model)
	}
	if c.mode != "" {
		cfg.Permissions.Mode = permission.Mode(c.mode)
	}
	return nil
}

// ---------- 终端渲染 ----------

func color(code, s string) string {
	if os.Getenv("NO_COLOR") != "" {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}
func dim(s string) string   { return color("2", s) }
func bold(s string) string  { return color("1", s) }
func red(s string) string   { return color("31", s) }
func green(s string) string { return color("32", s) }
func cyan(s string) string  { return color("36", s) }

// printer 把事件渲染到终端。
type printer struct {
	inText, inThink bool
	verbose         bool
}

func (p *printer) endLine() {
	if p.inText || p.inThink {
		fmt.Println()
	}
	p.inText, p.inThink = false, false
}

func (p *printer) handle(ev agent.Event) {
	sub := strings.Contains(ev.Agent, ">")
	indent := ""
	if sub {
		indent = "    "
	}
	switch ev.Type {
	case agent.EventThinking:
		if sub && !p.verbose {
			return
		}
		if !p.inThink {
			p.endLine()
			fmt.Print(indent + dim("✻ "))
			p.inThink = true
		}
		fmt.Print(dim(strings.ReplaceAll(ev.Text, "\n", "\n"+indent+"  ")))
	case agent.EventText:
		if sub && !p.verbose {
			return
		}
		if !p.inText {
			p.endLine()
			fmt.Print(indent)
			p.inText = true
		}
		fmt.Print(strings.ReplaceAll(ev.Text, "\n", "\n"+indent))
	case agent.EventToolCall:
		p.endLine()
		fmt.Printf("%s%s %s %s\n", indent, cyan("⏺"), bold(ev.Tool.Name), dim(summarizeInput(ev.Tool.Input)))
	case agent.EventToolResult:
		p.endLine()
		out := firstLines(ev.Tool.Output, 3)
		mark := green("⎿")
		if ev.Tool.IsError {
			mark = red("⎿")
		}
		fmt.Printf("%s  %s %s %s\n", indent, mark, strings.ReplaceAll(out, "\n", "\n"+indent+"    "), dim(fmt.Sprintf("(%dms)", ev.Tool.DurationMS)))
	case agent.EventCompact:
		p.endLine()
		fmt.Println(dim(fmt.Sprintf("↻ 上下文已压缩：%v → %v tokens", ev.Data["tokens_before"], ev.Data["tokens_after"])))
	case agent.EventRetry, agent.EventLog:
		p.endLine()
		fmt.Println(dim(indent + "· " + ev.Text))
	case agent.EventError:
		p.endLine()
		fmt.Println(red(indent + "✗ " + ev.Error))
	case agent.EventRunEnd:
		if sub {
			return
		}
		p.endLine()
		if ev.Usage != nil {
			fmt.Println(dim(fmt.Sprintf("── %v 轮 · 累计输入 %d / 输出 %d tokens", ev.Data["turns"], ev.Usage.InputTokens, ev.Usage.OutputTokens)))
		}
	}
}

func summarizeInput(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range []string{"command", "path", "pattern", "url", "description", "name", "query"} {
		if v, ok := m[k].(string); ok {
			return "(" + clip(v, 80) + ")"
		}
	}
	return clip(string(raw), 80)
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		return strings.Join(lines[:n], "\n") + dim(fmt.Sprintf(" …（共 %d 行）", len(lines)))
	}
	return strings.Join(lines, "\n")
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// cliApprover 在终端询问用户。并行的子 Agent 可能同时请求审批，用互斥锁让提示依次出现，
// 否则多个提示会交错打印、用户的输入也会被随机一个请求读走。
func cliApprover(in *bufio.Reader, p *printer) permission.Approver {
	var mu sync.Mutex
	return permission.ApproverFunc(func(ctx context.Context, req permission.Request, reason string) (bool, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if err := ctx.Err(); err != nil {
			return false, false, err
		}
		p.endLine()
		fmt.Printf("%s %s %s\n  %s\n", color("33", "?"), bold("允许调用 "+req.Tool+"？"), dim(summarizeInput(req.Input)), dim(reason))
		fmt.Print("  [y] 允许  [a] 本会话总是允许  [n] 拒绝 > ")
		line, err := in.ReadString('\n')
		if err != nil {
			return false, false, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes", "":
			return true, false, nil
		case "a", "always":
			return true, true, nil
		}
		return false, false, nil
	})
}

// ---------- chat ----------

func cmdChat(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	var c common
	c.register(fs)
	verbose := fs.Bool("v", false, "显示子 Agent 的详细输出")
	_ = fs.Parse(args)
	cfg, err := c.load()
	if err != nil {
		return err
	}
	in := bufio.NewReader(os.Stdin)
	p := &printer{verbose: *verbose}
	ctx := context.Background()
	a, err := app.New(ctx, cfg, app.Options{Approver: cliApprover(in, p), Override: c.apply, Watch: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		return err
	}
	defer a.Close()
	for _, w := range a.Warnings() {
		fmt.Println(color("33", "! ") + w)
	}
	sess, err := a.LoadSession(c.resume)
	if err != nil {
		return err
	}
	fmt.Printf("%s %s  %s\n", bold(cyan("GoAgent")), dim(version), dim(fmt.Sprintf("模型 %s · %d 个工具 · %d 个技能 · 权限 %s", a.Config().Model, len(a.Agent().Tools.List()), len(a.Skills()), a.Agent().Permissions.Mode())))
	fmt.Println(dim("工作目录 " + cfg.WorkDir + " · 会话 " + sess.ID + " · 输入 /help 查看命令 · 修改 .goagent/config.json 即时生效"))
	events, unsubscribe := a.Subscribe()
	defer unsubscribe()
	go func() {
		for ev := range events {
			if ev.Type == "config_error" {
				fmt.Println(color("33", "\n! 配置有误，继续使用上一份配置：") + ev.Error)
			} else {
				fmt.Println(dim(fmt.Sprintf("\n↻ 配置已重新加载（版本 %d），下一条消息生效", ev.Version)))
			}
		}
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	defer signal.Stop(sigs)
	intr := newInterrupter(func() {
		fmt.Println(dim("\n再见"))
		a.Close()
		os.Exit(0)
	})
	go func() {
		for range sigs {
			intr.handle()
		}
	}()
	for {
		fmt.Print("\n" + bold(cyan("› ")))
		line, err := in.ReadString('\n')
		if err != nil {
			fmt.Println()
			return nil
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			quit, err := slash(ctx, a, &sess, line)
			if err != nil {
				fmt.Println(red("✗ ") + err.Error())
			}
			if quit {
				return nil
			}
			continue
		}
		runCtx, cancel := context.WithCancel(ctx)
		intr.begin(cancel)
		_, err = a.Run(runCtx, sess, line, p.handle)
		intr.end()
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) {
			p.endLine()
		}
	}
}

// interrupter 处理 REPL 中的 Ctrl+C：运行中按下取消当前任务；空闲时第一次只给出提示，
// 2 秒内再按一次退出程序。
//
// v0.1.0 在每次运行时启动一个"看门" goroutine 读取容量为 1 的信号 channel：空闲时按下的 Ctrl+C
// 没人接收，一直留在 channel 里，下一个任务一开始就被它中断。现在由一个常驻 goroutine 消费全部信号，
// 按"此刻是否有任务在运行"决定含义，不会有残留信号。
type interrupter struct {
	mu       sync.Mutex
	cancel   context.CancelFunc
	lastIdle time.Time
	exit     func()
	now      func() time.Time
}

func newInterrupter(exit func()) *interrupter {
	return &interrupter{exit: exit, now: time.Now}
}

// begin 登记正在运行的任务。
func (i *interrupter) begin(cancel context.CancelFunc) {
	i.mu.Lock()
	i.cancel, i.lastIdle = cancel, time.Time{}
	i.mu.Unlock()
}

// end 表示任务结束，之后的 Ctrl+C 属于空闲状态。
func (i *interrupter) end() {
	i.mu.Lock()
	i.cancel = nil
	i.mu.Unlock()
}

// handle 处理一次 Ctrl+C，返回它的含义：cancelled | hint | exit。
func (i *interrupter) handle() string {
	i.mu.Lock()
	if i.cancel != nil {
		c := i.cancel
		i.cancel = nil
		i.mu.Unlock()
		fmt.Println(dim("\n· 已中断"))
		c()
		return "cancelled"
	}
	now := i.now()
	if !i.lastIdle.IsZero() && now.Sub(i.lastIdle) < 2*time.Second {
		i.mu.Unlock()
		i.exit()
		return "exit"
	}
	i.lastIdle = now
	i.mu.Unlock()
	fmt.Print(dim("\n（再按一次 Ctrl+C 退出，或输入 /exit）\n") + bold(cyan("› ")))
	return "hint"
}

func slash(ctx context.Context, a *app.App, sess **session.Session, line string) (bool, error) {
	fields := strings.Fields(line)
	switch fields[0] {
	case "/exit", "/quit":
		return true, nil
	case "/help":
		fmt.Println(`/new            新会话          /resume <id>  恢复会话
/sessions       会话列表        /compact      压缩上下文
/model <名称>   切换模型        /mode <模式>  切换权限模式
/tools          工具列表        /skills       技能列表
/agents         子 Agent 列表   /memory       长期记忆
/trace          本会话 trace    /files        访问过的文件
/config         配置与热更新    /reload       立即重新加载配置
/usage          token 用量      /exit         退出`)
	case "/new", "/clear":
		*sess = a.NewSession()
		fmt.Println(dim("已开始新会话 " + (*sess).ID))
	case "/resume":
		if len(fields) < 2 {
			return false, errors.New("用法：/resume <会话ID>")
		}
		s, err := a.Store.Load(fields[1])
		if err != nil {
			return false, err
		}
		*sess = s
		fmt.Println(dim(fmt.Sprintf("已恢复会话 %s（%d 条消息）", s.ID, len(s.Messages))))
	case "/sessions":
		return false, printSessions(a.Store)
	case "/compact":
		return false, a.Compact(ctx, *sess, func(ev agent.Event) {
			if ev.Type == agent.EventCompact {
				fmt.Println(dim(fmt.Sprintf("已压缩：%v → %v tokens", ev.Data["tokens_before"], ev.Data["tokens_after"])))
			}
		})
	case "/model":
		if len(fields) < 2 {
			fmt.Println("当前模型：" + a.Agent().Model)
			return false, nil
		}
		// 模型名或 profiles 中的配置名；Provider 一起切换，下一条消息生效
		if err := a.SetModel(fields[1]); err != nil {
			return false, err
		}
		fmt.Println(dim("模型：" + a.Agent().Model))
	case "/mode":
		if len(fields) < 2 {
			fmt.Println("当前权限模式：" + string(a.Agent().Permissions.Mode()))
			return false, nil
		}
		if err := a.SetMode(permission.Mode(fields[1])); err != nil {
			return false, err
		}
		fmt.Println(dim("权限模式：" + fields[1]))
	case "/config":
		st := a.Status()
		fmt.Printf("配置版本 %d · 加载于 %s · 监视 %d 个路径\n", st.Version, st.LoadedAt.Format("15:04:05"), len(st.Watched))
		fmt.Println(dim("来源：" + strings.Join(st.Sources, " → ")))
		if st.LastError != "" {
			fmt.Println(red("最近一次加载失败：") + st.LastError)
		}
	case "/reload":
		if err := a.Reload(ctx); err != nil {
			return false, err
		}
		fmt.Println(dim(fmt.Sprintf("已重新加载配置（版本 %d）", a.Status().Version)))
	case "/tools":
		for _, t := range a.Agent().Tools.List() {
			i := t.Info()
			ro := ""
			if i.ReadOnly {
				ro = dim(" [只读]")
			}
			fmt.Printf("  %s%s  %s\n", bold(i.Name), ro, dim(clip(i.Description, 70)))
		}
	case "/skills":
		if len(a.Skills()) == 0 {
			fmt.Println(dim("没有技能。在 .goagent/skills/<名称>/SKILL.md 中添加"))
		}
		for _, s := range a.Skills() {
			fmt.Printf("  %s  %s\n", bold(s.Name), dim(s.Description))
		}
	case "/agents":
		for _, d := range a.Subagents() {
			fmt.Printf("  %s  %s\n", bold(d.Name), dim(d.Description))
		}
	case "/memory":
		if a.Memory() == nil {
			return false, errors.New("记忆功能未启用")
		}
		idx := a.Memory().Index()
		if idx == "" {
			idx = "(暂无记忆)"
		}
		fmt.Println(idx)
	case "/trace":
		var traceIDs []string
		seen := map[string]bool{}
		spans := a.Traces.Spans("")
		for _, s := range spans {
			if s.Kind == trace.KindAgent && s.Attrs["session_id"] == (*sess).ID && !seen[s.TraceID] {
				seen[s.TraceID] = true
				traceIDs = append(traceIDs, s.TraceID)
			}
		}
		for _, id := range traceIDs {
			fmt.Print(trace.Tree(a.Traces.Spans(id)))
		}
	case "/files":
		for _, f := range a.Agent().Files((*sess).ID) {
			fmt.Printf("  %s %-6s %s\n", dim(f.At.Format("15:04:05")), f.Op, f.Path)
		}
	case "/usage":
		u := (*sess).Usage
		fmt.Printf("输入 %d · 输出 %d · 缓存读取 %d tokens\n", u.InputTokens, u.OutputTokens, u.CacheReadTokens)
	default:
		return false, fmt.Errorf("未知命令 %s，输入 /help 查看", fields[0])
	}
	return false, nil
}

func printSessions(store session.Store) error {
	list, err := store.List()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println(dim("暂无会话"))
	}
	for _, s := range list {
		fmt.Printf("  %s  %s  %s\n", bold(s.ID), dim(s.UpdatedAt.Format("01-02 15:04")), s.Title)
	}
	return nil
}

// ---------- run ----------

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var c common
	c.register(fs)
	promptText := fs.String("p", "", "任务内容（也可以从 stdin 读取）")
	jsonOut := fs.Bool("json", false, "以 JSONL 输出全部事件")
	_ = fs.Parse(args)
	if *promptText == "" && fs.NArg() > 0 {
		*promptText = strings.Join(fs.Args(), " ")
	}
	if *promptText == "" {
		b, _ := bufio.NewReader(os.Stdin).ReadString(0)
		*promptText = strings.TrimSpace(b)
	}
	if *promptText == "" {
		return errors.New("请用 -p 提供任务")
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a, err := app.New(ctx, cfg, app.Options{Approver: permission.AutoDeny, Override: c.apply})
	if err != nil {
		return err
	}
	defer a.Close()
	sess, err := a.LoadSession(c.resume)
	if err != nil {
		return err
	}
	p := &printer{}
	sink := p.handle
	if *jsonOut {
		sink = jsonlSink(os.Stdout)
	}
	res, err := a.Run(ctx, sess, *promptText, sink)
	if err != nil {
		return err
	}
	if !*jsonOut {
		p.endLine()
		fmt.Fprintln(os.Stderr, dim("会话 "+sess.ID+" · trace "+res.TraceID))
	}
	return nil
}

// jsonlSink 把事件逐行编码为 JSON。关闭 HTML 转义：否则 Agent 路径 main>explore
// 会被编码成 main\u003eexplore，用 grep / jq 按路径过滤时搜不到。
func jsonlSink(w io.Writer) agent.Sink {
	var mu sync.Mutex
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return func(ev agent.Event) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(ev)
	}
}

// ---------- serve ----------

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var c common
	c.register(fs)
	addr := fs.String("addr", "", "监听地址（默认 127.0.0.1:8787，可用环境变量 GOAGENT_ADDR）")
	token := fs.String("token", os.Getenv("GOAGENT_SERVER_TOKEN"), "访问令牌（可选）")
	_ = fs.Parse(args)
	if *addr == "" {
		*addr = os.Getenv("GOAGENT_ADDR")
	}
	if *addr == "" {
		*addr = "127.0.0.1:8787"
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	approver := server.NewWebApprover()
	a, err := app.New(ctx, cfg, app.Options{Approver: approver, Override: c.apply, Watch: true, Logger: logger})
	if err != nil {
		return err
	}
	defer a.Close()
	for _, w := range a.Warnings() {
		logger.Warn(w)
	}
	server.Version = version
	srv := server.New(a, approver)
	srv.Token = *token
	srv.Log = logger
	hs := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- hs.ListenAndServe() }()
	logger.Info("GoAgent 服务已启动", "addr", "http://"+*addr, "model", a.Config().Model, "workdir", cfg.WorkDir, "tools", len(a.Agent().Tools.List()), "hot_reload", a.Status().Watching)
	if *token == "" && !strings.HasPrefix(*addr, "127.0.0.1") && !strings.HasPrefix(*addr, "localhost") {
		logger.Warn("服务监听在非本机地址且未设置 -token，任何能访问该端口的人都可以驱动 Agent 执行命令")
	}
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		logger.Info("正在优雅关闭…")
		srv.Shutdown() // 先取消运行中的对话，SSE 连接收到 run_end / done 后自然结束
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return hs.Shutdown(sctx)
	}
	return nil
}

// ---------- trace / sessions / config ----------

func cmdTrace(args []string) error {
	fs := flag.NewFlagSet("trace", flag.ExitOnError)
	dir := fs.String("dir", "", "工作目录")
	_ = fs.Parse(args)
	path := fs.Arg(0)
	if path == "" {
		wd := *dir
		if wd == "" {
			wd, _ = os.Getwd()
		}
		files, _ := filepath.Glob(filepath.Join(wd, ".goagent", "traces", "*.jsonl"))
		if len(files) == 0 {
			return errors.New("没有找到 trace 文件")
		}
		sort.Strings(files)
		path = files[len(files)-1]
	}
	spans, err := trace.ReadFile(path)
	if err != nil {
		return err
	}
	fmt.Println(dim(path))
	fmt.Print(trace.Tree(spans))
	return nil
}

func cmdSessions(args []string) error {
	fs := flag.NewFlagSet("sessions", flag.ExitOnError)
	var c common
	c.register(fs)
	_ = fs.Parse(args)
	cfg, err := c.load()
	if err != nil {
		return err
	}
	store, err := session.NewFileStore(filepath.Join(cfg.ProjectDir, "sessions"))
	if err != nil {
		return err
	}
	return printSessions(store)
}

func cmdConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	var c common
	c.register(fs)
	_ = fs.Parse(args)
	cfg, err := c.load()
	if err != nil {
		return err
	}
	fmt.Println(dim("配置来源：" + strings.Join(cfg.Sources, " → ")))
	fmt.Println(cfg.Redacted())
	return nil
}
