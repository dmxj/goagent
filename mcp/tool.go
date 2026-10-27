package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/dmxj/goagent/tool"
)

// mcpTool 把 MCP 远程工具适配为本地 tool.Tool。
// 命名规则与 Claude Code 一致：mcp__<服务器名>__<工具名>。
type mcpTool struct {
	client *Client
	info   ToolInfo
	name   string
}

func (t *mcpTool) Info() tool.Info {
	schema := t.info.InputSchema
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	desc := t.info.Description
	if desc == "" {
		desc = "MCP 工具 " + t.info.Name
	}
	return tool.Info{
		Name:        t.name,
		Description: fmt.Sprintf("[MCP:%s] %s", t.client.Name, desc),
		Schema:      schema,
		ReadOnly:    t.info.Annotations.ReadOnlyHint,
		Concurrent:  t.info.Annotations.ReadOnlyHint,
	}
}

func (t *mcpTool) Run(ctx context.Context, call *tool.Call) (*tool.Result, error) {
	res, err := t.client.CallTool(ctx, t.info.Name, call.Input)
	if err != nil {
		return tool.Errorf("调用 MCP 工具失败：%v", err), nil
	}
	return &tool.Result{Content: tool.Truncate(res.Text(), 30000), IsError: res.IsError, Meta: map[string]any{"mcp_server": t.client.Name, "mcp_tool": t.info.Name}}, nil
}

// ToolName 生成本地工具名。
func ToolName(server, name string) string {
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
				return r
			}
			return '_'
		}, s)
	}
	return "mcp__" + clean(server) + "__" + clean(name)
}

// Manager 管理多个 MCP 服务器连接。
//
// 配置热更新时调用 Sync：配置没变的服务器复用现有连接，新增 / 修改的重新连接，
// 删除 / 被替换的旧连接返回给调用方，由它在正在运行的对话结束后关闭。
type Manager struct {
	mu         sync.Mutex
	entries    map[string]*entry
	failedKeys map[string]string // 连接失败时的配置，配置不变时热更新不会反复重试
	Errors     map[string]error
}

type entry struct {
	key    string // 配置的 JSON，用于判断是否变化
	client *Client
	tools  []tool.Tool
}

// NewManager 创建管理器。
func NewManager() *Manager {
	return &Manager{entries: map[string]*entry{}, Errors: map[string]error{}}
}

func configKey(cfg ServerConfig) string {
	b, _ := json.Marshal(cfg)
	return string(b)
}

// ConnectAll 并发连接所有服务器；单个失败不影响其它（记录在 Errors 中）。
func (m *Manager) ConnectAll(ctx context.Context, servers map[string]ServerConfig) {
	m.Sync(ctx, servers, false)
}

// Sync 让连接与配置保持一致，返回不再使用的旧连接（调用方负责关闭）。
// retryFailed 为 true 时，配置没变但上次连接失败的服务器也会重新连接。
func (m *Manager) Sync(ctx context.Context, servers map[string]ServerConfig, retryFailed bool) []*Client {
	m.mu.Lock()
	var retired []*Client
	todo := map[string]ServerConfig{}
	for name, e := range m.entries {
		cfg, ok := servers[name]
		if !ok || configKey(cfg) != e.key {
			retired = append(retired, e.client)
			delete(m.entries, name)
		}
	}
	for name, cfg := range servers {
		if _, ok := m.entries[name]; ok {
			continue
		}
		if _, failed := m.Errors[name]; failed && !retryFailed && m.failedKeys[name] == configKey(cfg) {
			continue
		}
		todo[name] = cfg
	}
	for name := range m.Errors {
		if _, ok := servers[name]; !ok {
			delete(m.Errors, name)
			delete(m.failedKeys, name)
		}
	}
	m.mu.Unlock()

	names := make([]string, 0, len(todo))
	for name := range todo {
		names = append(names, name)
	}
	sort.Strings(names)
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func(name string, cfg ServerConfig) {
			defer wg.Done()
			c, err := Connect(ctx, name, cfg)
			var tools []tool.Tool
			if err == nil {
				var infos []ToolInfo
				if infos, err = c.ListTools(ctx); err == nil {
					for _, ti := range infos {
						tools = append(tools, &mcpTool{client: c, info: ti, name: ToolName(name, ti.Name)})
					}
				} else {
					c.Close()
				}
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			if err != nil {
				m.Errors[name] = err
				if m.failedKeys == nil {
					m.failedKeys = map[string]string{}
				}
				m.failedKeys[name] = configKey(cfg)
				return
			}
			delete(m.Errors, name)
			delete(m.failedKeys, name)
			m.entries[name] = &entry{key: configKey(cfg), client: c, tools: tools}
		}(name, servers[name])
	}
	wg.Wait()
	return retired
}

// Tools 返回所有 MCP 工具（按服务器名排序）。
func (m *Manager) Tools() []tool.Tool {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []tool.Tool
	for _, name := range m.namesLocked() {
		out = append(out, m.entries[name].tools...)
	}
	return out
}

func (m *Manager) namesLocked() []string {
	names := make([]string, 0, len(m.entries))
	for name := range m.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Clients 返回当前全部连接。
func (m *Manager) Clients() []*Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Client
	for _, name := range m.namesLocked() {
		out = append(out, m.entries[name].client)
	}
	return out
}

// Owns 报告连接是否仍由管理器持有（热更新后被换下的旧连接返回 false）。
func (m *Manager) Owns(c *Client) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		if e.client == c {
			return true
		}
	}
	return false
}

// Servers 返回已连接服务器名与其工具数。
func (m *Manager) Servers() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for name, e := range m.entries {
		out[name] = len(e.tools)
	}
	return out
}

// ErrorsSnapshot 返回连接错误的副本（并发安全）。
func (m *Manager) ErrorsSnapshot() map[string]error {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]error, len(m.Errors))
	for k, v := range m.Errors {
		out[k] = v
	}
	return out
}

// Instructions 返回各服务器在握手时提供的使用说明。
func (m *Manager) Instructions() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for name, e := range m.entries {
		if e.client.Instructions != "" {
			out[name] = e.client.Instructions
		}
	}
	return out
}

// Close 关闭所有连接。
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, e := range m.entries {
		e.client.Close()
		delete(m.entries, name)
	}
}

// ---------- 极简 MCP 服务器（stdio） ----------

// Server 把一组 tool.Tool 以 MCP 协议暴露出去，便于用 Go 编写自己的 MCP 服务器。
type Server struct {
	Name    string
	Version string
	Tools   []tool.Tool
}

// ServeStdio 在 r/w 上（通常是 os.Stdin/os.Stdout）提供服务，直到 r 关闭。
func (s *Server) ServeStdio(ctx context.Context, r io.Reader, w io.Writer) error {
	byName := map[string]tool.Tool{}
	for _, t := range s.Tools {
		byName[t.Info().Name] = t
	}
	var mu sync.Mutex
	reply := func(id *int64, result any, rerr *rpcError) {
		resp := map[string]any{"jsonrpc": "2.0", "id": id}
		if rerr != nil {
			resp["error"] = rerr
		} else {
			resp["result"] = result
		}
		b, _ := json.Marshal(resp)
		mu.Lock()
		defer mu.Unlock()
		_, _ = w.Write(append(b, '\n'))
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for sc.Scan() {
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		if req.ID == nil {
			continue // 通知无需回复
		}
		switch req.Method {
		case "initialize":
			reply(req.ID, map[string]any{
				"protocolVersion": ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
			}, nil)
		case "ping":
			reply(req.ID, map[string]any{}, nil)
		case "tools/list":
			list := make([]map[string]any, 0, len(s.Tools)) // 协议要求是数组，不能是 null
			for _, t := range s.Tools {
				info := t.Info()
				list = append(list, map[string]any{
					"name": info.Name, "description": info.Description, "inputSchema": info.Schema,
					"annotations": map[string]any{"readOnlyHint": info.ReadOnly},
				})
			}
			reply(req.ID, map[string]any{"tools": list}, nil)
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			t, ok := byName[p.Name]
			if !ok {
				reply(req.ID, nil, &rpcError{Code: -32602, Message: "未知工具 " + p.Name})
				continue
			}
			res, err := t.Run(ctx, &tool.Call{Name: p.Name, Input: p.Arguments, Env: &tool.Env{WorkDir: ".", Files: tool.NewFileTracker(), Values: &sync.Map{}}})
			if err != nil {
				res = tool.Errorf("%v", err)
			}
			if res == nil {
				res = tool.Text("")
			}
			reply(req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": res.Content}},
				"isError": res.IsError,
			}, nil)
		default:
			reply(req.ID, nil, &rpcError{Code: -32601, Message: "不支持的方法 " + req.Method})
		}
	}
	return sc.Err()
}
