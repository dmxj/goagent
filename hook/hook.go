// Package hook 实现生命周期钩子：在 Agent 运行的关键节点执行用户自定义逻辑。
//
// 借鉴 Claude Code Hooks：钩子可以是 shell 命令（从 stdin 读取 JSON 事件），
// 也可以是 Go 函数。钩子能够：阻止工具调用、修改工具参数、给模型追加上下文、
// 在任务结束时要求 Agent 继续工作（如"测试没过不许停"）。
package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/dmxj/goagent/internal/procutil"
)

// Event 是钩子事件名。
type Event string

const (
	SessionStart     Event = "SessionStart"
	UserPromptSubmit Event = "UserPromptSubmit"
	PreToolUse       Event = "PreToolUse"
	PostToolUse      Event = "PostToolUse"
	Stop             Event = "Stop"
	SubagentStop     Event = "SubagentStop"
	PreCompact       Event = "PreCompact"
)

// Input 是传给钩子的事件数据（命令钩子通过 stdin 以 JSON 收到）。
type Input struct {
	Event      Event           `json:"hook_event_name"`
	SessionID  string          `json:"session_id"`
	WorkDir    string          `json:"cwd"`
	Prompt     string          `json:"prompt,omitempty"`
	ToolName   string          `json:"tool_name,omitempty"`
	ToolInput  json.RawMessage `json:"tool_input,omitempty"`
	ToolOutput string          `json:"tool_output,omitempty"`
	ToolError  bool            `json:"tool_error,omitempty"`
	LastReply  string          `json:"last_reply,omitempty"`
	AgentName  string          `json:"agent_name,omitempty"`
}

// Output 是钩子的返回。
type Output struct {
	// Block 为 true 时：PreToolUse 阻止调用；UserPromptSubmit 拒绝该输入；Stop 要求 Agent 继续工作。
	Block  bool   `json:"block,omitempty"`
	Reason string `json:"reason,omitempty"`
	// AdditionalContext 会作为额外信息追加给模型。
	AdditionalContext string `json:"additional_context,omitempty"`
	// UpdatedInput 用于 PreToolUse 修改工具参数。
	UpdatedInput json.RawMessage `json:"updated_input,omitempty"`
}

// Handler 是钩子处理函数。
type Handler func(ctx context.Context, in *Input) (*Output, error)

// Config 是配置文件中的一条命令钩子。
type Config struct {
	// Matcher 是工具名正则（仅对 Pre/PostToolUse 生效），空表示全部。
	Matcher string `json:"matcher,omitempty"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"` // 秒，默认 60
}

type entry struct {
	matcher *regexp.Regexp
	handler Handler
	name    string
}

// Manager 管理所有已注册的钩子。
type Manager struct {
	mu      sync.RWMutex
	entries map[Event][]entry
}

// NewManager 创建钩子管理器。
func NewManager() *Manager { return &Manager{entries: map[Event][]entry{}} }

// On 注册一个 Go 函数钩子。matcher 为工具名正则，可为空。
func (m *Manager) On(ev Event, matcher string, h Handler) error {
	var re *regexp.Regexp
	if matcher != "" && matcher != "*" {
		var err error
		if re, err = regexp.Compile("^(?:" + matcher + ")$"); err != nil {
			return fmt.Errorf("钩子 matcher 无效: %w", err)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[ev] = append(m.entries[ev], entry{matcher: re, handler: h, name: "func"})
	return nil
}

// LoadConfig 从配置注册命令钩子：map[事件名][]Config。
func (m *Manager) LoadConfig(cfg map[string][]Config, workDir string) error {
	for ev, list := range cfg {
		for _, c := range list {
			c := c
			if strings.TrimSpace(c.Command) == "" {
				continue
			}
			if err := m.On(Event(ev), c.Matcher, CommandHandler(c, workDir)); err != nil {
				return err
			}
			m.mu.Lock()
			es := m.entries[Event(ev)]
			es[len(es)-1].name = c.Command
			m.mu.Unlock()
		}
	}
	return nil
}

// Count 返回某事件已注册的钩子数。
func (m *Manager) Count(ev Event) int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries[ev])
}

// Run 依次执行匹配的钩子并合并结果：任一 Block 即阻止；上下文拼接；最后一个 UpdatedInput 生效。
func (m *Manager) Run(ctx context.Context, in *Input) (*Output, error) {
	out := &Output{}
	if m == nil {
		return out, nil
	}
	m.mu.RLock()
	list := append([]entry(nil), m.entries[in.Event]...)
	m.mu.RUnlock()
	var ctxs []string
	var errs []error
	for _, e := range list {
		if e.matcher != nil && !e.matcher.MatchString(in.ToolName) {
			continue
		}
		o, err := e.handler(ctx, in)
		if err != nil {
			errs = append(errs, fmt.Errorf("钩子 %s: %w", e.name, err))
			continue
		}
		if o == nil {
			continue
		}
		if o.Block {
			out.Block = true
			if o.Reason != "" {
				out.Reason = strings.TrimSpace(out.Reason + "\n" + o.Reason)
			}
		}
		if o.AdditionalContext != "" {
			ctxs = append(ctxs, o.AdditionalContext)
		}
		if len(o.UpdatedInput) > 0 {
			out.UpdatedInput = o.UpdatedInput
			in.ToolInput = o.UpdatedInput // 后续钩子看到更新后的参数
		}
	}
	out.AdditionalContext = strings.Join(ctxs, "\n")
	return out, errors.Join(errs...)
}

// CommandHandler 把 shell 命令包装成钩子。
//
// 协议（与 Claude Code 一致）：
//   - 事件 JSON 写入 stdin
//   - 退出码 0：stdout 若是 JSON 则解析为 Output，否则作为 AdditionalContext
//   - 退出码 2：阻止，stderr 作为原因反馈给模型
//   - 其它退出码：钩子出错，但不阻止流程
func CommandHandler(c Config, workDir string) Handler {
	return func(ctx context.Context, in *Input) (*Output, error) {
		timeout := time.Duration(c.Timeout) * time.Second
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		payload, _ := json.Marshal(in)
		cmd := procutil.Command(ctx, shellFor(), c.Command)
		cmd.Dir = workDir
		cmd.Env = append(os.Environ(), "GOAGENT_HOOK_EVENT="+string(in.Event), "GOAGENT_TOOL_NAME="+in.ToolName)
		cmd.WaitDelay = 3 * time.Second
		cmd.Stdin = bytes.NewReader(payload)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ee.ExitCode() == 2 {
				return &Output{Block: true, Reason: strings.TrimSpace(stderr.String())}, nil
			}
			return nil, fmt.Errorf("退出码 %d: %s", ee.ExitCode(), strings.TrimSpace(stderr.String()))
		}
		if err != nil {
			return nil, err
		}
		s := strings.TrimSpace(stdout.String())
		if s == "" {
			return nil, nil
		}
		if strings.HasPrefix(s, "{") {
			var o Output
			if json.Unmarshal([]byte(s), &o) == nil {
				return &o, nil
			}
		}
		return &Output{AdditionalContext: s}, nil
	}
}

// shellFor 返回执行钩子命令的 shell：Unix 上用 /bin/sh（与 Claude Code 一致），Windows 上用默认的 cmd。
func shellFor() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	return "/bin/sh"
}
