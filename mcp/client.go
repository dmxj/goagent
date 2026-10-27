// Package mcp 实现 Model Context Protocol 客户端，把 MCP 服务器提供的工具
// 无缝接入 Agent 的工具系统。
//
// MCP 基于 JSON-RPC 2.0，支持两种传输：
//   - stdio：启动子进程，按行收发 JSON（本地工具最常用）
//   - Streamable HTTP：POST JSON，响应为 JSON 或 SSE 流（远程服务）
//
// 握手流程：initialize → notifications/initialized → tools/list → tools/call ...
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dmxj/goagent/llm"
)

// ProtocolVersion 是本客户端声明支持的协议版本。
const ProtocolVersion = "2025-06-18"

// ServerConfig 是一个 MCP 服务器的配置（与 Claude Code 的 .mcp.json 格式兼容）。
type ServerConfig struct {
	Type    string            `json:"type,omitempty"` // stdio | http
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("MCP 错误 %d: %s", e.Code, e.Message) }

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// transport 抽象底层传输。
type transport interface {
	call(ctx context.Context, req rpcRequest) (json.RawMessage, error)
	notify(ctx context.Context, method string, params any) error
	close() error
}

// ToolInfo 是 MCP 服务器暴露的工具描述。
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations struct {
		ReadOnlyHint bool `json:"readOnlyHint"`
	} `json:"annotations"`
}

// CallResult 是 tools/call 的返回。
type CallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
		// 资源或图片等其它类型只记录类型
		MimeType string `json:"mimeType,omitempty"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// Text 拼接返回中的文本内容。
func (r *CallResult) Text() string {
	var parts []string
	for _, c := range r.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		} else {
			parts = append(parts, fmt.Sprintf("[%s 内容 %s]", c.Type, c.MimeType))
		}
	}
	return strings.Join(parts, "\n")
}

// Client 是一个已连接的 MCP 客户端。
type Client struct {
	Name       string
	ServerInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	Instructions string
	t            transport
	seq          atomic.Int64
}

// ConnectIO 通过任意读写流（例如 io.Pipe 连接的进程内服务器）建立 stdio 风格连接。
func ConnectIO(ctx context.Context, name string, r io.Reader, w io.WriteCloser) (*Client, error) {
	t := &stdioTransport{stdin: w, done: make(chan struct{}), stderr: &lockedBuffer{}}
	go t.readLoop(r)
	return handshake(ctx, name, t)
}

// Connect 根据配置建立连接并完成握手。
func Connect(ctx context.Context, name string, cfg ServerConfig) (*Client, error) {
	var t transport
	var err error
	switch {
	case cfg.Type == "http" || cfg.Type == "streamable-http" || (cfg.Type == "" && cfg.URL != ""):
		t = newHTTPTransport(cfg)
	case cfg.Command != "":
		t, err = newStdioTransport(cfg)
	default:
		return nil, fmt.Errorf("MCP 服务器 %s 配置无效：需要 command 或 url", name)
	}
	if err != nil {
		return nil, err
	}
	return handshake(ctx, name, t)
}

func handshake(ctx context.Context, name string, t transport) (*Client, error) {
	c := &Client{Name: name, t: t}
	ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := c.call(ictx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "goagent", "version": "1.0.0"},
	})
	if err != nil {
		t.close()
		return nil, fmt.Errorf("MCP %s 初始化失败: %w", name, err)
	}
	var init struct {
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Instructions string `json:"instructions"`
	}
	_ = json.Unmarshal(raw, &init)
	c.ServerInfo = init.ServerInfo
	c.Instructions = init.Instructions
	if err := t.notify(ictx, "notifications/initialized", nil); err != nil {
		t.close()
		return nil, err
	}
	return c, nil
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.seq.Add(1)
	return c.t.call(ctx, rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
}

// ListTools 列出服务器的全部工具（自动处理分页）。
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	var all []ToolInfo
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var res struct {
			Tools      []ToolInfo `json:"tools"`
			NextCursor string     `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			return all, nil
		}
		cursor = res.NextCursor
	}
}

// CallTool 调用工具。
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (*CallResult, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	raw, err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, err
	}
	var res CallResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Close 断开连接。
func (c *Client) Close() error { return c.t.close() }

// ---------- stdio 传输 ----------

type stdioTransport struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	mu      sync.Mutex // 保护写入
	pending sync.Map   // id -> chan rpcResponse
	done    chan struct{}
	stderr  *lockedBuffer
}

// lockedBuffer 是并发安全的 bytes.Buffer：子进程的 stderr 由 exec 的后台 goroutine 写入，
// 而读取发生在请求 goroutine 中。只保留最后 64KB，避免长时间运行的服务器撑爆内存。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() > 64*1024 {
		tail := append([]byte(nil), b.buf.Bytes()[b.buf.Len()-32*1024:]...)
		b.buf.Reset()
		b.buf.Write(tail)
	}
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (t *stdioTransport) stderrText() string { return t.stderr.String() }

func newStdioTransport(cfg ServerConfig) (*stdioTransport, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+os.ExpandEnv(v))
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 MCP 服务器失败: %w", err)
	}
	t := &stdioTransport{cmd: cmd, stdin: stdin, done: make(chan struct{}), stderr: stderr}
	go t.readLoop(stdout)
	return t, nil
}

// readLoop 持续读取服务器输出，把响应分发给等待中的请求。
func (t *stdioTransport) readLoop(r io.Reader) {
	defer close(t.done)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for sc.Scan() {
		var resp rpcResponse
		if err := json.Unmarshal(sc.Bytes(), &resp); err != nil || resp.ID == nil {
			continue // 忽略通知与非 JSON 输出
		}
		if resp.Method != "" {
			// 带 id 与 method 的是服务器发给客户端的请求（例如 ping），不是对我们请求的响应：
			// 不能拿它去匹配在途请求（id 可能恰好相同），而是按协议回复。
			go func(id int64, method string) { _ = t.write(serverRequestReply(id, method)) }(*resp.ID, resp.Method)
			continue
		}
		if ch, ok := t.pending.LoadAndDelete(*resp.ID); ok {
			ch.(chan rpcResponse) <- resp
		}
	}
}

// serverRequestReply 构造对服务器请求的回复：ping 返回空结果，其它方法回复"方法不存在"。
func serverRequestReply(id int64, method string) map[string]any {
	reply := map[string]any{"jsonrpc": "2.0", "id": id}
	if method == "ping" {
		reply["result"] = map[string]any{}
	} else {
		reply["error"] = &rpcError{Code: -32601, Message: "客户端不支持方法 " + method}
	}
	return reply
}

func (t *stdioTransport) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, err = t.stdin.Write(append(b, '\n'))
	return err
}

func (t *stdioTransport) call(ctx context.Context, req rpcRequest) (json.RawMessage, error) {
	ch := make(chan rpcResponse, 1)
	t.pending.Store(*req.ID, ch)
	if err := t.write(req); err != nil {
		t.pending.Delete(*req.ID)
		return nil, err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-t.done:
		// 服务器在写出响应后立即退出时，响应可能已经在 ch 中，优先取出
		select {
		case resp := <-ch:
			if resp.Error != nil {
				return nil, resp.Error
			}
			return resp.Result, nil
		default:
		}
		t.pending.Delete(*req.ID)
		return nil, fmt.Errorf("MCP 服务器已退出: %s", strings.TrimSpace(t.stderrText()))
	case <-ctx.Done():
		t.pending.Delete(*req.ID)
		return nil, ctx.Err()
	}
}

func (t *stdioTransport) notify(ctx context.Context, method string, params any) error {
	return t.write(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (t *stdioTransport) close() error {
	_ = t.stdin.Close()
	if t.cmd == nil {
		return nil
	}
	select {
	case <-t.done:
	case <-time.After(2 * time.Second):
		_ = t.cmd.Process.Kill()
	}
	_ = t.cmd.Wait()
	return nil
}

// ---------- Streamable HTTP 传输 ----------

type httpTransport struct {
	url     string
	headers map[string]string
	client  *http.Client
	mu      sync.Mutex
	session string
}

func newHTTPTransport(cfg ServerConfig) *httpTransport {
	return &httpTransport{url: cfg.URL, headers: cfg.Headers, client: &http.Client{Timeout: 5 * time.Minute}}
}

func (t *httpTransport) post(ctx context.Context, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	for k, v := range t.headers {
		req.Header.Set(k, os.ExpandEnv(v))
	}
	t.mu.Lock()
	if t.session != "" {
		req.Header.Set("Mcp-Session-Id", t.session)
	}
	t.mu.Unlock()
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.mu.Lock()
		t.session = sid
		t.mu.Unlock()
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("MCP HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

func (t *httpTransport) call(ctx context.Context, req rpcRequest) (json.RawMessage, error) {
	resp, err := t.post(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out *rpcResponse
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		errFound := errors.New("found")
		err = llm.ReadSSE(resp.Body, func(e llm.SSEEvent) error {
			var r rpcResponse
			if json.Unmarshal([]byte(e.Data), &r) != nil || r.ID == nil {
				return nil
			}
			if r.Method != "" { // 服务器在流中发来的请求：另起请求回复，不当作响应
				go func(id int64, method string) {
					if resp, err := t.post(context.Background(), serverRequestReply(id, method)); err == nil {
						resp.Body.Close()
					}
				}(*r.ID, r.Method)
				return nil
			}
			if *r.ID == *req.ID {
				out = &r
				return errFound
			}
			return nil
		})
		if err != nil && !errors.Is(err, errFound) {
			return nil, err
		}
	} else {
		var r rpcResponse
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return nil, err
		}
		out = &r
	}
	if out == nil {
		return nil, fmt.Errorf("MCP 服务器没有返回响应")
	}
	if out.Error != nil {
		return nil, out.Error
	}
	return out.Result, nil
}

func (t *httpTransport) notify(ctx context.Context, method string, params any) error {
	resp, err := t.post(ctx, rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (t *httpTransport) close() error { return nil }
