// Package session 管理对话会话：消息历史、用量统计与持久化（支持断点续聊）。
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dmxj/goagent/llm"
)

// Session 是一次多轮对话的完整状态。
type Session struct {
	ID        string        `json:"id"`
	Title     string        `json:"title"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	Model     string        `json:"model"`
	Messages  []llm.Message `json:"messages"`
	Usage     llm.Usage     `json:"usage"`
	// Summary 是上下文压缩后保留的历史摘要。
	Summary string `json:"summary,omitempty"`
	// Compactions 记录压缩次数。
	Compactions int `json:"compactions,omitempty"`
	// Parent 是父会话 ID（子 Agent 的会话）。
	Parent string `json:"parent,omitempty"`

	mu sync.Mutex
}

// Snapshot 返回会话当前状态的一致快照（用于 HTTP 接口返回完整历史）。
func (s *Session) Snapshot() *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := &Session{ID: s.ID, Title: s.Title, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt, Model: s.Model,
		Messages: append([]llm.Message(nil), s.Messages...), Usage: s.Usage, Summary: s.Summary, Compactions: s.Compactions, Parent: s.Parent}
	return cp
}

// NewID 生成形如 20261027-a1b2c3 的会话 ID（时间前缀便于排序）。
func NewID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// New 创建空会话。
func New() *Session {
	now := time.Now()
	return &Session{ID: NewID(), CreatedAt: now, UpdatedAt: now}
}

// Append 追加消息。
func (s *Session) Append(msgs ...llm.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = append(s.Messages, msgs...)
	s.UpdatedAt = time.Now()
	if s.Title == "" {
		for _, m := range msgs {
			if m.Role == llm.RoleUser {
				if t := strings.TrimSpace(m.Text()); t != "" {
					s.Title = TitleFrom(t)
					break
				}
			}
		}
	}
}

// History 返回消息历史的拷贝。
func (s *Session) History() []llm.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]llm.Message(nil), s.Messages...)
}

// Replace 用新的历史替换（上下文压缩时使用）。
func (s *Session) Replace(msgs []llm.Message, summary string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = msgs
	s.Summary = summary
	s.Compactions++
	s.UpdatedAt = time.Now()
}

// AddUsage 累加用量。
func (s *Session) AddUsage(u llm.Usage) {
	s.mu.Lock()
	s.Usage.Add(u)
	s.mu.Unlock()
}

// Clear 清空历史（保留 ID）。
func (s *Session) Clear() {
	s.mu.Lock()
	s.Messages, s.Summary = nil, ""
	s.mu.Unlock()
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Info 是会话列表中的摘要信息。
type Info struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Messages  int       `json:"messages"`
	Model     string    `json:"model,omitempty"`
	Usage     llm.Usage `json:"usage"`
}

// Info 返回会话的摘要信息。
func (s *Session) Info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Info{ID: s.ID, Title: s.Title, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt, Messages: len(s.Messages), Model: s.Model, Usage: s.Usage}
}

// TitleFrom 根据文本生成会话标题（第一条用户消息的前 40 个字符，换行替换为空格）。
func TitleFrom(text string) string { return truncate(strings.TrimSpace(text), 40) }

// Store 是会话存储接口。
type Store interface {
	Save(s *Session) error
	Load(id string) (*Session, error)
	List() ([]Info, error)
	Delete(id string) error
}

// ErrNotFound 表示会话不存在。
var ErrNotFound = errors.New("会话不存在")

// MemoryStore 是内存实现（测试或无需持久化时使用）。
type MemoryStore struct {
	mu sync.Mutex
	m  map[string]*Session
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{m: map[string]*Session{}} }

func (ms *MemoryStore) Save(s *Session) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.m[s.ID] = s
	return nil
}

func (ms *MemoryStore) Load(id string) (*Session, error) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	s, ok := ms.m[id]
	if !ok {
		return nil, ErrNotFound
	}
	return s, nil
}

func (ms *MemoryStore) List() ([]Info, error) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	var out []Info
	for _, s := range ms.m {
		out = append(out, s.Info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

func (ms *MemoryStore) Delete(id string) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	delete(ms.m, id)
	return nil
}

// FileStore 把每个会话保存为一个 JSON 文件。写入采用"临时文件 + rename"保证原子性，
// 进程崩溃也不会留下半个文件。
type FileStore struct {
	Dir string
	mu  sync.Mutex
}

// NewFileStore 创建文件存储。
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &FileStore{Dir: dir}, nil
}

func (fs *FileStore) path(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return "", fmt.Errorf("非法会话 ID %q", id)
	}
	return filepath.Join(fs.Dir, id+".json"), nil
}

func (fs *FileStore) Save(s *Session) error {
	p, err := fs.path(s.ID)
	if err != nil {
		return err
	}
	// 持久化的永远是"配对完整"的历史：工具执行期间保存时，尚未得到结果的调用在文件里
	// 先以"被中断"的结果占位。进程此刻崩溃，恢复后的会话也能直接继续；
	// 正常执行完后的下一次保存会写入真实结果。内存中的历史不受影响。
	cp := s.Snapshot()
	cp.Messages, _ = RepairMessages(cp.Messages)
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return writeFileAtomic(p, b)
}

// writeFileAtomic 先写临时文件并 fsync，再 rename 覆盖目标，最后 fsync 目录，
// 保证断电后要么是旧文件、要么是完整的新文件。
func writeFileAtomic(p string, b []byte) error {
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(p)); err == nil {
		_ = d.Sync() // Windows 上目录不支持 Sync，忽略错误
		d.Close()
	}
	return nil
}

func (fs *FileStore) Load(id string) (*Session, error) {
	p, err := fs.path(id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("会话文件损坏: %w", err)
	}
	// 旧版本（或其它程序）写入的会话可能以悬空的 tool_use 结尾，加载时修复
	s.Repair()
	if s.Title == "" {
		for _, m := range s.Messages {
			if m.Role == llm.RoleUser {
				if t := strings.TrimSpace(m.Text()); t != "" {
					s.Title = TitleFrom(t)
					break
				}
			}
		}
	}
	return &s, nil
}

func (fs *FileStore) List() ([]Info, error) {
	entries, err := os.ReadDir(fs.Dir)
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		s, err := fs.Load(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		out = append(out, s.Info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

func (fs *FileStore) Delete(id string) error {
	p, err := fs.path(id)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
