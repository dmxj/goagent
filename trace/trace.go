// Package trace 提供 Agent 运行过程的可观测性：
// 把一次运行拆成树状的 Span（agent.run → llm.call / tool.exec / subagent ...），
// 记录耗时、输入输出、token 用量、错误，并导出到 JSONL 文件或实时推送给 UI。
//
// 数据模型借鉴 OpenTelemetry：TraceID 标识一次完整运行，SpanID/ParentID 组成树。
package trace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind 是 Span 的类别。
type Kind string

const (
	KindAgent    Kind = "agent"
	KindLLM      Kind = "llm"
	KindTool     Kind = "tool"
	KindSubagent Kind = "subagent"
	KindHook     Kind = "hook"
	KindCompact  Kind = "compact"
	KindMCP      Kind = "mcp"
)

// Event 是 Span 内的时间点事件（如重试、日志）。
type Event struct {
	Name  string         `json:"name"`
	At    time.Time      `json:"at"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// Span 是一段有开始和结束的操作。
type Span struct {
	TraceID  string         `json:"trace_id"`
	SpanID   string         `json:"span_id"`
	ParentID string         `json:"parent_id,omitempty"`
	Name     string         `json:"name"`
	Kind     Kind           `json:"kind"`
	Start    time.Time      `json:"start"`
	End      time.Time      `json:"end,omitempty"`
	Duration float64        `json:"duration_ms,omitempty"`
	Status   string         `json:"status"` // running | ok | error
	Error    string         `json:"error,omitempty"`
	Attrs    map[string]any `json:"attrs,omitempty"`
	Events   []Event        `json:"events,omitempty"`

	mu     sync.Mutex
	tracer *Tracer
	ended  bool
}

// SetAttr 设置属性（并发安全）。
func (s *Span) SetAttr(k string, v any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Attrs == nil {
		s.Attrs = map[string]any{}
	}
	s.Attrs[k] = v
}

// AddEvent 记录一个事件。
func (s *Span) AddEvent(name string, attrs map[string]any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.Events = append(s.Events, Event{Name: name, At: time.Now(), Attrs: attrs})
	s.mu.Unlock()
}

// Fail 标记错误。
func (s *Span) Fail(err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	s.Status, s.Error = "error", err.Error()
	s.mu.Unlock()
}

// Finish 结束 Span 并交给导出器。重复调用无副作用。
func (s *Span) Finish() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.End = time.Now()
	s.Duration = float64(s.End.Sub(s.Start).Microseconds()) / 1000
	if s.Status == "running" {
		s.Status = "ok"
	}
	snap := s.snapshotLocked()
	s.mu.Unlock()
	s.tracer.export(snap)
}

// Snapshot 返回 Span 当前状态的拷贝（可安全序列化）。
func (s *Span) Snapshot() SpanData {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *Span) snapshotLocked() SpanData {
	attrs := make(map[string]any, len(s.Attrs))
	for k, v := range s.Attrs {
		attrs[k] = v
	}
	return SpanData{
		TraceID: s.TraceID, SpanID: s.SpanID, ParentID: s.ParentID, Name: s.Name, Kind: s.Kind,
		Start: s.Start, End: s.End, Duration: s.Duration, Status: s.Status, Error: s.Error,
		Attrs: attrs, Events: append([]Event(nil), s.Events...),
	}
}

// SpanData 是 Span 的不可变快照，用于导出与传输。
type SpanData struct {
	TraceID  string         `json:"trace_id"`
	SpanID   string         `json:"span_id"`
	ParentID string         `json:"parent_id,omitempty"`
	Name     string         `json:"name"`
	Kind     Kind           `json:"kind"`
	Start    time.Time      `json:"start"`
	End      time.Time      `json:"end,omitempty"`
	Duration float64        `json:"duration_ms,omitempty"`
	Status   string         `json:"status"`
	Error    string         `json:"error,omitempty"`
	Attrs    map[string]any `json:"attrs,omitempty"`
	Events   []Event        `json:"events,omitempty"`
}

// Exporter 接收结束的 Span。
type Exporter interface {
	Export(SpanData)
}

// ExporterFunc 让普通函数实现 Exporter。
type ExporterFunc func(SpanData)

func (f ExporterFunc) Export(s SpanData) { f(s) }

// Tracer 创建并导出 Span。零值不可用，请用 New。
type Tracer struct {
	mu        sync.RWMutex
	exporters []Exporter
	// OnStart 在 Span 开始时回调（用于实时 UI 展示"进行中"的步骤）。
	OnStart func(SpanData)
}

// New 创建 Tracer。
func New(exporters ...Exporter) *Tracer { return &Tracer{exporters: exporters} }

// AddExporter 追加导出器。
func (t *Tracer) AddExporter(e Exporter) {
	t.mu.Lock()
	t.exporters = append(t.exporters, e)
	t.mu.Unlock()
}

func (t *Tracer) export(s SpanData) {
	if t == nil {
		return
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, e := range t.exporters {
		e.Export(s)
	}
}

type ctxKey struct{}

// FromContext 取出 ctx 中当前的 Span。
func FromContext(ctx context.Context) *Span {
	s, _ := ctx.Value(ctxKey{}).(*Span)
	return s
}

// ContextWith 把 Span 放入 ctx。
func ContextWith(ctx context.Context, s *Span) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

func newID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Start 开始一个新 Span：若 ctx 中已有 Span 则作为其子节点，否则开启新 Trace。
// 返回携带新 Span 的 ctx。tracer 为 nil 时返回一个不导出的 Span，调用方无需判空。
func (t *Tracer) Start(ctx context.Context, name string, kind Kind, attrs map[string]any) (context.Context, *Span) {
	parent := FromContext(ctx)
	s := &Span{SpanID: newID(8), Name: name, Kind: kind, Start: time.Now(), Status: "running", Attrs: attrs, tracer: t}
	if parent != nil {
		s.TraceID, s.ParentID = parent.TraceID, parent.SpanID
	} else {
		s.TraceID = newID(16)
	}
	if t != nil && t.OnStart != nil {
		t.OnStart(s.Snapshot())
	}
	return ContextWith(ctx, s), s
}

// ---------- 导出器 ----------

// Memory 在内存中保存最近的 Span（环形缓冲），用于 HTTP 查询。
type Memory struct {
	mu    sync.Mutex
	spans []SpanData
	limit int
}

// NewMemory 创建最多保存 limit 个 Span 的内存导出器。
func NewMemory(limit int) *Memory {
	if limit <= 0 {
		limit = 5000
	}
	return &Memory{limit: limit}
}

func (m *Memory) Export(s SpanData) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spans = append(m.spans, s)
	if len(m.spans) > m.limit {
		m.spans = m.spans[len(m.spans)-m.limit:]
	}
}

// Spans 返回指定 trace 的 Span（traceID 为空返回全部），按开始时间排序。
func (m *Memory) Spans(traceID string) []SpanData {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SpanData
	for _, s := range m.spans {
		if traceID == "" || s.TraceID == traceID {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// File 以 JSONL（每行一个 JSON）格式写入文件，便于 grep / jq 分析。
type File struct {
	mu sync.Mutex
	f  *os.File
}

// NewFile 打开（追加模式）trace 文件。
func NewFile(path string) (*File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &File{f: f}, nil
}

func (f *File) Export(s SpanData) {
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, _ = f.f.Write(append(b, '\n'))
}

// Close 关闭文件。
func (f *File) Close() error { return f.f.Close() }

// ReadFile 读取 JSONL trace 文件。
func ReadFile(path string) ([]SpanData, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []SpanData
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var s SpanData
		if err := json.Unmarshal([]byte(line), &s); err == nil {
			out = append(out, s)
		}
	}
	return out, nil
}

// Tree 把 Span 列表渲染为缩进树，便于在终端查看。
func Tree(spans []SpanData) string {
	children := map[string][]SpanData{}
	ids := map[string]bool{}
	for _, s := range spans {
		ids[s.SpanID] = true
	}
	var roots []SpanData
	for _, s := range spans {
		if s.ParentID == "" || !ids[s.ParentID] {
			roots = append(roots, s)
		} else {
			children[s.ParentID] = append(children[s.ParentID], s)
		}
	}
	byStart := func(xs []SpanData) {
		sort.Slice(xs, func(i, j int) bool { return xs[i].Start.Before(xs[j].Start) })
	}
	byStart(roots)
	var sb strings.Builder
	var walk func(s SpanData, prefix string, last bool)
	walk = func(s SpanData, prefix string, last bool) {
		branch := "├─ "
		next := prefix + "│  "
		if last {
			branch, next = "└─ ", prefix+"   "
		}
		status := "✓"
		if s.Status == "error" {
			status = "✗ " + s.Error
		}
		extra := ""
		if v, ok := s.Attrs["output_tokens"]; ok {
			extra = fmt.Sprintf(" tokens=%v/%v", s.Attrs["input_tokens"], v)
		}
		fmt.Fprintf(&sb, "%s%s[%s] %s %.0fms%s %s\n", prefix, branch, s.Kind, s.Name, s.Duration, extra, status)
		kids := children[s.SpanID]
		byStart(kids)
		for i, k := range kids {
			walk(k, next, i == len(kids)-1)
		}
	}
	for i, r := range roots {
		walk(r, "", i == len(roots)-1)
	}
	return sb.String()
}
