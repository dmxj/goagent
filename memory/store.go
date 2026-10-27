package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dmxj/goagent/internal/frontmatter"
	"github.com/dmxj/goagent/tool"
)

// Note 是一条长期记忆。
type Note struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Type        string    `json:"type"` // user | feedback | project | reference
	Content     string    `json:"content"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Store 把记忆保存为目录下的 Markdown 文件（带 frontmatter），并维护 MEMORY.md 索引。
// 用文件而不是数据库，是为了让人类也能直接查看和编辑 Agent 的记忆。
type Store struct {
	Dir string
	mu  sync.Mutex
}

// NewStore 创建记忆存储。
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}

var slugRe = regexp.MustCompile(`[^a-zA-Z0-9\p{Han}_-]+`)

func slug(name string) string {
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-"), "-")
	if s == "" {
		s = "note"
	}
	return s
}

// Save 新建或覆盖一条记忆。
func (s *Store) Save(n Note) (string, error) {
	if strings.TrimSpace(n.Name) == "" || strings.TrimSpace(n.Content) == "" {
		return "", fmt.Errorf("name 和 content 不能为空")
	}
	if n.Type == "" {
		n.Type = "project"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	file := slug(n.Name) + ".md"
	// 不同的名称可能生成同一个文件名（"用户/偏好" 与 "用户 偏好"），直接覆盖会悄悄丢掉另一条记忆
	if b, err := os.ReadFile(filepath.Join(s.Dir, file)); err == nil {
		if old := parseNote(string(b)); old.Name != "" && old.Name != n.Name {
			return "", fmt.Errorf("记忆名称 %q 与已有的 %q 对应同一个文件 %s，请换一个名称，或使用完全相同的名称覆盖", n.Name, old.Name, file)
		}
	}
	body := frontmatter.Render(map[string]string{
		"name": n.Name, "description": n.Description, "type": n.Type,
		"updated": time.Now().Format(time.RFC3339),
	}, n.Content)
	if err := os.WriteFile(filepath.Join(s.Dir, file), []byte(body), 0o644); err != nil {
		return "", err
	}
	return file, s.rebuildIndexLocked()
}

// Get 按名称读取记忆（名称必须与保存时一致，避免因文件名相同取到另一条记忆）。
func (s *Store) Get(name string) (*Note, error) {
	b, err := os.ReadFile(filepath.Join(s.Dir, slug(name)+".md"))
	if err != nil {
		return nil, fmt.Errorf("记忆 %q 不存在", name)
	}
	n := parseNote(string(b))
	if n.Name != "" && n.Name != name {
		return nil, fmt.Errorf("记忆 %q 不存在（同名文件属于 %q）", name, n.Name)
	}
	return n, nil
}

// Delete 删除记忆。
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(filepath.Join(s.Dir, slug(name)+".md")); err != nil {
		return fmt.Errorf("删除失败: %w", err)
	}
	return s.rebuildIndexLocked()
}

func parseNote(text string) *Note {
	meta, body := frontmatter.Parse(text)
	n := &Note{Name: meta["name"], Description: meta["description"], Type: meta["type"], Content: strings.TrimSpace(body)}
	n.UpdatedAt, _ = time.Parse(time.RFC3339, meta["updated"])
	return n
}

// List 返回全部记忆（按更新时间倒序）。
func (s *Store) List() []Note {
	entries, _ := os.ReadDir(s.Dir)
	var out []Note
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || e.Name() == "MEMORY.md" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, *parseNote(string(b)))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// Search 用简单的关键词打分检索记忆（名称/描述命中权重更高）。
func (s *Store) Search(query string, limit int) []Note {
	terms := strings.Fields(strings.ToLower(query))
	type scored struct {
		n     Note
		score int
	}
	var hits []scored
	for _, n := range s.List() {
		head := strings.ToLower(n.Name + " " + n.Description)
		body := strings.ToLower(n.Content)
		score := 0
		for _, t := range terms {
			score += 3*strings.Count(head, t) + strings.Count(body, t)
		}
		if score > 0 || len(terms) == 0 {
			hits = append(hits, scored{n, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	var out []Note
	for i, h := range hits {
		if limit > 0 && i >= limit {
			break
		}
		out = append(out, h.n)
	}
	return out
}

func (s *Store) rebuildIndexLocked() error {
	var sb strings.Builder
	sb.WriteString("# Memory Index\n\n")
	for _, n := range s.List() {
		fmt.Fprintf(&sb, "- [%s](%s.md) (%s) — %s\n", n.Name, slug(n.Name), n.Type, n.Description)
	}
	return os.WriteFile(filepath.Join(s.Dir, "MEMORY.md"), []byte(sb.String()), 0o644)
}

// 索引注入系统提示词的上限：记忆很多时只列出最近更新的，其余提示用 memory_search 检索。
const (
	MaxIndexLines = 200
	MaxIndexBytes = 25 * 1024
)

// Index 返回 MEMORY.md 索引内容（注入系统提示词），超过 MaxIndexLines 行或 MaxIndexBytes 字节时截断。
// 索引按更新时间倒序排列，截断时保留的是最近更新的记忆。
func (s *Store) Index() string {
	b, err := os.ReadFile(filepath.Join(s.Dir, "MEMORY.md"))
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(string(b))
	lines := strings.Split(text, "\n")
	size, keep := 0, 0
	for i, l := range lines {
		if i >= MaxIndexLines || size+len(l)+1 > MaxIndexBytes {
			break
		}
		size += len(l) + 1
		keep = i + 1
	}
	if keep == len(lines) {
		return text
	}
	rest := 0
	for _, l := range lines[keep:] {
		if strings.HasPrefix(l, "- ") {
			rest++
		}
	}
	return strings.Join(lines[:keep], "\n") + fmt.Sprintf("\n\n（还有 %d 条较早的记忆未列出，可用 memory_search 检索）", rest)
}

// ---------- 记忆工具 ----------

type saveArgs struct {
	Name        string `json:"name" desc:"记忆名称（简短、唯一，同名会覆盖）"`
	Description string `json:"description" desc:"一句话描述，用于判断将来是否相关"`
	Type        string `json:"type,omitempty" enum:"user,feedback,project,reference" desc:"记忆类型"`
	Content     string `json:"content" desc:"记忆正文（Markdown）"`
}

type searchArgs struct {
	Query string `json:"query" desc:"关键词，空格分隔；为空时列出全部"`
}

type nameArgs struct {
	Name string `json:"name" desc:"记忆名称"`
}

// Tools 返回记忆相关的工具。
func (s *Store) Tools() []tool.Tool {
	return []tool.Tool{
		tool.Func("memory_save", "保存一条跨会话的长期记忆：用户偏好、项目背景、反馈、外部资源位置等。不要保存可以从代码直接得出的信息或临时状态。", false,
			func(ctx context.Context, in saveArgs, c *tool.Call) (*tool.Result, error) {
				file, err := s.Save(Note{Name: in.Name, Description: in.Description, Type: in.Type, Content: in.Content})
				if err != nil {
					return tool.Errorf("%v", err), nil
				}
				return &tool.Result{Content: "已保存记忆 " + file, Meta: map[string]any{"op": "write", "path": filepath.Join(s.Dir, file)}}, nil
			}),
		tool.Func("memory_search", "按关键词检索长期记忆，返回匹配记忆的全文。", true,
			func(ctx context.Context, in searchArgs, c *tool.Call) (*tool.Result, error) {
				notes := s.Search(in.Query, 5)
				if len(notes) == 0 {
					return tool.Text("没有相关记忆"), nil
				}
				var sb strings.Builder
				for _, n := range notes {
					fmt.Fprintf(&sb, "## %s (%s)\n%s\n\n%s\n\n", n.Name, n.Type, n.Description, n.Content)
				}
				return tool.Text(sb.String()), nil
			}),
		tool.Func("memory_delete", "删除一条已过时或错误的长期记忆。", false,
			func(ctx context.Context, in nameArgs, c *tool.Call) (*tool.Result, error) {
				if err := s.Delete(in.Name); err != nil {
					return tool.Errorf("%v", err), nil
				}
				return tool.Text("已删除记忆 " + in.Name), nil
			}),
	}
}
