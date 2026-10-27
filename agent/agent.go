// Package agent 实现 Agent 的核心循环（Agent Loop）：
//
//	用户输入 → [组装上下文 → 调用模型（流式）→ 解析工具调用 → 权限/钩子 → 执行工具 → 回填结果] × N → 最终回答
//
// 这个循环就是所有"编程 Agent"（Claude Code、OpenCode、pi……）的心脏。
// 其余模块（工具、记忆、技能、子 Agent、MCP、Trace）都是插在这个循环上的扩展点。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/dmxj/goagent/hook"
	"github.com/dmxj/goagent/llm"
	"github.com/dmxj/goagent/memory"
	"github.com/dmxj/goagent/permission"
	"github.com/dmxj/goagent/session"
	"github.com/dmxj/goagent/tool"
	"github.com/dmxj/goagent/trace"
)

// Agent 是一个可运行的智能体。一个 Agent 可以同时服务多个会话（Session），
// 自身配置在运行期间只读，会话级状态都放在 Session 与 sessionState 中。
type Agent struct {
	Name        string
	Provider    llm.Provider
	Model       string
	MaxTokens   int
	Temperature *float64
	// ThinkingBudget > 0 时开启扩展思考。
	ThinkingBudget int
	// Prompt 在每次调用模型前生成系统提示词（可以动态包含记忆索引、当前工具等）。
	// 未设置时使用 System。
	Prompt PromptBuilder
	// System 是简单场景下的系统提示词函数。
	System func() string

	Tools       *tool.Registry
	Permissions *permission.Checker
	Hooks       *hook.Manager
	Tracer      *trace.Tracer
	Compactor   *memory.Compactor
	Store       session.Store

	WorkDir string
	// MaxTurns 是单次 Run 中调用模型的最大轮数，防止死循环。
	MaxTurns int
	// ToolTimeout 是单个工具调用的超时。
	ToolTimeout time.Duration
	// MaxParallelTools 是并行执行工具的最大并发数。
	MaxParallelTools int
	// MaxToolOutput 是回填给模型的工具输出最大字节数。
	MaxToolOutput int
	// KeepThinking 为 true 时在历史中保留无签名的思考块（部分网关需要）。
	KeepThinking bool

	states   sync.Map // sessionID -> *sessionState
	initOnce sync.Once
}

// sessionState 是会话级的运行时状态（不持久化）。
type sessionState struct {
	files   *tool.FileTracker
	values  *sync.Map
	mu      sync.Mutex
	steer   []string // 运行中途用户追加的消息
	running bool
}

func (a *Agent) state(id string) *sessionState {
	v, _ := a.states.LoadOrStore(id, &sessionState{files: tool.NewFileTracker(), values: &sync.Map{}})
	return v.(*sessionState)
}

// Files 返回某会话的文件访问记录。
func (a *Agent) Files(sessionID string) []tool.FileAccess {
	return a.state(sessionID).files.History()
}

// Values 返回某会话的共享状态（例如 todo 列表）。
func (a *Agent) Values(sessionID string) *sync.Map { return a.state(sessionID).values }

// Steer 在 Agent 运行过程中插入一条用户消息，会在下一次调用模型前送达。
// 这让用户可以"边看边纠正"，而不必等待或中断整个任务。
func (a *Agent) Steer(sessionID, text string) bool {
	st := a.state(sessionID)
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.running {
		return false
	}
	st.steer = append(st.steer, text)
	return true
}

// Running 报告会话是否正在运行。
func (a *Agent) Running(sessionID string) bool {
	st := a.state(sessionID)
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.running
}

func (st *sessionState) drainSteer() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := st.steer
	st.steer = nil
	return out
}

// Result 是一次 Run 的结果。
type Result struct {
	Text       string    `json:"text"`
	Turns      int       `json:"turns"`
	Usage      llm.Usage `json:"usage"`
	StopReason string    `json:"stop_reason"`
	TraceID    string    `json:"trace_id"`
}

// ErrMaxTurns 表示达到最大轮数。
var ErrMaxTurns = errors.New("达到最大轮数限制")

// ErrBusy 表示该会话已有任务在运行。
var ErrBusy = errors.New("该会话正在运行中")

// defaults 填充未设置字段的默认值。只执行一次：同一个 Agent 会被多个会话并发 Run，
// 每次 Run 都写字段会产生数据竞争。
func (a *Agent) defaults() { a.initOnce.Do(a.applyDefaults) }

func (a *Agent) applyDefaults() {
	if a.Name == "" {
		a.Name = "main"
	}
	if a.MaxTurns <= 0 {
		a.MaxTurns = 50
	}
	if a.ToolTimeout <= 0 {
		a.ToolTimeout = 10 * time.Minute
	}
	if a.MaxParallelTools <= 0 {
		a.MaxParallelTools = 8
	}
	if a.MaxToolOutput <= 0 {
		a.MaxToolOutput = 40000
	}
	if a.MaxTokens <= 0 {
		a.MaxTokens = 8192
	}
	if a.Tools == nil {
		a.Tools = tool.NewRegistry()
	}
}

// Run 处理一条用户输入，直到模型给出最终回答（不再调用工具）。
// sink 接收运行中的所有事件，可为 nil。
func (a *Agent) Run(ctx context.Context, sess *session.Session, input string, sink Sink) (res *Result, err error) {
	a.defaults()
	st := a.state(sess.ID)
	st.mu.Lock()
	if st.running {
		st.mu.Unlock()
		return nil, ErrBusy
	}
	st.running = true
	st.mu.Unlock()
	defer func() {
		st.mu.Lock()
		st.running = false
		st.steer = nil
		st.mu.Unlock()
	}()

	ctx = withPath(ctx, a.Name)
	path := AgentPath(ctx)
	if parentSink := SinkFrom(ctx); sink == nil {
		sink = parentSink
	}
	var emitMu sync.Mutex
	emit := func(ev Event) {
		ev.Time = time.Now()
		if ev.Agent == "" {
			ev.Agent = path
		}
		ev.SessionID = sess.ID
		emitMu.Lock()
		defer emitMu.Unlock()
		sink(ev)
	}
	ctx = WithSink(ctx, func(ev Event) { // 深层组件（审批器、子 Agent）发出的事件也带上会话信息
		if ev.Agent == "" {
			ev.Agent = path
		}
		if ev.SessionID == "" {
			ev.SessionID = sess.ID
		}
		if ev.Time.IsZero() {
			ev.Time = time.Now()
		}
		emitMu.Lock()
		defer emitMu.Unlock()
		sink(ev)
	})

	ctx, span := a.Tracer.Start(ctx, "agent.run", trace.KindAgent, map[string]any{
		"agent": path, "session_id": sess.ID, "model": a.Model, "input": clip(input, 2000),
	})
	res = &Result{TraceID: span.TraceID}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("agent panic: %v\n%s", r, debug.Stack())
		}
		res.Usage = sess.Usage
		if err != nil {
			span.Fail(err)
			emit(Event{Type: EventError, Error: err.Error()})
		}
		span.SetAttr("turns", res.Turns)
		span.SetAttr("output", clip(res.Text, 2000))
		span.Finish()
		emit(Event{Type: EventSpanEnd, Span: ptr(span.Snapshot())})
		if a.Store != nil {
			_ = a.Store.Save(sess)
		}
		emit(Event{Type: EventRunEnd, Text: res.Text, Usage: &res.Usage, Data: map[string]any{"turns": res.Turns, "stop_reason": res.StopReason, "trace_id": res.TraceID}})
	}()
	emit(Event{Type: EventSpanStart, Span: ptr(span.Snapshot())})
	emit(Event{Type: EventRunStart, Text: input, Data: map[string]any{"trace_id": span.TraceID, "model": a.Model}})

	// ① SessionStart 钩子（仅会话的第一条输入）与 UserPromptSubmit 钩子：
	// 可以拦截输入，或补充上下文（如注入当前 git 状态）
	var extraCtx []string
	if len(sess.History()) == 0 && a.Hooks.Count(hook.SessionStart) > 0 {
		sout, serr := a.Hooks.Run(ctx, &hook.Input{Event: hook.SessionStart, SessionID: sess.ID, WorkDir: a.WorkDir, Prompt: input, AgentName: a.Name})
		if serr != nil {
			emit(Event{Type: EventLog, Text: "SessionStart 钩子出错：" + serr.Error()})
		}
		if sout.AdditionalContext != "" {
			extraCtx = append(extraCtx, sout.AdditionalContext)
		}
	}
	hout, herr := a.Hooks.Run(ctx, &hook.Input{Event: hook.UserPromptSubmit, SessionID: sess.ID, WorkDir: a.WorkDir, Prompt: input, AgentName: a.Name})
	if herr != nil {
		emit(Event{Type: EventLog, Text: "UserPromptSubmit 钩子出错：" + herr.Error()})
	}
	if hout.Block {
		res.StopReason = "blocked"
		res.Text = "输入被钩子拦截：" + hout.Reason
		return res, nil
	}
	if hout.AdditionalContext != "" {
		extraCtx = append(extraCtx, hout.AdditionalContext)
	}
	userMsg := llm.UserText(input)
	if len(extraCtx) > 0 {
		userMsg.Content = append(userMsg.Content, llm.ContentBlock{Type: llm.BlockText, Text: "\n\n<hook-context>\n" + strings.Join(extraCtx, "\n") + "\n</hook-context>"})
	}
	sess.Append(userMsg)

	lastInput := 0
	stopHookRuns := 0
	for turn := 1; ; turn++ {
		if err := ctx.Err(); err != nil {
			res.StopReason = "cancelled"
			return res, err
		}
		if turn > a.MaxTurns {
			res.StopReason = "max_turns"
			return res, ErrMaxTurns
		}
		res.Turns = turn
		emit(Event{Type: EventTurnStart, Turn: turn})

		// ② 插入用户在运行中途追加的消息（steering）
		if msgs := st.drainSteer(); len(msgs) > 0 {
			sess.Append(llm.UserText("（用户补充）" + strings.Join(msgs, "\n")))
			emit(Event{Type: EventLog, Turn: turn, Text: "已插入用户补充消息"})
		}

		system := a.SystemPrompt()
		// ③ 上下文过长时自动压缩
		if a.Compactor.ShouldCompact(system, sess.History(), lastInput) {
			a.compact(ctx, sess, emit)
		}

		// ④ 调用模型（流式）
		resp, err := a.callModel(ctx, sess, system, turn, emit)
		if err != nil {
			res.StopReason = "error"
			if ctx.Err() != nil {
				res.StopReason = "cancelled"
			}
			return res, err
		}
		// 先取出原始工具调用（execOne 会对非法参数报错），再清理消息中的非法 JSON：
		// 模型输出被 max_tokens 截断时参数可能不完整，原样放进历史会导致
		// 下一次请求与会话保存都因 json.RawMessage 非法而序列化失败。
		calls := resp.Message.ToolUses()
		sanitizeToolInputs(&resp.Message)
		lastInput = resp.Usage.InputTokens + resp.Usage.CacheReadTokens
		sess.AddUsage(resp.Usage)
		sess.Append(resp.Message)
		msg := resp.Message
		emit(Event{Type: EventMessage, Turn: turn, Message: &msg})
		u := sess.Usage
		emit(Event{Type: EventUsage, Turn: turn, Usage: &u})
		if a.Store != nil {
			_ = a.Store.Save(sess)
		}

		if len(calls) == 0 {
			res.Text = resp.Message.Text()
			res.StopReason = resp.StopReason
			// ⑤ Stop 钩子：可以要求 Agent 继续工作（例如测试未通过）
			if a.Hooks.Count(hook.Stop) > 0 && stopHookRuns < 3 {
				out, _ := a.Hooks.Run(ctx, &hook.Input{Event: hook.Stop, SessionID: sess.ID, WorkDir: a.WorkDir, LastReply: res.Text, AgentName: a.Name})
				if out.Block {
					stopHookRuns++
					sess.Append(llm.UserText("Stop 钩子要求继续：" + out.Reason))
					emit(Event{Type: EventLog, Turn: turn, Text: "Stop 钩子要求继续：" + out.Reason})
					continue
				}
			}
			if resp.StopReason == llm.StopMaxTokens {
				emit(Event{Type: EventLog, Turn: turn, Text: "输出达到 max_tokens 上限，回答可能不完整"})
			}
			return res, nil
		}

		// ⑥ 执行工具并把结果作为一条 user 消息回填
		results := a.execTools(ctx, sess, calls, turn, emit)
		sess.Append(llm.Message{Role: llm.RoleUser, Content: results})
	}
}

// PromptBuilder 根据 Agent 当前配置生成系统提示词。
type PromptBuilder interface {
	Build(a *Agent) string
}

// PromptFunc 让函数实现 PromptBuilder。
type PromptFunc func(a *Agent) string

func (f PromptFunc) Build(a *Agent) string { return f(a) }

// SystemPrompt 返回当前的系统提示词。
func (a *Agent) SystemPrompt() string {
	switch {
	case a.Prompt != nil:
		return a.Prompt.Build(a)
	case a.System != nil:
		return a.System()
	}
	return ""
}

// callModel 组装请求并流式调用模型，把增量转成事件。
func (a *Agent) callModel(ctx context.Context, sess *session.Session, system string, turn int, emit func(Event)) (*llm.Response, error) {
	history := sess.History()
	if !a.KeepThinking {
		history = stripUnsignedThinking(history)
	}
	req := &llm.Request{
		Model:          a.Model,
		System:         system,
		Messages:       history,
		Tools:          a.Tools.Specs(),
		MaxTokens:      a.MaxTokens,
		Temperature:    a.Temperature,
		ThinkingBudget: a.ThinkingBudget,
	}
	ctx, span := a.Tracer.Start(ctx, "llm.call", trace.KindLLM, map[string]any{
		"model": a.Model, "turn": turn, "messages": len(history), "tools": len(req.Tools), "provider": a.Provider.Name(),
	})
	defer func() { span.Finish(); emit(Event{Type: EventSpanEnd, Turn: turn, Span: ptr(span.Snapshot())}) }()
	emit(Event{Type: EventSpanStart, Turn: turn, Span: ptr(span.Snapshot())})

	if rp, ok := a.Provider.(*llm.RetryProvider); ok && rp.OnRetry == nil {
		// 每次调用复制一份，避免并发修改共享的 OnRetry
		cp := *rp
		cp.OnRetry = func(attempt int, delay time.Duration, err error) {
			span.AddEvent("retry", map[string]any{"attempt": attempt, "delay_ms": delay.Milliseconds(), "error": err.Error()})
			emit(Event{Type: EventRetry, Turn: turn, Text: fmt.Sprintf("模型调用失败，%s 后第 %d 次重试：%v", delay.Round(time.Millisecond), attempt, err)})
		}
		return a.stream(ctx, &cp, req, span, turn, emit)
	}
	return a.stream(ctx, a.Provider, req, span, turn, emit)
}

func (a *Agent) stream(ctx context.Context, p llm.Provider, req *llm.Request, span *trace.Span, turn int, emit func(Event)) (*llm.Response, error) {
	start := time.Now()
	var firstToken time.Duration
	resp, err := p.Stream(ctx, req, func(ev llm.StreamEvent) {
		if firstToken == 0 && (ev.Type == llm.EventTextDelta || ev.Type == llm.EventThinkingDelta || ev.Type == llm.EventToolUseStart) {
			firstToken = time.Since(start)
		}
		switch ev.Type {
		case llm.EventTextDelta:
			emit(Event{Type: EventText, Turn: turn, Text: ev.Text})
		case llm.EventThinkingDelta:
			emit(Event{Type: EventThinking, Turn: turn, Text: ev.Text})
		case llm.EventToolUseStart:
			emit(Event{Type: EventToolStart, Turn: turn, Tool: &ToolEvent{ID: ev.Block.ID, Name: ev.Block.Name}})
		case llm.EventToolUseDelta:
			emit(Event{Type: EventToolInputDelta, Turn: turn, Text: ev.Text, Data: map[string]any{"index": ev.Index}})
		}
	})
	if err != nil {
		span.Fail(err)
		return nil, err
	}
	span.SetAttr("input_tokens", resp.Usage.InputTokens)
	span.SetAttr("output_tokens", resp.Usage.OutputTokens)
	span.SetAttr("cache_read_tokens", resp.Usage.CacheReadTokens)
	span.SetAttr("stop_reason", resp.StopReason)
	span.SetAttr("ttft_ms", firstToken.Milliseconds())
	span.SetAttr("output", clip(resp.Message.Text(), 4000))
	if n := len(resp.Message.ToolUses()); n > 0 {
		var names []string
		for _, c := range resp.Message.ToolUses() {
			names = append(names, c.Name)
		}
		span.SetAttr("tool_calls", names)
	}
	return resp, nil
}

// compact 执行上下文压缩。
func (a *Agent) compact(ctx context.Context, sess *session.Session, emit func(Event)) {
	ctx, span := a.Tracer.Start(ctx, "context.compact", trace.KindCompact, nil)
	emit(Event{Type: EventSpanStart, Span: ptr(span.Snapshot())})
	defer func() { span.Finish(); emit(Event{Type: EventSpanEnd, Span: ptr(span.Snapshot())}) }()
	_, _ = a.Hooks.Run(ctx, &hook.Input{Event: hook.PreCompact, SessionID: sess.ID, WorkDir: a.WorkDir})
	before := memory.EstimateMessages(sess.History())
	msgs, summary, err := a.Compactor.Compact(ctx, sess.History(), sess.Summary)
	if err != nil {
		span.Fail(err)
		emit(Event{Type: EventLog, Text: "上下文压缩失败：" + err.Error()})
		return
	}
	sess.Replace(msgs, summary)
	after := memory.EstimateMessages(msgs)
	span.SetAttr("tokens_before", before)
	span.SetAttr("tokens_after", after)
	emit(Event{Type: EventCompact, Text: summary, Data: map[string]any{"tokens_before": before, "tokens_after": after}})
}

// Compact 手动触发压缩（例如 CLI 的 /compact 命令）。
func (a *Agent) Compact(ctx context.Context, sess *session.Session, sink Sink) error {
	if a.Compactor == nil {
		return errors.New("未配置 Compactor")
	}
	if sink == nil {
		sink = func(Event) {}
	}
	var err error
	a.compact(ctx, sess, func(ev Event) {
		ev.Agent, ev.SessionID, ev.Time = a.Name, sess.ID, time.Now()
		if ev.Type == EventLog {
			err = errors.New(ev.Text)
		}
		sink(ev)
	})
	return err
}

// execTools 执行一组工具调用。相邻的可并发工具（只读工具、子 Agent）会并行执行，
// 有副作用的工具按顺序串行执行，结果顺序与调用顺序保持一致。
func (a *Agent) execTools(ctx context.Context, sess *session.Session, calls []llm.ContentBlock, turn int, emit func(Event)) []llm.ContentBlock {
	results := make([]llm.ContentBlock, len(calls))
	st := a.state(sess.ID)
	env := &tool.Env{
		WorkDir: a.WorkDir, SessionID: sess.ID, Files: st.files, Values: st.values,
		Log: func(format string, args ...any) {
			emit(Event{Type: EventLog, Turn: turn, Text: fmt.Sprintf(format, args...)})
		},
	}
	concurrent := func(c llm.ContentBlock) bool {
		t, ok := a.Tools.Get(c.Name)
		return ok && t.Info().Concurrent
	}
	for i := 0; i < len(calls); {
		j := i + 1
		if concurrent(calls[i]) {
			for j < len(calls) && concurrent(calls[j]) {
				j++
			}
		}
		if j-i == 1 {
			results[i] = a.execOne(ctx, sess, calls[i], env, turn, emit)
		} else {
			var wg sync.WaitGroup
			sem := make(chan struct{}, a.MaxParallelTools)
			for k := i; k < j; k++ {
				wg.Add(1)
				sem <- struct{}{}
				go func(k int) {
					defer wg.Done()
					defer func() { <-sem }()
					results[k] = a.execOne(ctx, sess, calls[k], env, turn, emit)
				}(k)
			}
			wg.Wait()
		}
		i = j
	}
	return results
}

// execOne 执行单个工具调用：钩子 → 权限 → 执行（超时、panic 保护）→ 钩子 → 截断。
func (a *Agent) execOne(ctx context.Context, sess *session.Session, call llm.ContentBlock, env *tool.Env, turn int, emit func(Event)) (block llm.ContentBlock) {
	start := time.Now()
	input := call.Input
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	ctx, span := a.Tracer.Start(ctx, "tool."+call.Name, trace.KindTool, map[string]any{
		"tool": call.Name, "call_id": call.ID, "input": clip(string(input), 4000),
	})
	emit(Event{Type: EventSpanStart, Turn: turn, Span: ptr(span.Snapshot())})
	var result *tool.Result
	defer func() {
		if r := recover(); r != nil {
			result = tool.Errorf("工具 %s 发生 panic：%v", call.Name, r)
		}
		content := tool.Truncate(result.Content, a.MaxToolOutput)
		if content == "" {
			content = "(无输出)"
		}
		block = llm.ContentBlock{Type: llm.BlockToolResult, ToolUseID: call.ID, Content: content, IsError: result.IsError}
		dur := time.Since(start)
		span.SetAttr("output", clip(content, 4000))
		span.SetAttr("is_error", result.IsError)
		for k, v := range result.Meta {
			span.SetAttr("meta."+k, v)
		}
		if result.IsError {
			span.Fail(errors.New(clip(content, 300)))
		}
		span.Finish()
		emit(Event{Type: EventSpanEnd, Turn: turn, Span: ptr(span.Snapshot())})
		emit(Event{Type: EventToolResult, Turn: turn, Tool: &ToolEvent{
			ID: call.ID, Name: call.Name, Input: input, Output: content, IsError: result.IsError, Meta: result.Meta, DurationMS: dur.Milliseconds(),
		}})
	}()

	t, ok := a.Tools.Get(call.Name)
	if !ok {
		result = tool.Errorf("工具 %q 不存在。可用工具：%s", call.Name, strings.Join(a.Tools.Names(), ", "))
		emit(Event{Type: EventToolCall, Turn: turn, Tool: &ToolEvent{ID: call.ID, Name: call.Name, Input: input}})
		return
	}
	if !json.Valid(input) {
		result = tool.Errorf("工具参数不是合法 JSON：%s", clip(string(input), 200))
		// 事件需要被序列化推送给 UI，非法 JSON 以字符串形式携带
		input, _ = json.Marshal(string(input))
		return
	}

	// PreToolUse 钩子：可阻止调用或改写参数
	pre, herr := a.Hooks.Run(ctx, &hook.Input{Event: hook.PreToolUse, SessionID: sess.ID, WorkDir: a.WorkDir, ToolName: call.Name, ToolInput: input, AgentName: a.Name})
	if herr != nil {
		emit(Event{Type: EventLog, Turn: turn, Text: "PreToolUse 钩子出错：" + herr.Error()})
	}
	if len(pre.UpdatedInput) > 0 {
		input = pre.UpdatedInput
	}
	emit(Event{Type: EventToolCall, Turn: turn, Tool: &ToolEvent{ID: call.ID, Name: call.Name, Input: input}})
	if pre.Block {
		result = tool.Errorf("调用被 PreToolUse 钩子阻止：%s", pre.Reason)
		return
	}

	// 权限检查
	if a.Permissions != nil {
		info := t.Info()
		if err := a.Permissions.Check(ctx, permission.Request{Tool: call.Name, Input: input, ReadOnly: info.ReadOnly, CallID: call.ID}); err != nil {
			result = tool.Errorf("%v", err)
			span.AddEvent("permission_denied", map[string]any{"reason": err.Error()})
			return
		}
	}

	tctx, cancel := context.WithTimeout(ctx, a.ToolTimeout)
	defer cancel()
	r, err := t.Run(tctx, &tool.Call{ID: call.ID, Name: call.Name, Input: input, Env: env})
	switch {
	case err != nil:
		result = tool.Errorf("工具执行出错：%v", err)
	case r == nil:
		result = tool.Text("(无输出)")
	default:
		result = r
	}
	if errors.Is(tctx.Err(), context.DeadlineExceeded) {
		result.Content += fmt.Sprintf("\n[工具执行超时（%s）]", a.ToolTimeout)
		result.IsError = true
	}

	post, _ := a.Hooks.Run(ctx, &hook.Input{Event: hook.PostToolUse, SessionID: sess.ID, WorkDir: a.WorkDir, ToolName: call.Name, ToolInput: input, ToolOutput: result.Content, ToolError: result.IsError, AgentName: a.Name})
	if post.AdditionalContext != "" {
		result.Content += "\n\n<hook-feedback>\n" + post.AdditionalContext + "\n</hook-feedback>"
	}
	if post.Block {
		result.Content += "\n\n<hook-feedback>\n" + post.Reason + "\n</hook-feedback>"
		result.IsError = true
	}
	return
}

// sanitizeToolInputs 把消息中不是合法 JSON 的工具参数替换为 {}。
func sanitizeToolInputs(m *llm.Message) {
	for i := range m.Content {
		b := &m.Content[i]
		if b.Type == llm.BlockToolUse && (len(b.Input) == 0 || !json.Valid(b.Input)) {
			b.Input = json.RawMessage("{}")
		}
	}
}

// stripUnsignedThinking 去掉没有签名的思考块：它们只对当次输出有意义，
// 回放给 Anthropic 官方 API 时还会因缺少签名而报错。
func stripUnsignedThinking(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role != llm.RoleAssistant {
			out = append(out, m)
			continue
		}
		nm := llm.Message{Role: m.Role}
		for _, b := range m.Content {
			if b.Type == llm.BlockThinking && b.Signature == "" {
				continue
			}
			nm.Content = append(nm.Content, b)
		}
		if len(nm.Content) == 0 {
			nm.Content = []llm.ContentBlock{{Type: llm.BlockText, Text: "(思考中)"}}
		}
		out = append(out, nm)
	}
	return out
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

func ptr[T any](v T) *T { return &v }
