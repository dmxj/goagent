// Package skill 实现 Skills（技能）系统——兼容 Anthropic Agent Skills 规范：
//
//	skills/
//	  pdf-report/
//	    SKILL.md        ← frontmatter 中的 name/description + 正文指令
//	    scripts/...     ← 技能附带的脚本、模板等资源
//
// 核心思想是"渐进式披露"（progressive disclosure）：
// 系统提示词里只放每个技能的 name + description（几十个 token），
// 模型判断需要时调用 skill 工具，才把完整的 SKILL.md 正文加载进上下文。
package skill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dmxj/goagent/internal/frontmatter"
	"github.com/dmxj/goagent/tool"
)

// Skill 是一个已发现的技能。
type Skill struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Dir          string   `json:"dir"`
	Path         string   `json:"path"`
	AllowedTools []string `json:"allowed_tools,omitempty"`
	Source       string   `json:"source"` // project | user | builtin
}

// Body 读取技能正文（去掉 frontmatter）。
func (s Skill) Body() (string, error) {
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return "", err
	}
	_, body := frontmatter.Parse(string(b))
	return strings.TrimSpace(body), nil
}

// Dir 描述一个技能搜索目录。
type Dir struct {
	Path   string
	Source string
}

// Discover 扫描目录，返回技能列表。后出现的目录中同名技能会覆盖之前的（项目级覆盖用户级）。
func Discover(dirs ...Dir) ([]Skill, []error) {
	byName := map[string]Skill{}
	var errs []error
	for _, d := range dirs {
		entries, err := os.ReadDir(d.Path)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			p := filepath.Join(d.Path, e.Name(), "SKILL.md")
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			meta, _ := frontmatter.Parse(string(b))
			name := meta["name"]
			if name == "" {
				name = e.Name()
			}
			if meta["description"] == "" {
				errs = append(errs, fmt.Errorf("技能 %s 缺少 description，已跳过", p))
				continue
			}
			byName[name] = Skill{
				Name: name, Description: meta["description"], Dir: filepath.Join(d.Path, e.Name()), Path: p,
				AllowedTools: frontmatter.List(meta["allowed-tools"]), Source: d.Source,
			}
		}
	}
	out := make([]Skill, 0, len(byName))
	for _, s := range byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errs
}

// PromptSection 生成注入系统提示词的技能清单。
func PromptSection(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("你可以使用以下技能（Skills）。当用户的任务与某个技能的描述匹配时，先调用 skill 工具加载该技能的完整说明，再严格按说明执行：\n\n")
	for _, s := range skills {
		fmt.Fprintf(&sb, "- **%s**: %s\n", s.Name, s.Description)
	}
	return sb.String()
}

type skillArgs struct {
	Name string `json:"name" desc:"要加载的技能名称"`
}

// Tool 返回 skill 工具：按名称加载技能正文。
func Tool(skills []Skill) tool.Tool {
	byName := map[string]Skill{}
	var names []string
	for _, s := range skills {
		byName[s.Name] = s
		names = append(names, s.Name)
	}
	desc := "加载一个技能（Skill）的完整说明。可用技能：" + strings.Join(names, ", ")
	return tool.Func("skill", desc, true, func(ctx context.Context, in skillArgs, c *tool.Call) (*tool.Result, error) {
		s, ok := byName[in.Name]
		if !ok {
			return tool.Errorf("技能 %q 不存在，可用技能：%s", in.Name, strings.Join(names, ", ")), nil
		}
		body, err := s.Body()
		if err != nil {
			return tool.Errorf("读取技能失败：%v", err), nil
		}
		c.Env.Files.Read(s.Path)
		var res strings.Builder
		fmt.Fprintf(&res, "# 技能：%s\n\n技能目录（引用的脚本/资源文件都相对于此目录）：%s\n\n%s", s.Name, s.Dir, body)
		if len(s.AllowedTools) > 0 {
			fmt.Fprintf(&res, "\n\n（该技能建议使用的工具：%s）", strings.Join(s.AllowedTools, ", "))
		}
		return &tool.Result{Content: res.String(), Meta: map[string]any{"skill": s.Name, "path": s.Path, "op": "read"}}, nil
	})
}
