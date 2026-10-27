package memory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dmxj/goagent/llm"
	"github.com/dmxj/goagent/tool"
)

func TestStore(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Index() != "" || len(s.List()) != 0 {
		t.Errorf("新建的记忆库应为空")
	}
	if _, err := s.Save(Note{Name: "", Content: "x"}); err == nil {
		t.Errorf("name 为空应报错")
	}
	if _, err := s.Save(Note{Name: "x", Content: "  "}); err == nil {
		t.Errorf("content 为空应报错")
	}
	file, err := s.Save(Note{Name: "Go 代码风格", Description: "用户偏好的 Go 写法", Type: "user", Content: "错误信息使用中文；表驱动测试"})
	if err != nil {
		t.Fatal(err)
	}
	if file != "go-代码风格.md" {
		t.Errorf("文件名 slug 错误：%s", file)
	}
	if _, err := s.Save(Note{Name: "部署流程", Description: "如何发布", Content: "先跑 make test 再 make build；部署到 staging"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(Note{Name: "!!!", Description: "怪名字", Content: "内容"}); err != nil {
		t.Fatal(err)
	}

	n, err := s.Get("Go 代码风格")
	if err != nil || n.Type != "user" || n.Description != "用户偏好的 Go 写法" || !strings.Contains(n.Content, "表驱动测试") || n.UpdatedAt.IsZero() {
		t.Errorf("Get 结果错误：%+v %v", n, err)
	}
	if n, _ := s.Get("部署流程"); n.Type != "project" {
		t.Errorf("默认类型应为 project")
	}
	if _, err := s.Get("不存在"); err == nil {
		t.Errorf("读取不存在的记忆应报错")
	}

	idx := s.Index()
	for _, want := range []string{"# Memory Index", "[Go 代码风格](go-代码风格.md) (user) — 用户偏好的 Go 写法", "[部署流程]", "(note.md)"} {
		if !strings.Contains(idx, want) {
			t.Errorf("索引应包含 %q：\n%s", want, idx)
		}
	}

	tests := []struct {
		query string
		limit int
		want  []string
	}{
		{"make", 5, []string{"部署流程"}},
		{"go", 5, []string{"Go 代码风格"}},
		{"中文 部署", 5, []string{"Go 代码风格", "部署流程"}},
		{"", 2, nil}, // 空查询列出全部（受 limit 限制）
		{"不存在的关键词", 5, nil},
	}
	for _, tt := range tests {
		t.Run("Search "+tt.query, func(t *testing.T) {
			got := s.Search(tt.query, tt.limit)
			if tt.query == "" {
				if len(got) != 2 {
					t.Errorf("空查询应返回 limit 条，实际 %d", len(got))
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("Search(%q) 返回 %d 条，期望 %v", tt.query, len(got), tt.want)
			}
			names := map[string]bool{}
			for _, n := range got {
				names[n.Name] = true
			}
			for _, w := range tt.want {
				if !names[w] {
					t.Errorf("Search(%q) 应包含 %s", tt.query, w)
				}
			}
		})
	}
	// 名称/描述命中权重更高
	if got := s.Search("发布", 5); len(got) == 0 || got[0].Name != "部署流程" {
		t.Errorf("描述命中应排在前面：%+v", got)
	}

	if err := s.Delete("部署流程"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.Index(), "部署流程") {
		t.Errorf("删除后索引应更新")
	}
	if err := s.Delete("部署流程"); err == nil {
		t.Errorf("删除不存在的记忆应报错")
	}
}

func TestStoreConcurrentSave(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = s.Save(Note{Name: "note" + string(rune('a'+i)), Content: "c"})
		}(i)
	}
	wg.Wait()
	if len(s.List()) != 10 || strings.Count(s.Index(), "\n- ") != 10 {
		t.Errorf("并发保存后应有 10 条记忆")
	}
}

func runTool(t *testing.T, tl tool.Tool, input any) *tool.Result {
	t.Helper()
	b, _ := json.Marshal(input)
	res, err := tl.Run(context.Background(), &tool.Call{Input: b})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestMemoryTools(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	tools := map[string]tool.Tool{}
	for _, tl := range s.Tools() {
		tools[tl.Info().Name] = tl
	}
	if len(tools) != 3 || !tools["memory_search"].Info().ReadOnly || tools["memory_save"].Info().ReadOnly {
		t.Fatalf("记忆工具定义错误")
	}
	if res := runTool(t, tools["memory_save"], map[string]any{"name": "偏好", "description": "d", "content": "喜欢简洁"}); res.IsError || !strings.Contains(res.Content, "已保存") {
		t.Errorf("memory_save 失败：%+v", res)
	}
	if res := runTool(t, tools["memory_save"], map[string]any{"name": "", "content": ""}); !res.IsError {
		t.Errorf("参数为空应报错")
	}
	if res := runTool(t, tools["memory_search"], map[string]any{"query": "简洁"}); !strings.Contains(res.Content, "## 偏好") {
		t.Errorf("memory_search 应返回全文：%s", res.Content)
	}
	if res := runTool(t, tools["memory_search"], map[string]any{"query": "无关"}); res.Content != "没有相关记忆" {
		t.Errorf("无结果提示错误：%s", res.Content)
	}
	if res := runTool(t, tools["memory_delete"], map[string]any{"name": "偏好"}); res.IsError {
		t.Errorf("memory_delete 失败：%s", res.Content)
	}
	if res := runTool(t, tools["memory_delete"], map[string]any{"name": "偏好"}); !res.IsError {
		t.Errorf("重复删除应报错")
	}
}

func TestLoadInstructions(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	sub := filepath.Join(repo, "pkg", "sub")
	user := filepath.Join(root, "home", ".goagent")
	for _, d := range []string{sub, user, filepath.Join(repo, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "AGENTS.md"), "git 根目录之外，不应加载")
	write(filepath.Join(user, "AGENTS.md"), "用户级")
	write(filepath.Join(repo, "AGENTS.md"), "仓库级")
	write(filepath.Join(repo, "CLAUDE.md"), "同目录低优先级，不应加载")
	write(filepath.Join(repo, "pkg", "GOAGENT.md"), "pkg 级")
	write(filepath.Join(sub, "AGENTS.md"), "   \n") // 空白文件跳过

	got := LoadInstructions(sub, user)
	var contents []string
	for _, in := range got {
		contents = append(contents, in.Content)
	}
	want := []string{"用户级", "仓库级", "pkg 级"}
	if strings.Join(contents, "|") != strings.Join(want, "|") {
		t.Errorf("加载顺序 = %v，期望 %v", contents, want)
	}
	if got := LoadInstructions(sub, ""); len(got) != 2 {
		t.Errorf("不传用户目录时应只加载项目文件：%d", len(got))
	}
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		s        string
		min, max int
	}{
		{"", 1, 1},
		{strings.Repeat("a", 400), 100, 102},
		{strings.Repeat("中", 300), 190, 210},
	}
	for _, tt := range tests {
		if got := EstimateTokens(tt.s); got < tt.min || got > tt.max {
			t.Errorf("EstimateTokens(%d 字节) = %d，期望 [%d,%d]", len(tt.s), got, tt.min, tt.max)
		}
	}
	msgs := []llm.Message{llm.UserText(strings.Repeat("a", 400)), {Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ToolCall("1", "read_file", map[string]string{"path": "x"})}}}
	if n := EstimateMessages(msgs); n < 100 {
		t.Errorf("EstimateMessages 过小：%d", n)
	}
}

// 构造一段带工具调用的对话：user → assistant(tool_use) → user(tool_result) → assistant 文本，重复 rounds 轮。
func conversation(rounds int) []llm.Message {
	var msgs []llm.Message
	for i := 0; i < rounds; i++ {
		id := string(rune('a' + i))
		msgs = append(msgs,
			llm.UserText("问题 "+id),
			llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ToolCall(id, "read_file", map[string]string{"path": id})}},
			llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: id, Content: "内容 " + id}}},
			llm.AssistantText("回答 "+id),
		)
	}
	return msgs
}

func TestSplitPoint(t *testing.T) {
	msgs := conversation(4) // 用户输入位于下标 0,4,8,12
	tests := []struct {
		name string
		msgs []llm.Message
		keep int
		want int
	}{
		{"保留最近 2 轮", msgs, 2, 8},
		{"保留最近 1 轮", msgs, 1, 12},
		{"keep<=0 使用默认 2", msgs, 0, 8},
		{"轮次不足时保留最后一轮", msgs, 10, 12},
		{"只有一轮无法压缩", conversation(1), 2, 0},
		{"空历史", nil, 2, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SplitPoint(tt.msgs, tt.keep)
			if got != tt.want {
				t.Fatalf("SplitPoint = %d，期望 %d", got, tt.want)
			}
			if got > 0 && !isUserTurn(tt.msgs[got]) {
				t.Errorf("切分点必须落在用户文字输入上")
			}
		})
	}
	// 关键性质：任意切分点都不会把 tool_use 与 tool_result 拆开
	for keep := 1; keep <= 4; keep++ {
		i := SplitPoint(msgs, keep)
		if i > 0 && len(msgs[i].Content) > 0 && msgs[i].Content[0].Type == llm.BlockToolResult {
			t.Errorf("keep=%d 时切分点落在 tool_result 上", keep)
		}
		if i > 0 && len(msgs[i-1].ToolUses()) > 0 {
			t.Errorf("keep=%d 时切分点前一条是 tool_use，tool_result 被拆到了另一侧", keep)
		}
	}
}

func TestShouldCompact(t *testing.T) {
	var nilC *Compactor
	if nilC.ShouldCompact("", conversation(10), 0) {
		t.Errorf("nil Compactor 不应压缩")
	}
	c := &Compactor{ContextWindow: 1000, Threshold: 0.5}
	small := []llm.Message{llm.UserText("hi")}
	big := []llm.Message{llm.UserText(strings.Repeat("a", 4000))}
	tests := []struct {
		name string
		c    *Compactor
		msgs []llm.Message
		last int
		want bool
	}{
		{"短历史不压缩", c, small, 0, false},
		{"估算超过阈值", c, big, 0, true},
		{"实际输入 token 超过阈值", c, small, 600, true},
		{"非法阈值回退到 0.8", &Compactor{ContextWindow: 1000, Threshold: 2}, []llm.Message{llm.UserText(strings.Repeat("a", 3000))}, 0, false},
		{"默认窗口 128000", &Compactor{}, big, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.c.ShouldCompact("", tt.msgs, tt.last); got != tt.want {
				t.Errorf("ShouldCompact = %v，期望 %v", got, tt.want)
			}
		})
	}
}

func TestCompact(t *testing.T) {
	var gotReq *llm.Request
	mock := &llm.MockProvider{Handler: func(req *llm.Request) (llm.Message, error) {
		gotReq = req
		return llm.AssistantText("  ## 用户目标\n读取文件  "), nil
	}}
	c := &Compactor{Provider: mock, Model: "m", KeepRecent: 1}
	msgs := conversation(3)
	out, summary, err := c.Compact(context.Background(), msgs, "旧摘要")
	if err != nil {
		t.Fatal(err)
	}
	if summary != "## 用户目标\n读取文件" {
		t.Errorf("摘要应去掉首尾空白：%q", summary)
	}
	if len(out) != 2+4 {
		t.Fatalf("压缩后应为 摘要+确认+最近 1 轮(4 条)，实际 %d 条", len(out))
	}
	if !strings.Contains(out[0].Text(), summary) || out[1].Role != llm.RoleAssistant || out[2].Text() != "问题 c" {
		t.Errorf("压缩后结构错误：%+v", out[:3])
	}
	input := gotReq.Messages[0].Text()
	for _, want := range []string{"【之前的摘要】\n旧摘要", "[user] 问题 a", "[assistant 调用工具 read_file]", "[工具结果] 内容 b"} {
		if !strings.Contains(input, want) {
			t.Errorf("发给摘要模型的内容应包含 %q：\n%s", want, input)
		}
	}
	if strings.Contains(input, "问题 c") {
		t.Errorf("保留的消息不应被摘要")
	}
	if gotReq.Model != "m" || !strings.Contains(gotReq.System, "对话压缩助手") {
		t.Errorf("摘要请求参数错误")
	}

	if _, _, err := c.Compact(context.Background(), conversation(1), ""); err == nil {
		t.Errorf("历史太短时应报错")
	}
	failing := &Compactor{Provider: &llm.MockProvider{Handler: func(*llm.Request) (llm.Message, error) { return llm.Message{}, errors.New("down") }}}
	if got, _, err := failing.Compact(context.Background(), msgs, ""); err == nil || len(got) != len(msgs) {
		t.Errorf("模型出错时应返回原消息与错误")
	}
}

func TestRenderAndClip(t *testing.T) {
	msgs := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockToolResult, Content: "失败", IsError: true}}}}
	if !strings.Contains(Render(msgs), "[工具错误] 失败") {
		t.Errorf("错误的工具结果应标注")
	}
	if out := clip(strings.Repeat("中", 10), 4); out != "中…(截断)" {
		t.Errorf("clip 应在字符边界截断：%q", out)
	}
}
