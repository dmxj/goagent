package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmxj/goagent/tool"
)

// testEnv 创建一个以临时目录为工作目录的工具运行环境。
func testEnv(t *testing.T) *tool.Env {
	t.Helper()
	return &tool.Env{WorkDir: t.TempDir(), SessionID: "test", Files: tool.NewFileTracker(), Values: &sync.Map{}}
}

// call 以 JSON 参数调用工具。
func call(t *testing.T, tl tool.Tool, env *tool.Env, input any) *tool.Result {
	t.Helper()
	b, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tl.Run(context.Background(), &tool.Call{ID: "c1", Name: tl.Info().Name, Input: b, Env: env})
	if err != nil {
		t.Fatalf("工具 %s 返回 Go 错误：%v", tl.Info().Name, err)
	}
	return res
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAllTools(t *testing.T) {
	tools := All(Options{})
	want := map[string]bool{"read_file": true, "write_file": true, "edit_file": true, "list_dir": true, "glob": true, "grep": true, "bash": true, "web_fetch": true, "todo_write": true}
	if len(tools) != len(want) {
		t.Fatalf("内置工具数量 = %d，期望 %d", len(tools), len(want))
	}
	for _, tl := range tools {
		info := tl.Info()
		if !want[info.Name] {
			t.Errorf("意外的工具 %s", info.Name)
		}
		if !json.Valid(info.Schema) || info.Description == "" {
			t.Errorf("工具 %s 的 Schema/描述无效", info.Name)
		}
	}
	if d := DefaultOptions(); !d.RestrictToWorkDir || !d.RequireReadBeforeEdit || d.MaxOutput == 0 {
		t.Errorf("默认配置错误：%+v", d)
	}
}

func TestReadFile(t *testing.T) {
	env := testEnv(t)
	writeFile(t, env.WorkDir, "a.txt", "第一行\nsecond\nthird\n")
	writeFile(t, env.WorkDir, "empty.txt", "")
	writeFile(t, env.WorkDir, "bin.dat", "ab\x00cd")
	writeFile(t, env.WorkDir, "long.txt", strings.Repeat("中", 1000)+"\n")
	rf := ReadFile(DefaultOptions())
	tests := []struct {
		name      string
		input     map[string]any
		wantErr   bool
		contains  []string
		forbidden []string
	}{
		{"带行号输出", map[string]any{"path": "a.txt"}, false, []string{"     1\t第一行", "     3\tthird"}, nil},
		{"offset 与 limit", map[string]any{"path": "a.txt", "offset": 2, "limit": 1}, false, []string{"     2\tsecond", "共 3 行"}, []string{"第一行", "third"}},
		{"offset 越界", map[string]any{"path": "a.txt", "offset": 10}, true, []string{"超过文件总行数"}, nil},
		{"空文件", map[string]any{"path": "empty.txt"}, false, []string{"空文件"}, nil},
		{"拒绝二进制文件", map[string]any{"path": "bin.dat"}, true, []string{"二进制"}, nil},
		{"文件不存在", map[string]any{"path": "nope.txt"}, true, []string{"读取失败"}, nil},
		{"路径为空", map[string]any{"path": ""}, true, []string{"不能为空"}, nil},
		{"越界路径", map[string]any{"path": "../outside.txt"}, true, []string{"超出工作目录"}, nil},
		{"超长行截断", map[string]any{"path": "long.txt"}, false, []string{"行过长已截断"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := call(t, rf, env, tt.input)
			if res.IsError != tt.wantErr {
				t.Fatalf("IsError = %v，期望 %v：%s", res.IsError, tt.wantErr, res.Content)
			}
			for _, s := range tt.contains {
				if !strings.Contains(res.Content, s) {
					t.Errorf("输出应包含 %q：\n%s", s, res.Content)
				}
			}
			for _, s := range tt.forbidden {
				if strings.Contains(res.Content, s) {
					t.Errorf("输出不应包含 %q：\n%s", s, res.Content)
				}
			}
		})
	}
	if _, ok := env.Files.ReadAt(filepath.Join(env.WorkDir, "a.txt")); !ok {
		t.Errorf("read_file 应记录读取")
	}
	abs := filepath.Join(env.WorkDir, "a.txt")
	if res := call(t, rf, env, map[string]any{"path": abs}); res.IsError {
		t.Errorf("工作目录内的绝对路径应允许：%s", res.Content)
	}
	open := ReadFile(Options{RestrictToWorkDir: false})
	outside := writeFile(t, t.TempDir(), "o.txt", "outside")
	if res := call(t, open, env, map[string]any{"path": outside}); res.IsError {
		t.Errorf("关闭目录限制后应能读取外部文件：%s", res.Content)
	}
}

func TestWriteFile(t *testing.T) {
	env := testEnv(t)
	wf := WriteFile(DefaultOptions())
	res := call(t, wf, env, map[string]any{"path": "sub/dir/new.txt", "content": "hello\nworld"})
	if res.IsError || !strings.Contains(res.Content, "已创建") || res.Meta["created"] != true {
		t.Fatalf("创建文件失败：%+v", res)
	}
	p := filepath.Join(env.WorkDir, "sub/dir/new.txt")
	if readFile(t, p) != "hello\nworld" {
		t.Errorf("文件内容错误")
	}
	// 新建后视为已读取，可以直接覆盖
	if res := call(t, wf, env, map[string]any{"path": "sub/dir/new.txt", "content": "v2"}); res.IsError || !strings.Contains(res.Content, "已覆盖") {
		t.Errorf("覆盖自己写过的文件应成功：%+v", res)
	}
	existing := writeFile(t, env.WorkDir, "exist.txt", "old")
	if res := call(t, wf, env, map[string]any{"path": "exist.txt", "content": "new"}); !res.IsError || !strings.Contains(res.Content, "必须先用 read_file") {
		t.Errorf("未读取就覆盖已有文件应被拒绝：%+v", res)
	}
	if readFile(t, existing) != "old" {
		t.Errorf("被拒绝的写入不应修改文件")
	}
	if res := call(t, wf, env, map[string]any{"path": "/etc/goagent-test", "content": "x"}); !res.IsError {
		t.Errorf("越界写入应被拒绝")
	}
	// 关闭先读后写保护
	loose := WriteFile(Options{RestrictToWorkDir: true})
	if res := call(t, loose, env, map[string]any{"path": "exist.txt", "content": "new"}); res.IsError {
		t.Errorf("关闭保护后应允许覆盖：%s", res.Content)
	}
	// 没有 FileTracker 时不应误报"读取后被修改"
	strict := WriteFile(DefaultOptions())
	bare := &tool.Env{WorkDir: env.WorkDir}
	if res := call(t, strict, bare, map[string]any{"path": "exist.txt", "content": "x"}); res.IsError {
		t.Errorf("没有 FileTracker 时应跳过先读检查：%s", res.Content)
	}
}

func TestEditFile(t *testing.T) {
	ef := EditFile(DefaultOptions())
	rf := ReadFile(DefaultOptions())
	tests := []struct {
		name     string
		content  string
		read     bool
		input    map[string]any
		wantErr  string
		wantFile string
	}{
		{"唯一匹配替换", "a := 1\nb := 2\n", true, map[string]any{"old_string": "b := 2", "new_string": "b := 3"}, "", "a := 1\nb := 3\n"},
		{"多处匹配报错", "x\nx\n", true, map[string]any{"old_string": "x", "new_string": "y"}, "出现了 2 次", "x\nx\n"},
		{"replace_all 替换全部", "x\nx\n", true, map[string]any{"old_string": "x", "new_string": "y", "replace_all": true}, "", "y\ny\n"},
		{"没有找到", "abc", true, map[string]any{"old_string": "zzz", "new_string": "y"}, "没有找到", "abc"},
		{"新旧相同", "abc", true, map[string]any{"old_string": "abc", "new_string": "abc"}, "相同", "abc"},
		{"已存在文件 old_string 为空", "abc", true, map[string]any{"old_string": "", "new_string": "z"}, "不能为空", "abc"},
		{"未读取就修改", "abc", false, map[string]any{"old_string": "abc", "new_string": "def"}, "必须先用 read_file", "abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := testEnv(t)
			p := writeFile(t, env.WorkDir, "f.go", tt.content)
			if tt.read {
				call(t, rf, env, map[string]any{"path": "f.go"})
			}
			tt.input["path"] = "f.go"
			res := call(t, ef, env, tt.input)
			if tt.wantErr == "" && res.IsError {
				t.Fatalf("不应出错：%s", res.Content)
			}
			if tt.wantErr != "" && (!res.IsError || !strings.Contains(res.Content, tt.wantErr)) {
				t.Fatalf("应报错包含 %q，实际：%+v", tt.wantErr, res)
			}
			if got := readFile(t, p); got != tt.wantFile {
				t.Errorf("文件内容 = %q，期望 %q", got, tt.wantFile)
			}
		})
	}

	t.Run("old_string 为空且文件不存在时创建", func(t *testing.T) {
		env := testEnv(t)
		res := call(t, ef, env, map[string]any{"path": "new/x.txt", "old_string": "", "new_string": "content"})
		if res.IsError || readFile(t, filepath.Join(env.WorkDir, "new/x.txt")) != "content" {
			t.Fatalf("创建失败：%+v", res)
		}
	})
	t.Run("读取后文件被外部修改", func(t *testing.T) {
		env := testEnv(t)
		p := writeFile(t, env.WorkDir, "f.txt", "abc")
		call(t, rf, env, map[string]any{"path": "f.txt"})
		future := time.Now().Add(time.Hour)
		if err := os.Chtimes(p, future, future); err != nil {
			t.Fatal(err)
		}
		res := call(t, ef, env, map[string]any{"path": "f.txt", "old_string": "abc", "new_string": "x"})
		if !res.IsError || !strings.Contains(res.Content, "被修改过") {
			t.Fatalf("应提示重新读取：%+v", res)
		}
	})
	t.Run("报告首处匹配行号", func(t *testing.T) {
		env := testEnv(t)
		writeFile(t, env.WorkDir, "f.txt", "1\n2\ntarget\n")
		call(t, rf, env, map[string]any{"path": "f.txt"})
		res := call(t, ef, env, map[string]any{"path": "f.txt", "old_string": "target", "new_string": "done"})
		if res.Meta["line"] != 3 {
			t.Errorf("行号 = %v，期望 3", res.Meta["line"])
		}
	})
	t.Run("越界路径", func(t *testing.T) {
		env := testEnv(t)
		res := call(t, ef, env, map[string]any{"path": "../../x", "old_string": "a", "new_string": "b"})
		if !res.IsError || !strings.Contains(res.Content, "超出工作目录") {
			t.Fatalf("越界编辑应被拒绝：%+v", res)
		}
	})
}

func TestResolvePath(t *testing.T) {
	wd := t.TempDir()
	env := &tool.Env{WorkDir: wd}
	strict := Options{RestrictToWorkDir: true}
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"相对路径", "a/b.txt", false},
		{"工作目录本身", ".", false},
		{"父目录", "..", true},
		{"父目录下的文件", "../x", true},
		{"绕回工作目录内", "a/../b", false},
		{"伪装成子目录的兄弟目录", "../" + filepath.Base(wd) + "-evil/x", true},
		{"外部绝对路径", "/tmp/../etc/passwd", true},
		{"空白路径", "   ", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolvePath(env, strict, tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("resolvePath(%q) err = %v，期望出错=%v", tt.path, err, tt.wantErr)
			}
		})
	}
	if p, err := resolvePath(nil, Options{}, "x"); err != nil || !filepath.IsAbs(p) {
		t.Errorf("nil Env 应基于当前目录解析：%s %v", p, err)
	}
	if display(nil, "/a/b") != "/a/b" || display(env, filepath.Join(wd, "c")) != "c" || display(env, "/elsewhere") != "/elsewhere" {
		t.Errorf("display 结果错误")
	}
}

func TestListDir(t *testing.T) {
	env := testEnv(t)
	writeFile(t, env.WorkDir, "b.txt", "12345")
	writeFile(t, env.WorkDir, "src/main.go", "package main")
	writeFile(t, env.WorkDir, "src/deep/x/y/z.go", "")
	writeFile(t, env.WorkDir, "node_modules/pkg/index.js", "")
	ld := ListDir(DefaultOptions())
	res := call(t, ld, env, map[string]any{})
	if res.IsError {
		t.Fatal(res.Content)
	}
	for _, s := range []string{"src/", "  main.go", "b.txt (5B)", "node_modules/"} {
		if !strings.Contains(res.Content, s) {
			t.Errorf("输出应包含 %q：\n%s", s, res.Content)
		}
	}
	if strings.Contains(res.Content, "index.js") {
		t.Errorf("node_modules 内容应被跳过")
	}
	if strings.Contains(res.Content, "z.go") {
		t.Errorf("默认深度 2 不应展开到 z.go")
	}
	if strings.Index(res.Content, "src/") > strings.Index(res.Content, "b.txt") {
		t.Errorf("目录应排在文件前面")
	}
	if res := call(t, ld, env, map[string]any{"path": "src", "depth": 9}); !strings.Contains(res.Content, "z.go") {
		t.Errorf("加大深度后应能看到深层文件：\n%s", res.Content)
	}
	emptyDir := filepath.Join(env.WorkDir, "empty")
	_ = os.Mkdir(emptyDir, 0o755)
	if res := call(t, ld, env, map[string]any{"path": "empty"}); !strings.Contains(res.Content, "空目录") {
		t.Errorf("空目录提示错误：%s", res.Content)
	}
	if humanSize(2048) != "2.0KB" || humanSize(3*1024*1024) != "3.0MB" {
		t.Errorf("humanSize 格式错误")
	}
}

func TestGlobToRegexp(t *testing.T) {
	tests := []struct {
		pattern string
		match   []string
		nomatch []string
	}{
		{"*.go", []string{"main.go"}, []string{"cmd/main.go", "main.gox"}},
		{"**/*.go", []string{"main.go", "a/b/c.go"}, []string{"a/b/c.txt"}},
		{"cmd/*/main.go", []string{"cmd/goagent/main.go"}, []string{"cmd/a/b/main.go"}},
		{"src/**", []string{"src/a", "src/a/b/c"}, []string{"other/a"}},
		{"*.{ts,tsx}", []string{"a.ts", "b.tsx"}, []string{"c.js"}},
		{"file?.txt", []string{"file1.txt"}, []string{"file10.txt", "file/.txt"}},
		{"a,b.txt", []string{"a,b.txt"}, nil},
		{"v1.2(x).md", []string{"v1.2(x).md"}, []string{"v1x2(x).md"}},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			re, err := GlobToRegexp(tt.pattern)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range tt.match {
				if !re.MatchString(s) {
					t.Errorf("%q 应匹配 %q（正则 %s）", tt.pattern, s, re)
				}
			}
			for _, s := range tt.nomatch {
				if re.MatchString(s) {
					t.Errorf("%q 不应匹配 %q（正则 %s）", tt.pattern, s, re)
				}
			}
		})
	}
}

func TestGlob(t *testing.T) {
	env := testEnv(t)
	writeFile(t, env.WorkDir, "main.go", "")
	writeFile(t, env.WorkDir, "pkg/a.go", "")
	writeFile(t, env.WorkDir, "pkg/b.ts", "")
	writeFile(t, env.WorkDir, "web/c.tsx", "")
	writeFile(t, env.WorkDir, ".git/x.go", "")
	g := Glob(DefaultOptions())
	tests := []struct {
		pattern string
		path    string
		want    []string
		not     []string
	}{
		{"**/*.go", "", []string{"main.go", "pkg/a.go"}, []string{".git"}},
		{"*.go", "", []string{"main.go"}, []string{"pkg/a.go"}},
		{"**/*.{ts,tsx}", "", []string{"pkg/b.ts", "web/c.tsx"}, []string{".go"}},
		{"*.go", "pkg", []string{"pkg/a.go"}, []string{"main.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"@"+tt.path, func(t *testing.T) {
			res := call(t, g, env, map[string]any{"pattern": tt.pattern, "path": tt.path})
			for _, w := range tt.want {
				if !strings.Contains(res.Content, w) {
					t.Errorf("结果应包含 %s：\n%s", w, res.Content)
				}
			}
			for _, n := range tt.not {
				if strings.Contains(res.Content, n) {
					t.Errorf("结果不应包含 %s：\n%s", n, res.Content)
				}
			}
		})
	}
	if res := call(t, g, env, map[string]any{"pattern": "*.none"}); res.Content != "没有匹配的文件" {
		t.Errorf("无匹配时提示错误：%s", res.Content)
	}
}

func TestGrep(t *testing.T) {
	env := testEnv(t)
	writeFile(t, env.WorkDir, "a.go", "package a\nfunc Hello() {}\n// hello world\nvar x = 1\n")
	writeFile(t, env.WorkDir, "b.txt", "Hello from txt\n")
	writeFile(t, env.WorkDir, "bin.dat", "Hello\x00binary")
	writeFile(t, env.WorkDir, "vendor/v.go", "func Hello() {}\n")
	gr := Grep(DefaultOptions())
	tests := []struct {
		name    string
		input   map[string]any
		want    []string
		not     []string
		wantErr bool
	}{
		{"基本搜索", map[string]any{"pattern": "Hello"}, []string{"a.go:2: func Hello() {}", "b.txt:1:"}, []string{"bin.dat", "vendor"}, false},
		{"include 过滤", map[string]any{"pattern": "Hello", "include": "*.go"}, []string{"a.go:2:"}, []string{"b.txt"}, false},
		{"忽略大小写", map[string]any{"pattern": "hello", "include": "*.go", "ignore_case": true}, []string{"a.go:2:", "a.go:3:"}, nil, false},
		{"区分大小写", map[string]any{"pattern": "hello", "include": "*.go"}, []string{"a.go:3:"}, []string{"a.go:2:"}, false},
		{"上下文行", map[string]any{"pattern": "Hello", "path": "a.go", "context": 1}, []string{"a.go-1- package a", "a.go:2: func Hello", "a.go-3- // hello"}, nil, false},
		{"结果数上限", map[string]any{"pattern": ".", "path": "a.go", "max_results": 2}, []string{"上限"}, []string{"a.go:3:"}, false},
		{"无效正则", map[string]any{"pattern": "("}, nil, nil, true},
		{"路径不存在", map[string]any{"pattern": "x", "path": "nope"}, nil, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := call(t, gr, env, tt.input)
			if res.IsError != tt.wantErr {
				t.Fatalf("IsError = %v，期望 %v：%s", res.IsError, tt.wantErr, res.Content)
			}
			for _, w := range tt.want {
				if !strings.Contains(res.Content, w) {
					t.Errorf("结果应包含 %q：\n%s", w, res.Content)
				}
			}
			for _, n := range tt.not {
				if strings.Contains(res.Content, n) {
					t.Errorf("结果不应包含 %q：\n%s", n, res.Content)
				}
			}
		})
	}
	if res := call(t, gr, env, map[string]any{"pattern": "不存在的内容"}); res.Content != "没有找到匹配内容" {
		t.Errorf("无匹配时提示错误：%s", res.Content)
	}
	long := strings.Repeat("中", 200)
	if out := clip(long); !strings.HasSuffix(out, "…") || strings.ContainsRune(out, '\uFFFD') {
		t.Errorf("clip 应在字符边界截断")
	}
}

func TestSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, outside, "secret.txt", "top secret")
	env := testEnv(t)
	writeFile(t, env.WorkDir, "inside/ok.txt", "fine")
	if err := os.Symlink(outside, filepath.Join(env.WorkDir, "link")); err != nil {
		t.Skip("当前系统不支持符号链接：", err)
	}
	if err := os.Symlink(filepath.Join(env.WorkDir, "inside"), filepath.Join(env.WorkDir, "alias")); err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions()
	for _, tt := range []struct {
		tool  tool.Tool
		input map[string]any
	}{
		{ReadFile(opts), map[string]any{"path": "link/secret.txt"}},
		{WriteFile(opts), map[string]any{"path": "link/new.txt", "content": "x"}},
		{ListDir(opts), map[string]any{"path": "link"}},
	} {
		res := call(t, tt.tool, env, tt.input)
		if !res.IsError || !strings.Contains(res.Content, "符号链接") {
			t.Errorf("%s 不应通过符号链接逃出工作目录：%s", tt.tool.Info().Name, res.Content)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); err == nil {
		t.Errorf("不应在工作目录外创建文件")
	}
	if res := call(t, ReadFile(opts), env, map[string]any{"path": "alias/ok.txt"}); res.IsError {
		t.Errorf("指向工作目录内部的符号链接应允许：%s", res.Content)
	}
}
