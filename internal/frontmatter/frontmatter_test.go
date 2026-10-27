package frontmatter

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		wantMeta map[string]string
		wantBody string
	}{
		{"基本", "---\nname: x\ndescription: \"带引号\"\n---\n正文\n", map[string]string{"name": "x", "description": "带引号"}, "正文\n"},
		{"CRLF 与注释", "---\r\n# 注释\r\nname: 'y'\r\n\r\n---\r\nbody", map[string]string{"name": "y"}, "body"},
		{"多行列表", "---\ntools:\n  - read_file\n  - \"grep\"\nmodel: m\n---\n", map[string]string{"tools": "read_file, grep", "model": "m"}, ""},
		{"值中包含冒号", "---\nurl: https://x.com\n---\n", map[string]string{"url": "https://x.com"}, ""},
		{"没有 frontmatter", "# 标题\n内容", map[string]string{}, "# 标题\n内容"},
		{"没有结束标记", "---\nname: x\n内容", map[string]string{}, "---\nname: x\n内容"},
		{"忽略无冒号的行", "---\nname: x\njunk\n---\nb", map[string]string{"name": "x"}, "b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta, body := Parse(tt.text)
			if !reflect.DeepEqual(meta, tt.wantMeta) {
				t.Errorf("meta = %v，期望 %v", meta, tt.wantMeta)
			}
			if body != tt.wantBody {
				t.Errorf("body = %q，期望 %q", body, tt.wantBody)
			}
		})
	}
}

func TestList(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"[a, b]", []string{"a", "b"}},
		{"a,b , c", []string{"a", "b", "c"}},
		{`["x", 'y']`, []string{"x", "y"}},
		{"[]", nil},
		{"", nil},
		{"a,,b", []string{"a", "b"}},
	}
	for _, tt := range tests {
		if got := List(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("List(%q) = %v，期望 %v", tt.in, got, tt.want)
		}
	}
}

func TestRenderRoundTrip(t *testing.T) {
	meta := map[string]string{"name": "n", "description": "多行\n描述", "type": "user"}
	text := Render(meta, "\n正文内容\n\n")
	want := "---\ndescription: 多行 描述\nname: n\ntype: user\n---\n\n正文内容\n"
	if text != want {
		t.Errorf("Render 输出 = %q，期望 %q", text, want)
	}
	got, body := Parse(text)
	if got["name"] != "n" || got["description"] != "多行 描述" || body != "\n正文内容\n" {
		t.Errorf("往返解析错误：%v %q", got, body)
	}
}
