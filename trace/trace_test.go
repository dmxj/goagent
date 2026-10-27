package trace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSpanParentChild(t *testing.T) {
	mem := NewMemory(0)
	var started []string
	tr := New(mem)
	tr.OnStart = func(s SpanData) { started = append(started, s.Name) }

	ctx, root := tr.Start(context.Background(), "agent.run", KindAgent, map[string]any{"session_id": "s1"})
	if FromContext(ctx) != root || root.TraceID == "" || root.ParentID != "" || root.Status != "running" {
		t.Fatalf("根 Span 错误：%+v", root)
	}
	cctx, child := tr.Start(ctx, "llm.call", KindLLM, nil)
	_, grandchild := tr.Start(cctx, "tool.bash", KindTool, nil)
	if child.TraceID != root.TraceID || child.ParentID != root.SpanID || grandchild.ParentID != child.SpanID {
		t.Errorf("父子关系错误")
	}
	child.SetAttr("input_tokens", 10)
	child.SetAttr("output_tokens", 5)
	child.AddEvent("retry", map[string]any{"attempt": 1})
	grandchild.Fail(errors.New("命令失败"))
	grandchild.Fail(nil) // nil 错误不改变状态
	grandchild.Finish()
	child.Finish()
	time.Sleep(time.Millisecond)
	root.Finish()
	root.Finish() // 重复结束无副作用

	if strings.Join(started, ",") != "agent.run,llm.call,tool.bash" {
		t.Errorf("OnStart 回调顺序错误：%v", started)
	}
	spans := mem.Spans("")
	if len(spans) != 3 {
		t.Fatalf("应导出 3 个 Span（重复 Finish 不重复导出），实际 %d", len(spans))
	}
	if spans[0].Name != "agent.run" {
		t.Errorf("Spans 应按开始时间排序：%v", spans[0].Name)
	}
	byName := map[string]SpanData{}
	for _, s := range spans {
		byName[s.Name] = s
	}
	if byName["tool.bash"].Status != "error" || byName["tool.bash"].Error != "命令失败" {
		t.Errorf("Fail 应标记错误：%+v", byName["tool.bash"])
	}
	if byName["llm.call"].Status != "ok" || byName["llm.call"].Attrs["input_tokens"] != 10 || len(byName["llm.call"].Events) != 1 {
		t.Errorf("属性/事件丢失：%+v", byName["llm.call"])
	}
	if byName["agent.run"].Duration <= 0 || byName["agent.run"].End.IsZero() {
		t.Errorf("Duration/End 未设置")
	}
	if got := mem.Spans(root.TraceID); len(got) != 3 {
		t.Errorf("按 trace_id 过滤错误")
	}
	if got := mem.Spans("other"); len(got) != 0 {
		t.Errorf("不存在的 trace 应返回空")
	}

	// 新的 ctx 开启新 Trace
	_, other := tr.Start(context.Background(), "x", KindHook, nil)
	if other.TraceID == root.TraceID {
		t.Errorf("没有父 Span 时应生成新的 TraceID")
	}
}

func TestNilTracerAndSpan(t *testing.T) {
	var tr *Tracer
	ctx, s := tr.Start(context.Background(), "x", KindTool, nil)
	if s == nil || FromContext(ctx) != s {
		t.Fatal("nil Tracer 也应返回可用的 Span")
	}
	s.SetAttr("k", "v")
	s.Finish() // 不 panic
	var ns *Span
	ns.SetAttr("k", 1)
	ns.AddEvent("e", nil)
	ns.Fail(errors.New("x"))
	ns.Finish()
}

func TestSnapshotIsCopy(t *testing.T) {
	_, s := New().Start(context.Background(), "x", KindTool, nil)
	s.SetAttr("a", 1)
	snap := s.Snapshot()
	snap.Attrs["a"] = 2
	if s.Snapshot().Attrs["a"] != 1 {
		t.Errorf("Snapshot 应返回属性的拷贝")
	}
}

func TestConcurrentSpans(t *testing.T) {
	mem := NewMemory(0)
	tr := New(mem)
	ctx, root := tr.Start(context.Background(), "root", KindAgent, nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, s := tr.Start(ctx, "tool", KindTool, nil)
			s.SetAttr("x", 1)
			root.AddEvent("child", nil)
			s.Finish()
		}()
	}
	wg.Wait()
	root.Finish()
	if len(mem.Spans("")) != 21 {
		t.Errorf("并发导出丢失 Span")
	}
}

func TestMemoryLimit(t *testing.T) {
	m := NewMemory(3)
	for i := 0; i < 5; i++ {
		m.Export(SpanData{SpanID: string(rune('a' + i)), Start: time.Unix(int64(i), 0)})
	}
	got := m.Spans("")
	if len(got) != 3 || got[0].SpanID != "c" {
		t.Errorf("环形缓冲应只保留最近 3 个：%+v", got)
	}
}

func TestFileExporter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "traces", "t.jsonl")
	f, err := NewFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tr := New(f)
	tr.AddExporter(ExporterFunc(func(SpanData) {}))
	ctx, root := tr.Start(context.Background(), "agent.run", KindAgent, map[string]any{"input": "你好"})
	_, child := tr.Start(ctx, "tool.read_file", KindTool, nil)
	child.Finish()
	root.Finish()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// 追加一行坏数据与空行，ReadFile 应跳过
	fh, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = fh.WriteString("\n{broken\n")
	fh.Close()

	spans, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 || spans[1].Name != "agent.run" || spans[1].Attrs["input"] != "你好" {
		t.Errorf("ReadFile 结果错误：%+v", spans)
	}
	if spans[0].ParentID != spans[1].SpanID {
		t.Errorf("父子关系在文件中丢失")
	}
	if _, err := ReadFile(filepath.Join(t.TempDir(), "missing.jsonl")); err == nil {
		t.Errorf("文件不存在应报错")
	}
	if _, err := NewFile(filepath.Join(path, "sub", "x.jsonl")); err == nil {
		t.Errorf("父路径是文件时应报错")
	}
}

func TestTree(t *testing.T) {
	base := time.Unix(1000, 0)
	spans := []SpanData{
		{SpanID: "r", Name: "agent.run", Kind: KindAgent, Start: base, Duration: 100, Status: "ok"},
		{SpanID: "l", ParentID: "r", Name: "llm.call", Kind: KindLLM, Start: base.Add(1), Duration: 50, Status: "ok", Attrs: map[string]any{"input_tokens": 10, "output_tokens": 5}},
		{SpanID: "t2", ParentID: "r", Name: "tool.grep", Kind: KindTool, Start: base.Add(3), Status: "ok"},
		{SpanID: "t1", ParentID: "r", Name: "tool.bash", Kind: KindTool, Start: base.Add(2), Status: "error", Error: "exit 1"},
		{SpanID: "o", ParentID: "missing", Name: "orphan", Kind: KindHook, Start: base.Add(4), Status: "ok"},
	}
	got := Tree(spans)
	want := "├─ [agent] agent.run 100ms ✓\n" +
		"│  ├─ [llm] llm.call 50ms tokens=10/5 ✓\n" +
		"│  ├─ [tool] tool.bash 0ms ✗ exit 1\n" +
		"│  └─ [tool] tool.grep 0ms ✓\n" +
		"└─ [hook] orphan 0ms ✓\n"
	if got != want {
		t.Errorf("Tree 输出：\n%s\n期望：\n%s", got, want)
	}
	if Tree(nil) != "" {
		t.Errorf("空列表应输出空字符串")
	}
}

func TestSetTextRedactAndTruncate(t *testing.T) {
	mem := NewMemory(0)
	tr := New(mem)
	tr.SetMaxAttrBytes(64)
	tr.SetSecrets("my-very-secret-key-123", "short")
	_, sp := tr.Start(context.Background(), "llm.call", KindLLM, map[string]any{"system": "key=my-very-secret-key-123"})
	sp.SetText("llm.request.body", `{"auth":"my-very-secret-key-123","x":"`+strings.Repeat("长", 40)+`"}`)
	sp.SetAttr("note", "token sk-ant-abcdefghijklmnopqrstuvwxyz0123 here")
	sp.SetJSON("llm.request.params", map[string]any{"model": "m", "max_tokens": 10})
	if len(tr.Active("")) != 1 {
		t.Errorf("进行中的 Span 应出现在 Active 中")
	}
	sp.Finish()
	if len(tr.Active("")) != 0 {
		t.Errorf("结束后应从 Active 中移除")
	}
	got := mem.Spans("")[0]
	body := got.Attrs["llm.request.body"].(string)
	if strings.Contains(body, "my-very-secret-key-123") || !strings.Contains(body, "[REDACTED]") {
		t.Errorf("配置的密钥应被抹掉：%s", body)
	}
	if !strings.Contains(body, "已截断") || len(body) > 64+80 {
		t.Errorf("超过上限应截断并带标记：%d %s", len(body), body)
	}
	if tr := got.Attrs[AttrTruncated].([]string); len(tr) != 1 || tr[0] != "llm.request.body" {
		t.Errorf("trace.truncated = %v", tr)
	}
	if strings.Contains(got.Attrs["system"].(string), "secret") || strings.Contains(got.Attrs["note"].(string), "sk-ant-") {
		t.Errorf("Start 属性与 SetAttr 也应脱敏：%v", got.Attrs)
	}
	if string(got.Attrs["llm.request.params"].(json.RawMessage)) != `{"max_tokens":10,"model":"m"}` {
		t.Errorf("SetJSON 应保存结构化 JSON：%s", got.Attrs["llm.request.params"])
	}
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"llm.request.params":{"max_tokens":10`) {
		t.Errorf("结构化属性在 JSON 中应是对象：%s", b)
	}
	light := got.Light(10, 5)
	if om, _ := light.Attrs[AttrOmitted].([]string); len(om) == 0 {
		t.Errorf("Light 应列出被省略的大属性")
	}
	if _, ok := mem.Span(got.SpanID); !ok {
		t.Errorf("Memory.Span 应能按 ID 查到")
	}
}

func TestOmitZeroEnd(t *testing.T) {
	_, sp := New().Start(context.Background(), "x", KindAgent, nil)
	b, _ := json.Marshal(sp.Snapshot())
	if strings.Contains(string(b), "0001-01-01") || strings.Contains(string(b), `"end"`) {
		t.Errorf("进行中的 Span 不应输出零值 end：%s", b)
	}
}

func TestMemoryByteBudget(t *testing.T) {
	m := NewMemory(100)
	m.MaxBytes = 3000
	for i := 0; i < 10; i++ {
		m.Export(SpanData{SpanID: string(rune('a' + i)), Start: time.Unix(int64(i), 0), Attrs: map[string]any{"body": strings.Repeat("x", 1000)}})
	}
	got := m.Spans("")
	if len(got) > 3 || got[len(got)-1].SpanID != "j" {
		t.Errorf("超过字节上限应淘汰最旧的 Span：剩 %d 个", len(got))
	}
}

func TestDailyFileRotates(t *testing.T) {
	dir := t.TempDir()
	d, err := NewDailyFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 27, 23, 59, 0, 0, time.Local)
	d.Now = func() time.Time { return day }
	d.Export(SpanData{SpanID: "a"})
	day = day.Add(2 * time.Minute)
	d.Export(SpanData{SpanID: "b"})
	d.SetEnabled(false)
	d.Export(SpanData{SpanID: "c"})
	_ = d.Close()
	a, _ := ReadFile(filepath.Join(dir, "20261027.jsonl"))
	b, _ := ReadFile(filepath.Join(dir, "20261028.jsonl"))
	if len(a) != 1 || len(b) != 1 || b[0].SpanID != "b" {
		t.Errorf("跨天应切换文件：%v %v", a, b)
	}
	if d.Path() != filepath.Join(dir, "20261028.jsonl") {
		t.Errorf("Path = %s", d.Path())
	}
}

func TestBuildTree(t *testing.T) {
	base := time.Unix(1000, 0)
	spans := []SpanData{
		{SpanID: "r", Name: "agent.run", Start: base},
		{SpanID: "t", ParentID: "r", Name: "tool.task", Start: base.Add(2)},
		{SpanID: "l", ParentID: "r", Name: "llm.call", Start: base.Add(1)},
		{SpanID: "s", ParentID: "t", Name: "subagent.explore", Start: base.Add(3)},
		{SpanID: "s", ParentID: "t", Name: "subagent.explore", Start: base.Add(3)},
		{SpanID: "o", ParentID: "gone", Name: "orphan", Start: base.Add(4)},
	}
	roots := BuildTree(spans)
	if len(roots) != 2 || roots[0].SpanID != "r" || roots[1].SpanID != "o" {
		t.Fatalf("根节点错误：%+v", roots)
	}
	if kids := roots[0].Children; len(kids) != 2 || kids[0].SpanID != "l" || len(kids[1].Children) != 1 {
		t.Errorf("子节点错误：%+v", kids)
	}
	b, _ := json.Marshal(roots[1])
	if !strings.Contains(string(b), `"span_id":"o"`) || !strings.Contains(string(b), `"children":[]`) {
		t.Errorf("Node JSON 应平铺 SpanData 字段并带 children：%s", b)
	}
}
