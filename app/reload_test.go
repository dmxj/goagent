package app

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmxj/goagent/config"
	"github.com/dmxj/goagent/llm"
)

// gate 是一个可以阻塞在模型调用中的 Provider，用来验证"运行中的轮次使用旧快照"。
type gate struct {
	mu      sync.Mutex
	models  []string
	systems []string
	block   chan struct{}
	entered chan struct{}
}

func (g *gate) Name() string { return "gate" }
func (g *gate) Stream(ctx context.Context, req *llm.Request, on func(llm.StreamEvent)) (*llm.Response, error) {
	g.mu.Lock()
	g.models = append(g.models, req.Model)
	g.systems = append(g.systems, req.System)
	block := g.block
	g.mu.Unlock()
	if block != nil {
		g.entered <- struct{}{}
		<-block
	}
	return &llm.Response{Message: llm.AssistantText("ok"), StopReason: llm.StopEndTurn}, nil
}

func projectConfig(t *testing.T, cfg *config.Config, body string) {
	t.Helper()
	write(t, cfg.ProjectDir+"/config.json", body)
}

func TestReloadAppliesConfig(t *testing.T) {
	cfg := testConfig(t)
	write(t, cfg.WorkDir+"/.goagent/prompts/system.md", "你是测试助手 A")
	projectConfig(t, cfg, `{"model":"model-a","system_prompt_file":".goagent/prompts/system.md",
		"params":{"max_tokens":1000,"temperature":0.1,"top_p":0.9,"stream":false},
		"tools":{"enabled":["read_file","bash","skill","task"],"options":{"bash":{"description":"只用来跑 go 命令"}}}}`)
	cfg, _ = config.Load(cfg.WorkDir)
	g := &gate{}
	a, err := New(context.Background(), cfg, Options{Provider: g, SkipMCP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ag := a.Agent()
	if ag.Model != "model-a" || ag.MaxTokens != 1000 || *ag.Temperature != 0.1 || *ag.TopP != 0.9 || !ag.NoStream {
		t.Errorf("模型参数应来自配置：%+v", ag)
	}
	if got := strings.Join(ag.Tools.Names(), ","); got != "bash,read_file,task" {
		t.Errorf("tools.enabled 应限定工具（没有技能时没有 skill 工具）：%s", got)
	}
	if b, _ := ag.Tools.Get("bash"); b.Info().Description != "只用来跑 go 命令" {
		t.Errorf("tools.options.description 应覆盖工具描述")
	}
	if !strings.HasPrefix(ag.SystemPrompt(), "你是测试助手 A") {
		t.Errorf("system_prompt_file 应替换身份：%s", ag.SystemPrompt()[:40])
	}

	// 修改配置 + 新增技能 + 修改提示词文件 → Reload 后下一轮生效
	write(t, cfg.WorkDir+"/.goagent/prompts/system.md", "你是测试助手 B")
	write(t, cfg.ProjectDir+"/skills/new-skill/SKILL.md", "---\nname: new-skill\ndescription: 新技能\n---\n正文")
	projectConfig(t, cfg, `{"model":"model-b","system_prompt_file":".goagent/prompts/system.md","prompt":{"append":"追加内容"},
		"tools":{"disabled":["web_fetch"]},"skills":{"disabled":["ghost"]}}`)
	if err := a.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	ag2 := a.Agent()
	if ag2 == ag || ag2.Model != "model-b" || ag2.Version != "2" || a.Status().Version != 2 {
		t.Errorf("Reload 应组装新的 Agent：%s v%s", ag2.Model, ag2.Version)
	}
	if _, ok := ag2.Tools.Get("skill"); !ok || len(a.Skills()) != 1 {
		t.Errorf("新增的技能应在下一轮可用")
	}
	if _, ok := ag2.Tools.Get("web_fetch"); ok {
		t.Errorf("tools.disabled 应生效")
	}
	if _, err := a.Run(context.Background(), a.NewSession(), "hi", nil); err != nil {
		t.Fatal(err)
	}
	if g.models[0] != "model-b" || !strings.HasPrefix(g.systems[0], "你是测试助手 B") || !strings.Contains(g.systems[0], "追加内容") || !strings.Contains(g.systems[0], "new-skill") {
		t.Errorf("下一轮应使用新模型与新提示词：%v", g.models)
	}
}

func TestInvalidConfigKeepsLastGood(t *testing.T) {
	cfg := testConfig(t)
	projectConfig(t, cfg, `{"model":"good"}`)
	cfg, _ = config.Load(cfg.WorkDir)
	a, err := New(context.Background(), cfg, Options{Provider: &gate{}, SkipMCP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	events, cancel := a.Subscribe()
	defer cancel()
	for _, bad := range []string{`{"model": "x",`, `{"permissions":{"allow":["bash("]}}`, `{"system_prompt_file":"missing.md"}`, `{"params":{"temperature":9}}`} {
		projectConfig(t, cfg, bad)
		if err := a.Reload(context.Background()); err == nil {
			t.Errorf("非法配置 %s 应报错", bad)
		}
		if a.Agent().Model != "good" || a.Status().LastError == "" {
			t.Errorf("非法配置应保留上一份可用配置并记录错误")
		}
		if ev := <-events; ev.Type != "config_error" || ev.Error == "" {
			t.Errorf("应发出 config_error 事件：%+v", ev)
		}
	}
	projectConfig(t, cfg, `{"model":"fixed"}`)
	if err := a.Reload(context.Background()); err != nil || a.Status().LastError != "" || a.Agent().Model != "fixed" {
		t.Errorf("修正后应恢复：%v %+v", err, a.Status())
	}
	if ev := <-events; ev.Type != "config_reloaded" {
		t.Errorf("应发出 config_reloaded 事件：%+v", ev)
	}
}

func TestRunningTurnKeepsOldSnapshot(t *testing.T) {
	cfg := testConfig(t)
	projectConfig(t, cfg, `{"model":"old"}`)
	cfg, _ = config.Load(cfg.WorkDir)
	g := &gate{block: make(chan struct{}), entered: make(chan struct{}, 1)}
	a, err := New(context.Background(), cfg, Options{Provider: g, SkipMCP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	sess := a.NewSession()
	done := make(chan error, 1)
	go func() { _, err := a.Run(context.Background(), sess, "长任务", nil); done <- err }()
	<-g.entered
	projectConfig(t, cfg, `{"model":"new"}`)
	if err := a.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !a.Agent().Running(sess.ID) {
		t.Errorf("新版本应能看到旧版本正在运行的会话")
	}
	g.mu.Lock()
	release := g.block
	g.block = nil // 下一轮不再阻塞
	g.mu.Unlock()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), sess, "下一轮", nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(g.models, ",") != "old,new" {
		t.Errorf("运行中的轮次应使用旧快照，下一轮使用新快照：%v", g.models)
	}
}

func TestWatcherPicksUpChanges(t *testing.T) {
	cfg := testConfig(t)
	projectConfig(t, cfg, `{"model":"w1","reload":{"interval_ms":100}}`)
	cfg, _ = config.Load(cfg.WorkDir)
	a, err := New(context.Background(), cfg, Options{Provider: &gate{}, SkipMCP: true, Watch: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	events, cancel := a.Subscribe()
	defer cancel()
	if !a.Status().Watching {
		t.Fatal("Watch 选项应启动监视")
	}
	time.Sleep(50 * time.Millisecond)
	write(t, cfg.ProjectDir+"/skills/hot/SKILL.md", "---\nname: hot\ndescription: 热加载的技能\n---\n")
	select {
	case ev := <-events:
		if ev.Type != "config_reloaded" || len(ev.Changed) == 0 {
			t.Errorf("事件错误：%+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("新增技能后应自动重新加载")
	}
	if len(a.Skills()) != 1 || a.Skills()[0].Name != "hot" {
		t.Errorf("新技能应立即可用：%+v", a.Skills())
	}
}

// TestRunChecksChangesSynchronously：刚新建的技能在紧接着的这条消息里就可用，不必等轮询。
func TestRunChecksChangesSynchronously(t *testing.T) {
	cfg := testConfig(t)
	projectConfig(t, cfg, `{"model":"m","reload":{"interval_ms":60000}}`)
	cfg, _ = config.Load(cfg.WorkDir)
	g := &gate{}
	a, err := New(context.Background(), cfg, Options{Provider: g, SkipMCP: true, Watch: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	write(t, cfg.ProjectDir+"/skills/instant/SKILL.md", "---\nname: instant\ndescription: 立即可用的技能\n---\n")
	if _, err := a.Run(context.Background(), a.NewSession(), "hi", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(g.systems[0], "instant") {
		t.Errorf("新建的技能应在下一条消息中可用")
	}
}

// TestReloadDisabledAtRuntime：reload.enabled 改为 false 后，技能等文件的修改不再触发重新加载；
// 把它改回 true 后热更新恢复（监视仍会留意配置文件本身）。
func TestReloadDisabledAtRuntime(t *testing.T) {
	cfg := testConfig(t)
	projectConfig(t, cfg, `{"model":"m1","reload":{"enabled":false,"interval_ms":60000}}`)
	cfg, _ = config.Load(cfg.WorkDir)
	g := &gate{}
	a, err := New(context.Background(), cfg, Options{Provider: g, SkipMCP: true, Watch: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.Status().Watching {
		t.Errorf("reload.enabled=false 时 Watching 应为 false")
	}
	write(t, cfg.ProjectDir+"/skills/ignored/SKILL.md", "---\nname: ignored\ndescription: 不应被加载\n---\n")
	if _, err := a.Run(context.Background(), a.NewSession(), "hi", nil); err != nil {
		t.Fatal(err)
	}
	if len(a.Skills()) != 0 || a.Status().Version != 1 {
		t.Errorf("热更新关闭时不应重新加载：版本 %d 技能 %d", a.Status().Version, len(a.Skills()))
	}
	time.Sleep(20 * time.Millisecond) // 保证 mtime 变化
	projectConfig(t, cfg, `{"model":"m2","reload":{"enabled":true,"interval_ms":60000}}`)
	if _, err := a.Run(context.Background(), a.NewSession(), "hi", nil); err != nil {
		t.Fatal(err)
	}
	if a.Status().Version != 2 || !a.Status().Watching || len(a.Skills()) != 1 || g.models[1] != "m2" {
		t.Errorf("重新打开热更新后应立即生效：版本 %d 模型 %v", a.Status().Version, g.models)
	}
	projectConfig(t, cfg, `{"model":"m3","reload":{"enabled":false}}`)
	if _, err := a.Run(context.Background(), a.NewSession(), "hi", nil); err != nil {
		t.Fatal(err)
	}
	if a.Status().Watching || a.Agent().Model != "m3" {
		t.Errorf("关闭热更新的那次修改本身应生效")
	}
	write(t, cfg.ProjectDir+"/skills/later/SKILL.md", "---\nname: later\ndescription: 关闭后新增\n---\n")
	if _, err := a.Run(context.Background(), a.NewSession(), "hi", nil); err != nil {
		t.Fatal(err)
	}
	if len(a.Skills()) != 1 || a.Status().Version != 3 {
		t.Errorf("运行中关闭热更新后，技能目录的修改不应触发重新加载：%d", len(a.Skills()))
	}
}

// TestFingerprintFollowsSymlinkedSkill：链接进 skills/ 的共享技能目录，修改其中的 SKILL.md 也会触发重新加载。
func TestFingerprintFollowsSymlinkedSkill(t *testing.T) {
	root := t.TempDir()
	shared := root + "/shared/linked"
	write(t, shared+"/SKILL.md", "---\ndescription: v1\n---\n")
	skills := root + "/skills"
	write(t, skills+"/.keep", "")
	if err := os.Symlink(shared, skills+"/linked"); err != nil {
		t.Skip("当前系统不支持符号链接：", err)
	}
	before := fingerprint([]string{skills})
	if _, ok := before[skills+"/linked/SKILL.md"]; !ok {
		t.Fatalf("指纹应包含链接目录中的文件：%v", before)
	}
	time.Sleep(20 * time.Millisecond)
	write(t, shared+"/SKILL.md", "---\ndescription: v2 更长的描述\n---\n")
	if fingerprint([]string{skills}).equal(before) {
		t.Errorf("修改链接目标中的 SKILL.md 应改变指纹")
	}
	// 链接成环不会无限递归
	if err := os.Symlink(skills, shared+"/loop"); err == nil {
		_ = fingerprint([]string{skills})
	}
}
