// Package memory 负责 Agent 的"记忆"，分三层：
//
//  1. 项目指令（Instructions）：AGENTS.md / GOAGENT.md 等文件，每次会话都加载进系统提示词；
//  2. 长期记忆（Store）：Agent 通过 memory_* 工具主动保存/检索的笔记，跨会话持久存在；
//  3. 短期记忆（会话历史）：由 session 包保存，超长时由 Compactor 压缩成摘要。
package memory

import (
	"os"
	"path/filepath"
	"strings"
)

// InstructionFiles 是会被自动加载的项目指令文件名（按优先级）。
var InstructionFiles = []string{"AGENTS.md", "GOAGENT.md", "CLAUDE.md"}

// Instruction 是一个已加载的指令文件。
type Instruction struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// LoadInstructions 从工作目录向上查找指令文件（直到 git 根目录或文件系统根），
// 再加上用户级 ~/.goagent/AGENTS.md。越靠近工作目录的文件越靠后（优先级越高）。
func LoadInstructions(workDir, userDir string) []Instruction {
	var found []Instruction
	seen := map[string]bool{}
	add := func(p string) {
		if seen[p] {
			return
		}
		b, err := os.ReadFile(p)
		if err != nil || len(strings.TrimSpace(string(b))) == 0 {
			return
		}
		seen[p] = true
		found = append(found, Instruction{Path: p, Content: string(b)})
	}
	if userDir != "" {
		add(filepath.Join(userDir, "AGENTS.md"))
	}
	dir, _ := filepath.Abs(workDir)
	var chain []string
	for {
		chain = append(chain, dir)
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// 从最外层到最内层，保证内层覆盖外层
	for i := len(chain) - 1; i >= 0; i-- {
		for _, name := range InstructionFiles {
			p := filepath.Join(chain[i], name)
			if _, err := os.Stat(p); err == nil {
				add(p)
				break // 同一目录只取优先级最高的一个
			}
		}
	}
	return found
}
