// Package builtin 提供 Agent 常用的内置工具：文件读写编辑、目录与搜索、
// Shell 命令、网页抓取、待办清单。
package builtin

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmxj/goagent/tool"
)

// Options 控制内置工具的行为。
type Options struct {
	// RestrictToWorkDir 为 true 时，文件工具只能访问工作目录内的路径。
	RestrictToWorkDir bool
	// RequireReadBeforeEdit 为 true 时，修改已存在的文件前必须先 read_file。
	RequireReadBeforeEdit bool
	// BashTimeout 是 bash 默认超时。
	BashTimeout time.Duration
	// MaxOutput 是工具输出给模型的最大字节数。
	MaxOutput int
	// Shell 默认 /bin/bash（不存在时回退 /bin/sh）。
	Shell string
}

// DefaultOptions 返回推荐配置。
func DefaultOptions() Options {
	return Options{
		RestrictToWorkDir:     true,
		RequireReadBeforeEdit: true,
		BashTimeout:           2 * time.Minute,
		MaxOutput:             30000,
	}
}

// All 返回全部内置工具。
func All(opts Options) []tool.Tool {
	if opts.MaxOutput <= 0 {
		opts.MaxOutput = 30000
	}
	if opts.BashTimeout <= 0 {
		opts.BashTimeout = 2 * time.Minute
	}
	return []tool.Tool{
		ReadFile(opts),
		WriteFile(opts),
		EditFile(opts),
		ListDir(opts),
		Glob(opts),
		Grep(opts),
		Bash(opts),
		WebFetch(opts),
		TodoWrite(),
	}
}

// resolvePath 把模型给出的路径解析为绝对路径，并做越界检查。
func resolvePath(env *tool.Env, opts Options, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("路径不能为空")
	}
	wd := "."
	if env != nil && env.WorkDir != "" {
		wd = env.WorkDir
	}
	wd, _ = filepath.Abs(wd)
	if !filepath.IsAbs(p) {
		p = filepath.Join(wd, p)
	}
	p = filepath.Clean(p)
	if opts.RestrictToWorkDir {
		rel, err := filepath.Rel(wd, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("路径 %s 超出工作目录 %s，已拒绝访问", p, wd)
		}
	}
	return p, nil
}

// display 返回相对工作目录的展示路径。
func display(env *tool.Env, p string) string {
	if env == nil || env.WorkDir == "" {
		return p
	}
	wd, _ := filepath.Abs(env.WorkDir)
	if rel, err := filepath.Rel(wd, p); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return p
}

// skipDir 判断搜索时应跳过的目录。
func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", ".idea", ".vscode", "__pycache__", "dist", "bin", ".goagent":
		return true
	}
	return false
}
