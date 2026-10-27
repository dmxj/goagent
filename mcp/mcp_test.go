package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"bufio"
	"github.com/dmxj/goagent/tool"
)

type echoArgs struct {
	Text string `json:"text" desc:"要回显的文本"`
}

type addArgs struct {
	A int `json:"a"`
	B int `json:"b"`
}

// demoTools 是测试用 MCP 服务器提供的工具。
func demoTools() []tool.Tool {
	return []tool.Tool{
		tool.Func("echo", "回显文本", true, func(ctx context.Context, in echoArgs, c *tool.Call) (*tool.Result, error) {
			return tool.Text("echo: " + in.Text), nil
		}),
		tool.Func("add", "两数相加", false, func(ctx context.Context, in addArgs, c *tool.Call) (*tool.Result, error) {
			return tool.Text(fmt.Sprint(in.A + in.B)), nil
		}),
		tool.Func("boom", "总是失败", false, func(ctx context.Context, in struct{}, c *tool.Call) (*tool.Result, error) {
			return nil, errors.New("内部错误")
		}),
		tool.Func("nothing", "返回 nil 结果", true, func(ctx context.Context, in struct{}, c *tool.Call) (*tool.Result, error) {
			return nil, nil
		}),
	}
}

func demoServer() *Server { return &Server{Name: "demo", Version: "0.1.0", Tools: demoTools()} }

// TestMain 让测试二进制自身充当 stdio MCP 服务器子进程，用于测试真实的进程传输。
func TestMain(m *testing.M) {
	if os.Getenv("GOAGENT_MCP_HELPER") == "1" {
		_ = demoServer().ServeStdio(context.Background(), os.Stdin, os.Stdout)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// pipeClient 用 io.Pipe 把客户端与进程内服务器连起来。
func pipeClient(t *testing.T, srv *Server) *Client {
	t.Helper()
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.ServeStdio(context.Background(), serverR, serverW)
		serverW.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := ConnectIO(ctx, "demo", clientR, clientW)
	if err != nil {
		t.Fatalf("握手失败：%v", err)
	}
	t.Cleanup(func() {
		c.Close()
		<-done
	})
	return c
}

func TestInProcessEndToEnd(t *testing.T) {
	c := pipeClient(t, demoServer())
	ctx := context.Background()
	if c.ServerInfo.Name != "demo" || c.ServerInfo.Version != "0.1.0" {
		t.Errorf("initialize 返回的服务器信息错误：%+v", c.ServerInfo)
	}
	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 4 || tools[0].Name != "echo" || !tools[0].Annotations.ReadOnlyHint || tools[1].Annotations.ReadOnlyHint {
		t.Fatalf("tools/list 结果错误：%+v", tools)
	}
	var schema map[string]any
	if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil || schema["type"] != "object" {
		t.Errorf("inputSchema 错误：%s", tools[0].InputSchema)
	}

	tests := []struct {
		name    string
		tool    string
		args    string
		want    string
		isError bool
		rpcErr  bool
	}{
		{"回显", "echo", `{"text":"你好"}`, "echo: 你好", false, false},
		{"相加", "add", `{"a":2,"b":3}`, "5", false, false},
		{"空参数", "echo", ``, "echo: ", false, false},
		{"参数错误", "add", `{"a":"x"}`, "参数解析失败", true, false},
		{"工具返回 Go 错误", "boom", `{}`, "内部错误", true, false},
		{"工具返回 nil 结果", "nothing", `{}`, "", false, false},
		{"未知工具", "nope", `{}`, "", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := c.CallTool(ctx, tt.tool, json.RawMessage(tt.args))
			if tt.rpcErr {
				var re *rpcError
				if !errors.As(err, &re) || re.Code != -32602 {
					t.Fatalf("期望 JSON-RPC 错误，实际 %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError != tt.isError || !strings.Contains(res.Text(), tt.want) {
				t.Errorf("结果 = %+v，期望包含 %q 且 isError=%v", res, tt.want, tt.isError)
			}
		})
	}
	// 未实现的方法与 ping
	if _, err := c.call(ctx, "resources/list", nil); err == nil || !strings.Contains(err.Error(), "-32601") {
		t.Errorf("不支持的方法应返回 -32601：%v", err)
	}
	if _, err := c.call(ctx, "ping", nil); err != nil {
		t.Errorf("ping 失败：%v", err)
	}
}

func TestConcurrentCalls(t *testing.T) {
	c := pipeClient(t, demoServer())
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := c.CallTool(context.Background(), "add", json.RawMessage(fmt.Sprintf(`{"a":%d,"b":1}`, i)))
			if err != nil {
				errs <- err
				return
			}
			if res.Text() != fmt.Sprint(i+1) {
				errs <- fmt.Errorf("第 %d 个请求得到了错误的响应 %q（响应 ID 分发错乱）", i, res.Text())
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestCallResultText(t *testing.T) {
	var r CallResult
	_ = json.Unmarshal([]byte(`{"content":[{"type":"text","text":"a"},{"type":"image","mimeType":"image/png"},{"type":"text","text":"b"}]}`), &r)
	if got := r.Text(); got != "a\n[image 内容 image/png]\nb" {
		t.Errorf("Text() = %q", got)
	}
}

func TestToolName(t *testing.T) {
	tests := []struct{ server, name, want string }{
		{"github", "create_issue", "mcp__github__create_issue"},
		{"my server", "do.it", "mcp__my_server__do_it"},
		{"文件", "read-file", "mcp______read-file"}, // 每个非法字符替换为 _
	}
	for _, tt := range tests {
		if got := ToolName(tt.server, tt.name); got != tt.want {
			t.Errorf("ToolName(%q, %q) = %q，期望 %q", tt.server, tt.name, got, tt.want)
		}
	}
}

func TestStdioSubprocessManager(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	m := NewManager()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m.ConnectAll(ctx, map[string]ServerConfig{
		"demo":   {Command: exe, Env: map[string]string{"GOAGENT_MCP_HELPER": "1"}},
		"broken": {Command: "/nonexistent/mcp-server"},
		"empty":  {},
	})
	defer m.Close()
	if len(m.Errors) != 2 || m.Errors["broken"] == nil || m.Errors["empty"] == nil {
		t.Errorf("失败的服务器应记录在 Errors 中：%v", m.Errors)
	}
	if got := m.Servers(); got["demo"] != 4 || len(got) != 1 {
		t.Errorf("Servers() = %v", got)
	}
	reg := tool.NewRegistry(m.Tools()...)
	echo, ok := reg.Get("mcp__demo__echo")
	if !ok {
		t.Fatalf("MCP 工具应以 mcp__<服务器>__<工具> 命名：%v", reg.Names())
	}
	info := echo.Info()
	if !info.ReadOnly || !info.Concurrent || !strings.HasPrefix(info.Description, "[MCP:demo]") {
		t.Errorf("工具元信息错误：%+v", info)
	}
	res, err := echo.Run(ctx, &tool.Call{Input: json.RawMessage(`{"text":"子进程"}`)})
	if err != nil || res.Content != "echo: 子进程" || res.Meta["mcp_server"] != "demo" {
		t.Errorf("通过子进程调用失败：%+v %v", res, err)
	}
	if add, _ := reg.Get("mcp__demo__add"); add.Info().ReadOnly {
		t.Errorf("没有 readOnlyHint 的工具不应是只读")
	}
	if len(m.Instructions()) != 0 {
		t.Errorf("demo 服务器没有 instructions")
	}
}

func TestStdioServerExit(t *testing.T) {
	m := NewManager()
	m.ConnectAll(context.Background(), map[string]ServerConfig{"quit": {Command: "/bin/sh", Args: []string{"-c", "echo oops >&2; exit 1"}}})
	if err := m.Errors["quit"]; err == nil || !strings.Contains(err.Error(), "已退出") {
		t.Errorf("服务器退出时应报告错误：%v", err)
	}
}

func TestMCPToolDefaults(t *testing.T) {
	mt := &mcpTool{client: &Client{Name: "s"}, info: ToolInfo{Name: "x"}, name: "mcp__s__x"}
	info := mt.Info()
	if string(info.Schema) != `{"type":"object","properties":{}}` || info.Description != "[MCP:s] MCP 工具 x" {
		t.Errorf("缺省 Schema/描述错误：%+v", info)
	}
}

// httpMCP 用 mcp.Server 处理单条 JSON-RPC 消息，模拟 Streamable HTTP 服务器。
type httpMCP struct {
	srv      *Server
	sse      bool
	mu       sync.Mutex
	sessions []string // 每个请求携带的 Mcp-Session-Id
	instr    string
}

func (h *httpMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	h.sessions = append(h.sessions, r.Header.Get("Mcp-Session-Id"))
	h.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer secret" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var out bytes.Buffer
	_ = h.srv.ServeStdio(r.Context(), bytes.NewReader(append(body, '\n')), &out)
	if out.Len() == 0 { // 通知
		w.WriteHeader(http.StatusAccepted)
		return
	}
	resp := bytes.TrimSpace(out.Bytes())
	if h.instr != "" && strings.Contains(string(body), `"initialize"`) {
		var m map[string]any
		_ = json.Unmarshal(resp, &m)
		m["result"].(map[string]any)["instructions"] = h.instr
		resp, _ = json.Marshal(m)
	}
	w.Header().Set("Mcp-Session-Id", "sess-123")
	if h.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		// 先推一条服务器通知，客户端应跳过它
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", resp)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

func TestHTTPTransport(t *testing.T) {
	for _, mode := range []struct {
		name string
		sse  bool
	}{{"JSON 响应", false}, {"SSE 响应", true}} {
		t.Run(mode.name, func(t *testing.T) {
			h := &httpMCP{srv: demoServer(), sse: mode.sse, instr: "先调用 echo"}
			ts := httptest.NewServer(h)
			defer ts.Close()
			t.Setenv("TEST_MCP_TOKEN", "secret")
			ctx := context.Background()
			c, err := Connect(ctx, "remote", ServerConfig{URL: ts.URL, Headers: map[string]string{"Authorization": "Bearer ${TEST_MCP_TOKEN}"}})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if c.Instructions != "先调用 echo" {
				t.Errorf("应读取服务器的 instructions：%q", c.Instructions)
			}
			tools, err := c.ListTools(ctx)
			if err != nil || len(tools) != 4 {
				t.Fatalf("tools/list 失败：%v %d", err, len(tools))
			}
			res, err := c.CallTool(ctx, "add", json.RawMessage(`{"a":40,"b":2}`))
			if err != nil || res.Text() != "42" {
				t.Fatalf("tools/call 失败：%+v %v", res, err)
			}
			if _, err := c.CallTool(ctx, "nope", nil); err == nil {
				t.Errorf("未知工具应返回错误")
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.sessions[0] != "" || h.sessions[len(h.sessions)-1] != "sess-123" {
				t.Errorf("后续请求应携带服务器分配的 Mcp-Session-Id：%v", h.sessions)
			}
		})
	}
	t.Run("HTTP 错误状态码", func(t *testing.T) {
		ts := httptest.NewServer(&httpMCP{srv: demoServer()})
		defer ts.Close()
		_, err := Connect(context.Background(), "remote", ServerConfig{Type: "http", URL: ts.URL})
		if err == nil || !strings.Contains(err.Error(), "401") {
			t.Errorf("401 应作为初始化错误返回：%v", err)
		}
	})
	t.Run("SSE 中没有对应响应", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":999,\"result\":{}}\n\n")
		}))
		defer ts.Close()
		if _, err := Connect(context.Background(), "x", ServerConfig{URL: ts.URL}); err == nil || !strings.Contains(err.Error(), "没有返回响应") {
			t.Errorf("应报告缺少响应：%v", err)
		}
	})
	t.Run("Manager 通过 HTTP 连接", func(t *testing.T) {
		ts := httptest.NewServer(&httpMCP{srv: demoServer(), instr: "说明"})
		defer ts.Close()
		m := NewManager()
		m.ConnectAll(context.Background(), map[string]ServerConfig{"web": {URL: ts.URL, Headers: map[string]string{"Authorization": "Bearer secret"}}})
		defer m.Close()
		if len(m.Tools()) != 4 || m.Instructions()["web"] != "说明" {
			t.Errorf("Manager 连接 HTTP 服务器失败：%v %v", m.Errors, m.Instructions())
		}
	})
}

func TestStdioCallContextCancel(t *testing.T) {
	// 服务器只读不回：调用应在 ctx 取消时返回
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, serverR) }()
	tr := &stdioTransport{stdin: clientW, done: make(chan struct{}), stderr: &lockedBuffer{}}
	go tr.readLoop(clientR)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	id := int64(1)
	if _, err := tr.call(ctx, rpcRequest{JSONRPC: "2.0", ID: &id, Method: "x"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("应返回 ctx 超时：%v", err)
	}
	serverW.Close()
	_ = tr.close()
}

func TestLockedBuffer(t *testing.T) {
	var b lockedBuffer
	chunk := bytes.Repeat([]byte("x"), 10*1024)
	for i := 0; i < 20; i++ {
		_, _ = b.Write(chunk)
	}
	if n := len(b.String()); n > 80*1024 {
		t.Errorf("stderr 缓冲应限制大小，实际 %d 字节", n)
	}
}

// TestServerInitiatedRequest：服务器先发一个与在途请求 id 相同的 ping，再回复真正的响应。
// 客户端应回复 ping，并且在途请求拿到的是真正的结果而不是 ping。
func TestServerInitiatedRequest(t *testing.T) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	t.Cleanup(func() { clientW.Close(); serverW.Close() })
	replies := make(chan map[string]any, 4)
	go func() {
		sc := bufio.NewScanner(serverR)
		for sc.Scan() {
			var req map[string]any
			_ = json.Unmarshal(sc.Bytes(), &req)
			id, hasID := req["id"]
			method, _ := req["method"].(string)
			switch {
			case method == "initialize":
				fmt.Fprintf(serverW, `{"jsonrpc":"2.0","id":%v,"result":{"serverInfo":{"name":"s","version":"1"}}}`+"\n", id)
			case method == "tools/call":
				// 先发一个 id 相同的服务器请求 ping，再发一个未知方法的请求，最后才是响应
				fmt.Fprintf(serverW, `{"jsonrpc":"2.0","id":%v,"method":"ping"}`+"\n", id)
				fmt.Fprintf(serverW, `{"jsonrpc":"2.0","id":999,"method":"sampling/createMessage"}`+"\n")
				fmt.Fprintf(serverW, `{"jsonrpc":"2.0","id":%v,"result":{"content":[{"type":"text","text":"真正的结果"}]}}`+"\n", id)
			case method == "" && hasID:
				replies <- req // 客户端对服务器请求的回复
			}
		}
	}()
	c, err := ConnectIO(context.Background(), "s", clientR, clientW)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.CallTool(context.Background(), "x", nil)
	if err != nil || res.Text() != "真正的结果" {
		t.Fatalf("在途请求应拿到真正的响应：%v %+v", err, res)
	}
	got := map[float64]map[string]any{}
	for i := 0; i < 2; i++ {
		select {
		case r := <-replies:
			got[r["id"].(float64)] = r
		case <-time.After(3 * time.Second):
			t.Fatalf("客户端应回复服务器请求，只收到 %v", got)
		}
	}
	if r := got[999]; r == nil || r["error"] == nil {
		t.Errorf("未知方法应回复 -32601：%v", r)
	}
	for id, r := range got {
		if id != 999 && r["result"] == nil {
			t.Errorf("ping 应回复空结果：%v", r)
		}
	}
}

func TestManagerSync(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	demo := ServerConfig{Command: exe, Env: map[string]string{"GOAGENT_MCP_HELPER": "1"}}
	m := NewManager()
	defer m.Close()
	if retired := m.Sync(ctx, map[string]ServerConfig{"a": demo, "bad": {Command: "/nonexistent"}}, false); len(retired) != 0 {
		t.Fatalf("首次同步不应有旧连接")
	}
	first := m.Tools()
	// 配置不变：复用连接，不重试失败的服务器
	if retired := m.Sync(ctx, map[string]ServerConfig{"a": demo, "bad": {Command: "/nonexistent"}}, false); len(retired) != 0 {
		t.Errorf("配置不变时不应替换连接")
	}
	if m.Tools()[0] != first[0] || m.ErrorsSnapshot()["bad"] == nil {
		t.Errorf("配置不变时应复用工具实例并保留错误")
	}
	// 修改 a、删除 bad、新增 b
	changed := demo
	changed.Args = []string{"-x"}
	retired := m.Sync(ctx, map[string]ServerConfig{"a": changed, "b": demo}, false)
	if len(retired) != 1 {
		t.Fatalf("修改的服务器应返回旧连接：%d", len(retired))
	}
	for _, c := range retired {
		c.Close()
	}
	if got := m.Servers(); len(got) != 2 || got["a"] != 4 || got["b"] != 4 {
		t.Errorf("同步后服务器 = %v", got)
	}
	if len(m.ErrorsSnapshot()) != 0 {
		t.Errorf("删除的服务器不应再保留错误：%v", m.ErrorsSnapshot())
	}
}
