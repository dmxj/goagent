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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dmxj/goagent/agent"
	"github.com/dmxj/goagent/app"
	"github.com/dmxj/goagent/llm"
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

	mu        sync.Mutex
	cancels   map[string]context.CancelFunc // sessionID -> 取消函数
	sessions  map[string]*session.Session   // 活跃会话缓存
	approver  *WebApprover
	closing   chan struct{}
	closeOnce sync.Once
}

// WebApprover 通过 SSE 事件把审批请求推给前端，等待 POST /v1/permissions/{id} 的决定。
//
// 提交是幂等且非阻塞的：同一个 ID 只有第一次提交生效（200），之后的重复提交返回 409，
// 不存在的 ID 返回 404。v0.1.0 用阻塞发送写入容量为 1 的 channel，同一 ID 提交三次以上时
// 多出的 HTTP 处理 goroutine 会永久阻塞。
type WebApprover struct {
	mu       sync.Mutex
	pending  map[string]*pendingApproval
	resolved map[string]time.Time // 已处理 / 已过期的 ID，用于区分 404 与 409
	Timeout  time.Duration
}

type pendingApproval struct {
	ch      chan decision
	decided bool
	event   agent.PermissionEvent
	session string
	agent   string
	at      time.Time
}

type decision struct{ allow, remember bool }

// DecideStatus 是提交审批结果的状态。
type DecideStatus int

const (
	DecideOK       DecideStatus = iota // 已送达
	DecideNotFound                     // ID 不存在
	DecideConflict                     // 已经处理过或已过期
)

// PendingApproval 是一个等待中的审批（GET /v1/permissions 返回，页面刷新后可以恢复审批弹窗）。
type PendingApproval struct {
	agent.PermissionEvent
	SessionID string    `json:"session_id"`
	Agent     string    `json:"agent"`
	At        time.Time `json:"at"`
}

// NewWebApprover 创建审批器。
func NewWebApprover() *WebApprover {
	return &WebApprover{pending: map[string]*pendingApproval{}, resolved: map[string]time.Time{}, Timeout: 5 * time.Minute}
}

// Approve 实现 permission.Approver。
func (w *WebApprover) Approve(ctx context.Context, req permission.Request, reason string) (bool, bool, error) {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	id := "perm_" + hex.EncodeToString(b)
	p := &pendingApproval{ch: make(chan decision, 1), session: req.SessionID, agent: agent.AgentPath(ctx), at: time.Now(),
		event: agent.PermissionEvent{ID: id, Tool: req.Tool, Input: req.Input, Reason: reason}}
	w.mu.Lock()
	w.pending[id] = p
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.pending, id)
		w.markResolvedLocked(id)
		w.mu.Unlock()
	}()
	ev := p.event
	agent.SinkFrom(ctx)(agent.Event{Type: agent.EventPermission, Agent: p.agent, Time: time.Now(), Permission: &ev})
	timer := time.NewTimer(w.Timeout)
	defer timer.Stop()
	select {
	case d := <-p.ch:
		return d.allow, d.remember, nil
	case <-ctx.Done():
		return false, false, ctx.Err()
	case <-timer.C:
		return false, false, errors.New("等待审批超时")
	}
}

func (w *WebApprover) markResolvedLocked(id string) {
	now := time.Now()
	w.resolved[id] = now
	for k, at := range w.resolved {
		if now.Sub(at) > 30*time.Minute {
			delete(w.resolved, k)
		}
	}
}

// Resolve 提交审批结果（非阻塞、幂等）。
func (w *WebApprover) Resolve(id string, allow, remember bool) DecideStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.pending[id]
	switch {
	case ok && !p.decided:
		p.decided = true
		select {
		case p.ch <- decision{allow, remember}: // 缓冲为 1 且只发送一次，永远不会阻塞
		default:
		}
		w.markResolvedLocked(id)
		return DecideOK
	case ok:
		return DecideConflict
	}
	if _, done := w.resolved[id]; done {
		return DecideConflict
	}
	return DecideNotFound
}

// Decide 提交审批结果，返回是否送达（兼容旧接口）。
func (w *WebApprover) Decide(id string, allow, remember bool) bool {
	return w.Resolve(id, allow, remember) == DecideOK
}

// Pending 返回等待中的审批（sessionID 为空表示全部），按时间排序。
func (w *WebApprover) Pending(sessionID string) []PendingApproval {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := []PendingApproval{}
	for _, p := range w.pending {
		if !p.decided && (sessionID == "" || p.session == sessionID) {
			out = append(out, PendingApproval{PermissionEvent: p.event, SessionID: p.session, Agent: p.agent, At: p.at})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// New 创建服务。approver 应与 app 组装时使用的同一个实例。
func New(a *app.App, approver *WebApprover) *Server {
	return &Server{
		App: a, approver: approver, Log: slog.Default(), closing: make(chan struct{}),
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
	mux.HandleFunc("GET /v1/permissions", s.pendingPermissions)
	mux.HandleFunc("POST /v1/permissions/{id}", s.permission)
	mux.HandleFunc("GET /v1/traces", s.traces)
	mux.HandleFunc("GET /v1/traces/{id}/tree", s.traceTree)
	mux.HandleFunc("GET /v1/traces/{id}/spans/{span}", s.traceSpan)
	mux.HandleFunc("GET /v1/config", s.config)
	mux.HandleFunc("POST /v1/reload", s.reload)
	mux.HandleFunc("GET /v1/events", s.events)
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
	rt := s.App.Current()
	type toolInfo struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		ReadOnly    bool   `json:"read_only"`
	}
	tools := []toolInfo{}
	for _, t := range rt.Agent.Tools.List() {
		i := t.Info()
		tools = append(tools, toolInfo{i.Name, i.Description, i.ReadOnly})
	}
	st := s.App.Status()
	writeJSON(w, 200, map[string]any{
		"version":         Version,
		"model":           rt.Agent.Model,
		"provider":        rt.Config.Provider.Type,
		"work_dir":        rt.Config.WorkDir,
		"permission_mode": rt.Agent.Permissions.Mode(),
		"tools":           tools,
		"skills":          rt.Skills,
		"subagents":       rt.Subagents,
		"mcp_servers":     s.App.MCP.Servers(),
		"warnings":        rt.Warnings,
		"config_sources":  rt.Config.Sources,
		"config_version":  rt.Version,
		"config_error":    st.LastError,
	})
}

// config 返回生效的配置（密钥已脱敏）与热更新状态。
func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	rt := s.App.Current()
	st := s.App.Status()
	writeJSON(w, 200, map[string]any{
		"effective":       rt.Config.RedactedJSON(),
		"version":         st.Version,
		"loaded_at":       st.LoadedAt,
		"last_checked_at": st.LastCheckedAt,
		"last_error":      st.LastError,
		"last_error_at":   st.LastErrorAt,
		"sources":         st.Sources,
		"watched":         st.Watched,
		"watching":        st.Watching,
		"warnings":        st.Warnings,
		"model":           rt.Agent.Model,
		"tools":           rt.Agent.Tools.Names(),
		"skills":          rt.Skills,
		"subagents":       rt.Subagents,
		"system_prompt":   rt.Agent.SystemPrompt(),
	})
}

// reload 立即重新加载配置。失败时返回 422 与错误，当前配置保持不变。
func (s *Server) reload(w http.ResponseWriter, r *http.Request) {
	err := s.App.Reload(r.Context())
	st := s.App.Status()
	body := map[string]any{"ok": err == nil, "version": st.Version, "loaded_at": st.LoadedAt, "warnings": st.Warnings}
	if err != nil {
		body["error"] = err.Error()
		writeJSON(w, http.StatusUnprocessableEntity, body)
		return
	}
	writeJSON(w, 200, body)
}

// events 以 SSE 推送服务级事件（配置热更新 config_reloaded / config_error）。
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, map[string]string{"error": "不支持流式输出"})
		return
	}
	ch, cancel := s.App.Subscribe()
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	st := s.App.Status()
	b, _ := json.Marshal(map[string]any{"version": st.Version, "last_error": st.LastError})
	fmt.Fprintf(w, "event: hello\ndata: %s\n\n", b)
	flusher.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		case <-s.done():
			return
		}
	}
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

// sessionInfo 是会话列表中的一项。
type sessionInfo struct {
	session.Info
	Running bool `json:"running"`
}

// listSessions 返回会话列表（按更新时间倒序），每项含 id、title、created_at、updated_at、messages、model、usage、running。
func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	list, err := s.App.Store.List()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	// 活跃会话以内存中的状态为准（可能比磁盘更新）
	s.mu.Lock()
	cached := make(map[string]*session.Session, len(s.sessions))
	for id, sess := range s.sessions {
		cached[id] = sess
	}
	s.mu.Unlock()
	ag := s.App.Agent()
	out := make([]sessionInfo, 0, len(list))
	seen := map[string]bool{}
	for _, info := range list {
		if sess, ok := cached[info.ID]; ok {
			info = sess.Info()
		}
		seen[info.ID] = true
		out = append(out, sessionInfo{Info: info, Running: ag.Running(info.ID)})
	}
	for id, sess := range cached { // 还没来得及保存的新会话
		if !seen[id] && len(sess.History()) > 0 {
			out = append(out, sessionInfo{Info: sess.Info(), Running: ag.Running(id)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	writeJSON(w, 200, map[string]any{"sessions": out})
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	sess := s.App.NewSession()
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.mu.Unlock()
	_ = s.App.Store.Save(sess)
	writeJSON(w, 201, map[string]any{"id": sess.ID})
}

// getSession 返回会话的完整历史：messages 中包含 text、thinking、tool_use、tool_result 全部内容块，
// 前端据此恢复聊天记录。
func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.loadSession(r.PathValue("id"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	snap := sess.Snapshot()
	if snap.Messages == nil {
		snap.Messages = []llm.Message{}
	}
	writeJSON(w, 200, map[string]any{
		"id": snap.ID, "title": snap.Title, "created_at": snap.CreatedAt, "updated_at": snap.UpdatedAt, "model": snap.Model,
		"messages": snap.Messages, "usage": snap.Usage, "summary": snap.Summary, "compactions": snap.Compactions,
		"running": s.App.Agent().Running(snap.ID), "pending_permissions": s.approverPending(snap.ID),
	})
}

func (s *Server) approverPending(sessionID string) []PendingApproval {
	if s.approver == nil {
		return []PendingApproval{}
	}
	return s.approver.Pending(sessionID)
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.App.Agent().Running(id) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "会话正在运行，请先取消"})
		return
	}
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
	if err := s.App.Store.Delete(id); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.App.ForgetSession(id)
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
	ok := s.App.Agent().Steer(r.PathValue("id"), body.Message)
	writeJSON(w, 200, map[string]any{"accepted": ok})
}

func (s *Server) compact(w http.ResponseWriter, r *http.Request) {
	sess, err := s.loadSession(r.PathValue("id"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	if s.App.Agent().Running(sess.ID) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "会话正在运行，请稍后再压缩"})
		return
	}
	if err := s.App.Compact(r.Context(), sess, nil); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	_ = s.App.Store.Save(sess)
	writeJSON(w, 200, map[string]any{"summary": sess.Summary})
}

func (s *Server) files(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"files": s.App.Agent().Files(r.PathValue("id"))})
}

func (s *Server) todos(w http.ResponseWriter, r *http.Request) {
	v, _ := s.App.Agent().Values(r.PathValue("id")).Load(builtin.TodoKey)
	writeJSON(w, 200, map[string]any{"todos": v})
}

// permission 提交审批结果：200 已送达；404 ID 不存在；409 已经处理过或已过期（重复提交是安全的）。
func (s *Server) permission(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Allow    bool `json:"allow"`
		Remember bool `json:"remember"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if s.approver == nil {
		writeJSON(w, 404, map[string]string{"error": "审批请求不存在"})
		return
	}
	switch s.approver.Resolve(r.PathValue("id"), body.Allow, body.Remember) {
	case DecideOK:
		writeJSON(w, 200, map[string]any{"ok": true})
	case DecideConflict:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该审批已经处理过或已过期"})
	default:
		writeJSON(w, 404, map[string]string{"error": "审批请求不存在"})
	}
}

// pendingPermissions 返回等待中的审批（?session_id= 过滤），页面刷新后用来恢复审批弹窗。
func (s *Server) pendingPermissions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"permissions": s.approverPending(r.URL.Query().Get("session_id"))})
}

// allSpans 返回指定 trace 的全部 Span：已结束的（内存导出器）+ 进行中的（Tracer.Active）。
func (s *Server) allSpans(traceID string) []trace.SpanData {
	done := s.App.Traces.Spans(traceID)
	seen := make(map[string]bool, len(done))
	for _, sp := range done {
		seen[sp.SpanID] = true
	}
	for _, sp := range s.App.Tracer.Active(traceID) {
		if !seen[sp.SpanID] {
			done = append(done, sp)
		}
	}
	sort.SliceStable(done, func(i, j int) bool { return done[i].Start.Before(done[j].Start) })
	return done
}

// traces 返回 Span 列表。?trace_id= 过滤 trace；?session_id= 返回该会话的全部 trace；
// ?light=1 时大属性（原始请求体、原始流、工具输出）只保留 2KB 预览，并在 attrs["trace.omitted"] 中列出。
func (s *Server) traces(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	spans := s.allSpans(q.Get("trace_id"))
	if sid := q.Get("session_id"); sid != "" {
		ids := map[string]bool{}
		for _, sp := range spans {
			if sp.Kind == trace.KindAgent && sp.ParentID == "" && sp.Attrs["session_id"] == sid {
				ids[sp.TraceID] = true
			}
		}
		out := []trace.SpanData{}
		for _, sp := range spans {
			if ids[sp.TraceID] {
				out = append(out, sp)
			}
		}
		spans = out
	}
	if spans == nil {
		spans = []trace.SpanData{}
	}
	if q.Get("light") == "1" || q.Get("light") == "true" {
		for i := range spans {
			spans[i] = spans[i].Light(16<<10, 2<<10)
		}
	}
	writeJSON(w, 200, map[string]any{"spans": spans})
}

// traceTree 以 JSON 树返回一次运行的完整 Trace（含进行中的 Span 与全部原始属性）；?format=text 返回终端风格的文本树。
func (s *Server) traceTree(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	spans := s.allSpans(id)
	if r.URL.Query().Get("format") == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(trace.Tree(spans)))
		return
	}
	if len(spans) == 0 {
		writeJSON(w, 404, map[string]string{"error": "trace 不存在或已被淘汰"})
		return
	}
	if r.URL.Query().Get("light") == "1" {
		for i := range spans {
			spans[i] = spans[i].Light(16<<10, 2<<10)
		}
	}
	writeJSON(w, 200, map[string]any{"trace_id": id, "span_count": len(spans), "roots": trace.BuildTree(spans)})
}

// traceSpan 返回单个 Span 的完整数据（实时事件中被省略的大属性从这里获取）。
func (s *Server) traceSpan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("span")
	for _, sp := range s.allSpans(r.PathValue("id")) {
		if sp.SpanID == id {
			writeJSON(w, 200, sp)
			return
		}
	}
	writeJSON(w, 404, map[string]string{"error": "span 不存在"})
}

// Shutdown 取消所有正在运行的对话：前端会收到 error、run_end 与 done 事件，随后 HTTP 服务可以优雅关闭。
func (s *Server) Shutdown() {
	s.closeOnce.Do(func() { close(s.closing) })
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.cancels))
	for _, c := range s.cancels {
		cancels = append(cancels, c)
	}
	s.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}

func (s *Server) done() <-chan struct{} { return s.closing }

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
	if s.App.Agent().Running(sess.ID) {
		writeJSON(w, 409, map[string]string{"error": "该会话正在运行，请稍候或先取消"})
		return
	}
	select {
	case <-s.closing:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "服务正在关闭"})
		return
	default:
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
	send("session", map[string]any{"session_id": sess.ID, "config_version": s.App.Status().Version})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := s.App.Run(ctx, sess, body.Message, func(ev agent.Event) {
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
					send("done", map[string]any{"session_id": sess.ID, "title": sess.Info().Title})
					return
				}
			}
		}
	}
}
