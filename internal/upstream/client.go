package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Client 是上游 HTTP 客户端。
//
// 超时是分层的，原因在各自的构造里说明。三段各归其位，不要合并成一个 Timeout。
type Client struct {
	chat      *http.Client // 聊天：无总时长上限，只有首字节与空闲超时
	rpc       *http.Client // 短 RPC：有总时长硬上限
	transport *http.Transport
	reqSeq    int64
}

// NewClient 构造客户端。
//
// 聊天流刻意用 Timeout=0（无总时长限制）：长思考 / 长输出不该被总时长掐断，
// 而首字节（header_timeout）与流中空闲（idle_timeout）两道闸已经足够防挂死。
// 若沿用官方 Node 客户端默认的 300s **总** requestTimeout，
// 活跃的 SSE 流也会被误判为超时——这是必修的坑。
func NewClient(headerTimeout, idleTimeout, rpcTimeout time.Duration) *Client {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second, // 连接超时：连不上的号别干等 5 分钟
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
		ForceAttemptHTTP2:     true,
	}
	return &Client{
		transport: tr,
		chat: &http.Client{
			Transport: tr,
			Timeout:   0, // 无总时长上限
		},
		rpc: &http.Client{
			Transport: tr,
			Timeout:   rpcTimeout,
		},
	}
}

// Close 关闭空闲连接。
func (c *Client) Close() { c.transport.CloseIdleConnections() }

// ChatStream 发起一次流式聊天请求。调用方负责关闭 resp.Body。
//
// onIdle 每收到一段数据就会被调用一次 —— 调用方可用它在活跃时"续命"，
// 实现"静默才掐流"的语义。
func (c *Client) ChatStream(ctx context.Context, url string, headers map[string]string, body []byte, onIdle func()) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = cloneHeaders(headers)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.chat.Do(req)
	if err != nil {
		return nil, wrapTransportErr(err)
	}
	if onIdle != nil {
		resp.Body = &idleReader{rc: resp.Body, onData: onIdle}
	}
	return resp, nil
}

// PostJSON 发起一次短 RPC（签到 / 积分 / 刷新 token / 模型列表）。
func (c *Client) PostJSON(ctx context.Context, url string, headers map[string]string, body any) (*http.Response, error) {
	var buf []byte
	switch b := body.(type) {
	case nil:
	case []byte:
		buf = b
	case string:
		buf = []byte(b)
	default:
		var err error
		if buf, err = json.Marshal(b); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header = cloneHeaders(headers)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.rpc.Do(req)
	if err != nil {
		return nil, wrapTransportErr(err)
	}
	return resp, nil
}

// GetJSON 发起一次 GET（模型列表等）。
func (c *Client) GetJSON(ctx context.Context, url string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header = cloneHeaders(headers)
	req.Header.Set("Accept", "application/json")
	resp, err := c.rpc.Do(req)
	if err != nil {
		return nil, wrapTransportErr(err)
	}
	return resp, nil
}

// idleReader 在每次读到数据时触发回调。
//
// 实现"活跃续命"：上层用 context deadline 表达空闲上限，
// 只要还在吐字就不断延长 deadline，静默超过阈值才断流。
type idleReader struct {
	rc     io.ReadCloser
	onData func()
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 && r.onData != nil {
		r.onData()
	}
	return n, err
}

func (r *idleReader) Close() error { return r.rc.Close() }

func cloneHeaders(h map[string]string) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		out.Set(k, v)
	}
	return out
}

// wrapTransportErr 把传输层错误翻译成中文可读文案，并标出错误类型。
func wrapTransportErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "context deadline exceeded"):
		return fmt.Errorf("上游响应超时")
	case strings.Contains(msg, "connection refused"):
		return fmt.Errorf("上游拒绝连接（可能是出口 IP 被封或代理失效）")
	case strings.Contains(msg, "no such host"):
		return fmt.Errorf("域名解析失败（检查 DNS 或容器网络）")
	case strings.Contains(msg, "certificate") || strings.Contains(msg, "tls:"):
		return fmt.Errorf("TLS 握手失败: %w", err)
	case strings.Contains(msg, "EOF"):
		return fmt.Errorf("上游连接被中断")
	}
	return err
}

// ErrorKind 是上游错误的分类，决定账号如何处置。
type ErrorKind int

const (
	KindOK           ErrorKind = iota
	KindSoftRate               // 429 / 限流文案 → 软冷却，指数退避
	KindModelRate              // code 6004 → 只冷却该模型，账号仍可用
	KindCredits                // 402 / 余额关键词 → 硬冷却到次日 04:00
	KindSessionDead            // 会话失效 → 连续 3 次才禁用
	KindNotFound               // 404 → 固定短冷却
	KindContentBlock           // 内容审核 → 不罚号，fail-fast
	KindBadRequest             // 400 请求体畸形 → 不罚号，但可换号
	KindServer                 // 5xx → 喂熔断计数
	KindWAF                    // WAF 拦截 → 走 IP 级闸门
	KindTransport              // 网络层 → 连败降权
	KindUnknown
)

func (k ErrorKind) String() string {
	switch k {
	case KindSoftRate:
		return "限流"
	case KindModelRate:
		return "模型限流"
	case KindCredits:
		return "余额耗尽"
	case KindSessionDead:
		return "会话失效"
	case KindNotFound:
		return "模型不存在"
	case KindContentBlock:
		return "内容审核"
	case KindBadRequest:
		return "请求体错误"
	case KindServer:
		return "上游故障"
	case KindWAF:
		return "WAF 拦截"
	case KindTransport:
		return "传输失败"
	}
	return "未知"
}

// APIError 是分类后的上游错误。
type APIError struct {
	Kind    ErrorKind
	Code    int    // HTTP 状态码
	Msg     string // 可读错误信息（已截断）
	Raw     string // 上游业务 code（字符串形式）
	Model   string // 触发错误的模型（模型级限流时记录）
	ResetAt int64  // 上游给出的重置墙钟秒；0 = 未提供
}

func (e *APIError) Error() string {
	if e.Code > 0 {
		return fmt.Sprintf("%s（HTTP %d）%s", e.Kind, e.Code, e.Msg)
	}
	return fmt.Sprintf("%s %s", e.Kind, e.Msg)
}

// IsRateLimit 判断是否属于限流类（决定要不要走模型级冷却分支）。
func (e *APIError) IsRateLimit() bool {
	return e.Kind == KindSoftRate || e.Kind == KindModelRate
}

var (
	// 限流文案。上游不一定给标准状态码，文案也要认。
	reRateLimitCN = regexp.MustCompile(`(?i)(rate.?limit|too many requests|请求过于频繁|调用过于频繁|超过频率|限流)`)
	// 余额耗尽文案
	reCreditsCN = regexp.MustCompile(`(?i)(insufficient|余额不足|积分不足|额度已用尽|额度不足|exceed your|quota|balance)`)
	// 会话失效文案
	reSessionDead = regexp.MustCompile(`(?i)(session not found|offline user|会话不存在|会话已失效|12153)`)
	// 内容审核文案
	reContentBlock = regexp.MustCompile(`(?i)(blocked by security policy|unapproved channel|illegal api invocation|内容审核|违规|敏感词)`)
	// WAF 拦截
	reWAF = regexp.MustCompile(`(?i)(11128|waf|forbidden by|请求被拦截)`)
)

// reResetTime 解析上游给出的限流重置时间。
//
// 形如 "usage will reset at 2026-09-19 18:29:03 UTC+8"。
// 有了它就能把冷却精确到墙钟，而不是盲目退避。
var reResetTime = regexp.MustCompile(`(\d{4}-\d{2}-\d{2})[\sT]+(\d{2}:\d{2}:\d{2})`)

// Classify 把上游响应分类。判定优先级：余额 → 会话 → 限流 → 状态码兜底。
//
// 顺序有讲究：余额耗尽和会话失效都会返回非 2xx，若先按状态码判会把它们
// 误判成普通故障，处置方式就错了。
func Classify(status int, body []byte, model string) *APIError {
	text := string(body)
	msg := truncate(text, 400)
	if msg == "" {
		msg = http.StatusText(status)
	}
	apiCode := extractBusinessCode(body)

	e := &APIError{Code: status, Msg: msg, Raw: apiCode, Model: model}

	switch {
	case reCreditsCN.MatchString(text) && (status == 402 || strings.Contains(text, "余额") || strings.Contains(text, "积分")):
		e.Kind = KindCredits
	case reSessionDead.MatchString(text):
		e.Kind = KindSessionDead
	case reWAF.MatchString(text):
		// WAF 判定必须在内容审核之前："illegal api invocation from an unapproved channel"
		// 两组正则都会命中，但它的真实成因是指纹匹配拒流，处置是走 IP 级闸门。
		e.Kind = KindWAF
	case reContentBlock.MatchString(text):
		e.Kind = KindContentBlock
	case status == 429 || reRateLimitCN.MatchString(text):
		e.Kind = KindSoftRate
		if apiCode == "6004" {
			// 6004 是"该模型用量超限"，不是账号整体被限流
			e.Kind = KindModelRate
			if ts := reResetTime.FindStringSubmatch(text); len(ts) == 3 {
				if t, err := time.ParseInLocation("2006-01-02 15:04:05", ts[1]+" "+ts[2], cstZone()); err == nil {
					e.ResetAt = t.Unix()
				}
			}
		}
	case status == 402:
		e.Kind = KindCredits
	case status == 404:
		e.Kind = KindNotFound
	case status == 400:
		// 11101 是请求体解析失败：问题在客户端 JSON，与账号健康无关
		e.Kind = KindBadRequest
	case status >= 500:
		e.Kind = KindServer
	case status == 403:
		e.Kind = KindWAF
	case status >= 400:
		e.Kind = KindBadRequest
	case status >= 200 && status < 300:
		e.Kind = KindOK
	default:
		e.Kind = KindUnknown
	}
	return e
}

// extractBusinessCode 从响应体里取业务 code。
func extractBusinessCode(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	// 常见形态：{"code": 6004} 或 {"error": {"code": 6004}}
	if v, ok := m["code"]; ok {
		return toStr(v)
	}
	if errObj, ok := m["error"].(map[string]any); ok {
		if v, ok := errObj["code"]; ok {
			return toStr(v)
		}
	}
	return ""
}

func toStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case json.Number:
		return t.String()
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ReadCST 读取响应体（限长 4MB，避免异常响应打爆内存）。
func ReadCST(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func cstZone() *time.Location {
	// 上游的重置时间文案按 UTC+8 解释。容器 TZ 未必是 CST，
	// 因此这里显式用固定时区解析，不依赖运行环境。
	return time.FixedZone("UTC+8", 8*3600)
}
