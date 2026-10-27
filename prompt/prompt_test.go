package prompt

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmxj/goagent/agent"
	"github.com/dmxj/goagent/memory"
	"github.com/dmxj/goagent/skill"
	"github.com/dmxj/goagent/subagent"
	"github.com/dmxj/goagent/tool"
)

func dummy(name string) tool.Tool {
	return tool.Func(name, "", true, func(context.Context, struct{}, *tool.Call) (*tool.Result, error) { return nil, nil })
}

func TestBuild(t *testing.T) {
	store, _ := memory.NewStore(t.TempDir())
	b := &Builder{
		WorkDir:      "/work",
		Instructions: []memory.Instruction{{Path: "/work/AGENTS.md", Content: "  使用中文注释  "}},
		Memory:       store,
		Skills:       []skill.Skill{{Name: "go-review", Description: "审查 Go 代码"}},
		Subagents:    []subagent.Definition{{Name: "explore", Description: "只读探索"}},
		MCPNotes:     map[string]string{"zeta": "Z 的说明", "alpha": "A 的说明", "empty": "  "},
		Extra:        "额外内容",
		Now:          func() time.Time { return time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC) },
	}
	full := &agent.Agent{Model: "m1", Tools: tool.NewRegistry(dummy("read_file"), dummy("skill"), dummy("task"), dummy("memory_save"))}
	out := b.Build(full)
	for _, want := range []string{
		"你是 GoAgent", "工作目录：/work", "当前日期：2026-10-27 Tuesday", "模型：m1",
		"memory_save, read_file, skill, task",
		"**go-review**: 审查 Go 代码", "**explore**: 只读探索",
		`<instructions path="/work/AGENTS.md">` + "\n使用中文注释\n</instructions>",
		"目前还没有任何记忆", "额外内容",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("系统提示词应包含 %q：\n%s", want, out)
		}
	}
	if strings.Index(out, "MCP 服务器 alpha") > strings.Index(out, "MCP 服务器 zeta") || strings.Contains(out, "MCP 服务器 empty") {
		t.Errorf("MCP 说明应按名称排序且跳过空说明")
	}
	if b.Build(full) != out {
		t.Errorf("同样的输入应生成完全相同的提示词（提示词缓存依赖这一点）")
	}

	// 只描述 Agent 真正拥有的能力
	bare := &agent.Agent{Model: "m2", Tools: tool.NewRegistry(dummy("read_file"))}
	out = b.Build(bare)
	for _, not := range []string{"# 技能", "# 子 Agent", "# 长期记忆"} {
		if strings.Contains(out, not) {
			t.Errorf("没有对应工具时不应包含 %q", not)
		}
	}
	if _, err := store.Save(memory.Note{Name: "偏好", Description: "简洁", Content: "x"}); err != nil {
		t.Fatal(err)
	}
	if out := b.Build(full); !strings.Contains(out, "当前记忆索引") || !strings.Contains(out, "[偏好]") {
		t.Errorf("有记忆时应注入索引")
	}
	custom := &Builder{Identity: "自定义身份"}
	if out := custom.Build(&agent.Agent{}); !strings.HasPrefix(out, "自定义身份") || strings.Contains(out, "# 工具") {
		t.Errorf("自定义身份或无工具时输出错误：%s", out)
	}
}
