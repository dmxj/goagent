// Package tool 定义工具（Tool）抽象与注册表。
//
// 工具是 Agent 与外部世界交互的唯一方式：读写文件、执行命令、联网……
// 模型只能"提议"调用工具，真正的执行、权限检查、超时、输出截断都由框架负责。
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dmxj/goagent/llm"
)

// Info 是工具的元信息。
type Info struct {
	Name        string
	Description string
	// Schema 是参数的 JSON Schema（object 类型）。
	Schema json.RawMessage
	// ReadOnly 表示工具不会修改任何外部状态（plan 模式下仍可用、默认无需审批）。
	ReadOnly bool
	// Concurrent 表示可以与其它 Concurrent 工具并行执行。
	Concurrent bool
}

// Spec 转换为发给模型的工具描述。
func (i Info) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: i.Name, Description: i.Description, InputSchema: i.Schema}
}

// Env 是工具运行时可访问的环境。
type Env struct {
	// WorkDir 是工作目录，所有相对路径都基于它解析。
	WorkDir string
	// SessionID 是当前会话 ID。
	SessionID string
	// Files 记录本会话读写过的文件（用于"先读后写"检查与调试面板）。
	Files *FileTracker
	// Log 向事件流输出一条日志（可为 nil）。
	Log func(format string, args ...any)
	// Values 用于在工具之间共享会话级状态（例如 todo 列表）。
	Values *sync.Map
}

// Logf 安全地输出日志。
func (e *Env) Logf(format string, args ...any) {
	if e != nil && e.Log != nil {
		e.Log(format, args...)
	}
}

// Call 是一次工具调用。
type Call struct {
	ID    string
	Name  string
	Input json.RawMessage
	Env   *Env
}

// Result 是工具执行结果。
type Result struct {
	// Content 是返回给模型的文本。
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
	// Meta 是给 UI/trace 使用的结构化信息，不会发给模型。
	Meta map[string]any `json:"meta,omitempty"`
}

// Text 构造成功结果。
func Text(s string) *Result { return &Result{Content: s} }

// Errorf 构造错误结果（工具层面的失败，模型可以看到并自行修正）。
func Errorf(format string, args ...any) *Result {
	return &Result{Content: fmt.Sprintf(format, args...), IsError: true}
}

// Tool 是所有工具需要实现的接口。
type Tool interface {
	Info() Info
	Run(ctx context.Context, call *Call) (*Result, error)
}

// Registry 是线程安全的工具注册表。
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry 创建注册表并注册给定工具。
func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{tools: map[string]Tool{}}
	for _, t := range tools {
		r.Register(t)
	}
	return r
}

// Register 注册（或覆盖）一个工具。
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Info().Name] = t
}

// Unregister 移除工具。
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
}

// Get 按名称查找工具。nil 注册表视为空。
func (r *Registry) Get(name string) (Tool, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// List 返回按名称排序的所有工具。nil 注册表视为空（Names、Specs、Filter 都基于它）。
func (r *Registry) List() []Tool {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Info().Name < out[j].Info().Name })
	return out
}

// Names 返回所有工具名。
func (r *Registry) Names() []string {
	var names []string
	for _, t := range r.List() {
		names = append(names, t.Info().Name)
	}
	return names
}

// Specs 返回发给模型的工具描述列表。
func (r *Registry) Specs() []llm.ToolSpec {
	var specs []llm.ToolSpec
	for _, t := range r.List() {
		specs = append(specs, t.Info().Spec())
	}
	return specs
}

// Filter 返回只包含指定工具的新注册表；names 为空或包含 "*" 时复制全部。
// 名称支持前缀通配，例如 "mcp__github__*"。
func (r *Registry) Filter(names []string, exclude ...string) *Registry {
	out := NewRegistry()
	ex := map[string]bool{}
	for _, n := range exclude {
		ex[n] = true
	}
	for _, t := range r.List() {
		name := t.Info().Name
		if ex[name] {
			continue
		}
		if len(names) == 0 || matchAny(names, name) {
			out.Register(t)
		}
	}
	return out
}

func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "*" || p == name {
			return true
		}
		if strings.HasSuffix(p, "*") && strings.HasPrefix(name, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}

// InputReadOnly 由"是否只读取决于参数"的工具实现。
//
// 例如 task 工具：委派给只读的 explore 子 Agent 时整个调用都不会修改环境，
// plan 模式应当允许；委派给 general-purpose 时则不是只读的。
type InputReadOnly interface {
	ReadOnlyFor(input json.RawMessage) bool
}

// IsReadOnly 判断某次调用是否只读：优先询问工具的 ReadOnlyFor，否则使用 Info().ReadOnly。
func IsReadOnly(t Tool, input json.RawMessage) bool {
	if ro, ok := t.(InputReadOnly); ok {
		return ro.ReadOnlyFor(input)
	}
	return t.Info().ReadOnly
}

// Options 是通过配置覆盖的单个工具选项（对应配置 tools.options.<工具名>）。
type Options struct {
	// Description 非空时替换发给模型的工具描述。
	Description string `json:"description,omitempty"`
	// TimeoutSec > 0 时为该工具单独设置超时（秒）。
	TimeoutSec int `json:"timeout_sec,omitempty"`
	// MaxOutput > 0 时把该工具的输出截断到这么多字节。
	MaxOutput int `json:"max_output,omitempty"`
	// Concurrent 非 nil 时覆盖是否允许并行执行。
	Concurrent *bool `json:"concurrent,omitempty"`
}

// IsZero 报告是否没有任何覆盖。
func (o Options) IsZero() bool {
	return o.Description == "" && o.TimeoutSec <= 0 && o.MaxOutput <= 0 && o.Concurrent == nil
}

// Configure 用配置选项包装一个工具；没有任何覆盖时原样返回。
func Configure(t Tool, o Options) Tool {
	if o.IsZero() {
		return t
	}
	return &configured{inner: t, opts: o}
}

type configured struct {
	inner Tool
	opts  Options
}

func (c *configured) Info() Info {
	info := c.inner.Info()
	if c.opts.Description != "" {
		info.Description = c.opts.Description
	}
	if c.opts.Concurrent != nil {
		info.Concurrent = *c.opts.Concurrent
	}
	return info
}

func (c *configured) ReadOnlyFor(input json.RawMessage) bool { return IsReadOnly(c.inner, input) }

func (c *configured) Run(ctx context.Context, call *Call) (*Result, error) {
	if c.opts.TimeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(c.opts.TimeoutSec)*time.Second)
		defer cancel()
	}
	r, err := c.inner.Run(ctx, call)
	if r != nil && c.opts.MaxOutput > 0 {
		r.Content = Truncate(r.Content, c.opts.MaxOutput)
	}
	return r, err
}

// Unwrap 返回被包装的工具。
func (c *configured) Unwrap() Tool { return c.inner }
