package llm

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Exchange 是一次与模型服务的 HTTP 往返的原始记录，供 Trace 展示"模型到底收到了什么、返回了什么"。
//
// Provider 只在 ctx 中带有 Recorder 时才记录（见 WithRecorder），没有记录器时零开销。
// 认证相关的请求头会被脱敏，但其它请求头、完整请求体与完整响应都会原样保留。
type Exchange struct {
	Provider string `json:"provider"`
	Method   string `json:"method"`
	URL      string `json:"url"`
	// RequestHeaders 是实际发出的请求头（x-api-key、authorization 等已脱敏）。
	RequestHeaders map[string]string `json:"request_headers"`
	// RequestBody 是发给厂商的原生请求体，与发送的字节完全一致。
	RequestBody string `json:"request_body"`
	Stream      bool   `json:"stream"`

	Status          int               `json:"status"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`
	// ResponseBody 是非流式响应体，或错误响应体。
	ResponseBody string `json:"response_body,omitempty"`
	// RawStream 是流式响应的完整原始 SSE 文本（含 event:/data: 行）。
	RawStream string `json:"raw_stream,omitempty"`
	// ResponseBytes 是响应体的真实字节数（超过上限被截断时仍是完整大小）。
	ResponseBytes int64 `json:"response_bytes"`
	Truncated     bool  `json:"truncated,omitempty"`

	Error    string        `json:"error,omitempty"`
	Start    time.Time     `json:"start"`
	Duration time.Duration `json:"-"`
	// LatencyMS 是从发出请求到读完响应的耗时。
	LatencyMS int64 `json:"latency_ms"`
	// TTFBMS 是从发出请求到收到响应头的耗时。
	TTFBMS int64 `json:"ttfb_ms"`
}

// Recorder 接收 Provider 产生的原始往返记录。一次 Stream 调用（含重试）可能产生多条记录。
type Recorder struct {
	// MaxBytes 是单个原始字段（请求体、响应体、原始流）保留的最大字节数，<=0 表示 4MB。
	MaxBytes int
	// OnExchange 在每次往返结束（成功或失败）时被调用，可能被并发调用。
	OnExchange func(*Exchange)
}

type recorderKey struct{}

// WithRecorder 把记录器放进 ctx。
func WithRecorder(ctx context.Context, r *Recorder) context.Context {
	return context.WithValue(ctx, recorderKey{}, r)
}

// RecorderFrom 取出 ctx 中的记录器（可能为 nil）。
func RecorderFrom(ctx context.Context) *Recorder {
	r, _ := ctx.Value(recorderKey{}).(*Recorder)
	return r
}

// DefaultMaxRecordBytes 是原始字段的默认上限。
const DefaultMaxRecordBytes = 4 << 20

func (r *Recorder) max() int {
	if r == nil || r.MaxBytes <= 0 {
		return DefaultMaxRecordBytes
	}
	return r.MaxBytes
}

// SensitiveHeader 判断请求头是否携带凭据。
func SensitiveHeader(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case "authorization", "proxy-authorization", "x-api-key", "api-key", "cookie", "set-cookie", "x-goog-api-key":
		return true
	}
	return strings.Contains(n, "token") || strings.Contains(n, "secret") || strings.Contains(n, "key") || strings.Contains(n, "auth")
}

// RedactHeaders 把请求头转换为 map，并隐藏凭据（保留 Bearer 等认证方案名便于排查）。
func RedactHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := strings.Join(h[k], ", ")
		if SensitiveHeader(k) && v != "" {
			if scheme, _, ok := strings.Cut(v, " "); ok && (strings.EqualFold(scheme, "Bearer") || strings.EqualFold(scheme, "Basic")) {
				v = scheme + " [REDACTED]"
			} else {
				v = "[REDACTED]"
			}
		}
		out[strings.ToLower(k)] = v
	}
	return out
}

// capBuffer 只保存前 max 个字节，但统计全部字节数。
type capBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	max   int
	total int64
}

func (c *capBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total += int64(len(p))
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

func (c *capBuffer) text() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String(), c.total > int64(c.buf.Len())
}

// exchangeRec 是 Provider 内部使用的记录辅助对象，nil 时所有方法都是空操作。
type exchangeRec struct {
	rec    *Recorder
	ex     *Exchange
	stream *capBuffer
	body   *capBuffer
	once   sync.Once
}

// startExchange 在发送请求前调用。
func startExchange(ctx context.Context, provider string, hreq *http.Request, body []byte, stream bool) *exchangeRec {
	rec := RecorderFrom(ctx)
	if rec == nil || rec.OnExchange == nil {
		return nil
	}
	reqBody := string(body)
	truncated := false
	if len(reqBody) > rec.max() {
		reqBody, truncated = cutBytes(reqBody, rec.max()), true
	}
	return &exchangeRec{rec: rec, ex: &Exchange{
		Provider: provider, Method: hreq.Method, URL: hreq.URL.String(), RequestHeaders: RedactHeaders(hreq.Header),
		RequestBody: reqBody, Stream: stream, Start: time.Now(), Truncated: truncated,
	}}
}

// response 记录响应头。
func (e *exchangeRec) response(resp *http.Response) {
	if e == nil {
		return
	}
	e.ex.Status = resp.StatusCode
	e.ex.ResponseHeaders = RedactHeaders(resp.Header)
	e.ex.TTFBMS = time.Since(e.ex.Start).Milliseconds()
}

// tee 让读取响应体的同时保存一份原始字节。
func (e *exchangeRec) tee(r io.Reader, streaming bool) io.Reader {
	if e == nil {
		return r
	}
	b := &capBuffer{max: e.rec.max()}
	if streaming {
		e.stream = b
	} else {
		e.body = b
	}
	return io.TeeReader(r, b)
}

// done 结束记录并回调。
func (e *exchangeRec) done(err error) {
	if e == nil {
		return
	}
	e.once.Do(func() {
		ex := e.ex
		ex.Duration = time.Since(ex.Start)
		ex.LatencyMS = ex.Duration.Milliseconds()
		if e.stream != nil {
			s, cut := e.stream.text()
			ex.RawStream, ex.ResponseBytes = s, e.stream.total
			ex.Truncated = ex.Truncated || cut
		}
		if e.body != nil {
			s, cut := e.body.text()
			ex.ResponseBody, ex.ResponseBytes = s, e.body.total
			ex.Truncated = ex.Truncated || cut
		}
		if err != nil {
			ex.Error = err.Error()
			var ae *APIError
			if ex.ResponseBody == "" && asAPIError(err, &ae) {
				ex.ResponseBody = ae.Body
			}
		}
		e.rec.OnExchange(ex)
	})
}

func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
