// Package frontmatter 解析/生成 Markdown 文件头部的 YAML 风格元数据：
//
//	---
//	name: code-review
//	description: 审查代码
//	tools: [read_file, grep]
//	---
//	正文……
//
// 只支持 "key: value" 单行与 [a, b] 简单列表，足以覆盖 Skill / Agent 定义文件，
// 这样框架无需引入 YAML 依赖。
package frontmatter

import (
	"sort"
	"strings"
)

// Parse 返回元数据与正文。没有 frontmatter 时元数据为空 map。
//
// 支持的 YAML 子集：
//   - key: value 单行值（可带引号）
//   - [a, b] 与 "  - item" 两种列表
//   - key: > / key: | 多行块（> 折叠为一行，| 保留换行）
//   - 嵌套映射（metadata:\n  author: x）以 "metadata.author" 的形式保存，不会覆盖顶层同名键
//
// 文件开头的 UTF-8 BOM 与空行会被忽略。
func Parse(text string) (map[string]string, string) {
	meta := map[string]string{}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	trimmed := strings.TrimLeft(strings.TrimPrefix(text, "\ufeff"), "\n")
	if !strings.HasPrefix(trimmed, "---\n") {
		return meta, text
	}
	rest := trimmed[4:]
	end := strings.Index(rest, "\n---")
	if strings.HasPrefix(rest, "---") { // 空 frontmatter："---\n---"
		rest, end = "\n"+rest, 0
	}
	if end < 0 {
		return meta, text
	}
	var (
		lastKey   string
		blockKey  string // 正在读取多行块的键
		blockFold bool
		block     []string
	)
	flush := func() {
		if blockKey == "" {
			return
		}
		sep := "\n"
		if blockFold {
			sep = " "
		}
		meta[blockKey] = strings.TrimSpace(strings.Join(block, sep))
		blockKey, block = "", nil
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		indented := strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
		t := strings.TrimSpace(line)
		if blockKey != "" {
			if indented || t == "" {
				block = append(block, t)
				continue
			}
			flush()
		}
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		// 多行列表项： "  - item"
		if strings.HasPrefix(t, "- ") && lastKey != "" {
			item := unquote(strings.TrimSpace(t[2:]))
			if meta[lastKey] == "" {
				meta[lastKey] = item
			} else {
				meta[lastKey] += ", " + item
			}
			continue
		}
		k, v, ok := strings.Cut(t, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if indented && lastKey != "" {
			// 嵌套映射：保存为 parent.child，不覆盖顶层键
			parent := strings.SplitN(lastKey, ".", 2)[0]
			meta[parent+"."+k] = unquote(v)
			continue
		}
		lastKey = k
		switch v {
		case ">", ">-", ">+", "|", "|-", "|+":
			blockKey, blockFold = k, strings.HasPrefix(v, ">")
			meta[k] = ""
			continue
		}
		meta[k] = unquote(v)
	}
	flush()
	body := rest[end+4:]
	body = strings.TrimPrefix(body, "\n")
	return meta, body
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

// List 把 "[a, b]" 或 "a, b" 解析为字符串切片。
func List(v string) []string {
	v = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(v), "["), "]"))
	if v == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = unquote(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Render 生成带 frontmatter 的文本（键按字母序，保证输出稳定）。
func Render(meta map[string]string, body string) string {
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString("---\n")
	for _, k := range keys {
		v := strings.ReplaceAll(meta[k], "\n", " ")
		sb.WriteString(k + ": " + v + "\n")
	}
	sb.WriteString("---\n\n")
	sb.WriteString(strings.TrimSpace(body))
	sb.WriteString("\n")
	return sb.String()
}
