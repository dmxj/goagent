package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/dmxj/goagent/llm"
)

// EstimateTokens 粗略估算文本 token 数：
// 英文约 4 字符/token，中文约 1.5 字符/token。用于决定何时压缩，不需要精确。
func EstimateTokens(s string) int {
	n := utf8.RuneCountInString(s)
	ascii := 0
	for i := 0; i < len(s); i++ {
		if s[i] < 0x80 {
			ascii++
		}
	}
	nonASCII := n - ascii
	return ascii/4 + nonASCII*2/3 + 1
}

// EstimateMessages 估算消息列表的 token 数。
func EstimateMessages(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		total += 4
		for _, b := range m.Content {
			total += EstimateTokens(b.Text) + EstimateTokens(b.Thinking) + EstimateTokens(b.Content) + EstimateTokens(string(b.Input)) + len(b.Name)/4
		}
	}
	return total
}

// Compactor 在上下文过长时，把较早的对话压缩成一段摘要。
//
// 策略：
//   - 当估算 token 超过 ContextWindow*Threshold 时触发；
//   - 保留最近 KeepRecent 轮"真正的用户输入"之后的消息原样不动；
//   - 更早的消息交给模型生成结构化摘要，以一条 user 消息的形式放回历史开头；
//   - 切分点必须落在"用户的文字输入"上，绝不能把 tool_use 与 tool_result 拆开。
type Compactor struct {
	Provider      llm.Provider
	Model         string
	ContextWindow int     // 模型上下文窗口（token），默认 128000
	Threshold     float64 // 触发比例，默认 0.8
	KeepRecent    int     // 保留最近几轮用户输入，默认 2
}

// ShouldCompact 判断是否需要压缩。lastInputTokens 是上一次调用实际消耗的输入 token（可为 0）。
func (c *Compactor) ShouldCompact(system string, msgs []llm.Message, lastInputTokens int) bool {
	if c == nil {
		return false
	}
	window := c.ContextWindow
	if window <= 0 {
		window = 128000
	}
	th := c.Threshold
	if th <= 0 || th >= 1 {
		th = 0.8
	}
	est := EstimateTokens(system) + EstimateMessages(msgs)
	if lastInputTokens > est {
		est = lastInputTokens
	}
	return float64(est) > float64(window)*th
}

// isUserTurn 判断消息是否为用户的文字输入（而不是工具结果）。
func isUserTurn(m llm.Message) bool {
	if m.Role != llm.RoleUser {
		return false
	}
	for _, b := range m.Content {
		if b.Type == llm.BlockToolResult {
			return false
		}
	}
	return true
}

// SplitPoint 返回压缩切分下标：msgs[:i] 被摘要，msgs[i:] 保留。返回 0 表示无法压缩。
func SplitPoint(msgs []llm.Message, keepRecent int) int {
	if keepRecent <= 0 {
		keepRecent = 2
	}
	seen := 0
	for i := len(msgs) - 1; i > 0; i-- {
		if isUserTurn(msgs[i]) {
			seen++
			if seen == keepRecent {
				return i
			}
		}
	}
	// 用户轮次不足时，退而求其次：保留最后一轮
	for i := len(msgs) - 1; i > 0; i-- {
		if isUserTurn(msgs[i]) {
			return i
		}
	}
	return 0
}

const compactPrompt = `你是对话压缩助手。请把下面的 Agent 对话历史压缩成一份结构化摘要，供 Agent 在后续工作中继续使用。
要求：
1. 保留：用户的原始目标与所有明确要求、已做出的关键决策、修改过的文件及原因、执行过的重要命令及结果、发现的错误与解决办法、尚未完成的待办事项。
2. 保留关键的代码标识符、文件路径、配置值，不要改写它们。
3. 省略：寒暄、重复的工具输出、已经无关的探索过程。
4. 使用以下 Markdown 结构输出：
## 用户目标
## 已完成的工作
## 关键信息（文件 / 命令 / 决策）
## 遇到的问题
## 待办与下一步`

// Render 把消息渲染为纯文本，供摘要模型阅读。
func Render(msgs []llm.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		for _, b := range m.Content {
			switch b.Type {
			case llm.BlockText:
				fmt.Fprintf(&sb, "[%s] %s\n", m.Role, b.Text)
			case llm.BlockToolUse:
				in, _ := json.Marshal(b.Input)
				fmt.Fprintf(&sb, "[assistant 调用工具 %s] %s\n", b.Name, clip(string(in), 2000))
			case llm.BlockToolResult:
				status := "结果"
				if b.IsError {
					status = "错误"
				}
				fmt.Fprintf(&sb, "[工具%s] %s\n", status, clip(b.Content, 3000))
			}
		}
	}
	return sb.String()
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) { // 不截断在 UTF-8 字符中间
		n--
	}
	return s[:n] + "…(截断)"
}

// Compact 执行压缩，返回新的消息列表与摘要文本。previousSummary 为之前的摘要（会被合并）。
func (c *Compactor) Compact(ctx context.Context, msgs []llm.Message, previousSummary string) ([]llm.Message, string, error) {
	i := SplitPoint(msgs, c.KeepRecent)
	if i <= 0 {
		return msgs, previousSummary, fmt.Errorf("历史太短，无法压缩")
	}
	old, keep := msgs[:i], msgs[i:]
	var input strings.Builder
	if previousSummary != "" {
		input.WriteString("【之前的摘要】\n" + previousSummary + "\n\n")
	}
	input.WriteString("【需要压缩的对话】\n")
	input.WriteString(Render(old))
	resp, err := llm.Complete(ctx, c.Provider, &llm.Request{
		Model:     c.Model,
		System:    compactPrompt,
		Messages:  []llm.Message{llm.UserText(input.String())},
		MaxTokens: 4096,
	})
	if err != nil {
		return msgs, previousSummary, fmt.Errorf("生成摘要失败: %w", err)
	}
	summary := strings.TrimSpace(resp.Message.Text())
	head := llm.UserText("以下是此前对话的摘要（早期消息已被压缩以节省上下文）：\n\n" + summary)
	ack := llm.AssistantText("好的，我已了解之前的进展，会基于摘要继续工作。")
	out := append([]llm.Message{head, ack}, keep...)
	return out, summary, nil
}
