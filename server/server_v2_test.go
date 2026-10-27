package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmxj/goagent/agent"
	"github.com/dmxj/goagent/llm"
)

// TestWebApproverIdempotent：同一个 ID 提交多次不会阻塞任何 goroutine，第一次生效，其余返回 409。
func TestWebApproverIdempotent(t *testing.T) {
	w := NewWebApprover()
	w.Timeout = 5 * time.Second
	got := make(chan bool, 1)
	sink := make(chan string, 1)
	ctx := agentSinkCtx(func(id string) { sink <- id })
	go func() {
		allow, _, _ := w.Approve(ctx, permissionRequest(), "需要确认")
		got <- allow
	}()
	id := <-sink
	if p := w.Pending(""); len(p) != 1 || p[0].ID != id {
		t.Fatalf("Pending 应列出等待中的审批：%+v", p)
	}
	before := runtime.NumGoroutine()
	var wg sync.WaitGroup
	results := make([]DecideStatus, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = w.Resolve(id, true, false)
		}(i)
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("重复提交审批不应阻塞")
	}
	ok := 0
	for _, r := range results {
		switch r {
		case DecideOK:
			ok++
		case DecideConflict:
		default:
			t.Errorf("重复提交应返回 409 状态，得到 %v", r)
		}
	}
	if ok != 1 || !<-got {
		t.Errorf("只有一次提交生效：ok=%d", ok)
	}
	if w.Resolve(id, false, false) != DecideConflict || w.Resolve("perm_missing", true, false) != DecideNotFound {
		t.Errorf("已处理 → 409，不存在 → 404")
	}
	time.Sleep(50 * time.Millisecond)
	if n := runtime.NumGoroutine(); n > before+2 {
		t.Errorf("不应有泄漏的 goroutine：前 %d 后 %d", before, n)
	}
}

func TestPermissionEndpointStatuses(t *testing.T) {
	mock := &llm.MockProvider{Handler: func(req *llm.Request) (llm.Message, error) {
		if len(req.Messages) == 1 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ToolCall("w", "write_file", map[string]string{"path": "a.txt", "content": "x"})}}, nil
		}
		return llm.AssistantText("ok"), nil
	}}
	ts, _ := newTestServer(t, mock, "")
	var codes []int
	chat(t, ts, map[string]any{"session_id": "s-perm", "message": "写"}, func(e sseEvent) {
		if e.name != "permission_request" {
			return
		}
		var pend map[string]any
		getJSON(t, ts.URL+"/v1/permissions?session_id=s-perm", &pend)
		if list, _ := pend["permissions"].([]any); len(list) != 1 {
			t.Errorf("GET /v1/permissions 应列出等待中的审批：%v", pend)
		}
		id := e.data["permission"].(map[string]any)["id"].(string)
		for i := 0; i < 3; i++ {
			codes = append(codes, postJSON(t, ts.URL+"/v1/permissions/"+id, map[string]bool{"allow": true}, nil))
		}
	})
	if len(codes) != 3 || codes[0] != 200 || codes[1] != 409 || codes[2] != 409 {
		t.Errorf("重复提交的状态码应为 200, 409, 409：%v", codes)
	}
}

func TestSessionHistoryAPI(t *testing.T) {
	mock := &llm.MockProvider{Handler: func(req *llm.Request) (llm.Message, error) {
		if len(req.Messages) == 1 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Type: llm.BlockThinking, Thinking: "先看看目录", Signature: "sig"},
				{Type: llm.BlockText, Text: "我来看看"},
				llm.ToolCall("c1", "list_dir", map[string]string{"path": "."}),
			}}, nil
		}
		return llm.AssistantText("目录是空的"), nil
	}}
	ts, _ := newTestServer(t, mock, "")
	chat(t, ts, map[string]any{"session_id": "hist-1", "message": "第一条消息：列出目录\n第二行"}, nil)
	var list struct {
		Sessions []map[string]any `json:"sessions"`
	}
	getJSON(t, ts.URL+"/v1/sessions", &list)
	if len(list.Sessions) != 1 || list.Sessions[0]["title"] != "第一条消息：列出目录 第二行" || list.Sessions[0]["updated_at"] == nil || list.Sessions[0]["running"] != false {
		t.Errorf("会话列表应包含标题与更新时间：%v", list.Sessions)
	}
	var detail struct {
		Title    string        `json:"title"`
		Messages []llm.Message `json:"messages"`
	}
	getJSON(t, ts.URL+"/v1/sessions/hist-1", &detail)
	if len(detail.Messages) != 4 {
		t.Fatalf("应返回完整历史（user、assistant、tool_result、assistant），得到 %d", len(detail.Messages))
	}
	types := []string{}
	for _, m := range detail.Messages {
		for _, b := range m.Content {
			types = append(types, string(b.Type))
		}
	}
	if strings.Join(types, ",") != "text,thinking,text,tool_use,tool_result,text" {
		t.Errorf("应包含全部内容块：%v", types)
	}
	if detail.Messages[1].Content[0].Thinking != "先看看目录" || detail.Messages[2].Content[0].ToolUseID != "c1" {
		t.Errorf("内容块字段丢失：%+v", detail.Messages)
	}
}

func TestTraceTreeJSON(t *testing.T) {
	ts, _ := newTestServer(t, llm.NewMock(llm.AssistantText("你好")), "")
	evs := chat(t, ts, map[string]any{"session_id": "tr-1", "message": "hi"}, nil)
	traceID := find(evs, "run_start")[0].data["data"].(map[string]any)["trace_id"].(string)
	var tree struct {
		TraceID string           `json:"trace_id"`
		Roots   []map[string]any `json:"roots"`
	}
	if code := getJSON(t, ts.URL+"/v1/traces/"+traceID+"/tree", &tree); code != 200 || len(tree.Roots) != 1 {
		t.Fatalf("tree 应返回 JSON 树：%d %+v", code, tree)
	}
	root := tree.Roots[0]
	kids := root["children"].([]any)
	llmSpan := kids[0].(map[string]any)
	attrs := llmSpan["attrs"].(map[string]any)
	for _, k := range []string{"llm.request.params", "llm.request.body", "llm.request.headers", "llm.response.status", "llm.response.body", "llm.usage", "llm.latency_ms", "llm.retries", "llm.attempts", "llm.response.message"} {
		if _, ok := attrs[k]; !ok {
			t.Errorf("LLM Span 缺少属性 %s", k)
		}
	}
	if _, ok := attrs["llm.request.params"].(map[string]any); !ok {
		t.Errorf("llm.request.params 应是对象")
	}
	spanID := llmSpan["span_id"].(string)
	var one map[string]any
	if code := getJSON(t, ts.URL+"/v1/traces/"+traceID+"/spans/"+spanID, &one); code != 200 || one["span_id"] != spanID {
		t.Errorf("单个 Span 接口错误：%d", code)
	}
	resp, _ := http.Get(ts.URL + "/v1/traces/" + traceID + "/tree?format=text")
	sc := bufio.NewScanner(resp.Body)
	sc.Scan()
	resp.Body.Close()
	if !strings.Contains(sc.Text(), "agent.run") {
		t.Errorf("format=text 应返回文本树：%q", sc.Text())
	}
	if code := getJSON(t, ts.URL+"/v1/traces/missing/tree", nil); code != 404 {
		t.Errorf("不存在的 trace 应返回 404")
	}
	var light struct {
		Spans []map[string]any `json:"spans"`
	}
	getJSON(t, ts.URL+"/v1/traces?session_id=tr-1&light=1", &light)
	if len(light.Spans) < 2 {
		t.Errorf("按会话查询 trace 失败：%v", light)
	}
}

func TestConfigReloadEndpoints(t *testing.T) {
	ts, s := newTestServer(t, llm.NewMock(), "")
	cfgPath := filepath.Join(s.App.Config().ProjectDir, "config.json")
	_ = os.MkdirAll(filepath.Dir(cfgPath), 0o755)
	_ = os.WriteFile(cfgPath, []byte(`{"model":"reloaded-model","provider":{"type":"anthropic","api_key":"sk-ant-secret-should-be-hidden-123"}}`), 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 32)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	var out map[string]any
	if code := postJSON(t, ts.URL+"/v1/reload", nil, &out); code != 200 || out["version"] != float64(2) {
		t.Fatalf("POST /v1/reload 失败：%d %v", code, out)
	}
	var cfg map[string]any
	getJSON(t, ts.URL+"/v1/config", &cfg)
	raw, _ := json.Marshal(cfg)
	if cfg["model"] != "reloaded-model" || strings.Contains(string(raw), "secret-should-be-hidden") || cfg["effective"] == nil {
		t.Errorf("/v1/config 应返回脱敏后的生效配置：%s", raw)
	}
	deadline := time.After(3 * time.Second)
	for found := false; !found; {
		select {
		case l := <-lines:
			found = strings.Contains(l, "config_reloaded")
		case <-deadline:
			t.Fatal("/v1/events 应推送 config_reloaded")
		}
	}
	_ = os.WriteFile(cfgPath, []byte(`{"model": broken`), 0o644)
	if code := postJSON(t, ts.URL+"/v1/reload", nil, &out); code != 422 || out["error"] == nil {
		t.Errorf("非法配置应返回 422：%d %v", code, out)
	}
	getJSON(t, ts.URL+"/v1/config", &cfg)
	if cfg["model"] != "reloaded-model" || cfg["last_error"] == "" {
		t.Errorf("非法配置应保留上一份可用配置并报告错误：%v", cfg["last_error"])
	}
}

func TestShutdownCancelsRuns(t *testing.T) {
	gp := &gateProvider{started: make(chan struct{})}
	ts, s := newTestServer(t, gp, "")
	done := make(chan []sseEvent, 1)
	go func() { done <- chat(t, ts, map[string]any{"message": "长任务"}, nil) }()
	<-gp.started
	s.Shutdown()
	select {
	case evs := <-done:
		if len(find(evs, "run_end")) != 1 || names(evs)[len(evs)-1] != "done" {
			t.Errorf("关闭时应收到 run_end 与 done：%v", names(evs))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown 应取消正在运行的对话")
	}
	resp, _ := http.Post(ts.URL+"/v1/chat", "application/json", strings.NewReader(`{"message":"x"}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("关闭后应拒绝新对话：%d", resp.StatusCode)
	}
}

// agentSinkCtx 返回一个带事件接收器的 ctx，收到审批事件时回调其 ID。
func agentSinkCtx(onPermission func(id string)) context.Context {
	return agent.WithSink(context.Background(), func(ev agent.Event) {
		if ev.Permission != nil {
			onPermission(ev.Permission.ID)
		}
	})
}
