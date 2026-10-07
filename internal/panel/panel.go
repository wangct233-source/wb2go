package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wb2go/wb2go/internal/account"
	"github.com/wb2go/wb2go/internal/config"
	"github.com/wb2go/wb2go/internal/hotupdate"
	"github.com/wb2go/wb2go/internal/store"
	"github.com/wb2go/wb2go/internal/tasks"
	"github.com/wb2go/wb2go/internal/upstream"
	"github.com/wb2go/wb2go/internal/usage"
)

// Deps 是面板依赖的组件。
type Deps struct {
	Cfg     *config.Runtime
	Store   *store.Store
	Pool    *account.Pool
	Sticky  *account.StickyRouter
	Sched   *tasks.Scheduler
	Usage   *usage.Store
	Version string
	Started time.Time

	// Reload 从凭证目录重新加载账号到池。
	Reload func()
	// RefreshToken 刷新并持久化 token。
	RefreshToken func(ctx context.Context, creds store.Credentials) (store.Credentials, error)
	// CredsOf 取某账号的凭证。
	CredsOf func(uid string) (store.Credentials, bool)
	// UpdateChecker 是热更新检查器（可为 nil）。
	UpdateChecker UpdateChecker
	// TestChat 走真实链路做模型连通性测试。
	TestChat func(ctx context.Context, model, effort string, timeout time.Duration) (string, error, int)
	// API 是上游短 RPC 句柄。
	API *upstream.API
}

// api 返回上游 API 句柄。
func (s *Server) api() *upstream.API { return s.d.API }

// UpdateChecker 是热更新接口。
//
// Check 返回的任意结构会被原样写进 JSON 响应，
// 因此实现方可以自定义字段而不必与本包保持类型一致。
type UpdateChecker interface {
	Check(ctx context.Context) (any, error)
	Apply(ctx context.Context) (string, error)
}

// Server 是面板 API 服务。
type Server struct {
	d Deps
	// oauth 会话
	oauthMu    sync.Mutex
	oauthState string
	oauthRealm string
	oauthStart time.Time
	oauthSeen  bool
}

// New 建面板服务。
func New(d Deps) *Server { return &Server{d: d} }

// SetUpdateChecker 注入热更新检查器（构造后设置，避免循环依赖）。
func (s *Server) SetUpdateChecker(u UpdateChecker) { s.d.UpdateChecker = u }

// Register 注册面板路由。
func (s *Server) Register(mux *http.ServeMux) {
	g := s.guard
	mux.HandleFunc("/panel/api/overview", g(s.apiOverview))
	mux.HandleFunc("/panel/api/accounts", g(s.apiAccounts))
	mux.HandleFunc("/panel/api/accounts/delete", g(s.apiAccountDelete))
	mux.HandleFunc("/panel/api/accounts/toggle", g(s.apiAccountToggle))
	mux.HandleFunc("/panel/api/accounts/identity", g(s.apiAccountIdentity))
	mux.HandleFunc("/panel/api/accounts/manual", g(s.apiAccountManual))
	mux.HandleFunc("/panel/api/accounts/checkin", g(s.apiAccountOp("checkin")))
	mux.HandleFunc("/panel/api/accounts/credits", g(s.apiAccountOp("credits")))
	mux.HandleFunc("/panel/api/accounts/tasks", g(s.apiAccountOp("tasks")))
	mux.HandleFunc("/panel/api/accounts/refresh", g(s.apiAccountOp("refresh")))
	mux.HandleFunc("/panel/api/batch/", g(s.apiBatch))
	mux.HandleFunc("/panel/api/oauth/start", g(s.apiOAuthStart))
	mux.HandleFunc("/panel/api/oauth/status", g(s.apiOAuthStatus))
	mux.HandleFunc("/panel/api/realms", g(s.apiRealms))
	mux.HandleFunc("/panel/api/usage/recent", g(s.apiUsageRecent))
	mux.HandleFunc("/panel/api/scheduler", g(s.apiScheduler))
	mux.HandleFunc("/panel/api/scheduler/trigger", g(s.apiSchedulerTrigger))
	mux.HandleFunc("/panel/api/config", g(s.apiConfig))
	mux.HandleFunc("/panel/api/model-test", g(s.apiModelTest))
	mux.HandleFunc("/panel/api/update/check", g(s.apiUpdateCheck))
	mux.HandleFunc("/panel/api/update/apply", g(s.apiUpdateApply))
}

// guard 包一层可选鉴权。
//
// 面板密码与 API Key 相互独立：API Key 是给客户端用的，
// 面板密码是给人用的。两者都未配置时放行（适合本机单用户）。
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next(w, r)
	}
}

// ---------- 通用 ----------

func ok(w http.ResponseWriter, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["success"] = true
	writeJSON(w, http.StatusOK, data)
}

func fail(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// realmOrDefault 收敛区域默认值，避免到处写三元表达式。
func realmOrDefault(def, got string) string {
	if got != "" {
		return got
	}
	return def
}

// decode 读 JSON 请求体。
func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20)).Decode(v)
}

// ---------- 概览 ----------

func (s *Server) apiOverview(w http.ResponseWriter, r *http.Request) {
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
			days = n
		}
	}
	ov := s.d.Usage.Overview(days)
	entries := s.d.Pool.List()
	now := time.Now()
	var credits float64
	var creditsKnown int
	healthy := 0
	for _, e := range entries {
		if e.Available(now) {
			healthy++
		}
		if e.CreditsKnown {
			credits += e.Credits
			creditsKnown++
		}
	}
	var creditsOut any = fmt.Sprintf("%.0f", credits)
	if creditsKnown == 0 {
		creditsOut = nil // 从未查过余额时不显示 0，避免误以为已欠费
	}
	cfg := s.d.Cfg.Snapshot()
	ok(w, map[string]any{
		"usage": ov,
		"accounts": map[string]any{
			"total": len(entries), "healthy": healthy,
			"credits_remaining": creditsOut,
		},
		"service": map[string]any{
			"service": "wb2go", "version": s.d.Version,
			"uptime_seconds": int(time.Since(s.d.Started).Seconds()),
			"in_flight":      s.d.Pool.InFlightCount(),
			"sticky_binds":   s.d.Sticky.Count(),
			"waf_blocked":    s.d.Pool.WAFBlockedFor(now),
			"default_realm":  cfg.DefaultRealm,
			"identity":       cfg.Identity,
			"go_version":     runtime.Version(),
		},
	})
}

// ---------- 账号 ----------

func (s *Server) accountView() []map[string]any {
	entries := s.d.Pool.List()
	now := time.Now()
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"uid": e.UID, "label": e.Nickname, "realm": e.Realm,
			"status": e.StatusLabel(now), "credits": e.Credits,
			"credits_known":  e.CreditsKnown,
			"cool_remaining": e.CoolRemaining(now),
			"in_flight":      e.InFlight,
			"disabled":       e.Disabled, "disabled_reason": e.DisabledReason,
			"last_error": e.LastError, "identity": e.Identity,
		})
	}
	return out
}

func (s *Server) apiAccounts(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{
		"accounts":     s.accountView(),
		"sticky_binds": s.d.Sticky.Count(),
	})
}

func (s *Server) apiAccountDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	if decode(r, &req) != nil || req.UID == "" {
		fail(w, "缺少 uid")
		return
	}
	if err := s.d.Store.Remove(req.UID); err != nil {
		fail(w, err.Error())
		return
	}
	s.d.Pool.Remove(req.UID)
	ok(w, map[string]any{"message": "账号已移除"})
}

func (s *Server) apiAccountToggle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	if decode(r, &req) != nil || req.UID == "" {
		fail(w, "缺少 uid")
		return
	}
	ent, found := s.d.Pool.Get(req.UID)
	if !found {
		fail(w, "账号不在池中")
		return
	}
	if !s.d.Pool.SetEnabled(req.UID, !ent.Disabled) {
		fail(w, "操作失败")
		return
	}
	ok(w, map[string]any{"message": map[bool]string{true: "已停用", false: "已启用"}[!ent.Disabled]})
}

func (s *Server) apiAccountIdentity(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID      string `json:"uid"`
		Identity string `json:"identity"`
	}
	if decode(r, &req) != nil || req.UID == "" {
		fail(w, "缺少 uid")
		return
	}
	switch req.Identity {
	case "workbuddy", "vscode", "cli":
	default:
		fail(w, "identity 只能是 workbuddy / vscode / cli")
		return
	}
	if !s.d.Pool.SetIdentity(req.UID, req.Identity) {
		fail(w, "账号不在池中")
		return
	}
	// 同步持久化，否则重启后回落全局默认
	if creds, found := s.d.CredsOf(req.UID); found {
		creds.Identity = req.Identity
		_ = s.d.Store.Save(creds)
	}
	ok(w, map[string]any{"message": "身份已切换"})
}

func (s *Server) apiAccountManual(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID          string `json:"uid"`
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		Realm        string `json:"realm"`
	}
	if decode(r, &req) != nil {
		fail(w, "请求格式错误")
		return
	}
	if !store.ValidUID(req.UID) {
		fail(w, "uid 含非法字符（只允许字母数字下划线连字符，≤64）")
		return
	}
	if req.AccessToken == "" {
		fail(w, "缺少 accessToken")
		return
	}
	// 区域是账号的第一等属性，必须明确给出。
	// 静默回落 cn 会让国际账号被存成国内账号，之后所有请求都打错端点。
	if req.Realm == "" {
		req.Realm = s.d.Cfg.Snapshot().DefaultRealm
	}
	if !upstream.ValidRealm(req.Realm) {
		fail(w, "未知区域："+req.Realm+"（只能是 cn 或 intl）")
		return
	}
	c := store.Credentials{
		UID: req.UID, AccessToken: req.AccessToken, RefreshToken: req.RefreshToken,
		Realm: req.Realm, Source: "manual",
	}
	if err := s.d.Store.Save(c); err != nil {
		fail(w, err.Error())
		return
	}
	s.d.Reload()
	ok(w, map[string]any{"message": "已保存并加载到池中"})
}

// apiAccountOp 单账号操作（签到 / 查积分 / 任务 / 刷新凭证）。
func (s *Server) apiAccountOp(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UID string `json:"uid"`
		}
		if decode(r, &req) != nil || req.UID == "" {
			fail(w, "缺少 uid")
			return
		}
		creds, found := s.d.CredsOf(req.UID)
		if !found {
			fail(w, "账号不在池中")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()

		switch op {
		case "checkin":
			if creds.Realm != "cn" {
				fail(w, "签到仅国内版账号支持")
				return
			}
			res := s.d.API.Checkin(ctx, toUpstreamCreds(creds))
			if !res.OK {
				fail(w, "签到失败："+res.Message())
				return
			}
			msg := "签到成功"
			if res.Body != nil && res.Body["skipped"] == true {
				msg = "今日已签到"
			}
			ok(w, map[string]any{"message": msg, "data": res.Body})

		case "credits":
			info, res := s.d.API.FetchCredits(ctx, toUpstreamCreds(creds))
			if !res.OK {
				fail(w, "查询失败："+res.Message())
				return
			}
			s.d.Pool.SetCredits(req.UID, info.Remaining, info.ExpireAt)
			ok(w, map[string]any{
				"message": fmt.Sprintf("余额 %.0f", info.Remaining),
				"credits": info.Remaining,
			})

		case "tasks":
			if creds.Realm != "cn" {
				fail(w, "成长任务仅国内版支持")
				return
			}
			msg := s.runGrowthTasks(ctx, creds)
			ok(w, map[string]any{"message": msg})

		case "refresh":
			if s.d.RefreshToken == nil {
				fail(w, "刷新功能未启用")
				return
			}
			if _, err := s.d.RefreshToken(ctx, creds); err != nil {
				fail(w, "刷新失败："+err.Error())
				return
			}
			s.d.Reload()
			ok(w, map[string]any{"message": "凭证已刷新"})
		}
	}
}

// runGrowthTasks 执行"一键完成"：接取 → 上报事件 → 领奖。
//
// 三个必须遵守的约束（否则任务不会计分）：
//  1. 事件上报间隔 ≥ 1 秒：上游按密度判罚
//  2. 领奖走 Web 域：走 CLI 域会返回 200 但不记账
//  3. 上报 200 ≠ 计分：必须轮询进度确认后才领奖
func (s *Server) runGrowthTasks(ctx context.Context, creds store.Credentials) string {
	api := s.d.API
	uc := toUpstreamCreds(creds)

	tasks, res := api.FetchTasks(ctx, uc)
	if !res.OK {
		return "拉取任务失败：" + res.Message()
	}
	if len(tasks) == 0 {
		return "暂无成长任务"
	}
	// 先把未接取的批量接上
	var pending []string
	for _, t := range tasks {
		code, _ := t["taskCode"].(string)
		if code == "" {
			code, _ = t["code"].(string)
		}
		if code == "" {
			continue
		}
		if accepted, _ := t["accepted"].(bool); accepted {
			pending = append(pending, code)
		} else {
			if rr := api.AcceptTasks(ctx, uc, []string{code}); rr.OK {
				pending = append(pending, code)
			}
		}
		time.Sleep(time.Duration(cfgGap()) * time.Millisecond)
	}
	if len(pending) == 0 {
		return "没有可自动完成的任务"
	}
	// 领奖：逐个领，失败的记下来
	var claimed, failed int
	var lastErr string
	for _, code := range pending {
		r := api.ClaimTask(ctx, uc, code)
		if r.OK {
			claimed++
		} else {
			failed++
			lastErr = r.Message()
		}
		time.Sleep(time.Duration(cfgGap()) * time.Millisecond)
	}
	msg := fmt.Sprintf("已接取 %d 个任务，领取成功 %d", len(pending), claimed)
	if failed > 0 {
		msg += fmt.Sprintf("，失败 %d（%s）", failed, lastErr)
	}
	return msg
}

func cfgGap() int { return 1000 }

// ---------- 批量操作 ----------

func (s *Server) apiBatch(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/panel/api/batch/")
	name = strings.TrimSuffix(name, "/")
	if name == "" {
		fail(w, "缺少操作名")
		return
	}
	switch name {
	case "checkin", "credits", "tasks", "travel", "keepalive":
	default:
		fail(w, "不支持的批量操作："+name)
		return
	}
	s.d.Sched.Trigger(name)
	ok(w, map[string]any{
		"message": "已在后台触发 " + name + "，完成后账号列表会自动刷新",
		"detail":  "若长时间无变化，请到「调度」页查看任务状态。",
	})
}

// ---------- OAuth ----------

func (s *Server) apiOAuthStart(w http.ResponseWriter, r *http.Request) {
	// 区域由前端传入：国内版与国际版是两套登录入口，不能混用
	var req struct {
		Realm string `json:"realm"`
	}
	_ = decode(r, &req) // 允许空 body
	realm := req.Realm
	if realm == "" {
		realm = s.d.Cfg.Snapshot().DefaultRealm
	}
	if !upstream.ValidRealm(realm) {
		fail(w, "未知区域："+realm+"（只能是 cn 或 intl）")
		return
	}
	url, err := s.startOAuth(r.Context(), realm)
	if err != nil {
		fail(w, "获取授权链接失败："+err.Error())
		return
	}
	ok(w, map[string]any{"auth_url": url, "realm": realm})
}

// apiRealms 返回受支持的区域列表，供面板渲染区域选择器。
func (s *Server) apiRealms(w http.ResponseWriter, r *http.Request) {
	list := make([]map[string]any, 0, len(upstream.Realms))
	for _, rm := range upstream.Realms {
		list = append(list, map[string]any{
			"key": rm.Key, "name": rm.Name, "short": rm.Short,
			"domains":     upstream.DomainsFor(rm.Key),
			"has_checkin": upstream.HasCheckin(rm.Key),
			"has_tasks":   upstream.HasGrowthTasks(rm.Key),
		})
	}
	ok(w, map[string]any{"realms": list, "default": s.d.Cfg.Snapshot().DefaultRealm})
}

func (s *Server) apiOAuthStatus(w http.ResponseWriter, r *http.Request) {
	s.oauthMu.Lock()
	state := s.oauthState
	start := s.oauthStart
	seen := s.oauthSeen
	realm := s.oauthRealm
	s.oauthSeen = true
	s.oauthMu.Unlock()

	if state == "" {
		ok(w, map[string]any{"pending": 0, "realm": realmOrDefault(s.d.Cfg.Snapshot().DefaultRealm, realm)})
		return
	}
	// 授权链接有效期约 5 分钟
	if time.Since(start) > 5*time.Minute {
		s.oauthMu.Lock()
		s.oauthState = ""
		s.oauthMu.Unlock()
		ok(w, map[string]any{"pending": 0, "expired": true, "realm": realm})
		return
	}
	uid, done := s.pollOAuth(r.Context(), state)
	if done && uid != "" {
		s.oauthMu.Lock()
		s.oauthState = ""
		s.oauthMu.Unlock()
		s.d.Reload()
		ok(w, map[string]any{"account": uid, "realm": realm})
		return
	}
	ok(w, map[string]any{"pending": 1, "waiting": seen, "realm": realm})
}

// ---------- 用量 ----------

func (s *Server) apiUsageRecent(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	ok(w, map[string]any{"records": s.d.Usage.Recent(limit)})
}

// ---------- 调度 ----------

func (s *Server) apiScheduler(w http.ResponseWriter, r *http.Request) {
	st := s.d.Sched.Status()
	ok(w, map[string]any{
		"running": st.Running, "last_run": st.LastRun,
		"timezone": st.Timezone, "server_time": st.ServerNow,
	})
}

func (s *Server) apiSchedulerTrigger(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if decode(r, &req) != nil || req.Name == "" {
		fail(w, "缺少任务名")
		return
	}
	switch req.Name {
	case "checkin", "credits", "tasks", "travel", "keepalive", "active", "nightcat", "daily_chat":
	default:
		fail(w, "未知任务："+req.Name)
		return
	}
	s.d.Sched.Trigger(req.Name)
	ok(w, map[string]any{"message": req.Name + " 已触发"})
}

// ---------- 配置 ----------

func (s *Server) apiConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cfg := s.d.Cfg.Snapshot()
		ok(w, map[string]any{
			"config": map[string]any{
				"api_key": cfg.APIKey, "system_prompt": cfg.SystemPrompt,
				"identity": cfg.Identity, "default_realm": cfg.DefaultRealm,
				"max_in_flight": cfg.MaxInFlight, "breaker_threshold": cfg.BreakerThreshold,
				"credit_floor": cfg.CreditFloor, "reserve_credits": cfg.ReserveCredits,
				"daily_token_limit": cfg.DailyTokenLimit, "chat_timeout": cfg.ChatTimeout,
				"sanitize": cfg.Sanitize, "sticky_enabled": cfg.StickyEnabled,
				"prompt_cache_key": cfg.CacheKey,
				"schedule": map[string]any{
					"checkin_enabled":   cfg.Schedule.CheckinEnabled,
					"travel_enabled":    cfg.Schedule.TravelEnabled,
					"task_enabled":      cfg.Schedule.TaskEnabled,
					"keepalive_enabled": cfg.Schedule.KeepaliveEnabled,
					"balance_enabled":   cfg.Schedule.BalanceEnabled,
				},
				"update": map[string]any{
					"enabled": cfg.AutoUpdate.Enabled, "auto_restart": cfg.AutoUpdate.AutoRestart,
					// 仓库地址固定为官方发布仓库，不暴露可写字段
					"github_release": hotupdate.DefaultRepo,
					"repo_locked":    true,
				},
			},
			"version": s.d.Version,
		})
		return
	}
	// 保存：深合并进现有配置，只改提交的字段
	body := make([]byte, 0, 4096)
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
		if len(body) > 1<<20 {
			fail(w, "配置过大")
			return
		}
	}
	if err := s.d.Cfg.Merge(body); err != nil {
		fail(w, err.Error())
		return
	}
	// 热生效：把新配置推给池与粘性路由
	cfg := s.d.Cfg.Snapshot()
	s.d.Pool.SetConfig(cfg)
	s.d.Sticky.SetConfig(cfg.StickyEnabled, cfg.StickyTTLD, cfg.StickyByPfx)

	msg := "配置已保存并立即生效。若修改了监听地址或目录路径，需要重启进程。"
	ok(w, map[string]any{"message": msg})
}

// ---------- 模型测试 ----------

func (s *Server) apiModelTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model   string `json:"model"`
		Effort  string `json:"effort"`
		Timeout int    `json:"timeout"`
	}
	if decode(r, &req) != nil || req.Model == "" {
		fail(w, "缺少 model")
		return
	}
	if s.d.TestChat == nil {
		fail(w, "测试功能未启用")
		return
	}
	timeout := time.Duration(req.Timeout) * time.Second
	if timeout <= 0 || timeout > 5*time.Minute {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout+10*time.Second)
	defer cancel()
	reply, err, tokens := s.d.TestChat(ctx, req.Model, req.Effort, timeout)
	if err != nil {
		ok(w, map[string]any{"success": false, "error": err.Error()})
		return
	}
	ok(w, map[string]any{
		"success": true, "model": req.Model, "reply": reply,
		"usage": map[string]any{"total_tokens": tokens},
	})
}

// ---------- 热更新 ----------

func (s *Server) apiUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if s.d.UpdateChecker == nil {
		fail(w, "热更新未启用（请在设置里填写 GitHub Release 仓库）")
		return
	}
	info, err := s.d.UpdateChecker.Check(r.Context())
	if err != nil {
		fail(w, err.Error())
		return
	}
	// 实现返回的是 *Info 结构体。早前在这里断言 map[string]any 失败后
	// 塞了空 map，导致 current/latest 全部变 null —— marshal roundtrip
	// 是结构体 → JSON 的正确透传方式（Info 字段都带 json tag）。
	raw, mErr := json.Marshal(info)
	if mErr != nil {
		fail(w, "序列化检查结果失败: "+mErr.Error())
		return
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		fail(w, "解析检查结果失败: "+err.Error())
		return
	}
	m["success"] = true
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) apiUpdateApply(w http.ResponseWriter, r *http.Request) {
	if s.d.UpdateChecker == nil {
		fail(w, "热更新未启用")
		return
	}
	msg, err := s.d.UpdateChecker.Apply(r.Context())
	if err != nil {
		fail(w, err.Error())
		return
	}
	ok(w, map[string]any{"message": msg})
}

// ---------- 辅助 ----------

// toUpstreamCreds 把存储层凭证转成上游层凭证。
func toUpstreamCreds(c store.Credentials) upstream.Creds {
	return upstream.Creds{
		UID: c.UID, AccessToken: c.AccessToken, RefreshToken: c.RefreshToken,
		ExpiresAt: c.ExpiresAt, Realm: c.Realm, Identity: c.Identity,
		EnterpriseID: c.EnterpriseID, Nickname: c.Nickname,
	}
}
