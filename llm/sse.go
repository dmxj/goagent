package llm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// SSEEvent 是 Server-Sent Events 协议中的一个事件。
type SSEEvent struct {
	Event string
	Data  string
}

// ReadSSE 逐个解析 SSE 事件并回调 fn；fn 返回错误时停止。
//
// SSE 格式：以空行分隔事件，每行形如 "field: value"，
// data 字段可以出现多次（按换行拼接），以 ":" 开头的行是注释。
func ReadSSE(r io.Reader, fn func(SSEEvent) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var ev SSEEvent
	var data []string
	flush := func() error {
		if len(data) == 0 && ev.Event == "" {
			return nil
		}
		ev.Data = strings.Join(data, "\n")
		err := fn(ev)
		ev, data = SSEEvent{}, data[:0]
		return err
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			ev.Event = value
		case "data":
			data = append(data, value)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return flush()
}

// APIError 是模型服务返回的 HTTP 错误。
type APIError struct {
	Provider   string
	StatusCode int
	Body       string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	body := e.Body
	if len(body) > 500 {
		body = body[:500] + "..."
	}
	return fmt.Sprintf("%s API 错误 (HTTP %d): %s", e.Provider, e.StatusCode, body)
}

// Retryable 判断错误是否值得重试：限流、服务端错误、网络抖动。
func Retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.StatusCode == http.StatusTooManyRequests || ae.StatusCode == 529 || ae.StatusCode >= 500
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "connection reset")
}

func newAPIError(provider string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	e := &APIError{Provider: provider, StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			e.RetryAfter = time.Duration(n) * time.Second
		}
	}
	return e
}

// RetryProvider 为任意 Provider 增加指数退避重试。
//
// 只有在"尚未向上游发出任何流式事件"时才重试，避免输出重复内容。
type RetryProvider struct {
	Inner       Provider
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	// OnRetry 在每次重试前回调，可用于日志/trace。
	OnRetry func(attempt int, delay time.Duration, err error)
}

// WithRetry 用默认参数（最多 4 次，500ms 起步）包装 Provider。
func WithRetry(p Provider) *RetryProvider {
	return &RetryProvider{Inner: p, MaxAttempts: 4, BaseDelay: 500 * time.Millisecond, MaxDelay: 20 * time.Second}
}

func (r *RetryProvider) Name() string { return r.Inner.Name() }

func (r *RetryProvider) Stream(ctx context.Context, req *Request, onEvent func(StreamEvent)) (*Response, error) {
	var lastErr error
	for attempt := 1; attempt <= max(1, r.MaxAttempts); attempt++ {
		emitted := false
		wrapped := func(ev StreamEvent) {
			emitted = true
			if onEvent != nil {
				onEvent(ev)
			}
		}
		resp, err := r.Inner.Stream(ctx, req, wrapped)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if emitted || !Retryable(err) || attempt == r.MaxAttempts {
			return nil, err
		}
		delay := r.backoff(attempt, err)
		if r.OnRetry != nil {
			r.OnRetry(attempt, delay, err)
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

// backoff 计算第 attempt 次失败后的等待时间：指数增长 + 随机抖动。
func (r *RetryProvider) backoff(attempt int, err error) time.Duration {
	var ae *APIError
	if errors.As(err, &ae) && ae.RetryAfter > 0 {
		return min(ae.RetryAfter, r.MaxDelay)
	}
	d := r.BaseDelay << (attempt - 1)
	jitter := time.Duration(rand.Int64N(int64(d/2) + 1))
	return min(d+jitter, r.MaxDelay)
}
