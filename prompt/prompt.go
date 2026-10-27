// Package prompt 负责组装系统提示词。
//
// 系统提示词是 Agent 的"操作手册"，由多个片段拼接而成：
// 身份与行为准则 → 环境信息 → 工具使用规范 → 技能清单 → 子 Agent 清单 → 项目指令（AGENTS.md）→ 长期记忆索引。
// 每次调用模型前动态生成，因此 Agent 新保存的记忆会立即生效。
package prompt

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/dmxj/goagent/agent"
	"github.com/dmxj/goagent/memory"
	"github.com/dmxj/goagent/skill"
	"github.com/dmxj/goagent/subagent"
)

// DefaultIdentity 是默认的身份与行为准则。
const DefaultIdentity = `你是 GoAgent，一个运行在用户电脑上的通用 AI 助手，擅长软件工程任务：阅读和编写代码、调试、运行命令、查资料、整理文档。

# 工作方式
- 先理解再动手：修改代码前先阅读相关文件，遵循项目已有的风格与约定。
- 复杂任务（3 步以上）先用 todo_write 列出计划，执行中实时更新状态。
- 能并行的只读操作（读多个文件、多处搜索）放在同一次回复里一起调用。
- 修改文件优先用 edit_file 做精确替换；修改后用 bash 运行构建或测试验证。
- 遇到错误先分析根因，不要盲目重试同一个操作。
- 有风险或不可逆的操作（删除文件、强制推送、修改生产配置）先征得用户同意。

# 回复风格
- 简洁直接，用中文回答（代码与命令保持原样）。
- 引用代码位置时使用 文件路径:行号 的格式。
- 最终回复说明做了什么、结果如何，不要重复工具输出的大段内容。`

// Builder 组装系统提示词，实现 agent.PromptBuilder。
type Builder struct {
	Identity     string
	WorkDir      string
	Instructions []memory.Instruction
	Memory       *memory.Store
	Skills       []skill.Skill
	Subagents    []subagent.Definition
	MCPNotes     map[string]string // MCP 服务器名 -> 服务器提供的使用说明
	Extra        string            // 追加到末尾的自定义内容
	Now          func() time.Time
	// Sections 开关内置章节（environment、tools、skills、subagents、mcp、instructions、memory），
	// 值为 false 的章节不输出；未出现的章节默认输出。
	Sections map[string]bool
	// ExtraSections 是配置中追加的自定义章节，按顺序输出在内置章节之后、Extra 之前。
	ExtraSections []Section
}

// Section 是一个自定义提示词章节。
type Section struct {
	Title   string
	Content string
}

// 内置章节名。
const (
	SectionEnvironment  = "environment"
	SectionTools        = "tools"
	SectionSkills       = "skills"
	SectionSubagents    = "subagents"
	SectionMCP          = "mcp"
	SectionInstructions = "instructions"
	SectionMemory       = "memory"
)

func (b *Builder) on(name string) bool {
	v, ok := b.Sections[name]
	return !ok || v
}

// Build 为指定 Agent 生成系统提示词：只描述该 Agent 真正拥有的能力。
func (b *Builder) Build(a *agent.Agent) string {
	var sb strings.Builder
	id := b.Identity
	if id == "" {
		id = DefaultIdentity
	}
	sb.WriteString(id)

	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	if b.on(SectionEnvironment) {
		host, _ := os.Hostname()
		fmt.Fprintf(&sb, "\n\n# 环境\n- 工作目录：%s\n- 操作系统：%s/%s\n- 主机：%s\n- 当前日期：%s\n- 模型：%s\n",
			b.WorkDir, runtime.GOOS, runtime.GOARCH, host, now().Format("2006-01-02 Monday"), a.Model)
	}

	has := func(name string) bool {
		_, ok := a.Tools.Get(name)
		return ok
	}
	if names := a.Tools.Names(); len(names) > 0 && b.on(SectionTools) {
		fmt.Fprintf(&sb, "\n# 工具\n你可以调用这些工具：%s。工具的输出可能被截断；文件路径可以是相对工作目录的路径。\n", strings.Join(names, ", "))
	}
	if has("skill") && b.on(SectionSkills) {
		if s := skill.PromptSection(b.Skills); s != "" {
			sb.WriteString("\n# 技能\n" + s)
		}
	}
	if has("task") && b.on(SectionSubagents) {
		if s := subagent.PromptSection(b.Subagents); s != "" {
			sb.WriteString("\n# 子 Agent\n" + s)
		}
	}
	// map 遍历顺序随机，排序后输出：系统提示词保持稳定才能命中模型的提示词缓存
	mcpNames := make([]string, 0, len(b.MCPNotes))
	for name := range b.MCPNotes {
		mcpNames = append(mcpNames, name)
	}
	sort.Strings(mcpNames)
	for _, name := range mcpNames {
		if note := b.MCPNotes[name]; strings.TrimSpace(note) != "" && b.on(SectionMCP) {
			fmt.Fprintf(&sb, "\n# MCP 服务器 %s 的说明\n%s\n", name, note)
		}
	}
	if len(b.Instructions) > 0 && b.on(SectionInstructions) {
		sb.WriteString("\n# 项目指令\n以下指令来自用户的 AGENTS.md 等文件，必须遵守（越靠后优先级越高）：\n")
		for _, in := range b.Instructions {
			fmt.Fprintf(&sb, "\n<instructions path=%q>\n%s\n</instructions>\n", in.Path, strings.TrimSpace(in.Content))
		}
	}
	if b.Memory != nil && has("memory_save") && b.on(SectionMemory) {
		sb.WriteString("\n# 长期记忆\n你拥有跨会话的长期记忆。发现用户偏好、项目背景、用户纠正你的做法时，用 memory_save 记录；需要时用 memory_search 检索全文。")
		if idx := b.Memory.Index(); idx != "" {
			sb.WriteString("当前记忆索引：\n\n" + idx + "\n")
		} else {
			sb.WriteString("目前还没有任何记忆。\n")
		}
	}
	for _, sec := range b.ExtraSections {
		if strings.TrimSpace(sec.Content) == "" {
			continue
		}
		if sec.Title != "" {
			fmt.Fprintf(&sb, "\n# %s\n", sec.Title)
		}
		sb.WriteString(strings.TrimSpace(sec.Content) + "\n")
	}
	if b.Extra != "" {
		sb.WriteString("\n" + b.Extra + "\n")
	}
	return sb.String()
}
