package app

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// readText 读取文本文件并去掉首尾空白。
func readText(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// snapshot 是一组路径的"修改指纹"：路径 → 修改时间与大小。不存在的路径记为 missing，
// 这样新建文件（例如新增一个技能目录、第一次创建 AGENTS.md）也能被发现。
type snapshot map[string]string

const (
	maxWalkDepth   = 4
	maxWalkEntries = 4000
)

// fingerprint 用轮询 mtime 的方式计算指纹（零依赖，跨平台）。目录会递归到有限深度；
// 目录中指向目录的符号链接（例如链接进 skills/ 的共享技能）会被跟随一层，链接目标里的修改也能发现。
func fingerprint(paths []string) snapshot {
	snap := snapshot{}
	entries := 0
	for _, root := range paths {
		info, err := os.Stat(root)
		if err != nil {
			snap[root] = "missing"
			continue
		}
		snap[root] = stamp(info)
		if info.IsDir() {
			walk(root, root, snap, &entries, true)
		}
	}
	return snap
}

// walk 把 dir 下的条目记入 snap，键使用 base 下的路径（跟随链接时 base 是链接本身的路径）。
func walk(dir, base string, snap snapshot, entries *int, follow bool) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() && (strings.Count(rel, string(filepath.Separator)) >= maxWalkDepth || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		if *entries++; *entries > maxWalkEntries {
			return filepath.SkipAll
		}
		key := filepath.Join(base, rel)
		if d.Type()&fs.ModeSymlink != 0 {
			fi, err := os.Stat(p)
			if err != nil {
				snap[key] = "dangling"
				return nil
			}
			snap[key] = stamp(fi)
			if real, err := filepath.EvalSymlinks(p); err == nil && fi.IsDir() && follow {
				walk(real, key, snap, entries, false) // 只跟随一层，避免链接成环
			}
			return nil
		}
		if fi, err := d.Info(); err == nil {
			snap[key] = stamp(fi)
		}
		return nil
	})
}

func stamp(fi os.FileInfo) string {
	if fi.IsDir() {
		return "dir:" + fi.ModTime().UTC().Format("20060102150405.000000000")
	}
	return fi.ModTime().UTC().Format("20060102150405.000000000") + ":" + itoa(fi.Size())
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// changedFrom 判断与旧指纹相比是否有变化。full 为 false 时只比较 s 中出现的路径（旧指纹可能包含更多路径）。
func (s snapshot) changedFrom(old snapshot, full bool) bool {
	if full {
		return !s.equal(old)
	}
	for k, v := range s {
		if old[k] != v {
			return true
		}
	}
	return false
}

// diffFrom 返回变化的路径；full 为 false 时只考虑 s 中出现的路径。
func (s snapshot) diffFrom(old snapshot, full bool) []string {
	if full {
		return s.diff(old)
	}
	sub := snapshot{}
	for k := range s {
		sub[k] = old[k]
	}
	return s.diff(sub)
}

func (s snapshot) equal(o snapshot) bool {
	if len(s) != len(o) {
		return false
	}
	for k, v := range s {
		if o[k] != v {
			return false
		}
	}
	return true
}

// diff 返回发生变化的路径（排序，最多 20 个）。
func (s snapshot) diff(old snapshot) []string {
	var out []string
	for k, v := range s {
		if old[k] != v {
			out = append(out, k)
		}
	}
	for k := range old {
		if _, ok := s[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}
