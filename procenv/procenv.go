// Package procenv 为 Agent 启动的子进程（bash 工具、命令钩子）构造"干净"的环境变量。
//
// 直接把 os.Environ() 交给子进程，模型执行一条 `env` 就能看到 API Key 等密钥。
// 这里采用"白名单 + 密钥黑名单"：
//
//  1. 只继承运行命令真正需要的变量（PATH、HOME、LANG、TERM、TMPDIR、Go 工具链变量……）；
//  2. 名称看起来像密钥的变量（*KEY*、*TOKEN*、*SECRET*、*PASSWORD* 以及各大模型厂商前缀）一律剔除，
//     即使它在白名单里；
//  3. 用户可以在配置 tools.bash_env 中显式放行（allow）、追加拒绝（deny）或注入（extra）变量。
//
// 显式 allow 的精确变量名优先于内置黑名单（用户明确想把 GH_TOKEN 交给 Agent 时可以做到），
// 但配置中的 deny 优先于一切。
package procenv

import (
	"os"
	"path"
	"sort"
	"strings"
)

// Policy 描述子进程环境变量的过滤策略（对应配置 tools.bash_env）。
type Policy struct {
	// Allow 追加允许继承的变量名，支持 * 通配（如 "MY_APP_*"）。
	// 精确写出的变量名即使命中内置密钥黑名单也会放行。
	Allow []string `json:"allow,omitempty"`
	// Deny 追加拒绝的变量名模式，优先级最高。
	Deny []string `json:"deny,omitempty"`
	// Extra 注入额外变量，值支持 ${VAR} 引用父进程环境（例如 "GH_TOKEN": "${GH_TOKEN}"）。
	Extra map[string]string `json:"extra,omitempty"`
	// InheritAll 为 true 时从完整环境出发（仍会剔除密钥），适合确实需要大量环境变量的场景。
	InheritAll bool `json:"inherit_all,omitempty"`
}

// DefaultAllow 是默认允许继承的变量（* 为通配）。
var DefaultAllow = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "PWD", "LANG", "LANGUAGE", "LC_*", "TERM", "COLORTERM",
	"TMPDIR", "TMP", "TEMP", "TZ", "EDITOR", "PAGER",
	// Go 工具链
	"GO*", "CGO_*", "CC", "CXX", "AR", "PKG_CONFIG*",
	// 代理（国内网络常用）；带用户名密码的代理地址会被剔除
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "no_proxy", "all_proxy",
	// Windows 运行命令所需
	"SystemRoot", "SYSTEMROOT", "SystemDrive", "ComSpec", "COMSPEC", "PATHEXT", "WINDIR", "windir", "OS",
	"USERPROFILE", "APPDATA", "LOCALAPPDATA", "ProgramData", "ProgramFiles", "ProgramFiles(x86)", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",
	// macOS / Linux 桌面常见
	"XDG_*", "DISPLAY", "SSL_CERT_FILE", "SSL_CERT_DIR",
}

// SecretPatterns 是内置的密钥黑名单（大小写不敏感，* 为通配）。
var SecretPatterns = []string{
	"*KEY*", "*TOKEN*", "*SECRET*", "*PASSWORD*", "*PASSWD*", "*PASSPHRASE*", "*CREDENTIAL*", "*AUTH*", "*COOKIE*", "*SESSION*",
	"GOAGENT_*", "GOSTUDY_*", "ANTHROPIC_*", "OPENAI_*", "AZURE_*", "AWS_*", "GOOGLE_*", "GCP_*", "GEMINI_*",
	"DEEPSEEK_*", "DASHSCOPE_*", "MOONSHOT_*", "ZHIPU*", "QWEN_*", "ARK_*", "VOLC*", "HF_*", "HUGGING*",
	"GITHUB_*", "GH_*", "GITLAB_*", "NPM_*", "DOCKER_*", "KUBECONFIG", "SSH_AUTH_SOCK", "GPG_*", "DATABASE_URL", "*_DSN",
}

func match(pattern, name string, fold bool) bool {
	if fold {
		pattern, name = strings.ToUpper(pattern), strings.ToUpper(name)
	}
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

func matchAny(patterns []string, name string, fold bool) bool {
	for _, p := range patterns {
		if match(p, name, fold) {
			return true
		}
	}
	return false
}

// IsSecretName 判断变量名是否像密钥。
func IsSecretName(name string) bool { return matchAny(SecretPatterns, name, true) }

// hasCredentials 判断代理地址等值中是否带有 user:pass@。
func hasCredentials(v string) bool {
	if i := strings.Index(v, "://"); i >= 0 {
		v = v[i+3:]
	}
	at := strings.IndexByte(v, '@')
	slash := strings.IndexByte(v, '/')
	return at > 0 && (slash < 0 || at < slash)
}

// Allowed 判断名为 name、值为 value 的父进程变量能否传给子进程。
func (p Policy) Allowed(name, value string) bool {
	if name == "" || matchAny(p.Deny, name, false) {
		return false
	}
	for _, a := range p.Allow { // 精确放行优先于内置黑名单
		if a == name {
			return true
		}
	}
	if IsSecretName(name) {
		return false
	}
	if strings.HasSuffix(strings.ToUpper(name), "PROXY") && hasCredentials(value) {
		return false
	}
	return p.InheritAll || matchAny(DefaultAllow, name, false) || matchAny(p.Allow, name, false)
}

// Build 从 environ（通常是 os.Environ()）构造子进程环境，并追加 extra（如 GOAGENT=1）。
// 结果按变量名排序，便于测试与排查。
func (p Policy) Build(environ []string, extra ...string) []string {
	vars := map[string]string{}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if p.Allowed(k, v) {
			vars[k] = v
		}
	}
	lookup := func(k string) string {
		for _, kv := range environ {
			if name, v, ok := strings.Cut(kv, "="); ok && name == k {
				return v
			}
		}
		return ""
	}
	for k, v := range p.Extra {
		if matchAny(p.Deny, k, false) {
			continue
		}
		vars[k] = os.Expand(v, lookup)
	}
	for _, kv := range extra {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	out := make([]string, 0, len(vars))
	for k, v := range vars {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Environ 是 Build(os.Environ(), extra...) 的简写。
func (p Policy) Environ(extra ...string) []string { return p.Build(os.Environ(), extra...) }
