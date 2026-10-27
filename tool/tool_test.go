package tool

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

type inner struct {
	Value string `json:"value" desc:"内部值"`
}

type schemaArgs struct {
	Name     string            `json:"name" desc:"名称"`
	Count    int               `json:"count,omitempty"`
	Ratio    float64           `json:"ratio,omitempty"`
	Enabled  bool              `json:"enabled,omitempty"`
	Mode     string            `json:"mode,omitempty" enum:"fast,slow"`
	Must     string            `json:"must,omitempty" required:"true"`
	Tags     []string          `json:"tags,omitempty"`
	Nested   inner             `json:"nested"`
	Items    []inner           `json:"items,omitempty"`
	Ptr      *int              `json:"ptr,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Ignored  string            `json:"-"`
	NoTag    string
	private  string // 非导出字段应被跳过
	Anything any    `json:"anything,omitempty"`
}

func TestSchemaOf(t *testing.T) {
	var s map[string]any
	if err := json.Unmarshal(SchemaOf(&schemaArgs{}), &s); err != nil {
		t.Fatalf("Schema 不是合法 JSON：%v", err)
	}
	if s["type"] != "object" {
		t.Fatalf("顶层类型应为 object：%v", s)
	}
	props := s["properties"].(map[string]any)
	prop := func(name string) map[string]any {
		p, ok := props[name].(map[string]any)
		if !ok {
			t.Fatalf("缺少属性 %s：%v", name, props)
		}
		return p
	}
	tests := []struct {
		field, wantType string
	}{
		{"name", "string"}, {"count", "integer"}, {"ratio", "number"}, {"enabled", "boolean"},
		{"tags", "array"}, {"nested", "object"}, {"items", "array"}, {"ptr", "integer"}, {"labels", "object"}, {"NoTag", "string"},
	}
	for _, tt := range tests {
		if got := prop(tt.field)["type"]; got != tt.wantType {
			t.Errorf("字段 %s 类型 = %v，期望 %s", tt.field, got, tt.wantType)
		}
	}
	if prop("name")["description"] != "名称" {
		t.Errorf("desc 标签应生成 description")
	}
	if !reflect.DeepEqual(prop("mode")["enum"], []any{"fast", "slow"}) {
		t.Errorf("enum 标签解析错误：%v", prop("mode")["enum"])
	}
	if items := prop("tags")["items"].(map[string]any); items["type"] != "string" {
		t.Errorf("切片元素类型错误：%v", items)
	}
	nested := prop("nested")["properties"].(map[string]any)["value"].(map[string]any)
	if nested["description"] != "内部值" {
		t.Errorf("嵌套结构体字段描述丢失：%v", nested)
	}
	if elem := prop("items")["items"].(map[string]any); elem["type"] != "object" {
		t.Errorf("结构体切片元素应为 object：%v", elem)
	}
	if prop("labels")["additionalProperties"].(map[string]any)["type"] != "string" {
		t.Errorf("map 的 additionalProperties 错误")
	}
	for _, name := range []string{"-", "Ignored", "private"} {
		if _, ok := props[name]; ok {
			t.Errorf("字段 %s 不应出现在 Schema 中", name)
		}
	}
	var req []string
	for _, r := range s["required"].([]any) {
		req = append(req, r.(string))
	}
	wantReq := []string{"name", "must", "nested", "NoTag"}
	if !reflect.DeepEqual(req, wantReq) {
		t.Errorf("required = %v，期望 %v", req, wantReq)
	}
}

func TestSchemaOfEmptyStruct(t *testing.T) {
	var s map[string]any
	_ = json.Unmarshal(SchemaOf(struct{}{}), &s)
	if _, ok := s["required"]; ok || s["type"] != "object" {
		t.Errorf("空结构体 Schema 错误：%v", s)
	}
}

type addArgs struct {
	A int `json:"a"`
	B int `json:"b"`
}

func TestFuncTool(t *testing.T) {
	add := Func("add", "两数相加", true, func(ctx context.Context, in addArgs, c *Call) (*Result, error) {
		if c.Env == nil {
			t.Error("Env 不应为 nil")
		}
		return Text(strings.Repeat("x", in.A+in.B)), nil
	})
	info := add.Info()
	if info.Name != "add" || !info.ReadOnly || !info.Concurrent || len(info.Schema) == 0 {
		t.Errorf("Info 错误：%+v", info)
	}
	if spec := info.Spec(); spec.Name != "add" || spec.Description != "两数相加" {
		t.Errorf("Spec 转换错误：%+v", spec)
	}
	if add.WithConcurrent(false).Info().Concurrent {
		t.Errorf("WithConcurrent(false) 未生效")
	}
	tests := []struct {
		name    string
		input   string
		wantErr bool
		want    string
	}{
		{"正常参数", `{"a":1,"b":2}`, false, "xxx"},
		{"空参数", ``, false, ""},
		{"null 参数", `null`, false, ""},
		{"类型错误", `{"a":"one"}`, true, "参数解析失败"},
		{"非法 JSON", `{"a":`, true, "参数解析失败"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := add.Run(context.Background(), &Call{Input: json.RawMessage(tt.input)})
			if err != nil {
				t.Fatalf("Run 不应返回 Go 错误：%v", err)
			}
			if res.IsError != tt.wantErr || !strings.Contains(res.Content, tt.want) {
				t.Errorf("结果 = %+v，期望 IsError=%v 且包含 %q", res, tt.wantErr, tt.want)
			}
		})
	}
	if r := Errorf("错误 %d", 1); !r.IsError || r.Content != "错误 1" {
		t.Errorf("Errorf 构造错误：%+v", r)
	}
}

func named(name string) Tool {
	return Func(name, "", true, func(context.Context, struct{}, *Call) (*Result, error) { return Text(name), nil })
}

func TestRegistry(t *testing.T) {
	r := NewRegistry(named("read_file"), named("mcp__github__issues"), named("mcp__github__prs"), named("mcp__fs__read"), named("task"))
	if got := r.Names(); !reflect.DeepEqual(got, []string{"mcp__fs__read", "mcp__github__issues", "mcp__github__prs", "read_file", "task"}) {
		t.Errorf("Names 应按字母排序：%v", got)
	}
	if len(r.Specs()) != 5 {
		t.Errorf("Specs 数量错误")
	}
	if _, ok := r.Get("task"); !ok {
		t.Errorf("Get 找不到已注册工具")
	}
	r.Unregister("task")
	if _, ok := r.Get("task"); ok {
		t.Errorf("Unregister 未生效")
	}
	r.Register(named("task"))

	tests := []struct {
		name    string
		names   []string
		exclude []string
		want    []string
	}{
		{"空列表复制全部", nil, nil, []string{"mcp__fs__read", "mcp__github__issues", "mcp__github__prs", "read_file", "task"}},
		{"星号复制全部并排除 task", []string{"*"}, []string{"task"}, []string{"mcp__fs__read", "mcp__github__issues", "mcp__github__prs", "read_file"}},
		{"前缀通配", []string{"mcp__github__*"}, nil, []string{"mcp__github__issues", "mcp__github__prs"}},
		{"精确名称与空白", []string{" read_file ", "task"}, []string{"task"}, []string{"read_file"}},
		{"不存在的名称", []string{"nope"}, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.Filter(tt.names, tt.exclude...).Names()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Filter(%v, %v) = %v，期望 %v", tt.names, tt.exclude, got, tt.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	if Truncate("short", 10) != "short" {
		t.Errorf("未超长时不应截断")
	}
	tests := []struct {
		name string
		s    string
		max  int
	}{
		{"ASCII", strings.Repeat("a", 1000), 100},
		{"中文", strings.Repeat("中文字符", 300), 100},
		{"混合", strings.Repeat("a中b文🙂", 200), 101},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := Truncate(tt.s, tt.max)
			if !utf8.ValidString(out) {
				t.Errorf("截断结果不是合法 UTF-8：%q", out)
			}
			if !strings.Contains(out, "省略") {
				t.Errorf("截断结果应包含省略提示")
			}
			if len(out) > tt.max+100 {
				t.Errorf("截断后长度 %d 过长", len(out))
			}
		})
	}
}

func TestFileTracker(t *testing.T) {
	var nilTracker *FileTracker
	nilTracker.Read("x") // nil 安全
	if _, ok := nilTracker.ReadAt("x"); !ok || nilTracker.History() != nil {
		t.Errorf("nil FileTracker 行为错误")
	}
	f := NewFileTracker()
	if _, ok := f.ReadAt("a"); ok {
		t.Errorf("未读取的文件不应有记录")
	}
	f.Read("a")
	f.Wrote("b", "write")
	f.Touch("dir", "list")
	if _, ok := f.ReadAt("a"); !ok {
		t.Errorf("Read 后应有读取记录")
	}
	if _, ok := f.ReadAt("b"); !ok {
		t.Errorf("写入后应视为已读取最新内容")
	}
	if h := f.History(); len(h) != 3 || h[2].Op != "list" {
		t.Errorf("History 错误：%+v", h)
	}
	var env *Env
	env.Logf("nil 安全 %d", 1)
	var logged string
	(&Env{Log: func(format string, args ...any) { logged = format }}).Logf("hello")
	if logged != "hello" {
		t.Errorf("Logf 未调用 Log")
	}
}

func TestNilRegistry(t *testing.T) {
	var r *Registry
	if _, ok := r.Get("x"); ok || len(r.List()) != 0 || len(r.Names()) != 0 || len(r.Specs()) != 0 || len(r.Filter(nil).Names()) != 0 {
		t.Errorf("nil 注册表应视为空")
	}
}
