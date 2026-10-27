package procenv

import (
	"strings"
	"testing"
)

func TestPolicyBuild(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin", "HOME=/home/u", "LANG=zh_CN.UTF-8", "TERM=xterm", "TMPDIR=/tmp",
		"GOPATH=/go", "GOPROXY=https://goproxy.cn,direct", "GOFLAGS=-mod=mod", "CGO_ENABLED=0",
		"GOAGENT_API_KEY=sk-goagent", "ANTHROPIC_AUTH_TOKEN=sk-ant", "OPENAI_API_KEY=sk-oa", "DEEPSEEK_API_KEY=sk-ds",
		"GITHUB_TOKEN=ghp_x", "MY_SECRET=s", "DB_PASSWORD=p", "AWS_ACCESS_KEY_ID=a", "RANDOM_VAR=1",
		"HTTPS_PROXY=http://127.0.0.1:7890", "HTTP_PROXY=http://user:pass@proxy:8080", "GOSTUDY_LLM_API_KEY=k",
		"broken", "=x",
	}
	tests := []struct {
		name   string
		policy Policy
		want   []string // 必须出现
		absent []string // 必须不出现的变量名
	}{
		{
			name:   "默认白名单剔除密钥",
			want:   []string{"PATH=/usr/bin", "HOME=/home/u", "LANG=zh_CN.UTF-8", "GOPATH=/go", "GOPROXY=https://goproxy.cn,direct", "CGO_ENABLED=0", "HTTPS_PROXY=http://127.0.0.1:7890", "GOAGENT=1"},
			absent: []string{"GOAGENT_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "DEEPSEEK_API_KEY", "GITHUB_TOKEN", "MY_SECRET", "DB_PASSWORD", "AWS_ACCESS_KEY_ID", "RANDOM_VAR", "HTTP_PROXY", "GOSTUDY_LLM_API_KEY"},
		},
		{
			name:   "精确 allow 可以放行黑名单中的变量，通配 allow 不行",
			policy: Policy{Allow: []string{"GITHUB_TOKEN", "RANDOM_*", "*_API_KEY"}},
			want:   []string{"GITHUB_TOKEN=ghp_x", "RANDOM_VAR=1"},
			absent: []string{"OPENAI_API_KEY", "DEEPSEEK_API_KEY"},
		},
		{
			name:   "deny 优先于一切",
			policy: Policy{Allow: []string{"GITHUB_TOKEN"}, Deny: []string{"GITHUB_TOKEN", "GOPROXY", "INJECTED"}, Extra: map[string]string{"INJECTED": "x"}},
			absent: []string{"GITHUB_TOKEN", "GOPROXY", "INJECTED"},
		},
		{
			name:   "extra 注入并展开父环境",
			policy: Policy{Extra: map[string]string{"GH_TOKEN": "${GITHUB_TOKEN}", "MODE": "lab"}},
			want:   []string{"GH_TOKEN=ghp_x", "MODE=lab"},
			absent: []string{"GITHUB_TOKEN"},
		},
		{
			name:   "inherit_all 仍剔除密钥",
			policy: Policy{InheritAll: true},
			want:   []string{"RANDOM_VAR=1", "PATH=/usr/bin"},
			absent: []string{"OPENAI_API_KEY", "MY_SECRET", "GOAGENT_API_KEY"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.policy.Build(environ, "GOAGENT=1")
			joined := "\n" + strings.Join(got, "\n") + "\n"
			for _, w := range tt.want {
				if !strings.Contains(joined, "\n"+w+"\n") {
					t.Errorf("结果中缺少 %s，得到：%v", w, got)
				}
			}
			for _, a := range tt.absent {
				if strings.Contains(joined, "\n"+a+"=") {
					t.Errorf("结果中不应出现 %s，得到：%v", a, got)
				}
			}
		})
	}
}

func TestIsSecretName(t *testing.T) {
	for name, want := range map[string]bool{
		"OPENAI_API_KEY": true, "anthropic_base_url": true, "GOAGENT_MODEL": true, "MY_TOKEN": true,
		"db_password": true, "PATH": false, "GOPATH": false, "GOPRIVATE": false, "HOME": false,
	} {
		if got := IsSecretName(name); got != want {
			t.Errorf("IsSecretName(%q) = %v，期望 %v", name, got, want)
		}
	}
}
