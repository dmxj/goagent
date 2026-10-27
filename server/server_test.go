package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmxj/goagent/app"
	"github.com/dmxj/goagent/config"
	"github.com/dmxj/goagent/llm"
	"github.com/dmxj/goagent/permission"
)

// newTestServer 用 MockProvider 组装应用并启动 httptest 服务器。
func newTestServer(t *testing.T, provider llm.Provider, token string) (*httptest.Server, *Server) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("GOAGENT_NO_CLAUDE_SETTINGS", "1")
	for _, k := range []string{"GOAGENT_PROVIDER", "GOAGENT_MODEL", "GOAGENT_PERMISSION_MODE", "GOAGENT_MAX_TURNS"} {
		t.Setenv(k, "")
	}
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(work)
	if err != nil {
		t.Fatal(err)
	}
	approver := NewWebApprover()
	approver.Timeout = 10 * time.Second
	a, err := app.New(context.Background(), cfg, app.Options{Provider: provider, Approver: approver, SkipMCP: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	s := New(a, approver)
	s.Token = token
	s.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, s
}

type sseEvent struct {
	name string
	data map[string]any
}

// chat 发起 /v1/chat 请求，并对每个 SSE 事件回调 onEvent；返回全部事件。
func chat(t *testing.T, ts *httptest.Server, body map[string]any, onEvent func(sseEvent)) []sseEvent {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/v1/chat", "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("chat 状态码 %d：%s", resp.StatusCode, msg)
	}
	var events []sseEvent
	err = llm.ReadSSE(bufio.NewReader(resp.Body), func(e llm.SSEEvent) error {
		var data map[string]any
		if err := json.Unmarshal([]byte(e.Data), &data); err != nil {
			t.Errorf("事件 %s 的 data 不是合法 JSON：%s", e.Event, e.Data)
		}
		ev := sseEvent{e.Event, data}
		events = append(events, ev)
		if onEvent != nil {
			onEvent(ev)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func names(evs []sseEvent) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.name)
	}
	return out
}

func find(evs []sseEvent, name string) []sseEvent {
	var out []sseEvent
	for _, e := range evs {
		if e.name == name {
			out = append(out, e)
		}
	}
	return out
}

func getJSON(t *testing.T, url string, out any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func postJSON(t *testing.T, url string, body any, out any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func TestHealthAndInfo(t *testing.T) {
	ts, _ := newTestServer(t, llm.NewMock(), "")
	var health map[string]any
	if code := getJSON(t, ts.URL+"/health", &health); code != 200 || health["status"] != "ok" {
		t.Errorf("/health 错误：%d %v", code, health)
	}
	var info map[string]any
	if code := getJSON(t, ts.URL+"/v1/info", &info); code != 200 {
		t.Fatalf("/v1/info 状态码 %d", code)
	}
	if info["permission_mode"] != "default" || info["model"] == "" {
		t.Errorf("/v1/info 内容错误：%v", info)
	}
	tools, _ := info["tools"].([]any)
	if len(tools) < 10 {
		t.Errorf("/v1/info 应列出工具：%v", tools)
	}
	resp, _ := http.Get(ts.URL + "/")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "<html") {
		t.Errorf("根路径应提供内置 Web 界面：%d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodOptions, ts.URL+"/v1/chat", nil)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("CORS 预检错误：%d", resp.StatusCode)
	}
}

func TestToken(t *testing.T) {
	ts, _ := newTestServer(t, llm.NewMock(), "s3cret")
	if code := getJSON(t, ts.URL+"/v1/info", nil); code != http.StatusUnauthorized {
		t.Errorf("没有令牌应返回 401，实际 %d", code)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/info", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("正确令牌应通过，实际 %d", resp.StatusCode)
	}
	if code := getJSON(t, ts.URL+"/health", nil); code != 200 {
		t.Errorf("/health 不需要令牌")
	}
}

func TestChatSSE(t *testing.T) {
	mock := llm.NewMock(llm.AssistantText("你好，这是流式回复"), llm.AssistantText("第二轮"))
	ts, s := newTestServer(t, mock, "")
	evs := chat(t, ts, map[string]any{"message": "你好"}, nil)
	ns := names(evs)
	if ns[0] != "session" || ns[len(ns)-1] != "done" {
		t.Fatalf("事件应以 session 开始、done 结束：%v", ns)
	}
	sid, _ := evs[0].data["session_id"].(string)
	if sid == "" {
		t.Fatal("session 事件应携带 session_id")
	}
	var text strings.Builder
	for _, e := range find(evs, "text_delta") {
		text.WriteString(e.data["text"].(string))
	}
	if text.String() != "你好，这是流式回复" {
		t.Errorf("text_delta 拼接 = %q", text.String())
	}
	ends := find(evs, "run_end")
	if len(ends) != 1 || ends[0].data["text"] != "你好，这是流式回复" || ends[0].data["session_id"] != sid {
		t.Errorf("run_end 事件错误：%v", ends)
	}
	if len(find(evs, "run_start")) != 1 || len(find(evs, "span_end")) == 0 {
		t.Errorf("缺少 run_start/span 事件：%v", ns)
	}

	// 同一会话继续对话
	evs = chat(t, ts, map[string]any{"session_id": sid, "message": "继续"}, nil)
	if evs[0].data["session_id"] != sid {
		t.Errorf("应复用会话")
	}
	if n := len(mock.Requests[1].Messages); n != 3 {
		t.Errorf("第二轮请求应包含历史（3 条消息），实际 %d", n)
	}

	// 会话接口
	var list struct {
		Sessions []map[string]any `json:"sessions"`
	}
	getJSON(t, ts.URL+"/v1/sessions", &list)
	if len(list.Sessions) != 1 || list.Sessions[0]["id"] != sid {
		t.Errorf("/v1/sessions 错误：%v", list)
	}
	var detail map[string]any
	if code := getJSON(t, ts.URL+"/v1/sessions/"+sid, &detail); code != 200 || len(detail["messages"].([]any)) != 4 || detail["running"] != false {
		t.Errorf("/v1/sessions/{id} 错误：%d %v", code, detail)
	}
	if code := getJSON(t, ts.URL+"/v1/sessions/missing", nil); code != 404 {
		t.Errorf("不存在的会话应 404，实际 %d", code)
	}

	// trace 查询
	var tr struct {
		Spans []map[string]any `json:"spans"`
	}
	getJSON(t, ts.URL+"/v1/traces?session_id="+sid, &tr)
	traceIDs := map[string]bool{}
	for _, sp := range tr.Spans {
		traceIDs[sp["trace_id"].(string)] = true
	}
	if len(tr.Spans) == 0 || len(traceIDs) != 2 {
		t.Errorf("按 session_id 查询应返回两次运行的 Span：%d 个 Span，%d 个 trace", len(tr.Spans), len(traceIDs))
	}
	getJSON(t, ts.URL+"/v1/traces?session_id=other", &tr)
	if len(tr.Spans) != 0 {
		t.Errorf("其它会话不应返回 Span")
	}
	var oneTrace string
	for id := range traceIDs {
		oneTrace = id
	}
	resp, _ := http.Get(ts.URL + "/v1/traces/" + oneTrace + "/tree")
	tree, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(tree), "agent.run") {
		t.Errorf("trace 树应包含 agent.run：%s", tree)
	}

	// 其余会话接口
	var files, todos map[string]any
	getJSON(t, ts.URL+"/v1/sessions/"+sid+"/files", &files)
	getJSON(t, ts.URL+"/v1/sessions/"+sid+"/todos", &todos)
	if _, ok := files["files"]; !ok {
		t.Errorf("files 接口错误：%v", files)
	}
	var steer map[string]any
	postJSON(t, ts.URL+"/v1/sessions/"+sid+"/steer", map[string]string{"message": "x"}, &steer)
	if steer["accepted"] != false {
		t.Errorf("未运行时 steer 应不被接受")
	}
	var cancel map[string]any
	postJSON(t, ts.URL+"/v1/sessions/"+sid+"/cancel", nil, &cancel)
	if cancel["cancelled"] != false {
		t.Errorf("未运行时 cancel 应返回 false")
	}
	var created map[string]any
	if code := postJSON(t, ts.URL+"/v1/sessions", nil, &created); code != 201 || created["id"] == "" {
		t.Errorf("创建会话失败：%d %v", code, created)
	}
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/sessions/"+sid, nil)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("删除会话失败：%d", resp.StatusCode)
	}
	if _, err := s.App.Store.Load(sid); err == nil {
		t.Errorf("删除后会话不应存在")
	}
}

func TestChatBadRequest(t *testing.T) {
	ts, _ := newTestServer(t, llm.NewMock(), "")
	for _, body := range []string{`{}`, `{"message":"  "}`, `not json`} {
		resp, _ := http.Post(ts.URL+"/v1/chat", "application/json", strings.NewReader(body))
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("请求体 %s 应返回 400，实际 %d", body, resp.StatusCode)
		}
	}
	// 指定一个不存在的 session_id 时以该 ID 新建会话
	evs := chat(t, ts, map[string]any{"session_id": "custom-id", "message": "hi"}, nil)
	if evs[0].data["session_id"] != "custom-id" {
		t.Errorf("应使用客户端指定的会话 ID：%v", evs[0].data)
	}
	evs = chat(t, ts, map[string]any{"session_id": "bad.id", "message": "hi"}, nil)
	if evs[0].data["session_id"] == "bad.id" {
		t.Errorf("非法会话 ID 不应被采用")
	}
}

func TestPermissionFlow(t *testing.T) {
	for _, allow := range []bool{true, false} {
		name := "拒绝"
		if allow {
			name = "允许"
		}
		t.Run(name, func(t *testing.T) {
			mock := &llm.MockProvider{Handler: func(req *llm.Request) (llm.Message, error) {
				if len(req.Messages) == 1 {
					return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
						llm.ToolCall("w1", "write_file", map[string]string{"path": "out.txt", "content": "来自 Agent"}),
					}}, nil
				}
				last := req.Messages[len(req.Messages)-1].Content[0]
				return llm.AssistantText("工具结果：" + last.Content), nil
			}}
			ts, s := newTestServer(t, mock, "")
			var decided bool
			evs := chat(t, ts, map[string]any{"message": "写个文件"}, func(e sseEvent) {
				if e.name != "permission_request" {
					return
				}
				perm := e.data["permission"].(map[string]any)
				if perm["tool"] != "write_file" || perm["reason"] == "" {
					t.Errorf("审批请求内容错误：%v", perm)
				}
				var out map[string]any
				if code := postJSON(t, ts.URL+"/v1/permissions/"+perm["id"].(string), map[string]bool{"allow": allow}, &out); code != 200 {
					t.Errorf("提交审批失败：%d", code)
				}
				decided = true
			})
			if !decided {
				t.Fatalf("应收到 permission_request 事件：%v", names(evs))
			}
			results := find(evs, "tool_result")
			if len(results) != 1 {
				t.Fatalf("应有一个 tool_result：%v", names(evs))
			}
			tool := results[0].data["tool"].(map[string]any)
			p := filepath.Join(s.App.Config().WorkDir, "out.txt")
			_, statErr := os.Stat(p)
			if allow {
				if tool["is_error"] == true || statErr != nil {
					t.Errorf("允许后应写入文件：%v %v", tool, statErr)
				}
			} else if tool["is_error"] != true || statErr == nil {
				t.Errorf("拒绝后不应写入文件：%v", tool)
			}
		})
	}
	t.Run("不存在的审批 ID", func(t *testing.T) {
		ts, _ := newTestServer(t, llm.NewMock(), "")
		if code := postJSON(t, ts.URL+"/v1/permissions/perm_missing", map[string]bool{"allow": true}, nil); code != 404 {
			t.Errorf("应返回 404，实际 %d", code)
		}
	})
}

// gateProvider 第一次调用阻塞直到 ctx 取消，用于测试取消与并发冲突。
type gateProvider struct {
	started chan struct{}
	once    sync.Once
}

func (g *gateProvider) Name() string { return "gate" }

func (g *gateProvider) Stream(ctx context.Context, req *llm.Request, onEvent func(llm.StreamEvent)) (*llm.Response, error) {
	g.once.Do(func() { close(g.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCancelAndConflict(t *testing.T) {
	gp := &gateProvider{started: make(chan struct{})}
	ts, _ := newTestServer(t, gp, "")
	done := make(chan []sseEvent, 1)
	go func() {
		done <- chat(t, ts, map[string]any{"session_id": "sess-cancel", "message": "长任务"}, nil)
	}()
	select {
	case <-gp.started:
	case <-time.After(5 * time.Second):
		t.Fatal("运行未开始")
	}
	// 运行中再次发起同一会话的请求 → 409
	resp, err := http.Post(ts.URL+"/v1/chat", "application/json", strings.NewReader(`{"session_id":"sess-cancel","message":"又一个"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("会话运行中应返回 409，实际 %d", resp.StatusCode)
	}
	var detail map[string]any
	getJSON(t, ts.URL+"/v1/sessions/sess-cancel", &detail)
	if detail["running"] != true {
		t.Errorf("运行中 running 应为 true")
	}
	var steer map[string]any
	postJSON(t, ts.URL+"/v1/sessions/sess-cancel/steer", map[string]string{"message": "补充"}, &steer)
	if steer["accepted"] != true {
		t.Errorf("运行中 steer 应被接受")
	}
	var c map[string]any
	postJSON(t, ts.URL+"/v1/sessions/sess-cancel/cancel", nil, &c)
	if c["cancelled"] != true {
		t.Errorf("cancel 应返回 true")
	}
	select {
	case evs := <-done:
		ns := names(evs)
		if ns[len(ns)-1] != "done" {
			t.Errorf("取消后流应以 done 结束：%v", ns)
		}
		// 关键：通过 /cancel 取消时客户端仍在线，收尾事件必须送达
		if len(find(evs, "error")) != 1 || len(find(evs, "run_end")) != 1 {
			t.Errorf("取消后应收到 error 与 run_end 事件：%v", ns)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后 SSE 流未结束")
	}
}

func TestCompactEndpoint(t *testing.T) {
	ts, _ := newTestServer(t, llm.NewMock(llm.AssistantText("x")), "")
	evs := chat(t, ts, map[string]any{"message": "hi"}, nil)
	sid := evs[0].data["session_id"].(string)
	var out map[string]any
	if code := postJSON(t, ts.URL+"/v1/sessions/"+sid+"/compact", nil, &out); code != 400 {
		t.Errorf("历史太短时压缩应返回 400，实际 %d %v", code, out)
	}
	if code := postJSON(t, ts.URL+"/v1/sessions/missing/compact", nil, nil); code != 404 {
		t.Errorf("不存在的会话应返回 404，实际 %d", code)
	}
}

func TestWebApprover(t *testing.T) {
	w := NewWebApprover()
	w.Timeout = 50 * time.Millisecond
	if w.Decide("nope", true, false) {
		t.Errorf("不存在的 ID 应返回 false")
	}
	_, _, err := w.Approve(context.Background(), permissionRequest(), "需要确认")
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Errorf("应在超时后返回错误：%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := w.Approve(ctx, permissionRequest(), ""); !errors.Is(err, context.Canceled) {
		t.Errorf("ctx 取消应返回 Canceled：%v", err)
	}
}

func permissionRequest() permission.Request {
	return permission.Request{Tool: "bash", Input: json.RawMessage(`{"command":"ls"}`)}
}
