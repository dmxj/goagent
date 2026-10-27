package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmxj/goagent/llm"
)

func TestSessionBasics(t *testing.T) {
	s := New()
	if s.ID == "" || s.CreatedAt.IsZero() {
		t.Fatalf("New 应生成 ID 与时间")
	}
	if New().ID == s.ID {
		t.Errorf("两次 New 的 ID 不应相同")
	}
	s.Append(llm.AssistantText("先说话的助手"))
	if s.Title != "" {
		t.Errorf("助手消息不应作为标题")
	}
	s.Append(llm.UserText("  第一行\n" + strings.Repeat("长", 60)))
	if !strings.HasPrefix(s.Title, "第一行 ") || !strings.HasSuffix(s.Title, "…") || len([]rune(s.Title)) != 41 {
		t.Errorf("标题应取首条用户消息并截断到 40 个字符：%q", s.Title)
	}
	s.Append(llm.UserText("第二条"))
	if strings.Contains(s.Title, "第二条") {
		t.Errorf("标题设置后不应再变化")
	}
	h := s.History()
	h[0] = llm.UserText("篡改")
	if s.Messages[0].Text() == "篡改" {
		t.Errorf("History 应返回拷贝")
	}
	s.AddUsage(llm.Usage{InputTokens: 3, OutputTokens: 4})
	s.AddUsage(llm.Usage{InputTokens: 1})
	if s.Usage.InputTokens != 4 || s.Usage.OutputTokens != 4 {
		t.Errorf("用量累加错误：%+v", s.Usage)
	}
	s.Replace([]llm.Message{llm.UserText("摘要")}, "summary")
	if len(s.Messages) != 1 || s.Summary != "summary" || s.Compactions != 1 {
		t.Errorf("Replace 错误：%+v", s)
	}
	s.Clear()
	if len(s.Messages) != 0 || s.Summary != "" {
		t.Errorf("Clear 应清空历史")
	}
}

func TestSessionConcurrentAppend(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Append(llm.UserText("x"))
			s.AddUsage(llm.Usage{InputTokens: 1})
			_ = s.History()
		}()
	}
	wg.Wait()
	if len(s.History()) != 50 || s.Usage.InputTokens != 50 {
		t.Errorf("并发追加丢失数据：%d 条消息，%d tokens", len(s.History()), s.Usage.InputTokens)
	}
}

// storeContract 是所有 Store 实现都应满足的行为。
func storeContract(t *testing.T, store Store) {
	t.Helper()
	if _, err := store.Load("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("加载不存在的会话应返回 ErrNotFound，实际 %v", err)
	}
	a := New()
	a.Append(llm.UserText("会话 A"), llm.AssistantText("回答"))
	a.AddUsage(llm.Usage{InputTokens: 10, OutputTokens: 5})
	b := New()
	b.Append(llm.UserText("会话 B"))
	b.UpdatedAt = a.UpdatedAt.Add(time.Second)
	for _, s := range []*Session{a, b} {
		if err := store.Save(s); err != nil {
			t.Fatalf("保存失败：%v", err)
		}
	}
	got, err := store.Load(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "会话 A" || len(got.Messages) != 2 || got.Usage.InputTokens != 10 || got.Messages[1].Text() != "回答" {
		t.Errorf("加载内容不一致：%+v", got)
	}
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != b.ID || list[1].Messages != 2 {
		t.Errorf("List 应按更新时间倒序：%+v", list)
	}
	if err := store.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除后应不存在")
	}
	if err := store.Delete(a.ID); err != nil {
		t.Errorf("重复删除不应报错：%v", err)
	}
}

func TestMemoryStore(t *testing.T) { storeContract(t, NewMemoryStore()) }

func TestFileStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	storeContract(t, fs)

	t.Run("非法 ID", func(t *testing.T) {
		for _, id := range []string{"", "../escape", "a/b", `a\b`, "..", "x..y"} {
			if err := fs.Save(&Session{ID: id}); err == nil {
				t.Errorf("Save(%q) 应拒绝非法 ID", id)
			}
			if _, err := fs.Load(id); err == nil {
				t.Errorf("Load(%q) 应拒绝非法 ID", id)
			}
			if err := fs.Delete(id); err == nil {
				t.Errorf("Delete(%q) 应拒绝非法 ID", id)
			}
		}
	})
	t.Run("损坏的文件", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := fs.Load("broken"); err == nil || !strings.Contains(err.Error(), "损坏") {
			t.Errorf("损坏的会话文件应报错：%v", err)
		}
		_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644)
		list, err := fs.List()
		if err != nil {
			t.Fatal(err)
		}
		for _, info := range list {
			if info.ID == "broken" {
				t.Errorf("List 应跳过损坏的文件")
			}
		}
	})
	t.Run("原子写入不留临时文件", func(t *testing.T) {
		s := New()
		s.Append(llm.UserText("x"))
		if err := fs.Save(s); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, s.ID+".json.tmp")); !os.IsNotExist(err) {
			t.Errorf("保存后不应残留 .tmp 文件")
		}
	})
	t.Run("目录不存在时 List 报错", func(t *testing.T) {
		bad := &FileStore{Dir: filepath.Join(t.TempDir(), "nope")}
		if _, err := bad.List(); err == nil {
			t.Errorf("目录不存在应报错")
		}
	})
}
