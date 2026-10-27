package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestModelParamsAndPrompt(t *testing.T) {
	home, work := isolate(t)
	t.Setenv("TEAM_HEADER", "go-team")
	writeJSON(t, filepath.Join(home, ".goagent", "config.json"), `{"max_tokens":4000,"temperature":0.5,"params":{"top_p":0.8}}`)
	writeJSON(t, filepath.Join(work, ".goagent", "config.json"), `{
		"params":{"max_tokens":2000,"stop":["END"],"stream":false,"extra_headers":{"x-team":"${TEAM_HEADER}"},"extra_body":{"enable_thinking":true}},
		"system_prompt_file":".goagent/prompts/system.md","prompt":{"append_file":"~/extra.md","extra":[{"title":"t","file":"docs/rules.md"}],"sections":{"memory":false}},
		"tools":{"enabled":["read_file","mcp__*"],"options":{"bash":{"timeout_sec":5,"description":"d"}},"bash_env":{"allow":["MY_VAR"],"extra":{"TOKEN_X":"${TEAM_HEADER}"}}},
		"skills":{"dirs":[".goagent/skills","~/shared-skills"],"disabled":["old-*"]},
		"agents":{"enabled":["explore"]},
		"reload":{"interval_ms":250},
		"trace":{"enabled":true,"max_field_bytes":1024}}`)
	c, err := Load(work)
	if err != nil {
		t.Fatal(err)
	}
	p := c.EffectiveParams()
	if p.MaxTokens != 2000 || *p.Temperature != 0.5 || *p.TopP != 0.8 || p.Stop[0] != "END" || *p.Stream || p.ExtraHeaders["x-team"] != "go-team" || p.ExtraBody["enable_thinking"] != true {
		t.Errorf("params 合并错误：%+v", p)
	}
	if got := c.ResolvePath(c.SystemPromptFile); got != filepath.Join(work, ".goagent", "prompts", "system.md") {
		t.Errorf("相对路径应基于工作目录：%s", got)
	}
	files := c.PromptFiles()
	if len(files) != 3 || files[1] != filepath.Join(home, "extra.md") || files[2] != filepath.Join(work, "docs", "rules.md") {
		t.Errorf("PromptFiles = %v", files)
	}
	if dirs := c.SkillDirs(); len(dirs) != 2 || dirs[1] != filepath.Join(home, "shared-skills") {
		t.Errorf("SkillDirs = %v", dirs)
	}
	if !c.Skills.Allowed("new") || c.Skills.Allowed("old-review") || !c.Agents.Allowed("explore") || c.Agents.Allowed("general-purpose") {
		t.Errorf("enabled / disabled 名单错误")
	}
	tools := ListConfig{Enabled: c.Tools.Enabled, Disabled: c.Tools.Disabled}
	if !tools.Allowed("mcp__github__issue") || tools.Allowed("bash") {
		t.Errorf("tools.enabled 通配错误")
	}
	if c.Tools.Options["bash"].TimeoutSec != 5 || c.Tools.BashEnv.Allow[0] != "MY_VAR" || c.ReloadInterval().Milliseconds() != 250 || !c.ReloadEnabled() || c.Trace.MaxFieldBytes != 1024 {
		t.Errorf("tools / reload / trace 配置解析错误：%+v", c.Tools)
	}
	if err := c.ValidateStatic(); err == nil || !strings.Contains(err.Error(), "提示词文件") {
		t.Errorf("引用的提示词文件不存在时应报错：%v", err)
	}
	cfgFiles := strings.Join(c.ConfigFiles(), ",")
	if !strings.Contains(cfgFiles, filepath.Join(work, ".goagent", "config.json")) {
		t.Errorf("ConfigFiles = %s", cfgFiles)
	}
}

func TestSecretsAndRedactNewFields(t *testing.T) {
	c := Default()
	c.Provider.APIKey = "sk-provider-secret-1234567"
	c.Params.ExtraHeaders = map[string]string{"Authorization": "Bearer header-secret-abcdef", "x-team": "visible-team"}
	c.Tools.BashEnv.Extra = map[string]string{"GH_TOKEN": "ghp_bashsecret123456"}
	secrets := strings.Join(c.Secrets(), "|")
	for _, want := range []string{"sk-provider-secret-1234567", "header-secret-abcdef", "ghp_bashsecret123456"} {
		if !strings.Contains(secrets, want) {
			t.Errorf("Secrets 应包含 %s：%s", want, secrets)
		}
	}
	if strings.Contains(secrets, "visible-team") {
		t.Errorf("非敏感请求头不应当作密钥")
	}
	out := c.Redacted()
	for _, leak := range []string{"provider-secret", "header-secret", "bashsecret"} {
		if strings.Contains(out, leak) {
			t.Errorf("Redacted 泄露了 %s", leak)
		}
	}
	if c.Params.ExtraHeaders["Authorization"] != "Bearer header-secret-abcdef" {
		t.Errorf("Redacted 不应修改原配置")
	}
}

func TestValidateStaticRanges(t *testing.T) {
	c := Default()
	hot := 3.0
	c.Params.Temperature = &hot
	if err := c.ValidateStatic(); err == nil {
		t.Errorf("temperature 超出范围应报错")
	}
	c = Default()
	c.Permissions.Mode = "yolo"
	if err := c.ValidateStatic(); err == nil {
		t.Errorf("未知权限模式应报错")
	}
}
