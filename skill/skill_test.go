package skill

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmxj/goagent/tool"
)

func writeSkill(t *testing.T, root, dir, content string) {
	t.Helper()
	p := filepath.Join(root, dir, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscover(t *testing.T) {
	user := t.TempDir()
	project := t.TempDir()
	writeSkill(t, user, "review", "---\nname: review\ndescription: 用户级审查\n---\n用户级正文")
	writeSkill(t, user, "pdf", "---\ndescription: 生成 PDF\nallowed-tools: [bash, write_file]\n---\n# PDF\n步骤")
	writeSkill(t, project, "review", "---\nname: review\ndescription: 项目级审查\n---\n项目级正文")
	writeSkill(t, project, "broken", "---\nname: broken\n---\n缺少描述")
	writeSkill(t, project, "plain", "没有 frontmatter")
	_ = os.WriteFile(filepath.Join(project, "README.md"), []byte("不是目录"), 0o644)
	_ = os.MkdirAll(filepath.Join(project, "empty"), 0o755)

	skills, errs := Discover(Dir{Path: user, Source: "user"}, Dir{Path: project, Source: "project"}, Dir{Path: filepath.Join(project, "nope"), Source: "x"})
	if len(errs) != 2 {
		t.Errorf("缺少 description 的技能应报告错误：%v", errs)
	}
	for _, e := range errs {
		if !strings.Contains(e.Error(), "缺少 description") {
			t.Errorf("错误信息不清晰：%v", e)
		}
	}
	if len(skills) != 2 {
		t.Fatalf("应发现 2 个技能，实际 %+v", skills)
	}
	if skills[0].Name != "pdf" || skills[1].Name != "review" {
		t.Errorf("技能应按名称排序：%+v", skills)
	}
	pdf, review := skills[0], skills[1]
	if pdf.Source != "user" || strings.Join(pdf.AllowedTools, ",") != "bash,write_file" {
		t.Errorf("目录名应作为默认名称，allowed-tools 应解析为列表：%+v", pdf)
	}
	if review.Source != "project" || review.Description != "项目级审查" {
		t.Errorf("项目级技能应覆盖同名用户级技能：%+v", review)
	}
	if body, err := review.Body(); err != nil || body != "项目级正文" {
		t.Errorf("Body 应去掉 frontmatter：%q %v", body, err)
	}
	if _, err := (Skill{Path: "/nonexistent"}).Body(); err == nil {
		t.Errorf("文件不存在时 Body 应报错")
	}

	section := PromptSection(skills)
	if !strings.Contains(section, "**pdf**: 生成 PDF") || !strings.Contains(section, "skill 工具") {
		t.Errorf("提示词片段错误：%s", section)
	}
	if PromptSection(nil) != "" {
		t.Errorf("没有技能时提示词片段应为空")
	}
}

func TestSkillTool(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "pdf", "---\nname: pdf\ndescription: 生成 PDF\nallowed-tools: bash\n---\n运行 scripts/make.sh")
	skills, _ := Discover(Dir{Path: root, Source: "project"})
	tl := Tool(skills)
	info := tl.Info()
	if info.Name != "skill" || !info.ReadOnly || !strings.Contains(info.Description, "pdf") {
		t.Fatalf("skill 工具定义错误：%+v", info)
	}
	files := tool.NewFileTracker()
	run := func(name string) *tool.Result {
		b, _ := json.Marshal(map[string]string{"name": name})
		res, err := tl.Run(context.Background(), &tool.Call{Input: b, Env: &tool.Env{Files: files}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := run("pdf")
	if res.IsError || !strings.Contains(res.Content, "运行 scripts/make.sh") || !strings.Contains(res.Content, skills[0].Dir) || !strings.Contains(res.Content, "建议使用的工具：bash") {
		t.Errorf("应加载技能正文与目录：%s", res.Content)
	}
	if strings.Contains(res.Content, "description:") {
		t.Errorf("正文不应包含 frontmatter")
	}
	if _, ok := files.ReadAt(skills[0].Path); !ok {
		t.Errorf("加载技能应记录文件读取")
	}
	if res := run("nope"); !res.IsError || !strings.Contains(res.Content, "不存在") {
		t.Errorf("未知技能应报错：%s", res.Content)
	}
	_ = os.Remove(skills[0].Path)
	if res := run("pdf"); !res.IsError {
		t.Errorf("技能文件被删除后应报错")
	}
}

// TestRepoSkills 保证仓库自带的示例技能格式正确。
func TestRepoSkills(t *testing.T) {
	skills, errs := Discover(Dir{Path: filepath.Join("..", ".goagent", "skills"), Source: "project"})
	if len(errs) != 0 {
		t.Fatalf("示例技能有错误：%v", errs)
	}
	names := map[string]bool{}
	for _, s := range skills {
		names[s.Name] = true
		if body, err := s.Body(); err != nil || body == "" {
			t.Errorf("技能 %s 正文为空：%v", s.Name, err)
		}
	}
	if !names["go-review"] || !names["commit-message"] {
		t.Errorf("缺少示例技能：%v", names)
	}
}

// TestDiscoverFollowsSymlinkedDir：skills/ 下指向目录的符号链接也是技能目录。
func TestDiscoverFollowsSymlinkedDir(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	writeSkill(t, shared, "linked", "---\ndescription: 来自共享仓库\n---\n正文")
	skills := filepath.Join(root, "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(shared, "linked"), filepath.Join(skills, "linked")); err != nil {
		t.Skip("当前系统不支持符号链接：", err)
	}
	// 指向文件的符号链接与悬空链接都应被忽略
	_ = os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o644)
	_ = os.Symlink(filepath.Join(root, "file.txt"), filepath.Join(skills, "file-link"))
	_ = os.Symlink(filepath.Join(root, "missing"), filepath.Join(skills, "dangling"))
	got, errs := Discover(Dir{Path: skills, Source: "project"})
	if len(errs) != 0 || len(got) != 1 || got[0].Name != "linked" {
		t.Fatalf("应发现符号链接指向的技能目录：%+v %v", got, errs)
	}
	if body, err := got[0].Body(); err != nil || body != "正文" {
		t.Errorf("应能读取链接目录中的正文：%q %v", body, err)
	}
}
