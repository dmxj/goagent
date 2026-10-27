// Package server 把 Agent 以 HTTP API 的形式提供出去：
// POST /v1/chat 以 SSE（Server-Sent Events）流式推送 Agent 运行事件，
// 前端据此实现打字机效果、工具调用过程、权限审批弹窗与 Trace 调试面板。
package server

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/dmxj/goagent/agent"
	"github.com/dmxj/goagent/app"
	"github.com/dmxj/goagent/permission"
	"github.com/dmxj/goagent/session"
	"github.com/dmxj/goagent/tool/builtin"
	"github.com/dmxj/goagent/trace"
)

//go:embed web
var webFS embed.FS

// Version 由构建时 -ldflags 注入。
var Version = "dev"

// Server 是 Agent 的 HTTP 服务。
type Server struct {
	App   *app.App
	Token string // 非空时要求请求头 Authorization: Bearer <Token>
	Log   *slog.Logger

	mu       sync.Mutex
	cancels  map[string]context.CancelFunc // sessionID -> 取消函数
	sessions map[string]*session.Session   // 活跃会话缓存
	approver *WebApprover
}

// WebApprover 通过 SSE 事件把审批请求推给前端，等待 POST /v1/permissions/{id} 的决定。
type WebApprover struct {
	mu      sync.Mutex
	pending map[string]chan decision
	Timeout time.Duration
}

type decision struct{ allow, remember bool }

// NewWebApprover 创建审批器。
func NewWebApprover() *WebApprover {
	return &WebApprover{pending: map[string]chan decision{}, Timeout: 5 * time.Minute}
}

// Approve 实现 permission.Approver。
func (w *WebApprover) Approve(ctx context.Context, req permission.Request, reason string) (bool, bool, error) {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	id := "perm_" + hex.EncodeToString(b)
	ch := make(chan decision, 1)
	w.mu.Lock()
	w.pending[id] = ch
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()
	}()
	agent.SinkFrom(ctx)(agent.Event{Type: agent.EventPermission, Agent: agent.AgentPath(ctx), Time: time.Now(),
		Permission: &agent.PermissionEvent{ID: id, Tool: req.Tool, Input: req.Input, Reason: reason}})
	select {
	case d := <-ch:
		return d.allow, d.remember, nil
	case <-ctx.Done():
		return false, false, ctx.Err()
	case <-time.After(w.Timeout):
		return false, false, errors.New("等待审批超时")
	}
}

// Decide 提交审批结果。
func (w *WebApprover) Decide(id string, allow, remember bool) bool {
	w.mu.Lock()
	ch, ok := w.pending[id]
	w.mu.Unlock()
	if ok {
		ch <- decision{allow, remember}
	}
	return ok
}

// New 创建服务。approver 应与 app 组装时使用的同一个实例。
func New(a *app.App, approver *WebApprover) *Server {
	return &Server{
		App: a, approver: approver, Log: slog.Default(),
		cancels: map[string]context.CancelFunc{}, sessions: map[string]*session.Session{},
	}
}

// Handler 返回路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"status": "ok", "version": Version})
	})
	mux.HandleFunc("GET /v1/info", s.info)
	mux.HandleFunc("GET /v1/sessions", s.listSessions)
	mux.HandleFunc("POST /v1/sessions", s.createSession)
	mux.HandleFunc("GET /v1/sessions/{id}", s.getSession)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.deleteSession)
	mux.HandleFunc("POST /v1/sessions/{id}/cancel", s.cancel)
	mux.HandleFunc("POST /v1/sessions/{id}/steer", s.steer)
	mux.HandleFunc("POST /v1/sessions/{id}/compact", s.compact)
	mux.HandleFunc("GET /v1/sessions/{id}/files", s.files)
	mux.HandleFunc("GET /v1/sessions/{id}/todos", s.todos)
	mux.HandleFunc("POST /v1/chat", s.chat)
	mux.HandleFunc("POST /v1/permissions/{id}", s.permission)
	mux.HandleFunc("GET /v1/traces", s.traces)
	mux.HandleFunc("GET /v1/traces/{id}/tree", s.traceTree)
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(sub))
	return s.middleware(mux)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if s.Token != "" && strings.HasPrefix(r.URL.Path, "/v1/") && r.Header.Get("Authorization") != "Bearer "+s.Token {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未授权"})
			return
		}
		defer func() {
			if rec := recover(); rec != nil {
				s.Log.Error("panic", "err", rec, "stack", string(debug.Stack()))
				writeJSON(w, 500, map[string]string{"error": fmt.Sprint(rec)})
			}
		}()
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/health" {
			s.Log.Info("http", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start).Round(time.Millisecond))
		}
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	a := s.App
	type toolInfo struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		ReadOnly    bool   `json:"read_only"`
	}
	var tools []toolInfo
	for _, t := range a.Agent.Tools.List() {
		i := t.Info()
		tools = append(tools, toolInfo{i.Name, i.Description, i.ReadOnly})
	}
	writeJSON(w, 200, map[string]any{
		"version":         Version,
		"model":           a.Agent.Model,
		"provider":        a.Config.Provider.Type,
		"work_dir":        a.Config.WorkDir,
		"permission_mode": a.Agent.Permissions.Mode(),
		"tools":           tools,
		"skills":          a.Skills,
		"subagents":       a.Subagents,
		"mcp_servers":     a.MCP.Servers(),
		"warnings":        a.Warnings,
		"config_sources":  a.Config.Sources,
	})
}

func (s *Server) loadSession(id string) (*session.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		return sess, nil
	}
	sess, err := s.App.Store.Load(id)
	if err != nil {
		return nil, err
	}
	s.sessions[id] = sess
	return sess, nil
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	list, err := s.App.Store.List()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"sessions": list})
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	sess := s.App.NewSession()
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.mu.Unlock()
	_ = s.App.Store.Save(sess)
	writeJSON(w, 201, map[string]any{"id": sess.ID})
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.loadSession(r.PathValue("id"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"id": sess.ID, "title": sess.Title, "messages": sess.History(), "usage": sess.Usage,
		"summary": sess.Summary, "running": s.App.Agent.Running(sess.ID),
	})
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
	if err := s.App.Store.Delete(id); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	c, ok := s.cancels[r.PathValue("id")]
	s.mu.Unlock()
	if ok {
		c()
	}
	writeJSON(w, 200, map[string]any{"cancelled": ok})
}

func (s *Server) steer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	ok := s.App.Agent.Steer(r.PathValue("id"), body.Message)
	writeJSON(w, 200, map[string]any{"accepted": ok})
}

func (s *Server) compact(w http.ResponseWriter, r *http.Request) {
	sess, err := s.loadSession(r.PathValue("id"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	if err := s.App.Agent.Compact(r.Context(), sess, nil); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	_ = s.App.Store.Save(sess)
	writeJSON(w, 200, map[string]any{"summary": sess.Summary})
}

func (s *Server) files(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"files": s.App.Agent.Files(r.PathValue("id"))})
}

func (s *Server) todos(w http.ResponseWriter, r *http.Request) {
	v, _ := s.App.Agent.Values(r.PathValue("id")).Load(builtin.TodoKey)
	writeJSON(w, 200, map[string]any{"todos": v})
}

func (s *Server) permission(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Allow    bool `json:"allow"`
		Remember bool `json:"remember"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if s.approver == nil || !s.approver.Decide(r.PathValue("id"), body.Allow, body.Remember) {
		writeJSON(w, 404, map[string]string{"error": "审批请求不存在或已过期"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) traces(w http.ResponseWriter, r *http.Request) {
	spans := s.App.Traces.Spans(r.URL.Query().Get("trace_id"))
	if sid := r.URL.Query().Get("session_id"); sid != "" {
		// 找出该会话的 trace（agent.run span 的 session_id 属性）
		ids := map[string]bool{}
		for _, sp := range spans {
			if sp.Kind == trace.KindAgent && sp.Attrs["session_id"] == sid {
				ids[sp.TraceID] = true
			}
		}
		var out []trace.SpanData
		for _, sp := range spans {
			if ids[sp.TraceID] {
				out = append(out, sp)
			}
		}
		spans = out
	}
	writeJSON(w, 200, map[string]any{"spans": spans})
}

func (s *Server) traceTree(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(trace.Tree(s.App.Traces.Spans(r.PathValue("id")))))
}

// chat 运行一轮对话，以 SSE 推送事件。客户端断开连接会取消运行。
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
		Message   string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Message) == "" {
		writeJSON(w, 400, map[string]string{"error": "需要 message 字段"})
		return
	}
	var sess *session.Session
	if body.SessionID != "" {
		var err error
		if sess, err = s.loadSession(body.SessionID); err != nil && !errors.Is(err, session.ErrNotFound) {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
	}
	if sess == nil {
		sess = s.App.NewSession()
		if body.SessionID != "" && !strings.ContainsAny(body.SessionID, "/\\.") {
			sess.ID = body.SessionID
		}
		s.mu.Lock()
		s.sessions[sess.ID] = sess
		s.mu.Unlock()
	}
	if s.App.Agent.Running(sess.ID) {
		writeJSON(w, 409, map[string]string{"error": "该会话正在运行，请稍候或先取消"})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, map[string]string{"error": "不支持流式输出"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	s.mu.Lock()
	s.cancels[sess.ID] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.cancels, sess.ID)
		s.mu.Unlock()
	}()

	events := make(chan agent.Event, 256)
	var writeMu sync.Mutex
	send := func(name string, v any) {
		b, _ := json.Marshal(v)
		writeMu.Lock()
		defer writeMu.Unlock()
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
		flusher.Flush()
	}
	send("session", map[string]string{"session_id": sess.ID})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := s.App.Agent.Run(ctx, sess, body.Message, func(ev agent.Event) {
			// 只在客户端断开时丢弃事件。不能以运行 ctx 为条件：通过 /cancel 取消时
			// 客户端仍在线，收尾的 error / run_end 事件必须送达。
			select {
			case events <- ev:
			case <-r.Context().Done():
			}
		})
		if errors.Is(err, agent.ErrBusy) {
			// 两个请求同时通过了上面的 Running 检查，后到的这个没有真正运行
			select {
			case events <- agent.Event{Type: agent.EventError, SessionID: sess.ID, Error: err.Error(), Time: time.Now()}:
			case <-r.Context().Done():
			}
		}
		if err != nil {
			s.Log.Warn("run", "session", sess.ID, "err", err)
		}
	}()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case ev := <-events:
			send(string(ev.Type), ev)
		case <-heartbeat.C:
			writeMu.Lock()
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
			writeMu.Unlock()
		case <-done:
			for {
				select {
				case ev := <-events:
					send(string(ev.Type), ev)
				default:
					send("done", map[string]any{"session_id": sess.ID})
					return
				}
			}
		}
	}
}
