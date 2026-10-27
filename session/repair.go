package session

import (
	"github.com/dmxj/goagent/llm"
)

// InterruptedResult 是为悬空工具调用补上的错误结果内容。
const InterruptedResult = "工具调用被中断：进程在执行这个工具期间退出（例如被 kill -9 或断电），没有得到结果。如有需要请重新执行。"

// RepairMessages 修复消息历史中的工具调用配对问题，返回修复后的副本与是否做了修改。
//
// 模型 API 要求：助手消息中的每个 tool_use，都必须在紧随其后的 user 消息里有对应的 tool_result，
// 并且 tool_result 放在文字之前；也不允许出现没有对应调用的 tool_result。
// 进程在执行工具期间崩溃时，保存下来的会话会以"有调用、没结果"的助手消息结尾，恢复后每次请求都被拒绝。
//
// 修复规则：
//  1. 缺少结果的 tool_use：补一个 IsError=true、内容为 InterruptedResult 的结果；
//     下一条不是 user 消息（或已经是末尾）时插入一条新的 user 消息；
//  2. 找不到对应 tool_use 的 tool_result：删除（整条消息因此变空时用一段说明文字占位）；
//  3. 结果块统一移到 user 消息的最前面，顺序与调用顺序一致。
func RepairMessages(msgs []llm.Message) ([]llm.Message, bool) {
	out := make([]llm.Message, 0, len(msgs)+1)
	changed := false
	var pending []llm.ContentBlock // 上一条助手消息中的 tool_use
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if m.Role == llm.RoleUser {
			fixed, c := fixUser(m, pending)
			changed = changed || c
			pending = nil
			out = append(out, fixed)
			continue
		}
		if len(pending) > 0 { // 助手消息之后又是助手消息：上一轮的调用没有结果
			out = append(out, llm.Message{Role: llm.RoleUser, Content: interrupted(pending)})
			changed = true
		}
		out = append(out, m)
		pending = m.ToolUses()
	}
	if len(pending) > 0 {
		out = append(out, llm.Message{Role: llm.RoleUser, Content: interrupted(pending)})
		changed = true
	}
	if !changed {
		return msgs, false
	}
	return out, true
}

func interrupted(calls []llm.ContentBlock) []llm.ContentBlock {
	out := make([]llm.ContentBlock, 0, len(calls))
	for _, c := range calls {
		out = append(out, llm.ContentBlock{Type: llm.BlockToolResult, ToolUseID: c.ID, Content: InterruptedResult, IsError: true})
	}
	return out
}

// fixUser 让一条 user 消息与上一条助手消息的工具调用配对。
func fixUser(m llm.Message, calls []llm.ContentBlock) (llm.Message, bool) {
	results := map[string]llm.ContentBlock{}
	var others []llm.ContentBlock
	changed := false
	firstNonResult := -1
	for i, b := range m.Content {
		if b.Type == llm.BlockToolResult {
			if firstNonResult >= 0 {
				changed = true // 结果块出现在文字之后
			}
			if _, dup := results[b.ToolUseID]; dup {
				changed = true
				continue
			}
			results[b.ToolUseID] = b
			continue
		}
		if firstNonResult < 0 {
			firstNonResult = i
		}
		others = append(others, b)
	}
	var content []llm.ContentBlock
	for _, c := range calls {
		if r, ok := results[c.ID]; ok {
			content = append(content, r)
			delete(results, c.ID)
		} else {
			content = append(content, interrupted([]llm.ContentBlock{c})...)
			changed = true
		}
	}
	if len(results) > 0 { // 孤儿结果：没有对应的调用
		changed = true
	}
	if len(calls) > 0 && len(content) > 0 && !changed {
		// 调用顺序与结果顺序不一致也视为修改（重新排列）
		j := 0
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult {
				if j >= len(content) || content[j].ToolUseID != b.ToolUseID {
					changed = true
					break
				}
				j++
			}
		}
	}
	if !changed {
		return m, false
	}
	content = append(content, others...)
	if len(content) == 0 {
		content = []llm.ContentBlock{{Type: llm.BlockText, Text: "（已移除无法配对的工具结果）"}}
	}
	return llm.Message{Role: m.Role, Content: content}, true
}

// Repair 原地修复会话的消息历史，返回是否做了修改。
func (s *Session) Repair() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	fixed, changed := RepairMessages(s.Messages)
	if changed {
		s.Messages = fixed
	}
	return changed
}
