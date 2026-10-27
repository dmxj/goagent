// Package permission 决定一次工具调用是否允许执行。
//
// 参考 Claude Code 的权限模型：
//   - 模式（Mode）：default 默认询问 / accept_edits 自动允许文件修改 / plan 只读规划 / bypass 全部放行
//   - 规则（Rule）：allow / deny / ask 三类，形如 "bash(go test:*)"、"write_file(docs/**)"、"web_fetch"
//   - 审批（Approver）：规则判为 ask 时，交给人（CLI 提示、Web 弹窗）决定
//
// 优先级：deny > ask > allow > 模式默认行为。
//
// bash 命令先被拆成子命令（见 shell.go）：allow 规则要求每个子命令都命中，deny / ask 规则任一子命令命中即生效。
package permission

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
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
	// SessionID 是发起调用的会话。"本会话总是允许"按会话记录，不会影响其它会话。
	SessionID string `json:"session_id,omitempty"`
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

// Match 判断规则是否"放行"请求（allow 语义）。
//
// 模式语法：
//   - "go test:*"   前缀匹配（Claude Code 风格，冒号后的 * 表示任意参数；按单词边界匹配）
//   - "src/**"      glob，** 匹配多级目录
//   - "domain:x.com" 匹配 web_fetch 的域名
//
// bash 命令会先拆成子命令：**每个**子命令都命中才算放行；含有命令替换、eval、
// 写文件的重定向等无法安全分析的结构时一律不放行（交给审批）。
func (r Rule) Match(req Request) bool { return r.match(req, false) }

// MatchAny 判断规则是否命中请求中的**任意**部分（deny / ask 语义）：
// bash 命令的任一子命令（包括 $(...)、sh -c、sudo 等内部的命令）命中即算命中。
func (r Rule) MatchAny(req Request) bool { return r.match(req, true) }

func (r Rule) toolMatches(name string) bool {
	return r.Tool == name || (strings.HasSuffix(r.Tool, "*") && strings.HasPrefix(name, strings.TrimSuffix(r.Tool, "*")))
}

func (r Rule) match(req Request, any bool) bool {
	if !r.toolMatches(req.Tool) {
		return false
	}
	if r.Pattern == "" || r.Pattern == "*" {
		return true
	}
	if req.Tool == "bash" {
		if cmd, ok := commandOf(req); ok {
			return r.matchBash(AnalyzeCommand(cmd), any)
		}
	}
	return r.matchText(subject(req), any)
}

// commandOf 取出 bash 调用的 command 参数。
func commandOf(req Request) (string, bool) {
	var m map[string]any
	if json.Unmarshal(req.Input, &m) != nil {
		return "", false
	}
	c, ok := m["command"].(string)
	return c, ok
}

// matchText 用规则模式匹配一个字符串。lenient 为 true 时前缀匹配不要求单词边界（用于 deny，宁可多拦）。
func (r Rule) matchText(sub string, lenient bool) bool {
	if strings.HasSuffix(r.Pattern, ":*") {
		prefix := strings.TrimSuffix(r.Pattern, ":*")
		if lenient {
			return strings.HasPrefix(sub, prefix)
		}
		return sub == prefix || strings.HasPrefix(sub, prefix+" ")
	}
	re, err := globRegexp(r.Pattern)
	if err != nil {
		return false
	}
	return re.MatchString(sub) || re.MatchString(strings.TrimPrefix(sub, "./"))
}

// 放行时可以安全剥掉的包装命令（只限制运行方式，不改变执行的命令）。
var transparentWrappers = map[string]bool{"time": true, "nice": true, "nohup": true, "timeout": true}

// allowText 返回子命令用于 allow 匹配的文本：剥掉 time / timeout 等透明包装；
// 带变量赋值时保留赋值（FOO=1 go test 不会被 bash(go test:*) 放行，需要显式写出赋值）。
func allowText(c SubCommand) string {
	words := c.Words
	for len(words) > 1 && transparentWrappers[words[0]] {
		words = words[1:]
		for len(words) > 1 && (strings.HasPrefix(words[0], "-") || isDurationLike(words[0])) {
			words = words[1:]
		}
	}
	return strings.TrimSpace(strings.Join(append(append([]string(nil), c.Assigns...), words...), " "))
}

func (r Rule) matchBash(a *Analysis, any bool) bool {
	if any {
		for _, c := range a.All() {
			for _, text := range denyTexts(c) {
				if r.matchText(text, true) {
					return true
				}
			}
		}
		return false
	}
	if !a.Safe() || len(a.Commands) == 0 {
		return false
	}
	for _, c := range a.Commands {
		for _, rd := range c.Redirects {
			if rd.writesFile() {
				return false // 重定向写文件是额外的副作用，不能被"运行某命令"的规则放行
			}
		}
		if !r.matchText(allowText(c), false) {
			return false
		}
	}
	return true
}

// coverBash 判断 bash 命令的每个子命令是否都被某条 allow 规则覆盖（不同子命令可以由不同规则覆盖，
// 例如 go test ./... && go vet ./... 由 bash(go test:*) 与 bash(go vet:*) 共同放行）。
func coverBash(rules []Rule, req Request) ([]string, bool) {
	if req.Tool != "bash" || len(rules) == 0 {
		return nil, false
	}
	cmd, ok := commandOf(req)
	if !ok {
		return nil, false
	}
	a := AnalyzeCommand(cmd)
	if !a.Safe() || len(a.Commands) < 2 {
		return nil, false // 单个子命令的情况 firstMatch 已经处理
	}
	var used []string
	seen := map[string]bool{}
	for _, c := range a.Commands {
		one := &Analysis{Commands: []SubCommand{c}}
		found := false
		for _, r := range rules {
			if r.toolMatches("bash") && r.matchBash(one, false) {
				if !seen[r.String()] {
					seen[r.String()] = true
					used = append(used, r.String())
				}
				found = true
				break
			}
		}
		if !found {
			return nil, false
		}
	}
	return used, true
}

// denyTexts 返回子命令用于 deny/ask 检查的多种写法：规范化文本、带赋值的文本、命令名取 basename（/bin/rm → rm）。
func denyTexts(c SubCommand) []string {
	out := []string{c.String()}
	if len(c.Assigns) > 0 {
		out = append(out, strings.Join(c.Assigns, " ")+" "+c.String())
	}
	if len(c.Words) > 0 {
		if base := filepath.Base(c.Words[0]); base != c.Words[0] {
			out = append(out, strings.Join(append([]string{base}, c.Words[1:]...), " "))
		}
	}
	return out
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

// Grants 保存"本会话总是允许"的决定，按会话隔离。
//
// v0.1.0 把它放在进程级共享的 Checker 里，goagent serve 下一个用户点了"总是允许 bash"，
// 所有会话都不再询问。现在以 SessionID 为键单独存放，并且可以在配置热更新重建 Checker 时
// 继续共享同一个 Grants，用户的决定不会因为改了配置而丢失。
//
// 对 bash，"总是允许"记住的是命令前缀而不是整个工具：批准 `go test ./...` 后，本会话内
// `go test -run X` 不再询问，但 `rm -rf build` 仍然需要确认。
type Grants struct {
	mu sync.RWMutex
	m  map[string][]Rule // sessionID -> 记住的规则
}

// NewGrants 创建空的会话授权表。
func NewGrants() *Grants { return &Grants{m: map[string][]Rule{}} }

// Allow 记录某会话允许整个工具。
func (g *Grants) Allow(sessionID, tool string) { g.add(sessionID, Rule{Tool: tool, raw: tool}) }

func (g *Grants) add(sessionID string, r Rule) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, old := range g.m[sessionID] {
		if old.raw == r.raw {
			return
		}
	}
	g.m[sessionID] = append(g.m[sessionID], r)
}

// Remember 记住一次被批准的调用：bash 记住每个子命令的前缀规则，其它工具记住工具名。
// 返回记住的规则；命令无法安全分析时什么也不记（只放行这一次）。
func (g *Grants) Remember(req Request) []string {
	rules := RememberRules(req)
	for _, r := range rules {
		g.add(req.SessionID, r)
	}
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = r.String()
	}
	return out
}

// RememberRules 计算"总是允许"应当记住的规则。
func RememberRules(req Request) []Rule {
	if req.Tool != "bash" {
		return []Rule{{Tool: req.Tool, raw: req.Tool}}
	}
	cmd, ok := commandOf(req)
	if !ok {
		return nil
	}
	a := AnalyzeCommand(cmd)
	if !a.Safe() {
		return nil
	}
	var out []Rule
	seen := map[string]bool{}
	for _, c := range a.Commands {
		words := strings.Fields(allowText(c))
		if len(words) == 0 {
			continue
		}
		prefix := words[0]
		if len(words) > 1 && isSubcommandWord(words[1]) {
			prefix += " " + words[1]
		}
		raw := "bash(" + prefix + ":*)"
		if !seen[raw] {
			seen[raw] = true
			out = append(out, Rule{Tool: "bash", Pattern: prefix + ":*", raw: raw})
		}
	}
	return out
}

// isSubcommandWord 判断单词像不像子命令（go test、git status 中的 test、status）。
func isSubcommandWord(w string) bool {
	if w == "" || len(w) > 32 {
		return false
	}
	for i, r := range w {
		if r >= 'a' && r <= 'z' || i > 0 && (r >= '0' && r <= '9' || r == '-' || r == '_') {
			continue
		}
		return false
	}
	return true
}

// Allowed 报告某会话是否已经允许这次调用。
func (g *Grants) Allowed(req Request) bool {
	if g == nil {
		return false
	}
	g.mu.RLock()
	rules := append([]Rule(nil), g.m[req.SessionID]...)
	g.mu.RUnlock()
	if len(rules) == 0 {
		return false
	}
	if _, ok := firstMatch(rules, req, false); ok {
		return true
	}
	_, ok := coverBash(rules, req)
	return ok
}

// Clear 清除某会话的全部授权（会话删除时调用）。
func (g *Grants) Clear(sessionID string) {
	g.mu.Lock()
	delete(g.m, sessionID)
	g.mu.Unlock()
}

// Rules 返回某会话记住的规则（排序）。
func (g *Grants) Rules(sessionID string) []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var out []string
	for _, r := range g.m[sessionID] {
		out = append(out, r.String())
	}
	sort.Strings(out)
	return out
}

// Checker 执行权限判定。并发安全。
type Checker struct {
	mu       sync.RWMutex
	mode     Mode
	allow    []Rule
	deny     []Rule
	ask      []Rule
	grants   *Grants
	Approver Approver
}

// NewChecker 根据配置创建 Checker。
func NewChecker(cfg Config, approver Approver) (*Checker, error) {
	c := &Checker{mode: cfg.Mode, grants: NewGrants(), Approver: approver}
	if c.mode == "" {
		c.mode = ModeDefault
	}
	switch c.mode {
	case ModeDefault, ModeAcceptEdits, ModePlan, ModeBypass:
	default:
		return nil, fmt.Errorf("未知的权限模式 %q（可选 default / accept_edits / plan / bypass）", c.mode)
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

// UseGrants 让多个 Checker（例如配置热更新前后的两个版本）共享同一份会话授权。
func (c *Checker) UseGrants(g *Grants) {
	if g == nil {
		return
	}
	c.mu.Lock()
	c.grants = g
	c.mu.Unlock()
}

// Grants 返回会话授权表。
func (c *Checker) Grants() *Grants {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.grants
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

func firstMatch(rules []Rule, req Request, any bool) (Rule, bool) {
	for _, r := range rules {
		if r.match(req, any) {
			return r, true
		}
	}
	return Rule{}, false
}

var editTools = map[string]bool{"write_file": true, "edit_file": true, "todo_write": true}

// Evaluate 只做静态判定，不触发审批。返回判定结果与原因。
//
// 优先级：deny（任一部分命中）> plan 模式 > ask（任一部分命中）> allow（全部命中）> 本会话授权 > 模式默认。
// 对 bash 命令，无法安全分析的结构（命令替换、eval……）在非 bypass 模式下至少需要询问。
func (c *Checker) Evaluate(req Request) (Decision, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if r, ok := firstMatch(c.deny, req, true); ok {
		return Deny, "命中拒绝规则 " + r.String()
	}
	if c.mode == ModePlan && !req.ReadOnly {
		return Deny, "当前为 plan（只读规划）模式，不允许执行有副作用的工具；请先给出计划"
	}
	if r, ok := firstMatch(c.ask, req, true); ok && c.mode != ModeBypass {
		return Ask, "命中询问规则 " + r.String()
	}
	if r, ok := firstMatch(c.allow, req, false); ok {
		return Allow, "命中允许规则 " + r.String()
	}
	if rules, ok := coverBash(c.allow, req); ok {
		return Allow, "每个子命令都命中允许规则 " + strings.Join(rules, "、")
	}
	if c.grants.Allowed(req) {
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
	if req.Tool == "bash" {
		if cmd, ok := commandOf(req); ok {
			if a := AnalyzeCommand(cmd); !a.Safe() {
				return Ask, "命令包含无法自动分析的结构（" + strings.Join(a.Unsafe, "、") + "），需要确认"
			}
		}
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
		c.Grants().Remember(req)
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
