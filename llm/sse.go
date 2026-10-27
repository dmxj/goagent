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

// Retryable 判断错误本身是否值得重试：限流、服务端错误、网络抖动（含单次连接超时）。
//
// 注意：从 Go 1.23 起，拨号 / 读写超时等网络超时错误同样满足
// errors.Is(err, context.DeadlineExceeded)，仅凭错误无法区分"调用方的 ctx 到期了"
// 与"这一次连接超时了"。RetryProvider 使用 RetryableContext，结合 ctx.Err() 做判断。
func Retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.StatusCode == http.StatusTooManyRequests || ae.StatusCode == 529 || ae.StatusCode >= 500
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true // 拨号失败、连接超时、TLS 握手超时……
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// 没有 ctx 信息时，把单纯的 DeadlineExceeded 视为单次请求超时（可重试）；
		// 调用方 ctx 到期的情况由 RetryableContext 排除。
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "connection reset")
}

// RetryableContext 在 Retryable 的基础上排除"调用方已经放弃"的情况：
// ctx 已取消或到期时一律不重试；ctx 仍然有效时，单次请求的超时（拨号超时、响应头超时）可以重试。
func RetryableContext(ctx context.Context, err error) bool {
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	return Retryable(err)
}

func asAPIError(err error, target **APIError) bool { return errors.As(err, target) }

// RetryNotify 是重试通知回调：attempt 是失败的第几次尝试，delay 是即将等待的时间。
type RetryNotify func(attempt int, delay time.Duration, err error)

type retryKey struct{}

// WithRetryNotify 把重试通知放进 ctx。无论 RetryProvider 被包在多少层 Provider 里面
// （例如外面再包一层预算控制、模型降级），都能收到通知，而不必对最外层做类型断言。
func WithRetryNotify(ctx context.Context, fn RetryNotify) context.Context {
	return context.WithValue(ctx, retryKey{}, fn)
}

func retryNotifyFrom(ctx context.Context) RetryNotify {
	fn, _ := ctx.Value(retryKey{}).(RetryNotify)
	return fn
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
	// OnRetry 在每次重试前回调，可用于日志。按调用区分的通知请用 WithRetryNotify 放进 ctx。
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
		if emitted || !RetryableContext(ctx, err) || attempt == r.MaxAttempts {
			return nil, err
		}
		delay := r.backoff(attempt, err)
		if r.OnRetry != nil {
			r.OnRetry(attempt, delay, err)
		}
		if fn := retryNotifyFrom(ctx); fn != nil {
			fn(attempt, delay, err)
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
