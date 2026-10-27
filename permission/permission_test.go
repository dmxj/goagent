package permission

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func req(tool string, input any, readOnly bool) Request {
	b, _ := json.Marshal(input)
	return Request{Tool: tool, Input: b, ReadOnly: readOnly}
}

func TestParseRule(t *testing.T) {
	tests := []struct {
		in        string
		tool, pat string
		wantErr   bool
		wantStr   string
	}{
		{"bash", "bash", "", false, "bash"},
		{" bash(go test:*) ", "bash", "go test:*", false, "bash(go test:*)"},
		{"write_file(src/**)", "write_file", "src/**", false, "write_file(src/**)"},
		{"web_fetch(domain:go.dev)", "web_fetch", "domain:go.dev", false, "web_fetch(domain:go.dev)"},
		{"mcp__*", "mcp__*", "", false, "mcp__*"},
		{"bash(go test", "", "", true, ""},
		{"(x)", "", "", true, ""},
		{"", "", "", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			r, err := ParseRule(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseRule(%q) err = %v，期望出错=%v", tt.in, err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if r.Tool != tt.tool || r.Pattern != tt.pat || r.String() != tt.wantStr {
				t.Errorf("ParseRule(%q) = %+v，期望 tool=%q pattern=%q", tt.in, r, tt.tool, tt.pat)
			}
		})
	}
}

func TestRuleMatch(t *testing.T) {
	tests := []struct {
		rule string
		req  Request
		want bool
	}{
		{"bash", req("bash", map[string]string{"command": "rm -rf /"}, false), true},
		{"bash(go test:*)", req("bash", map[string]string{"command": "go test ./..."}, false), true},
		{"bash(go test:*)", req("bash", map[string]string{"command": "go build"}, false), false},
		{"bash(git status)", req("bash", map[string]string{"command": "git status"}, false), true},
		{"bash(git status)", req("bash", map[string]string{"command": "git status --short"}, false), false},
		{"write_file(src/**)", req("write_file", map[string]string{"path": "src/a/b.go"}, false), true},
		{"write_file(src/**)", req("write_file", map[string]string{"path": "./src/a.go"}, false), true},
		{"write_file(src/**)", req("write_file", map[string]string{"path": "docs/a.md"}, false), false},
		{"write_file(*.md)", req("write_file", map[string]string{"path": "README.md"}, false), true},
		{"write_file(*.md)", req("write_file", map[string]string{"path": "docs/README.md"}, false), false},
		{"edit_file(a?.go)", req("edit_file", map[string]string{"path": "ab.go"}, false), true},
		{"web_fetch(domain:go.dev)", req("web_fetch", map[string]string{"url": "https://go.dev/doc"}, true), true},
		{"web_fetch(domain:go.dev)", req("web_fetch", map[string]string{"url": "http://evil.com/go.dev"}, true), false},
		{"mcp__github__*", req("mcp__github__create_issue", map[string]string{}, false), true},
		{"mcp__github__*", req("mcp__slack__post", map[string]string{}, false), false},
		{"read_file", req("write_file", map[string]string{"path": "x"}, false), false},
		{"bash(*)", req("bash", map[string]string{"command": "anything"}, false), true},
		{"grep(TODO)", req("grep", map[string]string{"pattern": "TODO"}, true), true},
	}
	for _, tt := range tests {
		t.Run(tt.rule+"/"+string(tt.req.Input), func(t *testing.T) {
			r, err := ParseRule(tt.rule)
			if err != nil {
				t.Fatal(err)
			}
			if got := r.Match(tt.req); got != tt.want {
				t.Errorf("规则 %s 匹配 %s = %v，期望 %v", tt.rule, tt.req.Input, got, tt.want)
			}
		})
	}
}

func TestEvaluate(t *testing.T) {
	write := req("write_file", map[string]string{"path": "a.txt"}, false)
	read := req("read_file", map[string]string{"path": "a.txt"}, true)
	bash := req("bash", map[string]string{"command": "ls"}, false)
	tests := []struct {
		name string
		cfg  Config
		req  Request
		want Decision
	}{
		{"default 模式只读工具放行", Config{}, read, Allow},
		{"default 模式写文件需要询问", Config{}, write, Ask},
		{"default 模式命令需要询问", Config{}, bash, Ask},
		{"accept_edits 自动允许文件修改", Config{Mode: ModeAcceptEdits}, write, Allow},
		{"accept_edits 命令仍需询问", Config{Mode: ModeAcceptEdits}, bash, Ask},
		{"plan 模式拒绝写操作", Config{Mode: ModePlan}, write, Deny},
		{"plan 模式允许只读", Config{Mode: ModePlan}, read, Allow},
		{"plan 模式即使有 allow 规则也拒绝写", Config{Mode: ModePlan, Allow: []string{"write_file"}}, write, Deny},
		{"bypass 全部放行", Config{Mode: ModeBypass}, bash, Allow},
		{"bypass 下 ask 规则不生效", Config{Mode: ModeBypass, Ask: []string{"bash"}}, bash, Allow},
		{"bypass 下 deny 规则仍生效", Config{Mode: ModeBypass, Deny: []string{"bash"}}, bash, Deny},
		{"allow 规则放行", Config{Allow: []string{"bash(ls)"}}, bash, Allow},
		{"deny 优先于 allow", Config{Allow: []string{"bash"}, Deny: []string{"bash(ls)"}}, bash, Deny},
		{"ask 优先于 allow", Config{Allow: []string{"bash"}, Ask: []string{"bash"}}, bash, Ask},
		{"deny 可以拒绝只读工具", Config{Deny: []string{"read_file"}}, read, Deny},
		{"ask 规则可以作用于只读工具", Config{Ask: []string{"read_file"}}, read, Ask},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewChecker(tt.cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, reason := c.Evaluate(tt.req)
			if got != tt.want {
				t.Errorf("Evaluate = %s（%s），期望 %s", got, reason, tt.want)
			}
			if reason == "" {
				t.Errorf("判定应附带原因")
			}
		})
	}
}

func TestNewCheckerInvalidRule(t *testing.T) {
	for _, cfg := range []Config{{Allow: []string{"bash("}}, {Deny: []string{""}}, {Ask: []string{"(x)"}}} {
		if _, err := NewChecker(cfg, nil); err == nil {
			t.Errorf("非法规则 %+v 应返回错误", cfg)
		}
	}
	c, _ := NewChecker(Config{}, nil)
	if c.Mode() != ModeDefault {
		t.Errorf("默认模式应为 default")
	}
	c.SetMode(ModePlan)
	if c.Mode() != ModePlan {
		t.Errorf("SetMode 未生效")
	}
}

func TestCheck(t *testing.T) {
	write := req("write_file", map[string]string{"path": "a.txt"}, false)
	edit := req("edit_file", map[string]string{"path": "a.txt"}, false)
	ctx := context.Background()

	t.Run("没有审批者时拒绝", func(t *testing.T) {
		c, _ := NewChecker(Config{}, nil)
		var de *DeniedError
		if err := c.Check(ctx, write); !errors.As(err, &de) || !strings.Contains(de.Error(), "没有配置审批者") {
			t.Fatalf("期望 DeniedError，实际 %v", err)
		}
	})
	t.Run("AutoApprove 与 AutoDeny", func(t *testing.T) {
		c, _ := NewChecker(Config{}, AutoApprove)
		if err := c.Check(ctx, write); err != nil {
			t.Errorf("AutoApprove 应放行：%v", err)
		}
		c, _ = NewChecker(Config{}, AutoDeny)
		if err := c.Check(ctx, write); err == nil || !strings.Contains(err.Error(), "用户拒绝") {
			t.Errorf("AutoDeny 应拒绝：%v", err)
		}
	})
	t.Run("deny 规则不会询问审批者", func(t *testing.T) {
		asked := false
		c, _ := NewChecker(Config{Deny: []string{"write_file"}}, ApproverFunc(func(context.Context, Request, string) (bool, bool, error) {
			asked = true
			return true, false, nil
		}))
		if err := c.Check(ctx, write); err == nil || asked {
			t.Errorf("deny 规则应直接拒绝：err=%v asked=%v", err, asked)
		}
	})
	t.Run("审批者出错", func(t *testing.T) {
		boom := errors.New("终端已关闭")
		c, _ := NewChecker(Config{}, ApproverFunc(func(context.Context, Request, string) (bool, bool, error) { return false, false, boom }))
		if err := c.Check(ctx, write); !errors.Is(err, boom) {
			t.Errorf("应透传审批者错误：%v", err)
		}
	})
	t.Run("remember 记住同一工具", func(t *testing.T) {
		asks := 0
		c, _ := NewChecker(Config{}, ApproverFunc(func(context.Context, Request, string) (bool, bool, error) {
			asks++
			return true, true, nil
		}))
		for i := 0; i < 3; i++ {
			if err := c.Check(ctx, write); err != nil {
				t.Fatal(err)
			}
		}
		if asks != 1 {
			t.Errorf("remember 后不应再询问，实际询问 %d 次", asks)
		}
		if d, _ := c.Evaluate(write); d != Allow {
			t.Errorf("记住后 Evaluate 应为 allow")
		}
		if d, _ := c.Evaluate(edit); d != Ask {
			t.Errorf("remember 只对同一工具生效，edit_file 仍应询问")
		}
	})
	t.Run("remember 只作用于当前会话", func(t *testing.T) {
		asks := 0
		grants := NewGrants()
		c, _ := NewChecker(Config{}, ApproverFunc(func(context.Context, Request, string) (bool, bool, error) {
			asks++
			return true, true, nil
		}))
		c.UseGrants(grants)
		a := write
		a.SessionID = "sess-A"
		b := write
		b.SessionID = "sess-B"
		if err := c.Check(ctx, a); err != nil {
			t.Fatal(err)
		}
		if d, _ := c.Evaluate(a); d != Allow {
			t.Errorf("会话 A 记住后应直接允许")
		}
		if d, _ := c.Evaluate(b); d != Ask {
			t.Errorf("会话 A 的'总是允许'不应影响会话 B，得到 %s", d)
		}
		// 配置热更新会重建 Checker：共享同一个 Grants 时决定不丢失
		c2, _ := NewChecker(Config{}, AutoDeny)
		c2.UseGrants(grants)
		if d, _ := c2.Evaluate(a); d != Allow {
			t.Errorf("新 Checker 共享 Grants 后应保留会话 A 的授权")
		}
		grants.Clear("sess-A")
		if d, _ := c2.Evaluate(a); d != Ask {
			t.Errorf("Clear 后应重新询问")
		}
		if got := grants.Rules("sess-A"); len(got) != 0 {
			t.Errorf("Clear 后 Rules 应为空：%v", got)
		}
	})
	t.Run("ask 规则优先于 remember", func(t *testing.T) {
		c, _ := NewChecker(Config{Ask: []string{"write_file(secret/**)"}}, AutoApprove)
		c.Grants().Allow("", "write_file")
		if d, _ := c.Evaluate(req("write_file", map[string]string{"path": "secret/key"}, false)); d != Ask {
			t.Errorf("ask 规则应优先")
		}
	})
}

func TestSubject(t *testing.T) {
	tests := []struct {
		input any
		want  string
	}{
		{map[string]string{"command": "ls"}, "ls"},
		{map[string]string{"path": "a.go"}, "a.go"},
		{map[string]string{"file_path": "b.go"}, "b.go"},
		{map[string]string{"url": "https://x.com/a"}, "domain:x.com"},
		{map[string]string{"pattern": "**/*.go"}, "**/*.go"},
		{map[string]int{"n": 1}, `{"n":1}`},
	}
	for _, tt := range tests {
		r := req("t", tt.input, false)
		if got := subject(r); got != tt.want {
			t.Errorf("subject(%s) = %q，期望 %q", r.Input, got, tt.want)
		}
	}
}
