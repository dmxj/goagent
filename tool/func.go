package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
)

// SchemaOf 通过反射从结构体生成 JSON Schema。
//
// 支持的标签：
//
//	json:"name,omitempty"   字段名；带 omitempty 视为可选
//	desc:"说明"             字段描述
//	enum:"a,b,c"            枚举值
//	required:"true"         强制必填（即使带了 omitempty）
func SchemaOf(v any) json.RawMessage {
	t := reflect.TypeOf(v)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	b, _ := json.Marshal(schemaFor(t))
	return b
}

func schemaFor(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": schemaFor(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": schemaFor(t.Elem())}
	case reflect.Struct:
		props := map[string]any{}
		var required []string
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			s := schemaFor(f.Type)
			if d := f.Tag.Get("desc"); d != "" {
				s["description"] = d
			}
			if e := f.Tag.Get("enum"); e != "" {
				s["enum"] = strings.Split(e, ",")
			}
			props[name] = s
			if !strings.Contains(opts, "omitempty") || f.Tag.Get("required") == "true" {
				required = append(required, name)
			}
		}
		s := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	default:
		return map[string]any{}
	}
}

// FuncTool 是用泛型实现的"类型安全"工具：
// 输入 JSON 会自动解码为 In 类型，Schema 也由 In 自动生成。
type FuncTool[In any] struct {
	info Info
	fn   func(ctx context.Context, in In, call *Call) (*Result, error)
}

// Func 创建一个函数式工具。
//
//	type addArgs struct {
//		A int `json:"a" desc:"加数"`
//		B int `json:"b" desc:"被加数"`
//	}
//	t := tool.Func("add", "两数相加", true, func(ctx context.Context, in addArgs, c *tool.Call) (*tool.Result, error) {
//		return tool.Text(fmt.Sprint(in.A + in.B)), nil
//	})
func Func[In any](name, desc string, readOnly bool, fn func(ctx context.Context, in In, call *Call) (*Result, error)) *FuncTool[In] {
	var zero In
	return &FuncTool[In]{
		info: Info{Name: name, Description: desc, Schema: SchemaOf(zero), ReadOnly: readOnly, Concurrent: readOnly},
		fn:   fn,
	}
}

// WithConcurrent 设置是否允许并行执行。
func (f *FuncTool[In]) WithConcurrent(c bool) *FuncTool[In] { f.info.Concurrent = c; return f }

func (f *FuncTool[In]) Info() Info { return f.info }

func (f *FuncTool[In]) Run(ctx context.Context, call *Call) (*Result, error) {
	if call.Env == nil {
		// 单独调用工具（例如在测试或自定义程序中）时没有运行环境：给一个空环境，
		// 工具实现里就可以放心访问 c.Env.Files 等字段（FileTracker 的方法对 nil 安全）。
		cp := *call
		cp.Env = &Env{}
		call = &cp
	}
	var in In
	raw := call.Input
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Errorf("参数解析失败：%v。请按照工具的 JSON Schema 传参。", err), nil
	}
	return f.fn(ctx, in, call)
}

// FileTracker 记录会话中文件的读写情况。
//
// 用途：1) "先读后改"保护——防止模型凭记忆覆盖用户刚改过的文件；
// 2) 调试面板展示本次运行涉及了哪些文件。
type FileTracker struct {
	mu      sync.Mutex
	reads   map[string]time.Time
	writes  map[string]time.Time
	history []FileAccess
}

// FileAccess 是一次文件访问记录。
type FileAccess struct {
	Path string    `json:"path"`
	Op   string    `json:"op"` // read | write | edit | list | search
	At   time.Time `json:"at"`
}

func NewFileTracker() *FileTracker {
	return &FileTracker{reads: map[string]time.Time{}, writes: map[string]time.Time{}}
}

func (f *FileTracker) record(path, op string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	switch op {
	case "read":
		f.reads[path] = now
	case "write", "edit":
		f.writes[path] = now
		f.reads[path] = now // 写入后视为已知最新内容
	}
	f.history = append(f.history, FileAccess{Path: path, Op: op, At: now})
}

// Read 记录读取。
func (f *FileTracker) Read(path string) { f.record(path, "read") }

// Wrote 记录写入。
func (f *FileTracker) Wrote(path, op string) { f.record(path, op) }

// Touch 记录列目录/搜索等访问。
func (f *FileTracker) Touch(path, op string) { f.record(path, op) }

// ReadAt 返回文件最后一次被读取的时间。
func (f *FileTracker) ReadAt(path string) (time.Time, bool) {
	if f == nil {
		return time.Time{}, true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.reads[path]
	return t, ok
}

// History 返回访问历史副本。
func (f *FileTracker) History() []FileAccess {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FileAccess(nil), f.history...)
}

// Truncate 把过长的输出截断到 max 个字节，保留头尾，便于模型理解。
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	head := max * 2 / 3
	tail := max - head
	// 避免截断在 UTF-8 字符中间
	for head > 0 && !utf8Start(s[head]) {
		head--
	}
	start := len(s) - tail
	for start < len(s) && !utf8Start(s[start]) {
		start++
	}
	return s[:head] + fmt.Sprintf("\n\n...[省略 %d 字节]...\n\n", start-head) + s[start:]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
