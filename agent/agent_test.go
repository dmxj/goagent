package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmxj/goagent/hook"
	"github.com/dmxj/goagent/llm"
	"github.com/dmxj/goagent/memory"
	"github.com/dmxj/goagent/permission"
	"github.com/dmxj/goagent/session"
	"github.com/dmxj/goagent/tool"
	"github.com/dmxj/goagent/trace"
)

// recorder 收集 Agent 发出的事件（并发安全）。
type recorder struct {
	mu  sync.Mutex
	evs []Event
}

func (r *recorder) sink(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, ev)
}

func (r *recorder) events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.evs...)
}

func (r *recorder) ofType(typ EventType) []Event {
	var out []Event
	for _, e := range r.events() {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

type echoArgs struct {
	Text string `json:"text"`
}

// echoTool 是一个只读工具，返回 "echo:<text>"，并统计调用次数。
func echoTool(calls *atomic.Int32) tool.Tool {
	return tool.Func("echo", "回显", true, func(ctx context.Context, in echoArgs, c *tool.Call) (*tool.Result, error) {
		if calls != nil {
			calls.Add(1)
		}
		return tool.Text("echo:" + in.Text), nil
	})
}

// writeTool 是一个有副作用的工具（需要权限）。
func writeTool(calls *atomic.Int32) tool.Tool {
	return tool.Func("write", "写入", false, func(ctx context.Context, in echoArgs, c *tool.Call) (*tool.Result, error) {
		if calls != nil {
			calls.Add(1)
		}
		return tool.Text("wrote:" + in.Text), nil
	})
}

func newAgent(t *testing.T, p llm.Provider, tools ...tool.Tool) *Agent {
	t.Helper()
	return &Agent{Provider: p, Model: "mock-model", Tools: tool.NewRegistry(tools...), WorkDir: t.TempDir()}
}

func toolMsg(blocks ...llm.ContentBlock) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: blocks}
}

// lastToolResults 返回请求最后一条消息中的工具结果。
func lastToolResults(t *testing.T, req *llm.Request) []llm.ContentBlock {
	t.Helper()
	last := req.Messages[len(req.Messages)-1]
	if last.Role != llm.RoleUser {
		t.Fatalf("最后一条消息应为 user（工具结果），实际 %s", last.Role)
	}
	return last.Content
}

func TestRunSingleTurnText(t *testing.T) {
	mock := llm.NewMock(llm.AssistantText("你好，我是 GoAgent"))
	a := newAgent(t, mock)
	a.System = func() string { return "系统提示" }
	a.Store = session.NewMemoryStore()
	sess := session.New()
	rec := &recorder{}
	res, err := a.Run(context.Background(), sess, "你好", rec.sink)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "你好，我是 GoAgent" || res.Turns != 1 || res.StopReason != llm.StopEndTurn || res.TraceID == "" {
		t.Errorf("结果错误：%+v", res)
	}
	if res.Usage.OutputTokens == 0 {
		t.Errorf("应累计用量")
	}
	h := sess.History()
	if len(h) != 2 || h[0].Text() != "你好" || h[1].Role != llm.RoleAssistant {
		t.Errorf("会话历史错误：%+v", h)
	}
	if req := mock.Requests[0]; req.System != "系统提示" || req.Model != "mock-model" || req.MaxTokens != 8192 {
		t.Errorf("请求参数错误：%+v", req)
	}
	if _, err := a.Store.Load(sess.ID); err != nil {
		t.Errorf("运行结束后应保存会话：%v", err)
	}
	var text strings.Builder
	for _, e := range rec.ofType(EventText) {
		text.WriteString(e.Text)
	}
	if text.String() != res.Text {
		t.Errorf("text_delta 拼接 = %q，期望 %q", text.String(), res.Text)
	}
	for _, e := range rec.events() {
		if e.Agent != "main" || e.SessionID != sess.ID || e.Time.IsZero() {
			t.Errorf("事件缺少 Agent/SessionID/Time：%+v", e)
		}
	}
}

func TestEventSequence(t *testing.T) {
	var echoCalls atomic.Int32
	mock := llm.NewMock(toolMsg(llm.ToolCall("c1", "echo", echoArgs{"x"})), llm.AssistantText("完成"))
	a := newAgent(t, mock, echoTool(&echoCalls))
	rec := &recorder{}
	if _, err := a.Run(context.Background(), session.New(), "开始", rec.sink); err != nil {
		t.Fatal(err)
	}
	var seq []EventType
	for _, e := range rec.events() {
		switch e.Type {
		case EventSpanStart, EventSpanEnd, EventText, EventToolInputDelta, EventUsage:
			continue // 过滤高频与 trace 事件，只看主干
		}
		seq = append(seq, e.Type)
	}
	want := []EventType{EventRunStart, EventTurnStart, EventToolStart, EventMessage, EventToolCall, EventToolResult, EventTurnStart, EventMessage, EventRunEnd}
	if fmt.Sprint(seq) != fmt.Sprint(want) {
		t.Errorf("事件序列 = %v\n期望 %v", seq, want)
	}
	evs := rec.events()
	if evs[0].Type != EventSpanStart || evs[len(evs)-1].Type != EventRunEnd {
		t.Errorf("第一个事件应为 agent.run 的 span_start，最后一个应为 run_end")
	}
	end := evs[len(evs)-1]
	if end.Data["turns"] != 2 || end.Text != "完成" || end.Usage == nil {
		t.Errorf("run_end 内容错误：%+v", end)
	}
	// 所有事件都能被序列化（SSE 推送依赖这一点）
	for _, e := range evs {
		if _, err := json.Marshal(e); err != nil {
			t.Errorf("事件 %s 无法序列化：%v", e.Type, err)
		}
	}
}

func TestToolLoop(t *testing.T) {
	var echoCalls atomic.Int32
	mock := llm.NewMock(
		toolMsg(llm.ContentBlock{Type: llm.BlockText, Text: "我先调用工具"}, llm.ToolCall("call_1", "echo", echoArgs{"hello"})),
		llm.AssistantText("工具返回了 echo:hello"),
	)
	a := newAgent(t, mock, echoTool(&echoCalls))
	rec := &recorder{}
	sess := session.New()
	res, err := a.Run(context.Background(), sess, "调用 echo", rec.sink)
	if err != nil {
		t.Fatal(err)
	}
	if res.Turns != 2 || echoCalls.Load() != 1 || mock.Calls() != 2 {
		t.Fatalf("应执行 2 轮、调用工具 1 次：turns=%d calls=%d", res.Turns, echoCalls.Load())
	}
	results := lastToolResults(t, mock.Requests[1])
	if len(results) != 1 || results[0].Type != llm.BlockToolResult || results[0].ToolUseID != "call_1" || results[0].Content != "echo:hello" || results[0].IsError {
		t.Errorf("工具结果回填错误：%+v", results)
	}
	if len(mock.Requests[0].Tools) != 1 || mock.Requests[0].Tools[0].Name != "echo" {
		t.Errorf("请求应携带工具定义")
	}
	tr := rec.ofType(EventToolResult)
	if len(tr) != 1 || tr[0].Tool.Output != "echo:hello" || tr[0].Tool.Name != "echo" || string(tr[0].Tool.Input) != `{"text":"hello"}` {
		t.Errorf("tool_result 事件错误：%+v", tr)
	}
	if len(sess.History()) != 4 {
		t.Errorf("历史应为 user→assistant(tool_use)→user(tool_result)→assistant，实际 %d 条", len(sess.History()))
	}
	if len(a.Files(sess.ID)) != 0 || a.Values(sess.ID) == nil {
		t.Errorf("会话状态访问器错误")
	}
}

func TestUnknownTool(t *testing.T) {
	mock := llm.NewMock(toolMsg(llm.ToolCall("c1", "no_such_tool", map[string]any{})), llm.AssistantText("好的"))
	a := newAgent(t, mock, echoTool(nil))
	rec := &recorder{}
	if _, err := a.Run(context.Background(), session.New(), "x", rec.sink); err != nil {
		t.Fatal(err)
	}
	r := lastToolResults(t, mock.Requests[1])[0]
	if !r.IsError || !strings.Contains(r.Content, `"no_such_tool" 不存在`) || !strings.Contains(r.Content, "echo") {
		t.Errorf("未知工具应回填错误并列出可用工具：%+v", r)
	}
	if len(rec.ofType(EventToolCall)) != 1 {
		t.Errorf("未知工具也应发出 tool_call 事件")
	}
}

func TestInvalidToolInputJSON(t *testing.T) {
	// 模型输出被截断时，工具参数可能是不完整的 JSON
	bad := llm.ContentBlock{Type: llm.BlockToolUse, ID: "c1", Name: "echo", Input: json.RawMessage(`{"text":"unterminated`)}
	mock := llm.NewMock(toolMsg(bad), llm.AssistantText("重试"))
	var calls atomic.Int32
	a := newAgent(t, mock, echoTool(&calls))
	a.Store = session.NewMemoryStore()
	rec := &recorder{}
	sess := session.New()
	if _, err := a.Run(context.Background(), sess, "x", rec.sink); err != nil {
		t.Fatalf("非法参数不应中断运行：%v", err)
	}
	if calls.Load() != 0 {
		t.Errorf("参数非法时不应执行工具")
	}
	r := lastToolResults(t, mock.Requests[1])[0]
	if !r.IsError || !strings.Contains(r.Content, "不是合法 JSON") {
		t.Errorf("应回填参数错误：%+v", r)
	}
	if _, err := json.Marshal(mock.Requests[1]); err != nil {
		t.Errorf("下一次请求必须能序列化（历史中的非法 JSON 应被清理）：%v", err)
	}
	if _, err := json.Marshal(sess); err != nil {
		t.Errorf("会话必须能序列化保存：%v", err)
	}
	for _, e := range rec.events() {
		if _, err := json.Marshal(e); err != nil {
			t.Errorf("事件 %s 无法序列化：%v", e.Type, err)
		}
	}
}

// slowTool 是可并发的只读工具，记录同时运行的最大数量。
type slowTool struct {
	name    string
	delay   time.Duration
	running atomic.Int32
	peak    atomic.Int32
}

func (s *slowTool) Info() tool.Info {
	return tool.Info{Name: s.name, Schema: tool.SchemaOf(echoArgs{}), ReadOnly: true, Concurrent: true}
}

func (s *slowTool) Run(ctx context.Context, c *tool.Call) (*tool.Result, error) {
	n := s.running.Add(1)
	defer s.running.Add(-1)
	for {
		p := s.peak.Load()
		if n <= p || s.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(s.delay)
	var in echoArgs
	_ = json.Unmarshal(c.Input, &in)
	return tool.Text("slow:" + in.Text), nil
}

func TestParallelReadOnlyTools(t *testing.T) {
	tests := []struct {
		name        string
		maxParallel int
		calls       int
		wantPeak    int32
	}{
		{"全部并行", 0, 4, 4},
		{"受 MaxParallelTools 限制", 2, 4, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &slowTool{name: "slow", delay: 150 * time.Millisecond}
			var blocks []llm.ContentBlock
			for i := 0; i < tt.calls; i++ {
				blocks = append(blocks, llm.ToolCall(fmt.Sprintf("c%d", i), "slow", echoArgs{fmt.Sprint(i)}))
			}
			mock := llm.NewMock(toolMsg(blocks...), llm.AssistantText("done"))
			a := newAgent(t, mock, st)
			a.MaxParallelTools = tt.maxParallel
			start := time.Now()
			if _, err := a.Run(context.Background(), session.New(), "x", nil); err != nil {
				t.Fatal(err)
			}
			elapsed := time.Since(start)
			if st.peak.Load() != tt.wantPeak {
				t.Errorf("最大并发 = %d，期望 %d", st.peak.Load(), tt.wantPeak)
			}
			serial := time.Duration(tt.calls) * st.delay
			if elapsed >= serial {
				t.Errorf("并行执行耗时 %v，不应达到串行耗时 %v", elapsed, serial)
			}
			results := lastToolResults(t, mock.Requests[1])
			for i, r := range results {
				if r.ToolUseID != fmt.Sprintf("c%d", i) || r.Content != fmt.Sprintf("slow:%d", i) {
					t.Errorf("第 %d 个结果顺序错误：%+v", i, r)
				}
			}
		})
	}
}

func TestSideEffectToolsRunSerially(t *testing.T) {
	st := &slowTool{name: "slow", delay: 50 * time.Millisecond}
	var order []string
	var mu sync.Mutex
	serial := tool.Func("serial", "串行工具", false, func(ctx context.Context, in echoArgs, c *tool.Call) (*tool.Result, error) {
		mu.Lock()
		order = append(order, in.Text)
		running := st.running.Load()
		mu.Unlock()
		if running != 0 {
			t.Errorf("有副作用的工具执行时不应有其它工具在运行")
		}
		return tool.Text(in.Text), nil
	})
	mock := llm.NewMock(toolMsg(
		llm.ToolCall("a", "slow", echoArgs{"1"}), llm.ToolCall("b", "slow", echoArgs{"2"}),
		llm.ToolCall("c", "serial", echoArgs{"3"}),
		llm.ToolCall("d", "slow", echoArgs{"4"}),
	), llm.AssistantText("ok"))
	a := newAgent(t, mock, st, serial)
	a.Permissions, _ = permission.NewChecker(permission.Config{Mode: permission.ModeBypass}, nil)
	if _, err := a.Run(context.Background(), session.New(), "x", nil); err != nil {
		t.Fatal(err)
	}
	results := lastToolResults(t, mock.Requests[1])
	var ids []string
	for _, r := range results {
		ids = append(ids, r.ToolUseID)
	}
	if strings.Join(ids, "") != "abcd" {
		t.Errorf("结果顺序应与调用顺序一致：%v", ids)
	}
}

func TestPermissionDenied(t *testing.T) {
	var writes atomic.Int32
	mock := llm.NewMock(toolMsg(llm.ToolCall("c1", "write", echoArgs{"x"})), llm.AssistantText("被拒绝了"))
	a := newAgent(t, mock, writeTool(&writes))
	a.Permissions, _ = permission.NewChecker(permission.Config{}, permission.AutoDeny)
	if _, err := a.Run(context.Background(), session.New(), "写文件", nil); err != nil {
		t.Fatal(err)
	}
	if writes.Load() != 0 {
		t.Errorf("被拒绝的工具不应执行")
	}
	r := lastToolResults(t, mock.Requests[1])[0]
	if !r.IsError || !strings.Contains(r.Content, "被拒绝") {
		t.Errorf("应把拒绝原因回填给模型：%+v", r)
	}

	// 只读工具在 default 模式下无需审批
	mock2 := llm.NewMock(toolMsg(llm.ToolCall("c1", "echo", echoArgs{"x"})), llm.AssistantText("ok"))
	a2 := newAgent(t, mock2, echoTool(nil))
	a2.Permissions, _ = permission.NewChecker(permission.Config{}, permission.AutoDeny)
	_, _ = a2.Run(context.Background(), session.New(), "读", nil)
	if r := lastToolResults(t, mock2.Requests[1])[0]; r.IsError {
		t.Errorf("只读工具应直接放行：%+v", r)
	}
}

func TestHooks(t *testing.T) {
	t.Run("PreToolUse 阻止调用", func(t *testing.T) {
		var writes atomic.Int32
		mock := llm.NewMock(toolMsg(llm.ToolCall("c1", "write", echoArgs{"rm -rf /"})), llm.AssistantText("好"))
		a := newAgent(t, mock, writeTool(&writes))
		a.Hooks = hook.NewManager()
		_ = a.Hooks.On(hook.PreToolUse, "write", func(ctx context.Context, in *hook.Input) (*hook.Output, error) {
			return &hook.Output{Block: true, Reason: "危险操作"}, nil
		})
		_, _ = a.Run(context.Background(), session.New(), "x", nil)
		r := lastToolResults(t, mock.Requests[1])[0]
		if writes.Load() != 0 || !r.IsError || !strings.Contains(r.Content, "PreToolUse 钩子阻止：危险操作") {
			t.Errorf("钩子应阻止调用：writes=%d %+v", writes.Load(), r)
		}
	})
	t.Run("PreToolUse 改写参数与 PostToolUse 追加反馈", func(t *testing.T) {
		mock := llm.NewMock(toolMsg(llm.ToolCall("c1", "echo", echoArgs{"原始"})), llm.AssistantText("好"))
		a := newAgent(t, mock, echoTool(nil))
		a.Hooks = hook.NewManager()
		_ = a.Hooks.On(hook.PreToolUse, "", func(ctx context.Context, in *hook.Input) (*hook.Output, error) {
			return &hook.Output{UpdatedInput: json.RawMessage(`{"text":"改写"}`)}, nil
		})
		_ = a.Hooks.On(hook.PostToolUse, "echo", func(ctx context.Context, in *hook.Input) (*hook.Output, error) {
			if in.ToolOutput != "echo:改写" {
				t.Errorf("PostToolUse 应看到工具输出：%q", in.ToolOutput)
			}
			return &hook.Output{AdditionalContext: "已格式化", Block: true, Reason: "lint 失败"}, nil
		})
		_, _ = a.Run(context.Background(), session.New(), "x", nil)
		r := lastToolResults(t, mock.Requests[1])[0]
		if !strings.HasPrefix(r.Content, "echo:改写") || !strings.Contains(r.Content, "已格式化") || !strings.Contains(r.Content, "lint 失败") || !r.IsError {
			t.Errorf("钩子改写/反馈未生效：%+v", r)
		}
	})
	t.Run("UserPromptSubmit 拦截输入", func(t *testing.T) {
		mock := llm.NewMock(llm.AssistantText("不应调用"))
		a := newAgent(t, mock)
		a.Hooks = hook.NewManager()
		_ = a.Hooks.On(hook.UserPromptSubmit, "", func(ctx context.Context, in *hook.Input) (*hook.Output, error) {
			return &hook.Output{Block: true, Reason: "包含敏感词"}, nil
		})
		res, err := a.Run(context.Background(), session.New(), "密码是 123", nil)
		if err != nil || res.StopReason != "blocked" || !strings.Contains(res.Text, "包含敏感词") || mock.Calls() != 0 {
			t.Errorf("输入应被拦截：%+v %v", res, err)
		}
	})
	t.Run("SessionStart 与 UserPromptSubmit 补充上下文", func(t *testing.T) {
		mock := llm.NewMock(llm.AssistantText("1"), llm.AssistantText("2"))
		a := newAgent(t, mock)
		a.Hooks = hook.NewManager()
		_ = a.Hooks.On(hook.SessionStart, "", func(ctx context.Context, in *hook.Input) (*hook.Output, error) {
			return &hook.Output{AdditionalContext: "会话开始"}, nil
		})
		_ = a.Hooks.On(hook.UserPromptSubmit, "", func(ctx context.Context, in *hook.Input) (*hook.Output, error) {
			return &hook.Output{AdditionalContext: "当前分支 main"}, nil
		})
		sess := session.New()
		_, _ = a.Run(context.Background(), sess, "第一句", nil)
		first := mock.Requests[0].Messages[0].Content
		if len(first) != 2 || !strings.Contains(first[1].Text, "会话开始\n当前分支 main") {
			t.Errorf("第一轮应同时带上两个钩子的上下文：%+v", first)
		}
		_, _ = a.Run(context.Background(), sess, "第二句", nil)
		second := mock.Requests[1].Messages[2].Content
		if strings.Contains(second[1].Text, "会话开始") || !strings.Contains(second[1].Text, "当前分支 main") {
			t.Errorf("SessionStart 只应在会话第一条输入时触发：%+v", second)
		}
	})
	t.Run("Stop 钩子要求继续", func(t *testing.T) {
		mock := llm.NewMock(llm.AssistantText("我做完了"), llm.AssistantText("测试已修复"))
		a := newAgent(t, mock)
		a.Hooks = hook.NewManager()
		var stops atomic.Int32
		_ = a.Hooks.On(hook.Stop, "", func(ctx context.Context, in *hook.Input) (*hook.Output, error) {
			if stops.Add(1) == 1 {
				return &hook.Output{Block: true, Reason: "测试没有通过"}, nil
			}
			return nil, nil
		})
		res, err := a.Run(context.Background(), session.New(), "修 bug", nil)
		if err != nil || res.Text != "测试已修复" || mock.Calls() != 2 {
			t.Fatalf("Stop 钩子应让 Agent 再跑一轮：%+v calls=%d", res, mock.Calls())
		}
		msgs := mock.Requests[1].Messages
		if !strings.Contains(msgs[len(msgs)-1].Text(), "Stop 钩子要求继续：测试没有通过") {
			t.Errorf("应把继续原因告诉模型：%+v", msgs[len(msgs)-1])
		}
	})
	t.Run("Stop 钩子最多要求继续 3 次", func(t *testing.T) {
		mock := &llm.MockProvider{Handler: func(*llm.Request) (llm.Message, error) { return llm.AssistantText("完成"), nil }}
		a := newAgent(t, mock)
		a.Hooks = hook.NewManager()
		_ = a.Hooks.On(hook.Stop, "", func(ctx context.Context, in *hook.Input) (*hook.Output, error) {
			return &hook.Output{Block: true, Reason: "继续"}, nil
		})
		if _, err := a.Run(context.Background(), session.New(), "x", nil); err != nil {
			t.Fatal(err)
		}
		if mock.Calls() != 4 {
			t.Errorf("应为 1 次正常 + 3 次继续，实际调用 %d 次", mock.Calls())
		}
	})
}

func TestMaxTurns(t *testing.T) {
	mock := &llm.MockProvider{Handler: func(*llm.Request) (llm.Message, error) {
		return toolMsg(llm.ToolCall("", "echo", echoArgs{"again"})), nil
	}}
	a := newAgent(t, mock, echoTool(nil))
	a.MaxTurns = 3
	rec := &recorder{}
	res, err := a.Run(context.Background(), session.New(), "死循环", rec.sink)
	if !errors.Is(err, ErrMaxTurns) || res.StopReason != "max_turns" || mock.Calls() != 3 {
		t.Errorf("应在第 3 轮后停止：err=%v res=%+v calls=%d", err, res, mock.Calls())
	}
	if len(rec.ofType(EventError)) != 1 || len(rec.ofType(EventRunEnd)) != 1 {
		t.Errorf("出错时也应发出 error 与 run_end 事件")
	}
}

// blockingProvider 一直阻塞到 ctx 取消或 release 被关闭。
type blockingProvider struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingProvider) Name() string { return "blocking" }

func (b *blockingProvider) Stream(ctx context.Context, req *llm.Request, onEvent func(llm.StreamEvent)) (*llm.Response, error) {
	b.once.Do(func() { close(b.started) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.release:
		return &llm.Response{Message: llm.AssistantText("released"), StopReason: llm.StopEndTurn}, nil
	}
}

func TestContextCancel(t *testing.T) {
	t.Run("运行前已取消", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		mock := llm.NewMock(llm.AssistantText("x"))
		res, err := newAgent(t, mock).Run(ctx, session.New(), "x", nil)
		if !errors.Is(err, context.Canceled) || res.StopReason != "cancelled" || mock.Calls() != 0 {
			t.Errorf("应立即返回取消：%v %+v", err, res)
		}
	})
	t.Run("调用模型时取消", func(t *testing.T) {
		bp := &blockingProvider{started: make(chan struct{}), release: make(chan struct{})}
		a := newAgent(t, bp)
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-bp.started; cancel() }()
		res, err := a.Run(ctx, session.New(), "x", nil)
		if !errors.Is(err, context.Canceled) || res.StopReason != "cancelled" {
			t.Errorf("模型调用中取消应返回 cancelled：%v %+v", err, res)
		}
	})
}

func TestErrBusy(t *testing.T) {
	bp := &blockingProvider{started: make(chan struct{}), release: make(chan struct{})}
	a := newAgent(t, bp)
	sess := session.New()
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(context.Background(), sess, "第一个", nil)
		done <- err
	}()
	<-bp.started
	if !a.Running(sess.ID) {
		t.Errorf("运行中 Running 应为 true")
	}
	if _, err := a.Run(context.Background(), sess, "第二个", nil); !errors.Is(err, ErrBusy) {
		t.Errorf("同一会话并发运行应返回 ErrBusy，实际 %v", err)
	}
	// 其它会话不受影响
	other := newAgent(t, llm.NewMock(llm.AssistantText("ok")))
	if _, err := other.Run(context.Background(), session.New(), "x", nil); err != nil {
		t.Errorf("其它会话应能运行：%v", err)
	}
	close(bp.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if a.Running(sess.ID) {
		t.Errorf("结束后 Running 应为 false")
	}
}

func TestConcurrentSessionsShareAgent(t *testing.T) {
	// 同一个 Agent 被多个会话并发使用（HTTP 服务的典型场景），-race 下不应有数据竞争
	mock := &llm.MockProvider{Handler: llm.EchoHandler}
	a := newAgent(t, mock, echoTool(nil))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := a.Run(context.Background(), session.New(), fmt.Sprint("消息", i), nil)
			if err != nil || res.Text != fmt.Sprint("你说的是：消息", i) {
				t.Errorf("会话 %d 结果错误：%+v %v", i, res, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestSteer(t *testing.T) {
	var a *Agent
	sess := session.New()
	var steered bool
	mock := &llm.MockProvider{}
	mock.Handler = func(req *llm.Request) (llm.Message, error) {
		if len(req.Messages) == 1 {
			steered = a.Steer(sess.ID, "请改用中文注释")
			return toolMsg(llm.ToolCall("c1", "echo", echoArgs{"x"})), nil
		}
		return llm.AssistantText("好的"), nil
	}
	a = newAgent(t, mock, echoTool(nil))
	if a.Steer(sess.ID, "没在运行") {
		t.Errorf("未运行时 Steer 应返回 false")
	}
	rec := &recorder{}
	if _, err := a.Run(context.Background(), sess, "写代码", rec.sink); err != nil {
		t.Fatal(err)
	}
	if !steered {
		t.Fatalf("运行中 Steer 应返回 true")
	}
	msgs := mock.Requests[1].Messages
	if last := msgs[len(msgs)-1]; last.Role != llm.RoleUser || last.Text() != "（用户补充）请改用中文注释" {
		t.Errorf("补充消息应在下一次调用模型前插入：%+v", last)
	}
	if mock.Calls() != 2 {
		t.Errorf("调用次数错误")
	}
}

func TestAutoCompact(t *testing.T) {
	mock := &llm.MockProvider{Handler: func(req *llm.Request) (llm.Message, error) {
		if strings.Contains(req.System, "对话压缩助手") {
			return llm.AssistantText("摘要：用户在调试登录模块"), nil
		}
		return llm.AssistantText("继续工作"), nil
	}}
	a := newAgent(t, mock)
	a.Compactor = &memory.Compactor{Provider: mock, ContextWindow: 300, KeepRecent: 1}
	a.Hooks = hook.NewManager()
	var preCompact atomic.Int32
	_ = a.Hooks.On(hook.PreCompact, "", func(ctx context.Context, in *hook.Input) (*hook.Output, error) {
		preCompact.Add(1)
		return nil, nil
	})
	sess := session.New()
	long := strings.Repeat("很长的历史内容 ", 200)
	sess.Append(llm.UserText("第一轮 "+long), llm.AssistantText("回答一"), llm.UserText("第二轮 "+long), llm.AssistantText("回答二"))
	rec := &recorder{}
	res, err := a.Run(context.Background(), sess, "第三轮", rec.sink)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "继续工作" {
		t.Errorf("压缩后应正常回答：%q", res.Text)
	}
	compacts := rec.ofType(EventCompact)
	if len(compacts) != 1 || sess.Summary != "摘要：用户在调试登录模块" || sess.Compactions != 1 || preCompact.Load() != 1 {
		t.Fatalf("应触发一次自动压缩：events=%d summary=%q", len(compacts), sess.Summary)
	}
	if compacts[0].Data["tokens_before"].(int) <= compacts[0].Data["tokens_after"].(int) {
		t.Errorf("压缩后 token 应减少：%v", compacts[0].Data)
	}
	main := mock.Requests[len(mock.Requests)-1]
	if !strings.Contains(main.Messages[0].Text(), "摘要：用户在调试登录模块") || main.Messages[2].Text() != "第三轮" {
		t.Errorf("压缩后的请求应以摘要开头并保留最近输入：%+v", main.Messages)
	}

	// 手动压缩
	if err := newAgent(t, mock).Compact(context.Background(), sess, nil); err == nil {
		t.Errorf("未配置 Compactor 时应报错")
	}
	short := session.New()
	short.Append(llm.UserText("只有一句"))
	if err := a.Compact(context.Background(), short, nil); err == nil {
		t.Errorf("历史太短时手动压缩应报错")
	}
}

func TestToolPanicAndTimeout(t *testing.T) {
	panicky := tool.Func("panicky", "会 panic", true, func(ctx context.Context, in struct{}, c *tool.Call) (*tool.Result, error) {
		panic("出事了")
	})
	sleepy := tool.Func("sleepy", "会超时", true, func(ctx context.Context, in struct{}, c *tool.Call) (*tool.Result, error) {
		<-ctx.Done()
		return tool.Text("被取消"), nil
	})
	failing := tool.Func("failing", "返回 Go 错误", true, func(ctx context.Context, in struct{}, c *tool.Call) (*tool.Result, error) {
		return nil, errors.New("磁盘坏了")
	})
	empty := tool.Func("empty", "返回 nil", true, func(ctx context.Context, in struct{}, c *tool.Call) (*tool.Result, error) {
		return nil, nil
	})
	mock := llm.NewMock(toolMsg(
		llm.ToolCall("p", "panicky", struct{}{}), llm.ToolCall("s", "sleepy", struct{}{}),
		llm.ToolCall("f", "failing", struct{}{}), llm.ToolCall("e", "empty", struct{}{}),
	), llm.AssistantText("ok"))
	a := newAgent(t, mock, panicky, sleepy, failing, empty)
	a.ToolTimeout = 50 * time.Millisecond
	if _, err := a.Run(context.Background(), session.New(), "x", nil); err != nil {
		t.Fatal(err)
	}
	r := lastToolResults(t, mock.Requests[1])
	if !r[0].IsError || !strings.Contains(r[0].Content, "panic：出事了") {
		t.Errorf("工具 panic 应转为错误结果：%+v", r[0])
	}
	if !r[1].IsError || !strings.Contains(r[1].Content, "超时") {
		t.Errorf("工具超时应标记错误：%+v", r[1])
	}
	if !r[2].IsError || !strings.Contains(r[2].Content, "磁盘坏了") {
		t.Errorf("Go 错误应转为错误结果：%+v", r[2])
	}
	if r[3].IsError || r[3].Content != "(无输出)" {
		t.Errorf("nil 结果应转为 (无输出)：%+v", r[3])
	}
}

func TestToolOutputTruncated(t *testing.T) {
	big := tool.Func("big", "大输出", true, func(ctx context.Context, in struct{}, c *tool.Call) (*tool.Result, error) {
		return tool.Text(strings.Repeat("x", 10000)), nil
	})
	mock := llm.NewMock(toolMsg(llm.ToolCall("b", "big", struct{}{})), llm.AssistantText("ok"))
	a := newAgent(t, mock, big)
	a.MaxToolOutput = 500
	_, _ = a.Run(context.Background(), session.New(), "x", nil)
	if r := lastToolResults(t, mock.Requests[1])[0]; len(r.Content) > 700 || !strings.Contains(r.Content, "省略") {
		t.Errorf("工具输出应按 MaxToolOutput 截断，实际 %d 字节", len(r.Content))
	}
}

func TestModelErrorAndRetryEvents(t *testing.T) {
	t.Run("模型出错", func(t *testing.T) {
		mock := &llm.MockProvider{Handler: func(*llm.Request) (llm.Message, error) { return llm.Message{}, errors.New("服务不可用") }}
		rec := &recorder{}
		res, err := newAgent(t, mock).Run(context.Background(), session.New(), "x", rec.sink)
		if err == nil || res.StopReason != "error" {
			t.Fatalf("应返回模型错误：%v %+v", err, res)
		}
		if errs := rec.ofType(EventError); len(errs) != 1 || !strings.Contains(errs[0].Error, "服务不可用") {
			t.Errorf("应发出 error 事件：%+v", errs)
		}
	})
	t.Run("重试时发出 retry 事件", func(t *testing.T) {
		n := 0
		inner := &llm.MockProvider{Handler: func(*llm.Request) (llm.Message, error) {
			n++
			if n == 1 {
				return llm.Message{}, &llm.APIError{StatusCode: 429}
			}
			return llm.AssistantText("ok"), nil
		}}
		rp := &llm.RetryProvider{Inner: inner, MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
		rec := &recorder{}
		res, err := newAgent(t, rp).Run(context.Background(), session.New(), "x", rec.sink)
		if err != nil || res.Text != "ok" {
			t.Fatal(err)
		}
		if len(rec.ofType(EventRetry)) != 1 {
			t.Errorf("应发出 1 个 retry 事件")
		}
		if rp.OnRetry != nil {
			t.Errorf("不应修改共享的 RetryProvider")
		}
	})
}

func TestThinkingBlocks(t *testing.T) {
	thinking := toolMsg(
		llm.ContentBlock{Type: llm.BlockThinking, Thinking: "无签名思考"},
		llm.ContentBlock{Type: llm.BlockThinking, Thinking: "有签名思考", Signature: "sig"},
		llm.ContentBlock{Type: llm.BlockText, Text: "答"},
	)
	for _, keep := range []bool{false, true} {
		mock := llm.NewMock(llm.AssistantText("2"))
		a := newAgent(t, mock)
		a.KeepThinking = keep
		sess := session.New()
		sess.Append(llm.UserText("q"), thinking, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "只有思考"}}})
		rec := &recorder{}
		_, _ = a.Run(context.Background(), sess, "x", rec.sink)
		msgs := mock.Requests[0].Messages
		got := len(msgs[1].Content)
		if keep && got != 3 || !keep && got != 2 {
			t.Errorf("KeepThinking=%v 时内容块数 = %d", keep, got)
		}
		if !keep && msgs[2].Content[0].Text != "(思考中)" {
			t.Errorf("只含无签名思考的消息应保留占位文本：%+v", msgs[2])
		}
	}
}

func TestTraceSpans(t *testing.T) {
	mem := trace.NewMemory(0)
	mock := llm.NewMock(toolMsg(llm.ToolCall("c1", "echo", echoArgs{"x"})), llm.AssistantText("ok"))
	a := newAgent(t, mock, echoTool(nil))
	a.Tracer = trace.New(mem)
	sess := session.New()
	res, err := a.Run(context.Background(), sess, "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	spans := mem.Spans(res.TraceID)
	kinds := map[trace.Kind]int{}
	var root trace.SpanData
	for _, s := range spans {
		kinds[s.Kind]++
		if s.Kind == trace.KindAgent {
			root = s
		}
	}
	if kinds[trace.KindAgent] != 1 || kinds[trace.KindLLM] != 2 || kinds[trace.KindTool] != 1 {
		t.Errorf("Span 种类统计错误：%v", kinds)
	}
	if root.Attrs["session_id"] != sess.ID || root.Attrs["turns"] != 2 {
		t.Errorf("agent.run Span 属性错误：%v", root.Attrs)
	}
	for _, s := range spans {
		if s.Kind != trace.KindAgent && s.ParentID != root.SpanID {
			t.Errorf("Span %s 应挂在 agent.run 之下", s.Name)
		}
	}
}

func TestAgentPathAndSink(t *testing.T) {
	ctx := withPath(context.Background(), "main")
	ctx = withPath(ctx, "explore#1")
	if AgentPath(ctx) != "main>explore#1" {
		t.Errorf("AgentPath = %q", AgentPath(ctx))
	}
	SinkFrom(context.Background())(Event{}) // 空实现不 panic
	var got EventType
	ctx = WithSink(ctx, func(ev Event) { got = ev.Type })
	SinkFrom(ctx)(Event{Type: EventLog})
	if got != EventLog {
		t.Errorf("WithSink/SinkFrom 未生效")
	}
	a := &Agent{Prompt: PromptFunc(func(a *Agent) string { return "来自 Prompt " + a.Name }), Name: "x"}
	if a.SystemPrompt() != "来自 Prompt x" || (&Agent{}).SystemPrompt() != "" {
		t.Errorf("SystemPrompt 错误")
	}
	if clip("中文字符", 4) != "中…" || clip("abc", 5) != "abc" {
		t.Errorf("clip 应在字符边界截断：%q", clip("中文字符", 4))
	}
}
