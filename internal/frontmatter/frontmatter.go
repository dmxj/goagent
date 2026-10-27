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
func Parse(text string) (map[string]string, string) {
	meta := map[string]string{}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return meta, text
	}
	rest := text[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return meta, text
	}
	lastKey := ""
	for _, line := range strings.Split(rest[:end], "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		// 多行列表项： "  - item"
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "- ") && lastKey != "" {
			item := unquote(strings.TrimSpace(t[2:]))
			if meta[lastKey] == "" {
				meta[lastKey] = item
			} else {
				meta[lastKey] += ", " + item
			}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		lastKey = strings.TrimSpace(k)
		meta[lastKey] = unquote(strings.TrimSpace(v))
	}
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
