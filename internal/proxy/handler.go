package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wb2go/wb2go/internal/account"
	"github.com/wb2go/wb2go/internal/config"
	"github.com/wb2go/wb2go/internal/store"
	"github.com/wb2go/wb2go/internal/upstream"
	"github.com/wb2go/wb2go/internal/usage"
)

// Deps 是代理层依赖的全部组件。
type Deps struct {
	Cfg       *config.Runtime
	Store     *store.Store
	Pool      *account.Pool
	Sticky    *account.StickyRouter
	API       *upstream.API
	Usage     *usage.Store
	Version   string
	StartedAt time.Time

	// OnCreditsChange 余额变化时回写池状态。
	OnCreditsChange func(uid string, remain float64, expireAt int64)
	// RefreshCreds 刷新并持久化 token。
	RefreshCreds func(ctx context.Context, creds store.Credentials) (store.Credentials, error)
}

// Handler 是 API 处理器。
type Handler struct {
	d       Deps
	seq     int64
	started time.Time
}

// New 建处理器。
func New(d Deps) *Handler {
	return &Handler{d: d, started: d.StartedAt}
}

// ---------- 鉴权 ----------

// keyEntry 是一条 API Key 的配置。
type keyEntry struct {
	Name    string   `json:"name"`
	Key     string   `json:"key"`
	Enabled bool     `json:"enabled"`
	Realm   string   `json:"realm,omitempty"`  // 固定出口区域；空 = 跟随全局
	Models  []string `json:"models,omitempty"` // 模型白名单；空 = 不限
	// 签名不落盘：面板保存时只写哈希
	sha string
}

// verifyKey 校验请求的 API Key 并返回命中的 Key 配置。
//
// 只支持一个全局 Key（配置里的 api_key）。
// 多 Key 与模型白名单作为面板扩展点预留（见 docs/DESIGN.md）。
//
// 用常量时间比较：逐字节比较会在比较耗时的差异里泄露密钥前缀。
func (h *Handler) verifyKey(r *http.Request) bool {
	cfg := h.d.Cfg.Snapshot()
	if cfg.APIKey == "" {
		return true // 未配置密钥 = 不鉴权（仅适合本机/内网）
	}
	got := bearerToken(r)
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(cfg.APIKey)) == 1
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	// Anthropic SDK 用 x-api-key
	if v := r.Header.Get("x-api-key"); v != "" {
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(h)
}

// ---------- 入口路由 ----------

// Register 注册 API 路由。
func (h *Handler) Register(mux *http.ServeMux) {
	// OpenAI 兼容
	mux.HandleFunc("/v1/chat/completions", h.guard(h.handleChat))
	mux.HandleFunc("/v1/completions", h.guard(h.handleChat))
	mux.HandleFunc("/chat/completions", h.guard(h.handleChat))
	mux.HandleFunc("/v1/responses", h.guard(h.handleResponses))
	mux.HandleFunc("/responses", h.guard(h.handleResponses))
	mux.HandleFunc("/v1/models", h.guard(h.handleModels))
	mux.HandleFunc("/models", h.guard(h.handleModels))
	mux.HandleFunc("/v1/status", h.guard(h.handleStatus))
	mux.HandleFunc("/status", h.guard(h.handleStatus))
}

// guard 包一层鉴权。401 响应也带安全头（在面板层统一加）。
func (h *Handler) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.verifyKey(r) {
			writeErr(w, http.StatusUnauthorized, "Invalid API key",
				"请求缺少或携带了错误的 API Key。请在设置里配置 api_key 后重试。", "authentication_error")
			return
		}
		next(w, r)
	}
}

// ---------- 聊天主流程 ----------

func (h *Handler) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "Method not allowed", "请使用 POST 请求。", "invalid_request_error")
		return
	}
	// 不设请求体上限：多图会话的 base64 图片会膨胀 37%，
	// 网关侧提前拦截只会让用户看到无信息的 413，而上游的错误信息量更大。
	body, err := io.ReadAll(io.LimitReader(r.Body, 200<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "读取请求体失败："+err.Error(), "invalid_request_error")
		return
	}
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request",
			"请求体不是合法 JSON："+err.Error(), "invalid_request_error")
		return
	}
	h.dispatchChat(w, r, in, false)
}

// dispatchChat 是聊天主逻辑。
//
// 轮转策略：最多尝试 (账号数 + 2) 轮，每轮换一个账号。
// 确定性错误（内容审核 / 请求体畸形 / 图片非法）立刻返回，不轮转——
// 换个号撞的是同一堵墙，轮转只会白白消耗其他账号的额度并污染它们的计数。
func (h *Handler) dispatchChat(w http.ResponseWriter, r *http.Request, in map[string]any, isResponses bool) {
	cfg := h.d.Cfg.Snapshot()
	model := stringOf(in["model"])
	if model == "" {
		model = "default"
	}
	stream, _ := in["stream"].(bool)
	messages, _ := in["messages"].([]any)

	// 区域路由：客户端协议里没有"区域"概念，只给模型名。
	// 因此按模型名判断它属于哪套体系 —— gpt-*/grok-*/gemini-* 属国际版，
	// 其余默认国内版。这样同一个网关可以同时调度两区的账号，
	// 而用户只需按模型名调用，不用关心区域。
	realm := cfg.DefaultRealm
	if mRealm, known := RealmOfModel(model); known && mRealm != "" {
		realm = mRealm
	}
	keyName := ""

	// 会话键：显式 ID 优先，否则用内容前缀派生
	sid := account.ExtractKey(metaOf(in), in)
	if sid == "" {
		sid = h.d.Sticky.DerivePrefixKey(messages)
	}
	if h.d.Pool.WAFBlockedFor(time.Now()) {
		writeErr(w, http.StatusServiceUnavailable, "waf_blocked",
			"网关出口 IP 当前被上游 WAF 拦截，已自动暂停转发 60 秒后自动恢复。", "upstream_error")
		return
	}

	clientIP, ua := clientInfo(r)
	started := time.Now()
	rec := usage.Record{
		Model: model, Realm: realm, KeyName: keyName,
		Stream: stream, ClientIP: clientIP, UserAgent: ua,
		RequestID: "req-" + fmt.Sprintf("%d", time.Now().UnixNano()%1e9),
	}
	var ttfb int64

	try := map[string]bool{}
	maxRounds := 4
	if n, _ := h.d.Pool.Healthy(); n > maxRounds {
		maxRounds = n + 1
	}

	for round := 0; round < maxRounds; round++ {
		ent, cred := h.pickOne(realm, model, sid, try)
		if ent == nil {
			h.d.Usage.Record(rec)
			writeErr(w, http.StatusServiceUnavailable, "no_available_account",
				"当前没有可用账号（可能都在冷却或限流中）。请稍后重试，或在面板检查账号状态。", "unavailable_error")
			return
		}
		try[ent.UID] = true

		// 在途租约：防止单号被并发压垮
		if !h.d.Pool.Acquire(ent.UID, realm) {
			continue
		}

		res := h.oneAttempt(r.Context(), cfg, ent, cred, in, model, realm, sid, stream)
		h.d.Pool.Release(ent.UID)

		if res.err == nil {
			h.d.Pool.Report(ent.UID, model, account.ActionSuccess, 200, "", time.Now())
			if sid != "" {
				// 粘性跟随实际成功的号
				h.d.Sticky.Bind(sid, ent.UID, time.Now())
			}
			rec.UID = ent.UID
			rec.Status = res.status
			rec.Outcome = "completed"
			rec.PromptTok = res.usage.PromptTokens
			rec.CompletionTok = res.usage.CompletionTokens
			rec.CachedTok = res.usage.CachedTokens
			rec.ReasoningTok = res.usage.ReasoningTokens
			rec.TotalTok = res.usage.TotalTokens
			rec.TTFBms = ttfb
			rec.Durationms = time.Since(started).Milliseconds()
			rec.Identity = cred.Identity
			if rec.TotalTok > 0 {
				h.d.Pool.TouchDailyTokens(ent.UID, int64(rec.TotalTok))
			}
			h.d.Usage.Record(rec)
			res.write(w, r)
			return
		}

		// 按错误分类处置账号
		kind := res.err.Kind
		now := time.Now()
		switch {
		case kind == upstream.KindContentBlock, kind == upstream.KindBadRequest:
			// 确定性错误：不罚号、不轮转
			h.d.Pool.Report(ent.UID, model, account.ActionFailFast, res.err.Code, res.err.Msg, now)
			rec.UID, rec.Status, rec.Outcome = ent.UID, res.err.Code, "error"
			rec.ErrorKind = kind.String()
			rec.Durationms = time.Since(started).Milliseconds()
			h.d.Usage.Record(rec)
			writeUpstreamErr(w, res.err)
			return

		case kind == upstream.KindModelRate:
			// 只冷却该模型，账号仍可用于其他模型
			until := now.Add(cfg.SoftRateD)
			if res.err.ResetAt > 0 {
				until = time.Unix(res.err.ResetAt, 0)
				if max := now.Add(cfg.SoftRateMaxD); until.After(max) {
					until = max
				}
			}
			h.d.Pool.NoteModelCool(ent.UID, model, until)
			h.d.Pool.Report(ent.UID, model, account.ActionRetry, res.err.Code, res.err.Msg, now)
			if sid != "" {
				// 换模型请求时自动解绑：粘住的号被模型级限额后，
				// 同一模型继续请求就会一直撞冷却
				h.d.Sticky.Unbind(sid)
			}
			continue

		case kind == upstream.KindWAF:
			h.d.Pool.Report(ent.UID, model, account.ActionRetry, res.err.Code, res.err.Msg, now)
			if h.d.Pool.NoteWAF403(ent.UID, now) {
				h.d.Usage.Record(rec)
				writeErr(w, http.StatusServiceUnavailable, "waf_blocked",
					"网关出口 IP 被上游 WAF 拦截（多个账号同时被拒），已暂停转发 60 秒。", "upstream_error")
				return
			}
			continue

		default:
			h.d.Pool.Report(ent.UID, model, account.ActionRetry, res.err.Code, res.err.Msg, now)
			if sid != "" {
				h.d.Sticky.Unbind(sid)
			}
			continue
		}
	}

	rec.Outcome = "error"
	rec.ErrorKind = "轮转耗尽"
	rec.Durationms = time.Since(started).Milliseconds()
	h.d.Usage.Record(rec)
	writeErr(w, http.StatusBadGateway, "upstream_unavailable",
		fmt.Sprintf("已尝试 %d 个账号均未成功。最后一次错误：%s", len(try), "上游请求失败"), "upstream_error")
}

// attemptResult 是一次尝试的结果。
type attemptResult struct {
	err    *upstream.APIError
	status int
	usage  *upstream.Usage
	// write 把成功结果写给客户端
	write func(http.ResponseWriter, *http.Request)
}

// pickOne 选一个账号：优先粘性，其次正常选号。
func (h *Handler) pickOne(realm, model, sid string, try map[string]bool) (*account.Entry, store.Credentials) {
	// 1. 粘性命中优先
	if sid != "" && !try[""] {
		if uid := h.d.Sticky.Resolve(sid, time.Now()); uid != "" && !try[uid] {
			if ent, ok := h.d.Pool.Get(uid); ok && ent.AvailableFor(time.Now(), model) {
				if cred, ok := h.d.Store.Get(uid); ok {
					h.d.Pool.SetIdentityFor(uid, pickIdentity(ent.Identity, cred.Identity))
					cred.Identity = ent.Identity
					return ent, cred
				}
			}
		}
	}
	// 2. 正常选号
	ent := h.d.Pool.Pick(realm, model, try)
	if ent == nil {
		return nil, store.Credentials{}
	}
	cred, ok := h.d.Store.Get(ent.UID)
	if !ok {
		return nil, store.Credentials{}
	}
	// 账号级身份优先于全局默认
	if ent.Identity != "" {
		cred.Identity = ent.Identity
	} else if cred.Identity == "" {
		cred.Identity = ""
	}
	return ent, cred
}

func pickIdentity(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// oneAttempt 对一个账号发一次请求。
func (h *Handler) oneAttempt(ctx context.Context, cfg *config.Config, ent *account.Entry,
	cred store.Credentials, in map[string]any, model, realm, sid string, stream bool) *attemptResult {

	// 1. token 快过期先刷新
	if cred.NeedRefresh(time.Now().Unix()) && h.d.RefreshCreds != nil {
		fresh, err := h.d.RefreshCreds(ctx, cred)
		if err != nil {
			return &attemptResult{err: &upstream.APIError{
				Kind: upstream.KindSessionDead, Code: 401, Msg: "刷新 token 失败: " + err.Error()}}
		}
		cred = fresh
	}

	// 2. 构造身份与指纹
	id, err := upstream.BuildIdentity(credIdentity(cfg, cred.Identity), cfg.DesktopVer, cfg.CLIVersion, cfg.ProductLabel)
	if err != nil {
		return &attemptResult{err: &upstream.APIError{Kind: upstream.KindUnknown, Msg: err.Error()}}
	}
	id = id.Resolve(realm)
	seq := atomic.AddInt64(&h.seq, 1)
	fp := upstream.NewFingerprint(cred.UID, id)

	// 3. 组装出站请求体
	opt := upstream.Options{
		Identity: id, Fingerprint: fp, Model: model, Realm: realm,
		Stream: stream, Messages: extractMsgs(in),
		System: cfg.SystemPrompt, Effort: upstream.EffortForModel(in),
		Thinking: upstream.ThinkingEnabled(in), Sanitize: cfg.Sanitize,
		SanitizeSys: cfg.HideOwnPrompt, Sid: sid,
	}
	if cfg.CacheKey && fp.MachineID != "" {
		opt.CacheKey = fp.CacheKey(sid)
	}
	out := upstream.PrepareBody(in, opt)
	payload, _ := json.Marshal(out)

	// 4. 发起流式请求（带空闲续命）
	chatCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	headers := h.d.API.ChatHeaders(upstream.Creds{
		UID: cred.UID, AccessToken: cred.AccessToken, Realm: realm, Identity: id.Key,
		EnterpriseID: cred.EnterpriseID,
	}, id, seq)

	var idleTimer *time.Timer
	idleLimit := time.Duration(cfg.ChatTimeout) * time.Second
	touch := func() {
		if idleTimer != nil {
			idleTimer.Reset(idleLimit)
		}
	}
	idleTimer = time.AfterFunc(idleLimit, func() { cancel() })
	defer idleTimer.Stop()

	resp, err := h.d.API.Client.ChatStream(chatCtx, id.ChatBase+upstream.PathChatCompletions,
		headers, payload, touch)
	if err != nil {
		kind := upstream.KindTransport
		if chatCtx.Err() != nil {
			kind = upstream.KindServer // 超时按上游故障处理，喂熔断计数
		}
		return &attemptResult{err: &upstream.APIError{Kind: kind, Msg: err.Error()}}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := upstream.ReadCST(resp)
		apiErr := upstream.Classify(resp.StatusCode, raw, model)
		if apiErr.Kind == upstream.KindTransport {
			apiErr.Kind = upstream.KindServer
		}
		return &attemptResult{err: apiErr}
	}

	// 5. 分流：流式直接转发，非流式聚合
	if !stream {
		return h.aggregate(chatCtx, resp, model)
	}
	return h.forwardStream(chatCtx, w0{}, resp, model)
}

// credIdentity 决定本次用哪套身份：账号设置优先于全局配置。
func credIdentity(cfg *config.Config, perAccount string) string {
	if perAccount != "" {
		return perAccount
	}
	return cfg.Identity
}

// forwardStream 把上游 SSE 转发给客户端。
func (h *Handler) forwardStream(ctx context.Context, _ w0, resp *http.Response, model string) *attemptResult {
	// 注意：真正写响应要等 attemptResult.write 被调用，
	// 因为此时还不能确定这个账号会不会成功 —— 若后续还有轮转，
	// 已经写出去的状态码就收不回来了。
	//
	// 因此这里把 resp.Body 挂在 result 上，write 时才真正开始转写。
	body := resp.Body
	return &attemptResult{
		err:    nil,
		status: http.StatusOK,
		write: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-Accel-Buffering", "no") // 关掉反代的缓冲，否则流式会被攒住
			w.WriteHeader(http.StatusOK)

			wr := upstream.NewWriter(w, flusher(w), model)
			p := upstream.NewSSEParser(body)
			var agg *upstream.Aggregator
			for {
				data, ok, err := p.Next()
				if err != nil || !ok {
					break
				}
				ch, cerr := upstream.ToChunk(data)
				if cerr != nil {
					continue // 跳过畸形帧，不中断整个流
				}
				wr.WriteChunk(ch)
				if agg == nil {
					agg = upstream.NewAggregator()
				}
				agg.Add(ch)
			}
			wr.Done()
			_ = agg
		},
	}
}

// aggregate 把 SSE 聚合成一个非流式响应。
func (h *Handler) aggregate(_ context.Context, resp *http.Response, model string) *attemptResult {
	p := upstream.NewSSEParser(resp.Body)
	agg := upstream.NewAggregator()
	for {
		data, ok, err := p.Next()
		if err != nil || !ok {
			break
		}
		ch, cerr := upstream.ToChunk(data)
		if cerr != nil {
			// 流中途的错误帧要如实转成 HTTP 错误，不能伪装成正常结束
			if apiErr, ok := parseStreamErr(cerr); ok {
				return &attemptResult{err: apiErr}
			}
			continue
		}
		agg.Add(ch)
	}
	return &attemptResult{
		status: http.StatusOK,
		write: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(agg.Result(model))
		},
	}
}

func parseStreamErr(err error) (*upstream.APIError, bool) {
	msg := err.Error()
	if !strings.Contains(msg, "上游返回错误") {
		return nil, false
	}
	return &upstream.APIError{Kind: upstream.KindServer, Msg: msg}, true
}

// w0 是占位类型（转发函数暂不需要它）。
type w0 struct{}

func flusher(w http.ResponseWriter) func() {
	f, ok := w.(http.Flusher)
	if !ok {
		return func() {}
	}
	return f.Flush
}

// metaOf 取请求里的 metadata 对象。
func metaOf(in map[string]any) map[string]any {
	if m, ok := in["metadata"].(map[string]any); ok {
		return m
	}
	return nil
}

func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// clientIP 提取客户端 IP。
//
// 取 X-Forwarded-For 首段：这是常规做法。但如果这个网关前面还有一层
// 自己可控的代理，首段就可能被伪造 —— 仅用于展示与统计，不用于安全判定。
func clientInfo(r *http.Request) (ip, ua string) {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		ip = strings.TrimSpace(strings.Split(v, ",")[0])
	} else if v := r.Header.Get("X-Real-IP"); v != "" {
		ip = strings.TrimSpace(v)
	} else {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			ip = host
		}
	}
	if len(ip) > 0 && net.ParseIP(ip) == nil {
		ip = "" // 明显不是 IP 的值不写进日志
	}
	ua = r.Header.Get("User-Agent")
	if len(ua) > 200 {
		ua = ua[:200]
	}
	return ip, ua
}

// hashOf 用于缓存键等场景。
func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum[:8])
}

// ---------- Responses API ----------

// handleResponses 实现 OpenAI Responses API。
//
// 做法：在 Chat Completions 之上做双向翻译。客户端拥有全部工具，
// 网关只做协议形状转换 + 指纹脱敏，不执行任何工具。
func (h *Handler) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "Method not allowed", "请使用 POST 请求。", "invalid_request_error")
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 200<<20))
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "请求体不是合法 JSON", "invalid_request_error")
		return
	}
	chat, err := responsesToChat(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error(), "invalid_request_error")
		return
	}
	// Responses 的流式事件与 Chat 的 SSE 不同，
	// 因此统一走非流式聚合，再翻译成 Responses 事件流。
	chat["stream"] = false
	streamOut, _ := in["stream"].(bool)
	respIn := &responsesShim{stream: streamOut}

	rr := newCapturingWriter()
	h.dispatchChatResponses(rr, r, chat, respIn)
	rr.flushTo(w)
}

// responsesShim 承载 Responses 协议的状态。
type responsesShim struct{ stream bool }

func (h *Handler) dispatchChatResponses(rr *capturingWriter, r *http.Request, chat map[string]any, shim *responsesShim) {
	// 复用 dispatchChat 的轮转逻辑，但把结果转成 Responses 形态。
	// 这里用内建 recorder 而非直接写 w，便于做协议形态转换。
	cfg := h.d.Cfg.Snapshot()
	model := stringOf(chat["model"])
	realm := cfg.DefaultRealm
	msgs, _ := chat["messages"].([]any)
	sid := account.ExtractKey(metaOf(chat), chat)
	if sid == "" {
		sid = h.d.Sticky.DerivePrefixKey(msgs)
	}
	try := map[string]bool{}
	for round := 0; round < 4; round++ {
		ent, cred := h.pickOne(realm, model, sid, try)
		if ent == nil {
			writeErr(rr, http.StatusServiceUnavailable, "no_available_account", "当前没有可用账号。", "unavailable_error")
			return
		}
		try[ent.UID] = true
		if !h.d.Pool.Acquire(ent.UID, realm) {
			continue
		}
		res := h.oneAttempt(r.Context(), cfg, ent, cred, chat, model, realm, sid, false)
		h.d.Pool.Release(ent.UID)
		if res.err == nil {
			h.d.Pool.Report(ent.UID, model, account.ActionSuccess, 200, "", time.Now())
			if sid != "" {
				h.d.Sticky.Bind(sid, ent.UID, time.Now())
			}
			if res.usage != nil && res.usage.TotalTokens > 0 {
				h.d.Pool.TouchDailyTokens(ent.UID, int64(res.usage.TotalTokens))
			}
			// 转成 Responses 形态
			res.write(rr, r)
			rr.setResponses(chatToResponses(rr.bodyJSON(), model, shim.stream))
			return
		}
		h.d.Pool.Report(ent.UID, model, account.ActionRetry, res.err.Code, res.err.Msg, time.Now())
		if sid != "" {
			h.d.Sticky.Unbind(sid)
		}
	}
	writeErr(rr, http.StatusBadGateway, "upstream_unavailable", "所有账号均失败。", "upstream_error")
}

// capturingWriter 是一个内存 ResponseWriter，用于协议形态转换。
type capturingWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
	resp   any
}

func newCapturingWriter() *capturingWriter {
	return &capturingWriter{header: http.Header{}, status: http.StatusOK}
}

func (c *capturingWriter) Header() http.Header { return c.header }
func (c *capturingWriter) WriteHeader(s int)   { c.status = s }
func (c *capturingWriter) Write(p []byte) (int, error) {
	return c.body.Write(p)
}
func (c *capturingWriter) bodyJSON() map[string]any {
	var m map[string]any
	_ = json.Unmarshal(c.body.Bytes(), &m)
	return m
}
func (c *capturingWriter) setResponses(v any) { c.resp = v }

// flushTo 把转换后的结果写给真实客户端。
func (c *capturingWriter) flushTo(w http.ResponseWriter) {
	for k, vs := range c.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if c.resp == nil {
		w.WriteHeader(c.status)
		w.Write(c.body.Bytes())
		return
	}
	data, _ := json.Marshal(c.resp)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(c.status)
	w.Write(data)
}

// ---------- 模型列表与状态 ----------

func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	cfg := h.d.Cfg.Snapshot()
	// 支持 ?realm=intl 只看某区的模型；默认返回全部，
	// 让客户端能同时看到两区的模型名（同名模型会带 realm 字段区分）
	realm := r.URL.Query().Get("realm")
	if realm != "" && !upstream.ValidRealm(realm) {
		writeErr(w, http.StatusBadRequest, "invalid_request",
			"未知区域："+realm+"（只能是 cn 或 intl）", "invalid_request_error")
		return
	}
	models := modelsForRealm(realm)
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   models,
		"realm":  realmOrDefault(cfg.DefaultRealm, realm),
	})
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	healthy, total := h.d.Pool.Healthy()
	entries := h.d.Pool.List()
	now := time.Now()
	accs := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		accs = append(accs, map[string]any{
			"uid": e.UID, "label": e.Nickname, "realm": e.Realm,
			"status": e.StatusLabel(now), "credits": e.Credits,
			"cool_remaining": e.CoolRemaining(now),
			"in_flight":      e.InFlight,
			"disabled":       e.Disabled, "disabled_reason": e.DisabledReason,
			"last_error": e.LastError, "identity": e.Identity,
		})
	}
	// 按区域汇总，前端据此展示"国内 N 个 / 国际 M 个"
	byRealm := map[string]int{}
	for _, e := range entries {
		byRealm[e.Realm]++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "wb2go", "version": h.d.Version,
		"realms":         byRealm,
		"uptime_seconds": int(time.Since(h.started).Seconds()),
		"healthy":        healthy, "total": total,
		"in_flight":     h.d.Pool.InFlightCount(),
		"sticky_binds":  h.d.Sticky.Count(),
		"waf_blocked":   h.d.Pool.WAFBlockedFor(now),
		"accounts":      accs,
		"default_realm": h.d.Cfg.Snapshot().DefaultRealm,
		"identity":      h.d.Cfg.Snapshot().Identity,
	})
}

// ---------- 工具函数 ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr 写一个 OpenAI 风格的错误体。
func writeErr(w http.ResponseWriter, status int, code, msg, typ string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": code},
	})
}

func writeUpstreamErr(w http.ResponseWriter, e *upstream.APIError) {
	status := http.StatusBadGateway
	switch e.Kind {
	case upstream.KindContentBlock:
		status = http.StatusBadRequest
	case upstream.KindCredits, upstream.KindSoftRate, upstream.KindModelRate:
		status = http.StatusTooManyRequests
	case upstream.KindSessionDead:
		status = http.StatusUnauthorized
	case upstream.KindNotFound:
		status = http.StatusNotFound
	case upstream.KindBadRequest:
		status = http.StatusBadRequest
	}
	writeErr(w, status, e.Kind.String(), e.Msg, "upstream_error")
}

// responsesToChat 把 Responses 请求翻译成 Chat 请求。
func responsesToChat(in map[string]any) (map[string]any, error) {
	out := map[string]any{}
	if m := stringOf(in["model"]); m != "" {
		out["model"] = m
	}
	var msgs []any
	// instructions 变成 system 消息
	if ins := stringOf(in["instructions"]); ins != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": ins})
	}
	items, _ := in["input"].([]any)
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			// input 也可以是一段纯字符串
			if s, ok := it.(string); ok && s != "" {
				msgs = append(msgs, map[string]any{"role": "user", "content": s})
			}
			continue
		}
		switch stringOf(m["type"]) {
		case "", "message":
			role := stringOf(m["role"])
			if role == "" {
				role = "user"
			}
			msg := map[string]any{"role": role, "content": flattenContent(m["content"])}
			msgs = append(msgs, msg)
		case "function_call":
			msgs = append(msgs, map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id": stringOf(m["call_id"]), "type": "function",
					"function": map[string]any{"name": stringOf(m["name"]), "arguments": stringOf(m["arguments"])},
				}},
			})
		case "function_call_output":
			msgs = append(msgs, map[string]any{
				"role": "tool", "tool_call_id": stringOf(m["call_id"]),
				"content": flattenContent(m["output"]),
			})
		case "reasoning":
			// 上一轮的思考内容：thinking 模式下上游要求回传
			if txt := flattenReasoning(m); txt != "" {
				msgs = append(msgs, map[string]any{
					"role": "assistant", "content": "",
					"reasoning_content": txt,
				})
			}
		}
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("input 里没有可识别的消息（需要字符串或 message / function_call 等 item）")
	}
	out["messages"] = msgs
	if v, ok := in["max_output_tokens"]; ok {
		out["max_tokens"] = v
	}
	if v, ok := in["stream"]; ok {
		out["stream"] = v
	}
	// tools：只转发 function 类型的自定义工具
	if tools, ok := in["tools"].([]any); ok {
		var ct []any
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok || stringOf(tm["type"]) != "function" {
				continue // 上游不支持 web_search 等服务端工具，声明透传会得到 unsupported call
			}
			ct = append(ct, tm)
		}
		if len(ct) > 0 {
			out["tools"] = ct
		}
	}
	// reasoning.effort 映射为 reasoning_effort
	if r, ok := in["reasoning"].(map[string]any); ok {
		if e, ok := r["effort"].(string); ok && e != "" {
			out["reasoning_effort"] = e
		}
	}
	return out, nil
}

func flattenContent(v any) any {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, p := range t {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			ty := stringOf(pm["type"])
			if ty == "input_text" || ty == "text" || ty == "output_text" {
				b.WriteString(stringOf(pm["text"]))
			} else if ty == "input_image" {
				if u := stringOf(pm["image_url"]); u != "" {
					return appendToImages(b.String(), u)
				}
			}
		}
		return b.String()
	}
	return v
}

func appendToImages(text, url string) any {
	parts := []any{}
	if text != "" {
		parts = append(parts, map[string]any{"type": "text", "text": text})
	}
	parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
	return parts
}

func flattenReasoning(m map[string]any) string {
	if s := stringOf(m["summary"]); s != "" {
		return s
	}
	if list, ok := m["summary"].([]any); ok {
		var b strings.Builder
		for _, s := range list {
			if sm, ok := s.(map[string]any); ok {
				b.WriteString(stringOf(sm["text"]))
			}
		}
		return b.String()
	}
	if txt := stringOf(m["content"]); txt != "" {
		return txt
	}
	return ""
}

// chatToResponses 把 Chat 响应翻译成 Responses 形态。
func chatToResponses(chat map[string]any, model string, stream bool) any {
	resp := map[string]any{
		"id":     "resp_" + hashOf(fmt.Sprint(chat["id"])),
		"object": "response",
		"model":  model,
		"status": "completed",
	}
	choices, _ := chat["choices"].([]any)
	var out []any
	if len(choices) > 0 {
		c, _ := choices[0].(map[string]any)
		msg, _ := c["message"].(map[string]any)
		if msg != nil {
			// 思考内容单独成一个 reasoning item
			if rc := stringOf(msg["reasoning_content"]); rc != "" {
				out = append(out, map[string]any{
					"type": "reasoning",
					"summary": []any{map[string]any{
						"type": "summary_text", "text": rc,
					}},
				})
			}
			content := []any{}
			if t := stringOf(msg["content"]); t != "" {
				content = append(content, map[string]any{"type": "output_text", "text": t})
			}
			if tcs, ok := msg["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					tcm, _ := tc.(map[string]any)
					fn, _ := tcm["function"].(map[string]any)
					content = append(content, map[string]any{
						"type":    "function_call",
						"call_id": tcm["id"], "name": fn["name"], "arguments": fn["arguments"],
					})
				}
			}
			if len(content) > 0 {
				out = append(out, map[string]any{"type": "message", "role": "assistant", "content": content})
			}
			resp["output"] = out
			resp["output_text"] = stringOf(msg["content"])
		}
	}
	if u, ok := chat["usage"].(map[string]any); ok {
		resp["usage"] = u
	}
	return resp
}

// extractMsgs 从请求体里取消息数组。
func extractMsgs(in map[string]any) []any {
	m, _ := in["messages"].([]any)
	return m
}
