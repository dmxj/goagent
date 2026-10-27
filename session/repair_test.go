package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmxj/goagent/llm"
)

func call(id string) llm.ContentBlock {
	return llm.ContentBlock{Type: llm.BlockToolUse, ID: id, Name: "bash", Input: json.RawMessage(`{}`)}
}

func result(id string) llm.ContentBlock {
	return llm.ContentBlock{Type: llm.BlockToolResult, ToolUseID: id, Content: "ok"}
}

func asst(blocks ...llm.ContentBlock) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: blocks}
}

func user(blocks ...llm.ContentBlock) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: blocks}
}

var text = llm.ContentBlock{Type: llm.BlockText, Text: "继续"}

// validate 是配对校验器：每个 tool_use 恰好在下一条 user 消息里有结果，结果在文字之前，没有孤儿结果。
func validate(t *testing.T, msgs []llm.Message) {
	t.Helper()
	for i, m := range msgs {
		if m.Role != llm.RoleAssistant {
			continue
		}
		calls := m.ToolUses()
		if len(calls) == 0 {
			continue
		}
		if i+1 >= len(msgs) || msgs[i+1].Role != llm.RoleUser {
			t.Fatalf("第 %d 条助手消息的调用后面没有 user 消息", i)
		}
		next := msgs[i+1].Content
		for j, c := range calls {
			if j >= len(next) || next[j].Type != llm.BlockToolResult || next[j].ToolUseID != c.ID {
				t.Fatalf("调用 %s 没有在下一条消息的第 %d 个位置找到结果：%+v", c.ID, j, next)
			}
		}
	}
	for i, m := range msgs {
		if m.Role != llm.RoleUser {
			continue
		}
		var prev []llm.ContentBlock
		if i > 0 {
			prev = msgs[i-1].ToolUses()
		}
		ids := map[string]bool{}
		for _, c := range prev {
			ids[c.ID] = true
		}
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult && !ids[b.ToolUseID] {
				t.Fatalf("第 %d 条消息中有孤儿结果 %s", i, b.ToolUseID)
			}
		}
	}
}

func TestRepairMessages(t *testing.T) {
	tests := []struct {
		name        string
		in          []llm.Message
		wantChanged bool
		wantLen     int
		check       func(t *testing.T, out []llm.Message)
	}{
		{
			name:    "完整历史不修改",
			in:      []llm.Message{llm.UserText("hi"), asst(call("a")), user(result("a")), llm.AssistantText("done")},
			wantLen: 4,
		},
		{
			name:        "kill -9：以悬空 tool_use 结尾",
			in:          []llm.Message{llm.UserText("hi"), asst(text, call("a"), call("b"))},
			wantChanged: true, wantLen: 3,
			check: func(t *testing.T, out []llm.Message) {
				last := out[2]
				if len(last.Content) != 2 || !last.Content[0].IsError || last.Content[0].Content != InterruptedResult || last.Content[1].ToolUseID != "b" {
					t.Errorf("应为每个调用补上被中断的错误结果：%+v", last)
				}
			},
		},
		{
			name:        "部分结果缺失",
			in:          []llm.Message{llm.UserText("hi"), asst(call("a"), call("b")), user(result("b"))},
			wantChanged: true, wantLen: 3,
			check: func(t *testing.T, out []llm.Message) {
				c := out[2].Content
				if c[0].ToolUseID != "a" || !c[0].IsError || c[1].ToolUseID != "b" || c[1].IsError {
					t.Errorf("应补上 a 的结果并保持调用顺序：%+v", c)
				}
			},
		},
		{
			name:        "悬空调用后紧接着用户新消息",
			in:          []llm.Message{llm.UserText("hi"), asst(call("a")), llm.UserText("还在吗")},
			wantChanged: true, wantLen: 3,
			check: func(t *testing.T, out []llm.Message) {
				c := out[2].Content
				if c[0].Type != llm.BlockToolResult || c[1].Text != "还在吗" {
					t.Errorf("结果应插在用户文字之前：%+v", c)
				}
			},
		},
		{
			name:        "两条助手消息相连",
			in:          []llm.Message{llm.UserText("hi"), asst(call("a")), llm.AssistantText("?")},
			wantChanged: true, wantLen: 4,
		},
		{
			name:        "孤儿结果被移除",
			in:          []llm.Message{user(result("ghost"), text), llm.AssistantText("ok")},
			wantChanged: true, wantLen: 2,
			check: func(t *testing.T, out []llm.Message) {
				if len(out[0].Content) != 1 || out[0].Content[0].Text != "继续" {
					t.Errorf("孤儿结果应被删除：%+v", out[0])
				}
			},
		},
		{
			name:        "结果在文字之后被重新排序",
			in:          []llm.Message{llm.UserText("hi"), asst(call("a")), user(text, result("a"))},
			wantChanged: true, wantLen: 3,
		},
		{
			name:        "重复结果去重",
			in:          []llm.Message{llm.UserText("hi"), asst(call("a")), user(result("a"), result("a"))},
			wantChanged: true, wantLen: 3,
		},
		{
			name:        "只有孤儿结果的消息用文字占位",
			in:          []llm.Message{user(result("ghost"))},
			wantChanged: true, wantLen: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, _ := json.Marshal(tt.in)
			out, changed := RepairMessages(tt.in)
			if changed != tt.wantChanged {
				t.Errorf("changed = %v，期望 %v", changed, tt.wantChanged)
			}
			if len(out) != tt.wantLen {
				t.Fatalf("修复后 %d 条消息，期望 %d：%+v", len(out), tt.wantLen, out)
			}
			validate(t, out)
			if tt.check != nil {
				tt.check(t, out)
			}
			after, _ := json.Marshal(tt.in)
			if string(before) != string(after) {
				t.Errorf("RepairMessages 不应修改输入")
			}
		})
	}
}

func TestFileStorePersistsRepairedAndLoadRepairs(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := New()
	s.Append(llm.UserText("跑一下测试"), asst(call("a")))
	if err := fs.Save(s); err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 2 {
		t.Errorf("Save 不应修改内存中的历史")
	}
	b, _ := os.ReadFile(filepath.Join(dir, s.ID+".json"))
	if !strings.Contains(string(b), "工具调用被中断") {
		t.Errorf("持久化的历史应当配对完整：%s", b)
	}
	// 模拟 v0.1.0 写入的悬空会话
	raw := `{"id":"old","messages":[{"role":"user","content":[{"type":"text","text":"你好\n世界"}]},{"role":"assistant","content":[{"type":"tool_use","id":"x","name":"bash","input":{}}]}]}`
	_ = os.WriteFile(filepath.Join(dir, "old.json"), []byte(raw), 0o644)
	got, err := fs.Load("old")
	if err != nil {
		t.Fatal(err)
	}
	validate(t, got.Messages)
	if len(got.Messages) != 3 || got.Title != "你好 世界" {
		t.Errorf("加载时应修复悬空调用并补标题：%d 条，标题 %q", len(got.Messages), got.Title)
	}
	if _, err := os.Stat(filepath.Join(dir, s.ID+".json.tmp")); !os.IsNotExist(err) {
		t.Errorf("不应留下临时文件")
	}
	list, _ := fs.List()
	if len(list) != 2 || list[0].CreatedAt.IsZero() && list[1].CreatedAt.IsZero() {
		t.Errorf("List 应返回会话摘要：%+v", list)
	}
}
