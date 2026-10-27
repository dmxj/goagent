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
	"regexp"
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
	End      time.Time      `json:"end,omitzero"`
	Duration float64        `json:"duration_ms,omitempty"`
	Status   string         `json:"status"` // running | ok | error
	Error    string         `json:"error,omitempty"`
	Attrs    map[string]any `json:"attrs,omitempty"`
	Events   []Event        `json:"events,omitempty"`

	mu     sync.Mutex
	tracer *Tracer
	ended  bool
}

// SetAttr 设置属性（并发安全）。字符串值会经过 Tracer 的密钥脱敏与大小上限处理。
func (s *Span) SetAttr(k string, v any) {
	if s == nil {
		return
	}
	v = s.tracer.sanitizeValue(v)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Attrs == nil {
		s.Attrs = map[string]any{}
	}
	s.Attrs[k] = v
}

// SetText 设置一个可能很大的文本属性（原始请求体、原始流、工具输出……）：
// 先脱敏，再按 Tracer.MaxAttrBytes 截断并追加截断标记，被截断的键记录在 "trace.truncated" 属性中。
func (s *Span) SetText(k, text string) {
	if s == nil {
		return
	}
	text = s.tracer.redact(text)
	max := s.tracer.maxAttrBytes()
	truncated := len(text) > max
	if truncated {
		text = TruncateText(text, max)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Attrs == nil {
		s.Attrs = map[string]any{}
	}
	s.Attrs[k] = text
	if truncated {
		list, _ := s.Attrs[AttrTruncated].([]string)
		s.Attrs[AttrTruncated] = append(append([]string(nil), list...), k)
	}
}

// SetJSON 把 v 序列化为 JSON 后作为结构化属性保存（脱敏后仍是合法 JSON）；
// 超过大小上限时退化为截断后的字符串。
func (s *Span) SetJSON(k string, v any) {
	if s == nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		s.SetAttr(k, fmt.Sprintf("%v", v))
		return
	}
	text := s.tracer.redact(string(b))
	if len(text) > s.tracer.maxAttrBytes() || !json.Valid([]byte(text)) {
		s.SetText(k, text)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Attrs == nil {
		s.Attrs = map[string]any{}
	}
	s.Attrs[k] = json.RawMessage(text)
}

// AttrTruncated 记录被截断的属性名。
const AttrTruncated = "trace.truncated"

// AttrOmitted 记录在轻量快照（实时事件）中被省略的大属性名，完整内容需通过 HTTP 接口获取。
const AttrOmitted = "trace.omitted"

// TruncateText 把 s 截断到约 max 字节（不切开 UTF-8 字符），并追加截断标记。
func TruncateText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n…[已截断：保留前 %d 字节，共 %d 字节]", cut, len(s))
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
	if s.tracer != nil {
		s.tracer.active.Delete(s.SpanID)
	}
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
	TraceID  string    `json:"trace_id"`
	SpanID   string    `json:"span_id"`
	ParentID string    `json:"parent_id,omitempty"`
	Name     string    `json:"name"`
	Kind     Kind      `json:"kind"`
	Start    time.Time `json:"start"`
	// End 使用 omitzero（Go 1.24）：进行中的 Span 不再输出 "0001-01-01T00:00:00Z"。
	End      time.Time      `json:"end,omitzero"`
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

	// 以下字段通过 SetLimits / SetSecrets 修改，可在配置热更新时调整
	maxBytes int
	secrets  []string
	active   sync.Map // spanID -> *Span（尚未结束的 Span）
}

// DefaultMaxAttrBytes 是单个大属性（原始请求体、原始流、工具输出）的默认上限。
const DefaultMaxAttrBytes = 4 << 20

// New 创建 Tracer。
func New(exporters ...Exporter) *Tracer { return &Tracer{exporters: exporters} }

// SetMaxAttrBytes 设置单个大属性的上限（<=0 表示默认 4MB）。
func (t *Tracer) SetMaxAttrBytes(n int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.maxBytes = n
	t.mu.Unlock()
}

func (t *Tracer) maxAttrBytes() int {
	if t == nil {
		return DefaultMaxAttrBytes
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.maxBytes <= 0 {
		return DefaultMaxAttrBytes
	}
	return t.maxBytes
}

// MaxAttrBytes 返回当前生效的大属性上限。
func (t *Tracer) MaxAttrBytes() int { return t.maxAttrBytes() }

// SetSecrets 设置需要从 Trace 中抹掉的密钥原文（例如配置中的 API Key）。
// 除此之外，常见密钥格式（sk-…、ghp_…、AKIA… 等）也会被自动脱敏。
func (t *Tracer) SetSecrets(secrets ...string) {
	if t == nil {
		return
	}
	var list []string
	for _, s := range secrets {
		if len(s) >= 8 { // 太短的值替换会误伤正常文本
			list = append(list, s)
		}
	}
	t.mu.Lock()
	t.secrets = list
	t.mu.Unlock()
}

// secretRe 匹配常见的密钥格式。
var secretRe = regexp.MustCompile(`\b(sk-(?:ant-)?[A-Za-z0-9_\-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|AKIA[0-9A-Z]{16}|xox[baprs]-[A-Za-z0-9-]{10,}|AIza[0-9A-Za-z_\-]{30,})`)

// Redact 对文本做密钥脱敏（导出给需要自行脱敏的调用方使用）。
func (t *Tracer) Redact(s string) string { return t.redact(s) }

func (t *Tracer) redact(s string) string {
	if s == "" {
		return s
	}
	if t != nil {
		t.mu.RLock()
		secrets := t.secrets
		t.mu.RUnlock()
		for _, sec := range secrets {
			if strings.Contains(s, sec) {
				s = strings.ReplaceAll(s, sec, "[REDACTED]")
			}
		}
	}
	return secretRe.ReplaceAllString(s, "[REDACTED]")
}

// sanitizeValue 对字符串（含 map / 切片中的字符串）脱敏并限制大小。
func (t *Tracer) sanitizeValue(v any) any {
	switch x := v.(type) {
	case string:
		x = t.redact(x)
		if max := t.maxAttrBytes(); len(x) > max {
			x = TruncateText(x, max)
		}
		return x
	case json.RawMessage:
		r := t.redact(string(x))
		if json.Valid([]byte(r)) && len(r) <= t.maxAttrBytes() {
			return json.RawMessage(r)
		}
		return TruncateText(r, t.maxAttrBytes())
	case []string:
		out := make([]string, len(x))
		for i, s := range x {
			out[i] = t.redact(s)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[k] = t.sanitizeValue(vv)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = t.sanitizeValue(vv)
		}
		return out
	}
	return v
}

// Active 返回指定 trace（traceID 为空表示全部）中尚未结束的 Span 的快照。
func (t *Tracer) Active(traceID string) []SpanData {
	if t == nil {
		return nil
	}
	var out []SpanData
	t.active.Range(func(_, v any) bool {
		s := v.(*Span)
		if traceID == "" || s.TraceID == traceID {
			out = append(out, s.Snapshot())
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

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
	s := &Span{SpanID: newID(8), Name: name, Kind: kind, Start: time.Now(), Status: "running", tracer: t}
	if len(attrs) > 0 {
		s.Attrs = make(map[string]any, len(attrs))
		for k, v := range attrs {
			s.Attrs[k] = t.sanitizeValue(v)
		}
	}
	if parent != nil {
		s.TraceID, s.ParentID = parent.TraceID, parent.SpanID
	} else {
		s.TraceID = newID(16)
	}
	if t != nil {
		t.active.Store(s.SpanID, s)
		if t.OnStart != nil {
			t.OnStart(s.Snapshot())
		}
	}
	return ContextWith(ctx, s), s
}

// ---------- 导出器 ----------

// Memory 在内存中保存最近的 Span（环形缓冲），用于 HTTP 查询。
// 除了条数上限，还有字节上限（默认 256MB）：Span 里可能带着几 MB 的原始请求体，只按条数限制会撑爆内存。
type Memory struct {
	mu    sync.Mutex
	spans []SpanData
	sizes []int
	total int
	limit int
	// MaxBytes 是保存的属性总字节数上限，<=0 表示 256MB。
	MaxBytes int
}

// NewMemory 创建最多保存 limit 个 Span 的内存导出器。
func NewMemory(limit int) *Memory {
	if limit <= 0 {
		limit = 5000
	}
	return &Memory{limit: limit}
}

// approxSize 粗略估算 Span 的内存占用（字符串属性的字节数之和）。
func approxSize(s SpanData) int {
	n := 256
	for k, v := range s.Attrs {
		n += len(k)
		switch x := v.(type) {
		case string:
			n += len(x)
		case json.RawMessage:
			n += len(x)
		default:
			n += 64
		}
	}
	return n
}

func (m *Memory) Export(s SpanData) {
	size := approxSize(s)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spans = append(m.spans, s)
	m.sizes = append(m.sizes, size)
	m.total += size
	max := m.MaxBytes
	if max <= 0 {
		max = 256 << 20
	}
	drop := 0
	for (len(m.spans)-drop > m.limit || m.total > max) && drop < len(m.spans)-1 {
		m.total -= m.sizes[drop]
		drop++
	}
	if drop > 0 {
		m.spans = append([]SpanData(nil), m.spans[drop:]...)
		m.sizes = append([]int(nil), m.sizes[drop:]...)
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

// Span 按 ID 查找一个已结束的 Span。
func (m *Memory) Span(spanID string) (SpanData, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.spans) - 1; i >= 0; i-- {
		if m.spans[i].SpanID == spanID {
			return m.spans[i], true
		}
	}
	return SpanData{}, false
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

// DailyFile 按天滚动的 JSONL 导出器：每次导出时检查日期，跨天自动切换到新文件
// （v0.1.0 的文件名在启动时确定，长期运行的服务跨天后仍写昨天的文件）。
type DailyFile struct {
	Dir string
	// Now 用于测试注入时间。
	Now func() time.Time

	mu      sync.Mutex
	day     string
	f       *os.File
	enabled bool
}

// NewDailyFile 创建按天滚动的导出器，文件名形如 <dir>/20261027.jsonl。
func NewDailyFile(dir string) (*DailyFile, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &DailyFile{Dir: dir, enabled: true}, nil
}

// SetEnabled 开关导出（配置热更新 trace.enabled 时使用）。
func (d *DailyFile) SetEnabled(on bool) {
	d.mu.Lock()
	d.enabled = on
	d.mu.Unlock()
}

// Path 返回当前（或今天）的文件路径。
func (d *DailyFile) Path() string {
	return filepath.Join(d.Dir, d.now().Format("20060102")+".jsonl")
}

func (d *DailyFile) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *DailyFile) Export(s SpanData) {
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.enabled {
		return
	}
	day := d.now().Format("20060102")
	if d.f == nil || day != d.day {
		if d.f != nil {
			_ = d.f.Close()
			d.f = nil
		}
		f, err := os.OpenFile(filepath.Join(d.Dir, day+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		d.f, d.day = f, day
	}
	_, _ = d.f.Write(append(b, '\n'))
}

// Close 关闭当前文件。
func (d *DailyFile) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f == nil {
		return nil
	}
	err := d.f.Close()
	d.f = nil
	return err
}

// Light 返回适合放进实时事件的轻量快照：超过 max 字节的大属性只保留前 preview 字节，
// 并在 "trace.omitted" 中列出被省略的键（完整内容通过 /v1/traces 接口获取）。
func (s SpanData) Light(max, preview int) SpanData {
	var omitted []string
	attrs := make(map[string]any, len(s.Attrs))
	for k, v := range s.Attrs {
		switch x := v.(type) {
		case string:
			if len(x) > max {
				v = TruncateText(x, preview)
				omitted = append(omitted, k)
			}
		case json.RawMessage:
			if len(x) > max {
				v = TruncateText(string(x), preview)
				omitted = append(omitted, k)
			}
		}
		attrs[k] = v
	}
	if len(omitted) > 0 {
		sort.Strings(omitted)
		attrs[AttrOmitted] = omitted
	}
	s.Attrs = attrs
	return s
}

// Node 是 Span 树中的一个节点，JSON 中 SpanData 的字段平铺，子节点在 children 中。
type Node struct {
	SpanData
	Children []*Node `json:"children"`
}

// BuildTree 把 Span 列表组装成树（找不到父节点的 Span 作为根），兄弟节点按开始时间排序。
func BuildTree(spans []SpanData) []*Node {
	nodes := make(map[string]*Node, len(spans))
	for _, s := range spans {
		if _, dup := nodes[s.SpanID]; dup {
			continue
		}
		nodes[s.SpanID] = &Node{SpanData: s, Children: []*Node{}}
	}
	roots := []*Node{}
	placed := make(map[string]bool, len(spans))
	for _, s := range spans {
		if placed[s.SpanID] {
			continue // 同一个 Span 出现两次（例如进行中快照与结束快照）只保留第一次
		}
		placed[s.SpanID] = true
		n := nodes[s.SpanID]
		if p, ok := nodes[s.ParentID]; ok && s.ParentID != "" && p != n {
			p.Children = append(p.Children, n)
		} else {
			roots = append(roots, n)
		}
	}
	var sortRec func([]*Node)
	sortRec = func(ns []*Node) {
		sort.SliceStable(ns, func(i, j int) bool { return ns[i].Start.Before(ns[j].Start) })
		for _, n := range ns {
			sortRec(n.Children)
		}
	}
	sortRec(roots)
	return roots
}

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
