// Package permission 决定一次工具调用是否允许执行。
//
// 参考 Claude Code 的权限模型：
//   - 模式（Mode）：default 默认询问 / accept_edits 自动允许文件修改 / plan 只读规划 / bypass 全部放行
//   - 规则（Rule）：allow / deny / ask 三类，形如 "bash(go test:*)"、"write_file(docs/**)"、"web_fetch"
//   - 审批（Approver）：规则判为 ask 时，交给人（CLI 提示、Web 弹窗）决定
//
// 优先级：deny > ask > allow > 模式默认行为。
package permission

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// Mode 是权限模式。
type Mode string

const (
	ModeDefault     Mode = "default"
	ModeAcceptEdits Mode = "accept_edits"
	ModePlan        Mode = "plan"
	ModeBypass      Mode = "bypass"
)

// Decision 是权限判定结果。
type Decision string

const (
	Allow Decision = "allow"
	Deny  Decision = "deny"
	Ask   Decision = "ask"
)

// Request 是一次待判定的工具调用。
type Request struct {
	Tool     string          `json:"tool"`
	Input    json.RawMessage `json:"input"`
	ReadOnly bool            `json:"read_only"`
	CallID   string          `json:"call_id"`
}

// Rule 是一条权限规则。
type Rule struct {
	Tool    string // 工具名，支持前缀通配 "mcp__*"
	Pattern string // 参数匹配模式，空表示匹配所有调用
	raw     string
}

func (r Rule) String() string { return r.raw }

// ParseRule 解析 "tool" 或 "tool(pattern)" 形式的规则。
func ParseRule(s string) (Rule, error) {
	s = strings.TrimSpace(s)
	r := Rule{raw: s}
	if i := strings.IndexByte(s, '('); i >= 0 {
		if !strings.HasSuffix(s, ")") {
			return r, fmt.Errorf("规则 %q 缺少右括号", s)
		}
		r.Tool, r.Pattern = s[:i], s[i+1:len(s)-1]
	} else {
		r.Tool = s
	}
	if r.Tool == "" {
		return r, fmt.Errorf("规则 %q 缺少工具名", s)
	}
	return r, nil
}

// subject 提取用于规则匹配的"主体"：bash 用命令，文件工具用路径，web_fetch 用域名。
func subject(req Request) string {
	var m map[string]any
	_ = json.Unmarshal(req.Input, &m)
	for _, k := range []string{"command", "path", "file_path", "url", "pattern"} {
		if v, ok := m[k].(string); ok {
			if k == "url" {
				if u := strings.SplitN(strings.TrimPrefix(strings.TrimPrefix(v, "https://"), "http://"), "/", 2); len(u) > 0 {
					return "domain:" + u[0]
				}
			}
			return v
		}
	}
	return string(req.Input)
}

// Match 判断规则是否命中请求。
//
// 模式语法：
//   - "go test:*"   前缀匹配（Claude Code 风格，冒号后的 * 表示任意后缀）
//   - "src/**"      glob，** 匹配多级目录
//   - "domain:x.com" 匹配 web_fetch 的域名
func (r Rule) Match(req Request) bool {
	if !(r.Tool == req.Tool || (strings.HasSuffix(r.Tool, "*") && strings.HasPrefix(req.Tool, strings.TrimSuffix(r.Tool, "*")))) {
		return false
	}
	if r.Pattern == "" || r.Pattern == "*" {
		return true
	}
	sub := subject(req)
	if strings.HasSuffix(r.Pattern, ":*") {
		return strings.HasPrefix(sub, strings.TrimSuffix(r.Pattern, ":*"))
	}
	re, err := globRegexp(r.Pattern)
	if err != nil {
		return false
	}
	return re.MatchString(sub) || re.MatchString(strings.TrimPrefix(sub, "./"))
}

var reCache sync.Map

func globRegexp(p string) (*regexp.Regexp, error) {
	if v, ok := reCache.Load(p); ok {
		return v.(*regexp.Regexp), nil
	}
	var sb strings.Builder
	sb.WriteString("^")
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				sb.WriteString(".*")
				i++
			} else {
				sb.WriteString("[^/]*")
			}
		case '?':
			sb.WriteString(".")
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	sb.WriteString("$")
	re, err := regexp.Compile(sb.String())
	if err == nil {
		reCache.Store(p, re)
	}
	return re, err
}

// Approver 在需要人工确认时被调用。返回 remember=true 表示"本会话内不再询问同类调用"。
type Approver interface {
	Approve(ctx context.Context, req Request, reason string) (allow bool, remember bool, err error)
}

// ApproverFunc 让函数实现 Approver。
type ApproverFunc func(ctx context.Context, req Request, reason string) (bool, bool, error)

func (f ApproverFunc) Approve(ctx context.Context, req Request, reason string) (bool, bool, error) {
	return f(ctx, req, reason)
}

// AutoApprove 自动同意所有请求（适合无人值守 + 沙箱环境）。
var AutoApprove = ApproverFunc(func(context.Context, Request, string) (bool, bool, error) { return true, false, nil })

// AutoDeny 自动拒绝所有需要确认的请求（适合非交互的 CI 场景）。
var AutoDeny = ApproverFunc(func(context.Context, Request, string) (bool, bool, error) { return false, false, nil })

// Config 是权限配置（可从 JSON 配置文件加载）。
type Config struct {
	Mode  Mode     `json:"mode,omitempty"`
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
	Ask   []string `json:"ask,omitempty"`
}

// Checker 执行权限判定。并发安全。
type Checker struct {
	mu       sync.RWMutex
	mode     Mode
	allow    []Rule
	deny     []Rule
	ask      []Rule
	session  map[string]bool // 本会话记住的允许项（tool 名）
	Approver Approver
}

// NewChecker 根据配置创建 Checker。
func NewChecker(cfg Config, approver Approver) (*Checker, error) {
	c := &Checker{mode: cfg.Mode, session: map[string]bool{}, Approver: approver}
	if c.mode == "" {
		c.mode = ModeDefault
	}
	parse := func(ss []string) ([]Rule, error) {
		var out []Rule
		for _, s := range ss {
			r, err := ParseRule(s)
			if err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		return out, nil
	}
	var err error
	if c.allow, err = parse(cfg.Allow); err != nil {
		return nil, err
	}
	if c.deny, err = parse(cfg.Deny); err != nil {
		return nil, err
	}
	if c.ask, err = parse(cfg.Ask); err != nil {
		return nil, err
	}
	return c, nil
}

// Mode 返回当前模式。
func (c *Checker) Mode() Mode {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.mode
}

// SetMode 切换模式（例如从 plan 切到 default 开始执行）。
func (c *Checker) SetMode(m Mode) {
	c.mu.Lock()
	c.mode = m
	c.mu.Unlock()
}

func firstMatch(rules []Rule, req Request) (Rule, bool) {
	for _, r := range rules {
		if r.Match(req) {
			return r, true
		}
	}
	return Rule{}, false
}

var editTools = map[string]bool{"write_file": true, "edit_file": true, "todo_write": true}

// Evaluate 只做静态判定，不触发审批。返回判定结果与原因。
func (c *Checker) Evaluate(req Request) (Decision, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if r, ok := firstMatch(c.deny, req); ok {
		return Deny, "命中拒绝规则 " + r.String()
	}
	if c.mode == ModePlan && !req.ReadOnly {
		return Deny, "当前为 plan（只读规划）模式，不允许执行有副作用的工具；请先给出计划"
	}
	if r, ok := firstMatch(c.ask, req); ok && c.mode != ModeBypass {
		return Ask, "命中询问规则 " + r.String()
	}
	if r, ok := firstMatch(c.allow, req); ok {
		return Allow, "命中允许规则 " + r.String()
	}
	if c.session[req.Tool] {
		return Allow, "本会话已允许"
	}
	switch {
	case c.mode == ModeBypass:
		return Allow, "bypass 模式"
	case req.ReadOnly:
		return Allow, "只读工具"
	case c.mode == ModeAcceptEdits && editTools[req.Tool]:
		return Allow, "accept_edits 模式自动允许文件修改"
	}
	return Ask, fmt.Sprintf("工具 %s 会修改环境，需要确认", req.Tool)
}

// Check 完整判定：静态规则 + 必要时人工审批。返回 nil 表示允许。
func (c *Checker) Check(ctx context.Context, req Request) error {
	d, reason := c.Evaluate(req)
	switch d {
	case Allow:
		return nil
	case Deny:
		return &DeniedError{Tool: req.Tool, Reason: reason}
	}
	if c.Approver == nil {
		return &DeniedError{Tool: req.Tool, Reason: reason + "（没有配置审批者）"}
	}
	ok, remember, err := c.Approver.Approve(ctx, req, reason)
	if err != nil {
		return err
	}
	if !ok {
		return &DeniedError{Tool: req.Tool, Reason: "用户拒绝了此次调用"}
	}
	if remember {
		c.mu.Lock()
		c.session[req.Tool] = true
		c.mu.Unlock()
	}
	return nil
}

// DeniedError 表示调用被拒绝。
type DeniedError struct {
	Tool   string
	Reason string
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("调用 %s 被拒绝：%s", e.Tool, e.Reason)
}
