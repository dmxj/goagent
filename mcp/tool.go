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
type Manager struct {
	mu      sync.Mutex
	clients map[string]*Client
	tools   []tool.Tool
	Errors  map[string]error
}

// NewManager 创建管理器。
func NewManager() *Manager {
	return &Manager{clients: map[string]*Client{}, Errors: map[string]error{}}
}

// ConnectAll 并发连接所有服务器；单个失败不影响其它（记录在 Errors 中）。
func (m *Manager) ConnectAll(ctx context.Context, servers map[string]ServerConfig) {
	var wg sync.WaitGroup
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		wg.Add(1)
		go func(name string, cfg ServerConfig) {
			defer wg.Done()
			c, err := Connect(ctx, name, cfg)
			if err == nil {
				var infos []ToolInfo
				infos, err = c.ListTools(ctx)
				if err == nil {
					m.mu.Lock()
					m.clients[name] = c
					for _, ti := range infos {
						m.tools = append(m.tools, &mcpTool{client: c, info: ti, name: ToolName(name, ti.Name)})
					}
					m.mu.Unlock()
					return
				}
				c.Close()
			}
			m.mu.Lock()
			m.Errors[name] = err
			m.mu.Unlock()
		}(name, servers[name])
	}
	wg.Wait()
}

// Tools 返回所有 MCP 工具。
func (m *Manager) Tools() []tool.Tool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]tool.Tool(nil), m.tools...)
}

// Servers 返回已连接服务器名与其工具数。
func (m *Manager) Servers() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for _, t := range m.tools {
		out[t.(*mcpTool).client.Name]++
	}
	for name := range m.clients {
		if _, ok := out[name]; !ok {
			out[name] = 0
		}
	}
	return out
}

// Instructions 返回各服务器在握手时提供的使用说明。
func (m *Manager) Instructions() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for name, c := range m.clients {
		if c.Instructions != "" {
			out[name] = c.Instructions
		}
	}
	return out
}

// Close 关闭所有连接。
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.clients {
		c.Close()
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
