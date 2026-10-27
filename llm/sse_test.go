package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReadSSE(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []SSEEvent
	}{
		{
			name:  "单个事件",
			input: "event: ping\ndata: hello\n\n",
			want:  []SSEEvent{{Event: "ping", Data: "hello"}},
		},
		{
			name:  "多行 data 按换行拼接",
			input: "data: line1\ndata: line2\n\n",
			want:  []SSEEvent{{Data: "line1\nline2"}},
		},
		{
			name:  "注释行被忽略",
			input: ": keep-alive\ndata: x\n\n: another\n\n",
			want:  []SSEEvent{{Data: "x"}},
		},
		{
			name:  "没有结尾空行也能输出最后一个事件",
			input: "data: a\n\ndata: b",
			want:  []SSEEvent{{Data: "a"}, {Data: "b"}},
		},
		{
			name:  "CRLF 换行",
			input: "event: e\r\ndata: v\r\n\r\n",
			want:  []SSEEvent{{Event: "e", Data: "v"}},
		},
		{
			name:  "冒号后没有空格",
			input: "data:nospace\n\n",
			want:  []SSEEvent{{Data: "nospace"}},
		},
		{
			name:  "未知字段与多余空行",
			input: "\n\nid: 1\nretry: 10\ndata: z\n\n\n",
			want:  []SSEEvent{{Data: "z"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []SSEEvent
			err := ReadSSE(strings.NewReader(tt.input), func(e SSEEvent) error {
				got = append(got, e)
				return nil
			})
			if err != nil {
				t.Fatalf("ReadSSE 返回错误：%v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("解析结果 = %#v，期望 %#v", got, tt.want)
			}
		})
	}
}

func TestReadSSEStopOnError(t *testing.T) {
	stop := errors.New("stop")
	n := 0
	err := ReadSSE(strings.NewReader("data: 1\n\ndata: 2\n\n"), func(SSEEvent) error {
		n++
		return stop
	})
	if !errors.Is(err, stop) || n != 1 {
		t.Fatalf("回调返回错误后应立即停止：err=%v 调用次数=%d", err, n)
	}
}

func TestRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"429 限流", &APIError{StatusCode: 429}, true},
		{"529 过载", &APIError{StatusCode: 529}, true},
		{"500 服务端错误", &APIError{StatusCode: 500}, true},
		{"400 参数错误", &APIError{StatusCode: 400}, false},
		{"401 鉴权失败", &APIError{StatusCode: 401}, false},
		{"ctx 取消", context.Canceled, false},
		{"意外 EOF", io.ErrUnexpectedEOF, true},
		{"连接重置", errors.New("read: connection reset by peer"), true},
		{"普通错误", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Retryable(tt.err); got != tt.want {
				t.Errorf("Retryable(%v) = %v，期望 %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestAPIErrorMessage(t *testing.T) {
	e := &APIError{Provider: "x", StatusCode: 400, Body: strings.Repeat("a", 600)}
	if msg := e.Error(); !strings.Contains(msg, "HTTP 400") || !strings.HasSuffix(msg, "...") {
		t.Errorf("过长的错误体应被截断：%s", msg)
	}
	resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"3"}}, Body: io.NopCloser(strings.NewReader(" slow down "))}
	err := newAPIError("p", resp).(*APIError)
	if err.RetryAfter != 3*time.Second || err.Body != "slow down" {
		t.Errorf("Retry-After/Body 解析错误：%+v", err)
	}
}

// flakyProvider 前 failures 次返回 err，之后返回成功。emitBeforeFail 控制失败前是否先发出事件。
type flakyProvider struct {
	failures       int
	err            error
	emitBeforeFail bool
	calls          int
}

func (f *flakyProvider) Name() string { return "flaky" }

func (f *flakyProvider) Stream(ctx context.Context, req *Request, onEvent func(StreamEvent)) (*Response, error) {
	f.calls++
	if f.calls <= f.failures {
		if f.emitBeforeFail && onEvent != nil {
			onEvent(StreamEvent{Type: EventTextDelta, Text: "部分输出"})
		}
		return nil, f.err
	}
	return &Response{Message: AssistantText("ok"), StopReason: StopEndTurn}, nil
}

func TestRetryProvider(t *testing.T) {
	tests := []struct {
		name      string
		inner     *flakyProvider
		attempts  int
		wantErr   bool
		wantCalls int
	}{
		{"429 重试后成功", &flakyProvider{failures: 2, err: &APIError{StatusCode: 429}}, 4, false, 3},
		{"不可重试的错误立即返回", &flakyProvider{failures: 1, err: &APIError{StatusCode: 400}}, 4, true, 1},
		{"已发出事件后不再重试", &flakyProvider{failures: 1, err: &APIError{StatusCode: 500}, emitBeforeFail: true}, 4, true, 1},
		{"超过最大次数", &flakyProvider{failures: 10, err: &APIError{StatusCode: 503}}, 3, true, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			retries := 0
			rp := &RetryProvider{Inner: tt.inner, MaxAttempts: tt.attempts, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond,
				OnRetry: func(int, time.Duration, error) { retries++ }}
			resp, err := rp.Stream(context.Background(), &Request{}, nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v，期望出错=%v", err, tt.wantErr)
			}
			if !tt.wantErr && resp.Message.Text() != "ok" {
				t.Errorf("返回内容错误：%q", resp.Message.Text())
			}
			if tt.inner.calls != tt.wantCalls {
				t.Errorf("调用次数 = %d，期望 %d", tt.inner.calls, tt.wantCalls)
			}
			if retries != tt.wantCalls-1 {
				t.Errorf("OnRetry 回调次数 = %d，期望 %d", retries, tt.wantCalls-1)
			}
		})
	}
}

func TestRetryProviderContextCancel(t *testing.T) {
	inner := &flakyProvider{failures: 100, err: &APIError{StatusCode: 429}}
	rp := &RetryProvider{Inner: inner, MaxAttempts: 10, BaseDelay: time.Second, MaxDelay: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := rp.Stream(ctx, &Request{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ctx 超时后应返回 ctx 错误，实际：%v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("等待重试时应及时响应 ctx 取消，耗时 %v", time.Since(start))
	}
	if rp.Name() != "flaky" {
		t.Errorf("Name 应透传内部 Provider")
	}
}

func TestRetryBackoff(t *testing.T) {
	rp := &RetryProvider{BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second}
	if d := rp.backoff(1, &APIError{RetryAfter: 300 * time.Millisecond}); d != 300*time.Millisecond {
		t.Errorf("应优先使用 Retry-After：%v", d)
	}
	if d := rp.backoff(1, &APIError{RetryAfter: time.Hour}); d != time.Second {
		t.Errorf("Retry-After 应受 MaxDelay 限制：%v", d)
	}
	for attempt := 1; attempt <= 6; attempt++ {
		d := rp.backoff(attempt, errors.New("x"))
		base := rp.BaseDelay << (attempt - 1)
		if d < min(base, rp.MaxDelay) || d > rp.MaxDelay {
			t.Errorf("第 %d 次退避 %v 超出范围", attempt, d)
		}
	}
}

func TestNewProvider(t *testing.T) {
	tests := []struct {
		cfg      ProviderConfig
		wantName string
		wantErr  bool
		retry    bool
	}{
		{ProviderConfig{Type: ""}, "anthropic", false, true},
		{ProviderConfig{Type: "claude"}, "anthropic", false, true},
		{ProviderConfig{Type: "DeepSeek"}, "openai", false, true},
		{ProviderConfig{Type: "ollama", Retries: -1}, "openai", false, false},
		{ProviderConfig{Type: "mock"}, "mock", false, false},
		{ProviderConfig{Type: "unknown"}, "", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.cfg.Type, func(t *testing.T) {
			p, err := NewProvider(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v，期望出错=%v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if p.Name() != tt.wantName {
				t.Errorf("Name = %s，期望 %s", p.Name(), tt.wantName)
			}
			if _, ok := p.(*RetryProvider); ok != tt.retry {
				t.Errorf("是否带重试 = %v，期望 %v", ok, tt.retry)
			}
		})
	}
	p, _ := NewProvider(ProviderConfig{Type: "openai", Retries: 7})
	if p.(*RetryProvider).MaxAttempts != 7 {
		t.Errorf("Retries > 0 时应设置 MaxAttempts")
	}
}
