package builtin

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dmxj/goagent/procenv"
)

func TestBash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash 测试依赖 Unix shell")
	}
	env := testEnv(t)
	writeFile(t, env.WorkDir, "marker.txt", "")
	var logs []string
	env.Log = func(format string, args ...any) { logs = append(logs, format) }
	b := Bash(Options{BashTimeout: 5 * time.Second, MaxOutput: 30000})
	tests := []struct {
		name     string
		input    map[string]any
		wantErr  bool
		contains []string
		exitCode int
	}{
		{"标准输出", map[string]any{"command": "echo hello"}, false, []string{"hello"}, 0},
		{"合并 stderr", map[string]any{"command": "echo out; echo err 1>&2"}, false, []string{"out", "err"}, 0},
		{"在工作目录中执行", map[string]any{"command": "ls"}, false, []string{"marker.txt"}, 0},
		{"非零退出码", map[string]any{"command": "echo fail; exit 3"}, true, []string{"fail", "[退出码 3]"}, 3},
		{"无输出", map[string]any{"command": "true"}, false, []string{"(无输出)"}, 0},
		{"环境变量", map[string]any{"command": "echo $GOAGENT"}, false, []string{"1"}, 0},
		{"空命令", map[string]any{"command": "  "}, true, []string{"不能为空"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := call(t, b, env, tt.input)
			if res.IsError != tt.wantErr {
				t.Fatalf("IsError = %v，期望 %v：%s", res.IsError, tt.wantErr, res.Content)
			}
			for _, s := range tt.contains {
				if !strings.Contains(res.Content, s) {
					t.Errorf("输出应包含 %q：%s", s, res.Content)
				}
			}
			if code, ok := res.Meta["exit_code"]; ok && code != tt.exitCode {
				t.Errorf("退出码 = %v，期望 %d", code, tt.exitCode)
			}
		})
	}
	if len(logs) == 0 {
		t.Errorf("执行命令时应输出日志")
	}

	t.Run("超时会终止整个进程组", func(t *testing.T) {
		start := time.Now()
		res := call(t, b, env, map[string]any{"command": "sleep 30 & sleep 30", "timeout": 1})
		if time.Since(start) > 10*time.Second {
			t.Fatalf("超时后应尽快返回，耗时 %v", time.Since(start))
		}
		if !res.IsError || !strings.Contains(res.Content, "超时") || res.Meta["exit_code"] != -1 {
			t.Errorf("超时结果错误：%+v", res)
		}
	})
	t.Run("输出截断", func(t *testing.T) {
		small := Bash(Options{BashTimeout: 5 * time.Second, MaxOutput: 100})
		res := call(t, small, env, map[string]any{"command": "seq 1 1000"})
		if !strings.Contains(res.Content, "省略") {
			t.Errorf("超长输出应被截断")
		}
	})
	t.Run("环境变量过滤：密钥不会泄露给命令", func(t *testing.T) {
		t.Setenv("GOAGENT_API_KEY", "sk-should-not-leak")
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-ant-leak")
		t.Setenv("OPENAI_API_KEY", "sk-oa-leak")
		t.Setenv("MY_SERVICE_PASSWORD", "pw-leak")
		t.Setenv("GOPROXY", "https://goproxy.cn,direct")
		t.Setenv("CUSTOM_VISIBLE", "yes")
		res := call(t, b, env, map[string]any{"command": "env"})
		for _, leak := range []string{"sk-should-not-leak", "sk-ant-leak", "sk-oa-leak", "pw-leak", "CUSTOM_VISIBLE"} {
			if strings.Contains(res.Content, leak) {
				t.Errorf("env 输出泄露了 %s：\n%s", leak, res.Content)
			}
		}
		for _, keep := range []string{"PATH=", "GOPROXY=https://goproxy.cn,direct", "GOAGENT=1"} {
			if !strings.Contains(res.Content, keep) {
				t.Errorf("env 输出应保留 %s：\n%s", keep, res.Content)
			}
		}
		custom := Bash(Options{BashTimeout: 5 * time.Second, MaxOutput: 30000, Env: procenv.Policy{Allow: []string{"CUSTOM_VISIBLE"}, Extra: map[string]string{"INJECTED": "v-${CUSTOM_VISIBLE}"}}})
		res = call(t, custom, env, map[string]any{"command": "echo $CUSTOM_VISIBLE $INJECTED $OPENAI_API_KEY"})
		if strings.TrimSpace(strings.Split(res.Content, "\n")[0]) != "yes v-yes" {
			t.Errorf("bash_env.allow / extra 未生效：%q", res.Content)
		}
	})
	t.Run("指定 shell", func(t *testing.T) {
		sh := Bash(Options{BashTimeout: 5 * time.Second, MaxOutput: 1000, Shell: "/bin/sh"})
		if res := call(t, sh, env, map[string]any{"command": "echo sh"}); res.IsError || !strings.Contains(res.Content, "sh") {
			t.Errorf("自定义 shell 失败：%+v", res)
		}
	})
}

func TestTodoWrite(t *testing.T) {
	tw := TodoWrite()
	tests := []struct {
		name    string
		todos   []TodoItem
		wantErr string
		want    []string
	}{
		{"正常清单", []TodoItem{{"读代码", "completed"}, {"写测试", "in_progress"}, {"提交", "pending"}}, "", []string{"1/3 完成", "[x] 读代码", "[~] 写测试", "[ ] 提交"}},
		{"只能有一个 in_progress", []TodoItem{{"a", "in_progress"}, {"b", "in_progress"}}, "只能有一个", nil},
		{"无效状态", []TodoItem{{"a", "doing"}}, "无效状态", nil},
		{"空清单", nil, "", []string{"0/0 完成"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := testEnv(t)
			res := call(t, tw, env, map[string]any{"todos": tt.todos})
			if tt.wantErr != "" {
				if !res.IsError || !strings.Contains(res.Content, tt.wantErr) {
					t.Fatalf("应报错 %q：%+v", tt.wantErr, res)
				}
				if _, ok := env.Values.Load(TodoKey); ok {
					t.Errorf("无效清单不应被保存")
				}
				return
			}
			for _, w := range tt.want {
				if !strings.Contains(res.Content, w) {
					t.Errorf("输出应包含 %q：%s", w, res.Content)
				}
			}
			v, ok := env.Values.Load(TodoKey)
			if !ok || len(v.([]TodoItem)) != len(tt.todos) {
				t.Errorf("清单应保存到 Env.Values")
			}
		})
	}
}

func TestHTMLToText(t *testing.T) {
	tests := []struct {
		name, html string
		want       []string
		not        []string
	}{
		{"去掉标签与脚本", `<html><head><style>.a{}</style><script>alert(1)</script></head><body><h1>标题</h1><p>段落 <b>加粗</b></p></body></html>`,
			[]string{"标题", "段落 加粗"}, []string{"alert", ".a{}", "<"}},
		{"实体解码", `<p>a &lt;b&gt; &amp; &quot;c&quot; &#39;d&#39;&nbsp;e</p>`, []string{`a <b> & "c" 'd' e`}, nil},
		{"块级元素换行", `<div>一</div><div>二</div><ul><li>三</li></ul>`, []string{"一\n", "二\n", "三"}, nil},
		{"压缩空白", "<p>a    b\t\tc</p>\n\n\n\n<p>d</p>", []string{"a b c", "c\n\nd"}, []string{"\n\n\n"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HTMLToText(tt.html)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("结果应包含 %q：%q", w, got)
				}
			}
			for _, n := range tt.not {
				if strings.Contains(got, n) {
					t.Errorf("结果不应包含 %q：%q", n, got)
				}
			}
		})
	}
}

func TestWebFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/html":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<p>你好 <i>世界</i></p><script>x()</script>"))
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	wf := WebFetch(DefaultOptions())
	env := testEnv(t)
	if res := call(t, wf, env, map[string]any{"url": srv.URL + "/html"}); res.IsError || !strings.Contains(res.Content, "你好 世界") || strings.Contains(res.Content, "x()") {
		t.Errorf("HTML 应转为纯文本：%+v", res)
	}
	if res := call(t, wf, env, map[string]any{"url": srv.URL + "/json"}); !strings.Contains(res.Content, `{"ok":true}`) {
		t.Errorf("非 HTML 内容应原样返回：%s", res.Content)
	}
	if res := call(t, wf, env, map[string]any{"url": srv.URL + "/missing"}); !res.IsError || !strings.Contains(res.Content, "HTTP 404") {
		t.Errorf("4xx 应标记为错误：%+v", res)
	}
	if res := call(t, wf, env, map[string]any{"url": "file:///etc/passwd"}); !res.IsError {
		t.Errorf("非 http(s) 协议应被拒绝")
	}
	if res := call(t, wf, env, map[string]any{"url": "http://127.0.0.1:1/"}); !res.IsError {
		t.Errorf("连接失败应返回错误结果")
	}
}
