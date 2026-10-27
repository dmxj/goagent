package builtin

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dmxj/goagent/tool"
)

// ---------- read_file ----------

type readArgs struct {
	Path   string `json:"path" desc:"文件路径（相对工作目录或绝对路径）"`
	Offset int    `json:"offset,omitempty" desc:"从第几行开始读（1 起），默认 1"`
	Limit  int    `json:"limit,omitempty" desc:"最多读取多少行，默认 2000"`
}

// ReadFile 读取文本文件，输出带行号，便于模型后续精确编辑。
func ReadFile(opts Options) tool.Tool {
	return tool.Func("read_file",
		"读取文本文件内容，输出带行号（格式：行号<TAB>内容）。大文件请配合 offset/limit 分段读取。修改文件前必须先读取。",
		true,
		func(ctx context.Context, in readArgs, c *tool.Call) (*tool.Result, error) {
			p, err := resolvePath(c.Env, opts, in.Path)
			if err != nil {
				return tool.Errorf("%v", err), nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return tool.Errorf("读取失败：%v", err), nil
			}
			if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
				return tool.Errorf("%s 看起来是二进制文件（%d 字节），无法以文本读取", display(c.Env, p), len(data)), nil
			}
			c.Env.Files.Read(p)
			offset, limit := max(in.Offset, 1), in.Limit
			if limit <= 0 {
				limit = 2000
			}
			lines := strings.Split(string(data), "\n")
			if len(lines) > 0 && lines[len(lines)-1] == "" {
				lines = lines[:len(lines)-1]
			}
			if len(lines) == 0 {
				return &tool.Result{Content: "(空文件)", Meta: map[string]any{"path": display(c.Env, p), "op": "read"}}, nil
			}
			if offset > len(lines) {
				return tool.Errorf("offset %d 超过文件总行数 %d", offset, len(lines)), nil
			}
			end := min(len(lines), offset-1+limit)
			var sb strings.Builder
			for i := offset - 1; i < end; i++ {
				line := lines[i]
				if len(line) > 2000 {
					line = cutUTF8(line, 2000) + "…[行过长已截断]"
				}
				fmt.Fprintf(&sb, "%6d\t%s\n", i+1, line)
			}
			if end < len(lines) {
				fmt.Fprintf(&sb, "\n（文件共 %d 行，已显示 %d-%d 行，使用 offset 继续读取）", len(lines), offset, end)
			}
			return &tool.Result{
				Content: tool.Truncate(sb.String(), opts.MaxOutput),
				Meta:    map[string]any{"path": display(c.Env, p), "op": "read", "lines": end - offset + 1},
			}, nil
		})
}

// ---------- write_file ----------

type writeArgs struct {
	Path    string `json:"path" desc:"文件路径"`
	Content string `json:"content" desc:"完整的文件内容（会覆盖原文件）"`
}

func checkReadBeforeEdit(c *tool.Call, opts Options, p string) error {
	// 没有 FileTracker（例如工具被单独调用）时无法判断是否读过，跳过检查
	if !opts.RequireReadBeforeEdit || c.Env == nil || c.Env.Files == nil {
		return nil
	}
	st, err := os.Stat(p)
	if err != nil {
		return nil // 新文件无需先读
	}
	readAt, ok := c.Env.Files.ReadAt(p)
	if !ok {
		return fmt.Errorf("修改已存在的文件 %s 前必须先用 read_file 读取它", display(c.Env, p))
	}
	if st.ModTime().After(readAt.Add(time.Second)) {
		return fmt.Errorf("文件 %s 在上次读取后被修改过，请重新 read_file 后再修改", display(c.Env, p))
	}
	return nil
}

// WriteFile 创建或覆盖文件。
func WriteFile(opts Options) tool.Tool {
	return tool.Func("write_file",
		"创建新文件或完整覆盖已有文件。修改已有文件优先使用 edit_file；覆盖已有文件前必须先 read_file。父目录不存在时会自动创建。",
		false,
		func(ctx context.Context, in writeArgs, c *tool.Call) (*tool.Result, error) {
			p, err := resolvePath(c.Env, opts, in.Path)
			if err != nil {
				return tool.Errorf("%v", err), nil
			}
			if err := checkReadBeforeEdit(c, opts, p); err != nil {
				return tool.Errorf("%v", err), nil
			}
			_, statErr := os.Stat(p)
			created := os.IsNotExist(statErr)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return tool.Errorf("创建目录失败：%v", err), nil
			}
			if err := os.WriteFile(p, []byte(in.Content), 0o644); err != nil {
				return tool.Errorf("写入失败：%v", err), nil
			}
			c.Env.Files.Wrote(p, "write")
			verb := "已覆盖"
			if created {
				verb = "已创建"
			}
			n := strings.Count(in.Content, "\n") + 1
			return &tool.Result{
				Content: fmt.Sprintf("%s %s（%d 行，%d 字节）", verb, display(c.Env, p), n, len(in.Content)),
				Meta:    map[string]any{"path": display(c.Env, p), "op": "write", "created": created},
			}, nil
		})
}

// ---------- edit_file ----------

type editArgs struct {
	Path       string `json:"path" desc:"文件路径"`
	OldString  string `json:"old_string" desc:"要被替换的原文（必须与文件内容逐字一致，包括缩进）"`
	NewString  string `json:"new_string" desc:"替换后的新内容"`
	ReplaceAll bool   `json:"replace_all,omitempty" desc:"是否替换所有匹配，默认只允许唯一匹配"`
}

// EditFile 通过"精确字符串替换"修改文件——这是 Claude Code 等工具采用的方式，
// 比整文件重写更省 token，也更不容易误伤其它内容。
func EditFile(opts Options) tool.Tool {
	return tool.Func("edit_file",
		"对文件做精确字符串替换。old_string 必须在文件中唯一出现（否则提供更多上下文，或设置 replace_all）。old_string 为空且文件不存在时会创建文件。",
		false,
		func(ctx context.Context, in editArgs, c *tool.Call) (*tool.Result, error) {
			p, err := resolvePath(c.Env, opts, in.Path)
			if err != nil {
				return tool.Errorf("%v", err), nil
			}
			data, err := os.ReadFile(p)
			if os.IsNotExist(err) && in.OldString == "" {
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					return tool.Errorf("创建目录失败：%v", err), nil
				}
				if err := os.WriteFile(p, []byte(in.NewString), 0o644); err != nil {
					return tool.Errorf("写入失败：%v", err), nil
				}
				c.Env.Files.Wrote(p, "write")
				return &tool.Result{Content: "已创建 " + display(c.Env, p), Meta: map[string]any{"path": display(c.Env, p), "op": "write", "created": true}}, nil
			}
			if err != nil {
				return tool.Errorf("读取失败：%v", err), nil
			}
			if err := checkReadBeforeEdit(c, opts, p); err != nil {
				return tool.Errorf("%v", err), nil
			}
			if in.OldString == in.NewString {
				return tool.Errorf("old_string 与 new_string 相同，无需修改"), nil
			}
			content := string(data)
			count := strings.Count(content, in.OldString)
			switch {
			case in.OldString == "":
				return tool.Errorf("old_string 不能为空（文件已存在）"), nil
			case count == 0:
				return tool.Errorf("在 %s 中没有找到 old_string。请先 read_file 确认原文（注意缩进与空白）", display(c.Env, p)), nil
			case count > 1 && !in.ReplaceAll:
				return tool.Errorf("old_string 在文件中出现了 %d 次，请提供更多上下文使其唯一，或设置 replace_all=true", count), nil
			}
			var updated string
			if in.ReplaceAll {
				updated = strings.ReplaceAll(content, in.OldString, in.NewString)
			} else {
				updated = strings.Replace(content, in.OldString, in.NewString, 1)
			}
			if err := os.WriteFile(p, []byte(updated), 0o644); err != nil {
				return tool.Errorf("写入失败：%v", err), nil
			}
			c.Env.Files.Wrote(p, "edit")
			line := strings.Count(content[:strings.Index(content, in.OldString)], "\n") + 1
			n := 1
			if in.ReplaceAll {
				n = count
			}
			return &tool.Result{
				Content: fmt.Sprintf("已修改 %s：替换 %d 处（首处位于第 %d 行）", display(c.Env, p), n, line),
				Meta:    map[string]any{"path": display(c.Env, p), "op": "edit", "line": line, "old": in.OldString, "new": in.NewString},
			}, nil
		})
}

// ---------- list_dir ----------

type listArgs struct {
	Path  string `json:"path,omitempty" desc:"目录路径，默认工作目录"`
	Depth int    `json:"depth,omitempty" desc:"递归深度，默认 2，最大 5"`
}

// ListDir 以树形列出目录。
func ListDir(opts Options) tool.Tool {
	return tool.Func("list_dir", "以树形结构列出目录内容（自动跳过 .git、node_modules 等）。", true,
		func(ctx context.Context, in listArgs, c *tool.Call) (*tool.Result, error) {
			if in.Path == "" {
				in.Path = "."
			}
			p, err := resolvePath(c.Env, opts, in.Path)
			if err != nil {
				return tool.Errorf("%v", err), nil
			}
			depth := in.Depth
			if depth <= 0 {
				depth = 2
			}
			depth = min(depth, 5)
			var sb strings.Builder
			count := 0
			var walk func(dir, prefix string, level int)
			walk = func(dir, prefix string, level int) {
				entries, err := os.ReadDir(dir)
				if err != nil {
					return
				}
				sort.Slice(entries, func(i, j int) bool {
					if entries[i].IsDir() != entries[j].IsDir() {
						return entries[i].IsDir()
					}
					return entries[i].Name() < entries[j].Name()
				})
				for _, e := range entries {
					if count >= 500 {
						return
					}
					count++
					if e.IsDir() {
						sb.WriteString(prefix + e.Name() + "/\n")
						if level < depth && !skipDir(e.Name()) {
							walk(filepath.Join(dir, e.Name()), prefix+"  ", level+1)
						}
					} else {
						info, _ := e.Info()
						size := int64(0)
						if info != nil {
							size = info.Size()
						}
						fmt.Fprintf(&sb, "%s%s (%s)\n", prefix, e.Name(), humanSize(size))
					}
				}
			}
			walk(p, "", 1)
			c.Env.Files.Touch(p, "list")
			if count >= 500 {
				sb.WriteString("...（条目过多，已截断）\n")
			}
			if sb.Len() == 0 {
				return tool.Text("(空目录)"), nil
			}
			return &tool.Result{Content: display(c.Env, p) + "/\n" + sb.String(), Meta: map[string]any{"path": display(c.Env, p), "op": "list"}}, nil
		})
}

func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(n)/1024/1024)
	}
}

// ---------- glob ----------

type globArgs struct {
	Pattern string `json:"pattern" desc:"glob 模式，支持 **、*、?、{a,b}，例如 **/*.go"`
	Path    string `json:"path,omitempty" desc:"搜索根目录，默认工作目录"`
}

// GlobToRegexp 把 glob 模式转换为正则表达式。
func GlobToRegexp(pattern string) (*regexp.Regexp, error) {
	var sb strings.Builder
	sb.WriteString("^")
	inGroup := false
	for i := 0; i < len(pattern); i++ {
		ch := pattern[i]
		switch ch {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				if i+2 < len(pattern) && pattern[i+2] == '/' {
					sb.WriteString("(?:.*/)?")
					i += 2
				} else {
					sb.WriteString(".*")
					i++
				}
			} else {
				sb.WriteString("[^/]*")
			}
		case '?':
			sb.WriteString("[^/]")
		case '{':
			inGroup = true
			sb.WriteString("(?:")
		case '}':
			inGroup = false
			sb.WriteString(")")
		case ',':
			if inGroup {
				sb.WriteString("|")
			} else {
				sb.WriteString(",")
			}
		default:
			sb.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	sb.WriteString("$")
	return regexp.Compile(sb.String())
}

// Glob 按模式查找文件，结果按修改时间倒序。
func Glob(opts Options) tool.Tool {
	return tool.Func("glob", "按 glob 模式查找文件（如 **/*.go、cmd/*/main.go），结果按修改时间从新到旧排序，最多 200 条。", true,
		func(ctx context.Context, in globArgs, c *tool.Call) (*tool.Result, error) {
			if in.Path == "" {
				in.Path = "."
			}
			root, err := resolvePath(c.Env, opts, in.Path)
			if err != nil {
				return tool.Errorf("%v", err), nil
			}
			re, err := GlobToRegexp(in.Pattern)
			if err != nil {
				return tool.Errorf("无效的 glob 模式：%v", err), nil
			}
			type hit struct {
				path string
				mod  time.Time
			}
			var hits []hit
			_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err != nil || ctx.Err() != nil {
					return nil
				}
				if d.IsDir() {
					if p != root && skipDir(d.Name()) {
						return filepath.SkipDir
					}
					return nil
				}
				rel, _ := filepath.Rel(root, p)
				if re.MatchString(filepath.ToSlash(rel)) {
					info, _ := d.Info()
					var mod time.Time
					if info != nil {
						mod = info.ModTime()
					}
					hits = append(hits, hit{p, mod})
				}
				return nil
			})
			sort.Slice(hits, func(i, j int) bool { return hits[i].mod.After(hits[j].mod) })
			c.Env.Files.Touch(root, "search")
			if len(hits) == 0 {
				return tool.Text("没有匹配的文件"), nil
			}
			var sb strings.Builder
			for i, h := range hits {
				if i >= 200 {
					fmt.Fprintf(&sb, "...（共 %d 个，仅显示前 200 个）\n", len(hits))
					break
				}
				sb.WriteString(display(c.Env, h.path) + "\n")
			}
			return &tool.Result{Content: sb.String(), Meta: map[string]any{"op": "search", "matches": len(hits)}}, nil
		})
}

// ---------- grep ----------

type grepArgs struct {
	Pattern    string `json:"pattern" desc:"正则表达式（RE2 语法）"`
	Path       string `json:"path,omitempty" desc:"搜索的文件或目录，默认工作目录"`
	Include    string `json:"include,omitempty" desc:"只搜索匹配该 glob 的文件，如 *.go 或 **/*.{ts,tsx}"`
	IgnoreCase bool   `json:"ignore_case,omitempty" desc:"忽略大小写"`
	Context    int    `json:"context,omitempty" desc:"显示匹配行前后的行数，默认 0"`
	MaxResults int    `json:"max_results,omitempty" desc:"最多返回多少条匹配，默认 100"`
}

// Grep 在文件内容中搜索正则。
func Grep(opts Options) tool.Tool {
	return tool.Func("grep", "在文件内容中按正则搜索，输出 文件:行号: 内容。用于定位函数定义、引用、配置项等。", true,
		func(ctx context.Context, in grepArgs, c *tool.Call) (*tool.Result, error) {
			if in.Path == "" {
				in.Path = "."
			}
			root, err := resolvePath(c.Env, opts, in.Path)
			if err != nil {
				return tool.Errorf("%v", err), nil
			}
			pat := in.Pattern
			if in.IgnoreCase {
				pat = "(?i)" + pat
			}
			re, err := regexp.Compile(pat)
			if err != nil {
				return tool.Errorf("无效的正则：%v", err), nil
			}
			var inc *regexp.Regexp
			if in.Include != "" {
				g := in.Include
				if !strings.Contains(g, "/") {
					g = "**/" + g
				}
				if inc, err = GlobToRegexp(g); err != nil {
					return tool.Errorf("无效的 include：%v", err), nil
				}
			}
			limit := in.MaxResults
			if limit <= 0 {
				limit = 100
			}
			var sb strings.Builder
			matches, files := 0, 0
			searchFile := func(p string) {
				f, err := os.Open(p)
				if err != nil {
					return
				}
				defer f.Close()
				head := make([]byte, 512)
				n, _ := f.Read(head)
				if bytes.IndexByte(head[:n], 0) >= 0 {
					return // 跳过二进制
				}
				_, _ = f.Seek(0, 0)
				sc := bufio.NewScanner(f)
				sc.Buffer(make([]byte, 64*1024), 1024*1024)
				var lines []string
				for sc.Scan() {
					lines = append(lines, sc.Text())
				}
				hit := false
				for i, line := range lines {
					if matches >= limit {
						return
					}
					if !re.MatchString(line) {
						continue
					}
					hit = true
					matches++
					rel := display(c.Env, p)
					if in.Context > 0 {
						from, to := max(0, i-in.Context), min(len(lines)-1, i+in.Context)
						for j := from; j <= to; j++ {
							sep := "-"
							if j == i {
								sep = ":"
							}
							fmt.Fprintf(&sb, "%s%s%d%s %s\n", rel, sep, j+1, sep, clip(lines[j]))
						}
						sb.WriteString("--\n")
					} else {
						fmt.Fprintf(&sb, "%s:%d: %s\n", rel, i+1, clip(line))
					}
				}
				if hit {
					files++
				}
			}
			info, err := os.Stat(root)
			if err != nil {
				return tool.Errorf("%v", err), nil
			}
			if !info.IsDir() {
				searchFile(root)
			} else {
				_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
					if err != nil || ctx.Err() != nil || matches >= limit {
						return nil
					}
					if d.IsDir() {
						if p != root && skipDir(d.Name()) {
							return filepath.SkipDir
						}
						return nil
					}
					if inc != nil {
						rel, _ := filepath.Rel(root, p)
						if !inc.MatchString(filepath.ToSlash(rel)) {
							return nil
						}
					}
					searchFile(p)
					return nil
				})
			}
			c.Env.Files.Touch(root, "search")
			if matches == 0 {
				return tool.Text("没有找到匹配内容"), nil
			}
			if matches >= limit {
				fmt.Fprintf(&sb, "...（已达到 %d 条上限，请缩小范围）\n", limit)
			}
			return &tool.Result{Content: tool.Truncate(sb.String(), opts.MaxOutput), Meta: map[string]any{"op": "search", "matches": matches, "files": files}}, nil
		})
}

func clip(s string) string {
	if len(s) > 300 {
		return cutUTF8(s, 300) + "…"
	}
	return s
}

// cutUTF8 返回 s 的前 n 个字节，并回退到完整的 UTF-8 字符边界，避免产生乱码。
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
