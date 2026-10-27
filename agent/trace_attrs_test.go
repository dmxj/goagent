package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dmxj/goagent/llm"
	"github.com/dmxj/goagent/permission"
	"github.com/dmxj/goagent/session"
	"github.com/dmxj/goagent/tool"
	"github.com/dmxj/goagent/trace"
)

func spansByKind(mem *trace.Memory, kind trace.Kind) []trace.SpanData {
	var out []trace.SpanData
	for _, s := range mem.Spans("") {
		if s.Kind == kind {
			out = append(out, s)
		}
	}
	return out
}

// TestLLMSpanRawAttributes：llm.call Span 记录全部请求参数、原生请求体、原生响应、状态、用量与重试。
func TestLLMSpanRawAttributes(t *testing.T) {
	n := 0
	inner := &llm.MockProvider{Handler: func(req *llm.Request) (llm.Message, error) {
		n++
		if n == 1 {
			return llm.Message{}, &llm.APIError{StatusCode: 503, Body: "busy"}
		}
		if len(req.Messages) == 1 {
			return toolMsg(llm.ToolCall("c1", "echo", echoArgs{Text: strings.Repeat("x", 100)})), nil
		}
		return llm.AssistantText("完成"), nil
	}}
	mem := trace.NewMemory(0)
	temp, topP := 0.2, 0.8
	a := newAgent(t, &llm.RetryProvider{Inner: inner, MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}, echoTool(nil))
	a.Tracer = trace.New(mem)
	a.Temperature, a.TopP, a.TopK, a.ThinkingBudget = &temp, &topP, 7, 1024
	a.StopSequences = []string{"<END>"}
	a.ExtraHeaders = map[string]string{"x-trace": "1", "x-api-key": "sk-should-not-appear"}
	a.ExtraBody = map[string]any{"enable_thinking": true}
	a.System = func() string { return "系统提示词" }
	a.MaxToolOutput = 50
	rec := &recorder{}
	if _, err := a.Run(context.Background(), session.New(), "开始", rec.sink); err != nil {
		t.Fatal(err)
	}
	llmSpans := spansByKind(mem, trace.KindLLM)
	if len(llmSpans) != 2 {
		t.Fatalf("应有 2 个 llm.call Span，得到 %d", len(llmSpans))
	}
	first := llmSpans[0]
	var params map[string]any
	if err := json.Unmarshal(first.Attrs[AttrLLMParams].(json.RawMessage), &params); err != nil {
		t.Fatalf("llm.request.params 应是 JSON 对象：%v", err)
	}
	for k, want := range map[string]any{"model": "mock-model", "temperature": 0.2, "top_p": 0.8, "top_k": float64(7), "thinking_budget": float64(1024), "stream": true, "system": "系统提示词", "message_count": float64(1)} {
		if params[k] != want {
			t.Errorf("params[%s] = %v，期望 %v", k, params[k], want)
		}
	}
	if tools, _ := params["tools"].([]any); len(tools) != 1 {
		t.Errorf("params.tools 应包含工具定义：%v", params["tools"])
	}
	if h := params["extra_headers"].(map[string]any); h["x-api-key"] != "[REDACTED]" || h["x-trace"] != "1" {
		t.Errorf("extra_headers 应脱敏：%v", h)
	}
	body, _ := first.Attrs[AttrLLMRequestBody].(string)
	if !strings.Contains(body, `"开始"`) || !strings.Contains(body, "END") || !strings.Contains(body, "enable_thinking") {
		t.Errorf("llm.request.body 应是完整请求：%s", body)
	}
	if first.Attrs[AttrLLMStatus] != 200 || first.Attrs[AttrLLMRetries] != 1 || first.Attrs[AttrLLMResponseBody] == nil {
		t.Errorf("状态 / 重试 / 响应体缺失：%v", first.Attrs)
	}
	var attempts []map[string]any
	_ = json.Unmarshal(first.Attrs[AttrLLMAttempts].(json.RawMessage), &attempts)
	if len(attempts) != 2 || attempts[0]["status"] != float64(500) || attempts[1]["status"] != float64(200) {
		t.Errorf("llm.attempts 应记录每次尝试：%v", attempts)
	}
	if first.Attrs[AttrLLMUsage] == nil || first.Attrs[AttrLLMResponseMessage] == nil || first.Attrs[AttrLLMLatency] == nil {
		t.Errorf("usage / 组装后的消息 / 延迟缺失：%v", first.Attrs)
	}
	raw, _ := json.Marshal(first)
	if strings.Contains(string(raw), "sk-should-not-appear") {
		t.Errorf("Trace 中不应出现密钥")
	}

	toolSpan := spansByKind(mem, trace.KindTool)[0]
	if toolSpan.Attrs[AttrToolOutput] != "echo:"+strings.Repeat("x", 100) || toolSpan.Attrs[AttrToolModelTruncated] != true {
		t.Errorf("工具 Span 应记录完整输出并标记回填时被截断：%v", toolSpan.Attrs)
	}
	if !strings.Contains(toolSpan.Attrs[AttrToolInput].(string), strings.Repeat("x", 100)) {
		t.Errorf("工具 Span 应记录完整输入")
	}
	// 实时事件中的 Span 是轻量快照
	for _, ev := range rec.ofType(EventSpanEnd) {
		if b, _ := json.Marshal(ev.Span); len(b) > 64<<10 {
			t.Errorf("span_end 事件过大：%d 字节", len(b))
		}
	}
}

func TestLightSpanInEvents(t *testing.T) {
	big := strings.Repeat("长文本", 20000)
	mock := llm.NewMock(llm.AssistantText("ok"))
	mem := trace.NewMemory(0)
	a := newAgent(t, mock)
	a.Tracer = trace.New(mem)
	rec := &recorder{}
	if _, err := a.Run(context.Background(), session.New(), big, rec.sink); err != nil {
		t.Fatal(err)
	}
	var sawOmitted bool
	for _, ev := range rec.ofType(EventSpanEnd) {
		if ev.Span.Kind == trace.KindLLM {
			om, _ := ev.Span.Attrs[trace.AttrOmitted].([]string)
			sawOmitted = len(om) > 0
		}
	}
	if !sawOmitted {
		t.Errorf("大属性在事件中应被省略并列在 trace.omitted 中")
	}
	full := spansByKind(mem, trace.KindLLM)[0]
	if !strings.Contains(full.Attrs[AttrLLMRequestBody].(string), big) {
		t.Errorf("导出器中的 Span 应保留完整请求体")
	}
}

func TestRunRepairsDanglingToolUse(t *testing.T) {
	mock := llm.NewMock(llm.AssistantText("好的，继续"))
	a := newAgent(t, mock, echoTool(nil))
	sess := session.New()
	// 模拟进程在执行工具时被 kill -9：历史以悬空的 tool_use 结尾
	sess.Append(llm.UserText("跑测试"), toolMsg(llm.ToolCall("dangling", "echo", echoArgs{Text: "x"})))
	rec := &recorder{}
	if _, err := a.Run(context.Background(), sess, "还在吗", rec.sink); err != nil {
		t.Fatal(err)
	}
	req := mock.Requests[0]
	if len(req.Messages) != 4 {
		t.Fatalf("请求应有 4 条消息（补上的结果 + 新输入），得到 %d", len(req.Messages))
	}
	c := req.Messages[2].Content
	if c[0].Type != llm.BlockToolResult || c[0].ToolUseID != "dangling" || !c[0].IsError || req.Messages[3].Text() != "还在吗" {
		t.Errorf("悬空调用应补上被中断的结果，且结果在新输入之前：%+v", req.Messages)
	}
	var logged bool
	for _, ev := range rec.ofType(EventLog) {
		logged = logged || strings.Contains(ev.Text, "未完成的工具调用")
	}
	if !logged {
		t.Errorf("修复时应发出日志事件")
	}
}

// TestRememberIsPerSession：同一个 Agent 服务多个会话时，"总是允许"只对当前会话生效。
func TestRememberIsPerSession(t *testing.T) {
	asks := map[string]int{}
	perm, _ := permission.NewChecker(permission.Config{}, permission.ApproverFunc(func(ctx context.Context, req permission.Request, _ string) (bool, bool, error) {
		asks[req.SessionID]++
		return true, true, nil
	}))
	mock := &llm.MockProvider{Handler: func(req *llm.Request) (llm.Message, error) {
		if req.Messages[len(req.Messages)-1].Role == llm.RoleUser && req.Messages[len(req.Messages)-1].Content[0].Type == llm.BlockText {
			return toolMsg(llm.ToolCall("", "write", echoArgs{Text: "a"})), nil
		}
		return llm.AssistantText("ok"), nil
	}}
	a := newAgent(t, mock, writeTool(nil))
	a.Permissions = perm
	s1, s2 := session.New(), session.New()
	s2.ID = s1.ID + "-other"
	for _, s := range []*session.Session{s1, s1, s2} {
		if _, err := a.Run(context.Background(), s, "写", nil); err != nil {
			t.Fatal(err)
		}
	}
	if asks[s1.ID] != 1 || asks[s2.ID] != 1 {
		t.Errorf("每个会话应各询问一次：%v", asks)
	}
}

func TestSharedStatesAcrossVersions(t *testing.T) {
	states := NewStates()
	block := make(chan struct{})
	started := make(chan struct{})
	slow := &blockingTool{started: started, release: block}
	mock := llm.NewMock(toolMsg(llm.ToolCall("1", "block", map[string]string{})), llm.AssistantText("done"))
	v1 := newAgent(t, mock, slow)
	v1.States = states
	v2 := newAgent(t, llm.NewMock(llm.AssistantText("v2")))
	v2.States = states
	sess := session.New()
	done := make(chan error, 1)
	go func() {
		_, err := v1.Run(context.Background(), sess, "x", nil)
		done <- err
	}()
	<-started
	if !v2.Running(sess.ID) {
		t.Errorf("新版本 Agent 应看到旧版本正在运行的会话")
	}
	if _, err := v2.Run(context.Background(), sess, "y", nil); err != ErrBusy {
		t.Errorf("同一会话不能在两个版本上同时运行：%v", err)
	}
	if !v2.Steer(sess.ID, "补充") {
		t.Errorf("新版本 Agent 应能向旧版本的运行插话")
	}
	close(block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	states.Forget(sess.ID)
}

type blockingTool struct{ started, release chan struct{} }

func (b *blockingTool) Info() tool.Info {
	return tool.Info{Name: "block", Description: "阻塞", Schema: json.RawMessage(`{"type":"object"}`), ReadOnly: true}
}

func (b *blockingTool) Run(ctx context.Context, c *tool.Call) (*tool.Result, error) {
	close(b.started)
	<-b.release
	return tool.Text("ok"), nil
}

func TestNoStreamAndParamsReachProvider(t *testing.T) {
	mock := llm.NewMock(llm.AssistantText("ok"))
	a := newAgent(t, mock)
	topP := 0.5
	a.NoStream, a.TopP, a.ExtraBody, a.Version = true, &topP, map[string]any{"k": 1}, "v7"
	rec := &recorder{}
	if _, err := a.Run(context.Background(), session.New(), "x", rec.sink); err != nil {
		t.Fatal(err)
	}
	r := mock.Requests[0]
	if !r.NoStream || *r.TopP != 0.5 || r.ExtraBody["k"] != 1 {
		t.Errorf("参数应传给 Provider：%+v", r)
	}
	if ev := rec.ofType(EventRunStart); ev[0].Data["config_version"] != "v7" {
		t.Errorf("run_start 应带上配置版本：%v", ev[0].Data)
	}
}

func TestAbandonUnresponsiveTool(t *testing.T) {
	old := abandonGrace
	abandonGrace = 50 * time.Millisecond
	defer func() { abandonGrace = old }()
	release := make(chan struct{})
	defer close(release)
	stubborn := tool.Func("stubborn", "不理会 ctx", true, func(ctx context.Context, in struct{}, c *tool.Call) (*tool.Result, error) {
		<-release
		return tool.Text("太晚了"), nil
	})
	mock := llm.NewMock(toolMsg(llm.ToolCall("s", "stubborn", struct{}{})), llm.AssistantText("ok"))
	a := newAgent(t, mock, stubborn)
	a.ToolTimeout = 30 * time.Millisecond
	start := time.Now()
	if _, err := a.Run(context.Background(), session.New(), "x", nil); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("不尊重 ctx 的工具不应卡住 Agent：耗时 %v", time.Since(start))
	}
	r := lastToolResults(t, mock.Requests[1])
	if !r[0].IsError || !strings.Contains(r[0].Content, "放弃等待") {
		t.Errorf("应回填放弃等待的错误：%+v", r[0])
	}
}
