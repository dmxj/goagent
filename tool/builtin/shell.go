package builtin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dmxj/goagent/internal/procutil"
	"github.com/dmxj/goagent/tool"
)

// ---------- bash ----------

type bashArgs struct {
	Command     string `json:"command" desc:"要执行的 shell 命令"`
	Timeout     int    `json:"timeout,omitempty" desc:"超时秒数，默认 120，最大 600"`
	Description string `json:"description,omitempty" desc:"用 5~10 个字说明这条命令的目的"`
}

type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf.Len() > 4*1024*1024 {
		return len(p), nil // 防止内存被撑爆
	}
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// Bash 在工作目录中执行 shell 命令，合并 stdout/stderr，超时会杀掉整个进程组。
func Bash(opts Options) tool.Tool {
	shell := opts.Shell
	return tool.Func("bash",
		"在工作目录中执行 shell 命令并返回输出（stdout 与 stderr 合并）。适合运行构建、测试、git、go 命令等。不要用它来读写文件（请用专门的文件工具），不要执行交互式命令。",
		false,
		func(ctx context.Context, in bashArgs, c *tool.Call) (*tool.Result, error) {
			if strings.TrimSpace(in.Command) == "" {
				return tool.Errorf("command 不能为空"), nil
			}
			timeout := opts.BashTimeout
			if in.Timeout > 0 {
				timeout = time.Duration(min(in.Timeout, 600)) * time.Second
			}
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			cmd := procutil.Command(ctx, shell, in.Command)
			if c.Env != nil && c.Env.WorkDir != "" {
				cmd.Dir = c.Env.WorkDir
			}
			// 只传递白名单内的环境变量：模型执行 env 也看不到 API Key 等密钥
			cmd.Env = opts.Env.Environ("GOAGENT=1", "TERM=dumb", "NO_COLOR=1")
			cmd.WaitDelay = 3 * time.Second
			var out syncBuf
			cmd.Stdout, cmd.Stderr = &out, &out
			start := time.Now()
			c.Env.Logf("$ %s", in.Command)
			err := cmd.Run()
			elapsed := time.Since(start).Round(time.Millisecond)
			text := strings.TrimRight(out.String(), "\n")
			code := 0
			var ee *exec.ExitError
			switch {
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				text += fmt.Sprintf("\n\n[命令超时（%s），已终止]", timeout)
				code = -1
			case errors.As(err, &ee):
				code = ee.ExitCode()
			case err != nil:
				return tool.Errorf("执行失败：%v", err), nil
			}
			if text == "" {
				text = "(无输出)"
			}
			if code != 0 {
				text += fmt.Sprintf("\n\n[退出码 %d]", code)
			}
			return &tool.Result{
				Content: tool.Truncate(text, opts.MaxOutput),
				IsError: code != 0,
				Meta:    map[string]any{"command": in.Command, "exit_code": code, "duration_ms": elapsed.Milliseconds()},
			}, nil
		})
}

// ---------- web_fetch ----------

type fetchArgs struct {
	URL string `json:"url" desc:"要抓取的 http(s) 网址"`
}

var (
	scriptRe = regexp.MustCompile(`(?is)<(script|style|noscript|svg)[^>]*>.*?</(script|style|noscript|svg)>`)
	blockRe  = regexp.MustCompile(`(?i)</?(p|div|br|li|tr|h[1-6]|section|article|pre|ul|ol|table)[^>]*>`)
	tagRe    = regexp.MustCompile(`<[^>]+>`)
	spaceRe  = regexp.MustCompile(`[ \t\r]+`)
	nlRe     = regexp.MustCompile(`\n{3,}`)
)

// HTMLToText 是一个足够用的 HTML 转纯文本实现（不追求完美）。
func HTMLToText(html string) string {
	s := scriptRe.ReplaceAllString(html, "")
	s = blockRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, "")
	r := strings.NewReplacer("&nbsp;", " ", "&lt;", "<", "&gt;", ">", "&amp;", "&", "&quot;", `"`, "&#39;", "'")
	s = r.Replace(s)
	s = spaceRe.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	s = strings.Join(lines, "\n")
	return strings.TrimSpace(nlRe.ReplaceAllString(s, "\n\n"))
}

// WebFetch 抓取网页并转为纯文本。
func WebFetch(opts Options) tool.Tool {
	timeout := opts.FetchTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	return tool.Func("web_fetch", "抓取一个网页（http/https），返回去掉 HTML 标签后的正文文本，最多约 30KB。", true,
		func(ctx context.Context, in fetchArgs, c *tool.Call) (*tool.Result, error) {
			if !strings.HasPrefix(in.URL, "http://") && !strings.HasPrefix(in.URL, "https://") {
				return tool.Errorf("只支持 http/https 网址"), nil
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.URL, nil)
			if err != nil {
				return tool.Errorf("%v", err), nil
			}
			req.Header.Set("User-Agent", "goagent/1.0 (+https://github.com/dmxj/goagent)")
			resp, err := client.Do(req)
			if err != nil {
				return tool.Errorf("请求失败：%v", err), nil
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
			text := string(body)
			ct := resp.Header.Get("Content-Type")
			if strings.Contains(ct, "html") {
				text = HTMLToText(text)
			}
			res := &tool.Result{
				Content: fmt.Sprintf("HTTP %d %s\n\n%s", resp.StatusCode, ct, tool.Truncate(text, opts.MaxOutput)),
				IsError: resp.StatusCode >= 400,
				Meta:    map[string]any{"url": in.URL, "status": resp.StatusCode, "bytes": len(body)},
			}
			return res, nil
		})
}

// ---------- todo_write ----------

// TodoItem 是待办清单中的一项。
type TodoItem struct {
	Content string `json:"content" desc:"任务描述"`
	Status  string `json:"status" enum:"pending,in_progress,completed" desc:"状态"`
}

type todoArgs struct {
	Todos []TodoItem `json:"todos" desc:"完整的待办清单（每次都传全量）"`
}

// TodoKey 是待办清单在 Env.Values 中的键。
const TodoKey = "todos"

// TodoWrite 让模型为复杂任务维护一个待办清单——这是 Claude Code 中
// 提升长任务完成度的关键技巧：先规划，再逐项推进并实时更新状态。
func TodoWrite() tool.Tool {
	return tool.Func("todo_write",
		"创建/更新当前任务的待办清单。复杂的多步骤任务开始时先列出计划，执行中及时把正在做的标为 in_progress、完成的标为 completed。同一时间只能有一个 in_progress。",
		true,
		func(ctx context.Context, in todoArgs, c *tool.Call) (*tool.Result, error) {
			inProgress := 0
			for _, t := range in.Todos {
				switch t.Status {
				case "pending", "in_progress", "completed":
				default:
					return tool.Errorf("无效状态 %q", t.Status), nil
				}
				if t.Status == "in_progress" {
					inProgress++
				}
			}
			if inProgress > 1 {
				return tool.Errorf("同一时间只能有一个 in_progress 的任务"), nil
			}
			if c.Env != nil && c.Env.Values != nil {
				c.Env.Values.Store(TodoKey, in.Todos)
			}
			var sb strings.Builder
			done := 0
			for _, t := range in.Todos {
				mark := "[ ]"
				switch t.Status {
				case "in_progress":
					mark = "[~]"
				case "completed":
					mark = "[x]"
					done++
				}
				sb.WriteString(mark + " " + t.Content + "\n")
			}
			return &tool.Result{
				Content: fmt.Sprintf("待办清单已更新（%d/%d 完成）：\n%s", done, len(in.Todos), sb.String()),
				Meta:    map[string]any{"todos": in.Todos},
			}, nil
		})
}
