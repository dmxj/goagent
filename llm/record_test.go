package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// collectExchanges 返回一个记录全部往返的 ctx。
func collectExchanges(max int) (context.Context, func() []*Exchange) {
	var mu sync.Mutex
	var list []*Exchange
	ctx := WithRecorder(context.Background(), &Recorder{MaxBytes: max, OnExchange: func(ex *Exchange) {
		mu.Lock()
		list = append(list, ex)
		mu.Unlock()
	}})
	return ctx, func() []*Exchange {
		mu.Lock()
		defer mu.Unlock()
		return append([]*Exchange(nil), list...)
	}
}

func TestAnthropicRecordsRawStream(t *testing.T) {
	events := []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":3}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
	}
	var body map[string]any
	srv := sseServer(t, "/v1/messages", events, &body, nil)
	p := NewAnthropic(srv.URL, "sk-secret-value")
	p.Headers = map[string]string{"x-team": "go"}
	ctx, got := collectExchanges(0)
	temp, topP := 0.3, 0.9
	resp, err := p.Stream(ctx, &Request{Model: "m", System: "sys", Messages: []Message{UserText("hi")}, MaxTokens: 100,
		Temperature: &temp, TopP: &topP, TopK: 5, StopSequences: []string{"END"},
		Headers: map[string]string{"x-request-tag": "t1"}, ExtraBody: map[string]any{"metadata": map[string]any{"user_id": "u1"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Text() != "你好" {
		t.Fatalf("回复 = %q", resp.Message.Text())
	}
	if body["top_p"] != 0.9 || body["top_k"] != float64(5) || body["metadata"] == nil {
		t.Errorf("请求体缺少 top_p / top_k / extra_body：%v", body)
	}
	exs := got()
	if len(exs) != 1 {
		t.Fatalf("应记录 1 次往返，得到 %d", len(exs))
	}
	ex := exs[0]
	if ex.Status != 200 || !ex.Stream || ex.Method != "POST" || !strings.HasSuffix(ex.URL, "/v1/messages") {
		t.Errorf("往返元数据错误：%+v", ex)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(ex.RequestBody), &sent); err != nil || sent["system"] != "sys" || sent["stream"] != true {
		t.Errorf("RequestBody 应是发出的原生 JSON：%s", ex.RequestBody)
	}
	if ex.RequestHeaders["x-api-key"] != "[REDACTED]" || ex.RequestHeaders["authorization"] != "Bearer [REDACTED]" {
		t.Errorf("认证头应脱敏：%v", ex.RequestHeaders)
	}
	if ex.RequestHeaders["x-team"] != "go" || ex.RequestHeaders["x-request-tag"] != "t1" || ex.RequestHeaders["anthropic-version"] == "" {
		t.Errorf("其它请求头应原样记录：%v", ex.RequestHeaders)
	}
	if strings.Contains(ex.RequestBody+strings.Join(mapValues(ex.RequestHeaders), ""), "sk-secret-value") {
		t.Errorf("记录中不应出现密钥")
	}
	for _, e := range events {
		if !strings.Contains(ex.RawStream, strings.Split(e, "\n")[0]) {
			t.Errorf("RawStream 缺少原始事件 %q", e)
		}
	}
	if ex.ResponseBytes != int64(len(ex.RawStream)) || ex.Truncated {
		t.Errorf("ResponseBytes=%d len=%d truncated=%v", ex.ResponseBytes, len(ex.RawStream), ex.Truncated)
	}
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func TestRecorderTruncates(t *testing.T) {
	long := strings.Repeat("长", 200)
	events := []string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + long + `"}}`,
	}
	srv := sseServer(t, "/v1/messages", events, nil, nil)
	ctx, got := collectExchanges(100)
	if _, err := NewAnthropic(srv.URL, "k").Stream(ctx, &Request{Model: "m", Messages: []Message{UserText(long)}}, nil); err != nil {
		t.Fatal(err)
	}
	ex := got()[0]
	if !ex.Truncated || len(ex.RawStream) > 100 || len(ex.RequestBody) > 100 || ex.ResponseBytes <= 100 {
		t.Errorf("超过上限应截断并记录真实大小：truncated=%v raw=%d req=%d bytes=%d", ex.Truncated, len(ex.RawStream), len(ex.RequestBody), ex.ResponseBytes)
	}
}

func TestAnthropicNonStreaming(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"m","stop_reason":"tool_use","usage":{"input_tokens":7,"output_tokens":3},
			"content":[{"type":"thinking","thinking":"嗯","signature":"s"},{"type":"text","text":"查一下"},{"type":"tool_use","id":"t1","name":"grep","input":{"pattern":"x"}}]}`)
	}))
	defer srv.Close()
	ctx, got := collectExchanges(0)
	var evs []StreamEvent
	resp, err := NewAnthropic(srv.URL, "k").Stream(ctx, &Request{Model: "m", Messages: []Message{UserText("hi")}, NoStream: true}, collect(&evs))
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["stream"] != false {
		t.Errorf("NoStream 时请求体 stream 应为 false：%v", gotBody["stream"])
	}
	calls := resp.Message.ToolUses()
	if len(calls) != 1 || string(calls[0].Input) != `{"pattern":"x"}` || resp.StopReason != StopToolUse || resp.Usage.InputTokens != 7 {
		t.Errorf("非流式响应解析错误：%+v", resp)
	}
	if countType(evs, EventTextDelta) != 1 || countType(evs, EventToolUseEnd) != 1 || countType(evs, EventMessageEnd) != 1 {
		t.Errorf("非流式也应补发事件：%+v", evs)
	}
	ex := got()[0]
	if ex.Stream || !strings.Contains(ex.ResponseBody, `"stop_reason":"tool_use"`) || ex.RawStream != "" {
		t.Errorf("非流式应记录 ResponseBody：%+v", ex)
	}
}

func TestOpenAINonStreaming(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = io.WriteString(w, `{"model":"ds","choices":[{"message":{"role":"assistant","content":"好的","reasoning_content":"想",
			"tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":10,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":2}}}`)
	}))
	defer srv.Close()
	topP := 0.5
	resp, err := NewOpenAI(srv.URL, "k").Stream(context.Background(), &Request{Model: "ds", Messages: []Message{UserText("hi")}, NoStream: true, TopP: &topP,
		ExtraBody: map[string]any{"enable_thinking": true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["stream"] != false || gotBody["stream_options"] != nil || gotBody["top_p"] != 0.5 || gotBody["enable_thinking"] != true {
		t.Errorf("请求体错误：%v", gotBody)
	}
	if resp.Message.Text() != "好的" || len(resp.Message.ToolUses()) != 1 || resp.StopReason != StopToolUse || resp.Usage.CacheReadTokens != 2 {
		t.Errorf("响应解析错误：%+v", resp)
	}
	if resp.Message.Content[0].Type != BlockThinking {
		t.Errorf("reasoning_content 应成为思考块")
	}
}

func TestErrorBodyRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":{"message":"bad model"}}`)
	}))
	defer srv.Close()
	ctx, got := collectExchanges(0)
	_, err := NewOpenAI(srv.URL, "k").Stream(ctx, &Request{Model: "x"}, nil)
	if err == nil {
		t.Fatal("应返回错误")
	}
	ex := got()[0]
	if ex.Status != 400 || !strings.Contains(ex.ResponseBody, "bad model") || ex.Error == "" {
		t.Errorf("错误响应应被记录：%+v", ex)
	}
}

// TestRetryableDialTimeout：单次拨号超时也满足 errors.Is(err, context.DeadlineExceeded)，
// 调用方 ctx 仍然有效时应当重试；调用方 ctx 到期时不应重试。
func TestRetryableDialTimeout(t *testing.T) {
	d := net.Dialer{Timeout: time.Nanosecond}
	_, dialErr := d.Dial("tcp", "10.255.255.1:81")
	if dialErr == nil {
		t.Skip("意外拨号成功")
	}
	if !RetryableContext(context.Background(), dialErr) {
		t.Errorf("ctx 有效时单次拨号超时应可重试：%v（Is DeadlineExceeded=%v）", dialErr, errors.Is(dialErr, context.DeadlineExceeded))
	}
	urlErr := &httpTimeoutErr{}
	if !RetryableContext(context.Background(), urlErr) {
		t.Errorf("响应头超时应可重试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if RetryableContext(ctx, dialErr) || RetryableContext(ctx, context.DeadlineExceeded) {
		t.Errorf("调用方 ctx 已到期时不应重试")
	}
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	if RetryableContext(cctx, &APIError{StatusCode: 500}) {
		t.Errorf("调用方 ctx 已取消时不应重试")
	}
}

type httpTimeoutErr struct{}

func (httpTimeoutErr) Error() string   { return "net/http: timeout awaiting response headers" }
func (httpTimeoutErr) Timeout() bool   { return true }
func (httpTimeoutErr) Temporary() bool { return true }

// timeoutProvider 前几次返回"单次请求超时"（DeadlineExceeded），之后成功。
type timeoutProvider struct{ calls int }

func (p *timeoutProvider) Name() string { return "timeout" }
func (p *timeoutProvider) Stream(ctx context.Context, req *Request, on func(StreamEvent)) (*Response, error) {
	p.calls++
	if p.calls < 3 {
		attempt, cancel := context.WithTimeout(ctx, time.Nanosecond)
		defer cancel()
		<-attempt.Done()
		return nil, &netTimeout{attempt.Err()}
	}
	return &Response{Message: AssistantText("ok")}, nil
}

type netTimeout struct{ err error }

func (e *netTimeout) Error() string   { return "dial tcp: i/o timeout" }
func (e *netTimeout) Unwrap() error   { return e.err }
func (e *netTimeout) Timeout() bool   { return true }
func (e *netTimeout) Temporary() bool { return true }

func TestRetryProviderRetriesAttemptTimeout(t *testing.T) {
	inner := &timeoutProvider{}
	rp := &RetryProvider{Inner: inner, MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}
	var notified []int
	ctx := WithRetryNotify(context.Background(), func(attempt int, _ time.Duration, err error) { notified = append(notified, attempt) })
	resp, err := rp.Stream(ctx, &Request{}, nil)
	if err != nil || resp.Message.Text() != "ok" || inner.calls != 3 {
		t.Fatalf("单次超时应被重试直到成功：err=%v calls=%d", err, inner.calls)
	}
	if len(notified) != 2 {
		t.Errorf("WithRetryNotify 应收到 2 次通知，得到 %v", notified)
	}
}

// wrapper 模拟包在 RetryProvider 外层的 Provider（如预算控制），重试通知仍能通过 ctx 送达。
type wrapper struct{ inner Provider }

func (w wrapper) Name() string { return "wrap" }
func (w wrapper) Stream(ctx context.Context, req *Request, on func(StreamEvent)) (*Response, error) {
	return w.inner.Stream(ctx, req, on)
}

func TestRetryNotifyThroughWrapper(t *testing.T) {
	rp := &RetryProvider{Inner: &flakyProvider{failures: 1, err: &APIError{StatusCode: 503}}, MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	n := 0
	ctx := WithRetryNotify(context.Background(), func(int, time.Duration, error) { n++ })
	if _, err := (wrapper{rp}).Stream(ctx, &Request{}, nil); err != nil || n != 1 {
		t.Errorf("外层包装后重试通知应送达：err=%v n=%d", err, n)
	}
}

func TestRedactHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-1")
	h.Set("X-Api-Key", "sk-2")
	h.Set("X-Custom-Token", "t")
	h.Set("Content-Type", "application/json")
	got := RedactHeaders(h)
	if got["authorization"] != "Bearer [REDACTED]" || got["x-api-key"] != "[REDACTED]" || got["x-custom-token"] != "[REDACTED]" || got["content-type"] != "application/json" {
		t.Errorf("RedactHeaders = %v", got)
	}
}

func TestMockRecordsExchange(t *testing.T) {
	ctx, got := collectExchanges(0)
	if _, err := NewMock(AssistantText("hi")).Stream(ctx, &Request{Model: "m", Messages: []Message{UserText("q")}}, nil); err != nil {
		t.Fatal(err)
	}
	ex := got()
	if len(ex) != 1 || ex[0].Status != 200 || !strings.Contains(ex[0].RequestBody, `"q"`) || !strings.Contains(ex[0].ResponseBody, "hi") {
		t.Errorf("Mock 应产生往返记录：%+v", ex)
	}
}
