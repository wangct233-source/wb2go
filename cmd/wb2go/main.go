// wb2go —— 把 WorkBuddy / CodeBuddy 订阅包装成 OpenAI 兼容 API 的多账号网关。
//
// 设计目标（按优先级）：
//  1. 简洁可读 —— 代码是给人读的，注释解释"为什么"而不是"是什么"
//  2. 零第三方依赖 —— 只用标准库，部署无供应链风险
//  3. 单文件可执行 —— 前端 go:embed 进二进制，拷贝到任何机器都能跑
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/wb2go/wb2go/internal/account"
	"github.com/wb2go/wb2go/internal/config"
	"github.com/wb2go/wb2go/internal/hotupdate"
	"github.com/wb2go/wb2go/internal/panel"
	"github.com/wb2go/wb2go/internal/proxy"
	"github.com/wb2go/wb2go/internal/store"
	"github.com/wb2go/wb2go/internal/tasks"
	"github.com/wb2go/wb2go/internal/upstream"
	"github.com/wb2go/wb2go/internal/usage"
	"github.com/wb2go/wb2go/internal/web"
)

// Version 是构建版本号。由 CI 在打 tag 时注入。
var Version = "1.0.0"

func main() {
	var (
		configPath = flag.String("config", "config.json", "配置文件路径")
		showVer    = flag.Bool("version", false, "打印版本号后退出")
		genKey     = flag.Bool("gen-key", false, "生成一个随机 API Key 并退出")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("wb2go", Version)
		return
	}
	if *genKey {
		fmt.Println("wb-" + randomKey(32))
		return
	}

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("")

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	rt := config.NewRuntime(cfg)
	started := time.Now()

	// 首次启动且未配置密钥时，生成一个并落盘：
	// "首次就能用"比"默认开放代理"更合理。
	if cfg.APIKey == "" && *configPath != "" {
		if key := randomKey(24); key != "" {
			cfg.APIKey = key
			_ = cfg.SaveAs(*configPath)
			log.Printf("已生成随机 API Key: %s （已写入 %s，请妥善保存）", key, *configPath)
		}
	}

	// ---- 组件装配 ----
	credStore, err := store.New(cfg.AuthDir)
	if err != nil {
		log.Fatalf("初始化账号存储失败: %v", err)
	}
	pool := account.NewPool(cfg)
	sticky := account.NewStickyRouter(cfg.StickyEnabled, cfg.StickyTTLD, cfg.StickyByPfx)

	usageStore, err := usage.NewStore(cfg.UsageFile)
	if err != nil {
		log.Printf("初始化用量日志失败（统计将从头开始）: %v", err)
		usageStore = nil
	}

	client := upstream.NewClient(
		time.Duration(cfg.HeaderTimeout)*time.Second,
		time.Duration(cfg.ChatTimeout)*time.Second,
		time.Duration(cfg.RPCTimeout)*time.Second,
	)
	api := &upstream.API{
		Client: client,
		Identity: func(realm, identity string) upstream.Identity {
			key := identity
			if key == "" {
				key = rt.Snapshot().Identity
			}
			c := rt.Snapshot()
			id, err := upstream.BuildIdentity(key, c.DesktopVer, c.CLIVersion, c.ProductLabel)
			if err != nil {
				id, _ = upstream.BuildIdentity("workbuddy", c.DesktopVer, c.CLIVersion, c.ProductLabel)
			}
			return id.Resolve(realm)
		},
		FP: upstream.NewFingerprint,
	}

	// 账号池从凭证目录加载
	loadAccounts := func() {
		list := credStore.List()
		for _, c := range list {
			if !c.Valid() {
				continue
			}
			pool.Upsert(c.UID, c.Nickname, c.Realm, c.Identity)
		}
		log.Printf("已加载 %d 个账号到池中", len(list))
	}
	loadAccounts()

	// 恢复池状态与粘性绑定
	if st, err := account.LoadState(cfg.StateFile); err == nil {
		pool.Restore(st)
		sticky.RestoreBinds(st.Binds, cfg.StickyTTLD)
		log.Printf("已恢复池状态（%d 个账号有冷却记录，%d 条粘性绑定）", len(st.Entries), len(st.Binds))
	}

	// ---- 依赖注入 ----
	var (
		savePoolState = func() {
			_ = pool.Save(cfg.StateFile, sticky, cfg.StickyTTLD)
		}
		refreshToken = func(ctx context.Context, c store.Credentials) (store.Credentials, error) {
			uc := toUC(c)
			access, refresh, expires, res := api.RefreshToken(ctx, uc)
			if !res.OK {
				return c, fmt.Errorf("%s", res.APIError.Error())
			}
			c.AccessToken, c.RefreshToken, c.ExpiresAt = access, refresh, expires
			if err := credStore.Save(c); err != nil {
				return c, err
			}
			return c, nil
		}
		credOf = func(uid string) (store.Credentials, bool) { return credStore.Get(uid) }
	)

	// ---- 代理层 ----
	ph := proxy.New(proxy.Deps{
		Cfg: rt, Store: credStore, Pool: pool, Sticky: sticky,
		API: api, Usage: usageStore, Version: Version, StartedAt: started,
		OnCreditsChange: pool.SetCredits,
		RefreshCreds:    refreshToken,
	})

	// ---- 调度层 ----
	sched := tasks.NewScheduler(stdLogger{}, &taskHandler{
		api: api, pool: pool, store: credStore, cfg: rt, refresh: refreshToken, usage: usageStore,
	})

	// ---- 热更新 ----
	var updater *hotupdate.Updater
	// 仓库恒为官方地址（hotupdate.DefaultRepo）；启用热更新即启用对官方仓库的检查。
	// 若历史 config.json 里存了别的仓库值，这里也不读它 —— 防止从旧版本升级时带入第三方源。
	if cfg.AutoUpdate.Enabled {
		updater = hotupdate.New(hotupdate.DefaultRepo, Version, cfg.AutoUpdate.Mirror)
	}

	// ---- 面板层 ----
	ps := panel.New(panel.Deps{
		Cfg: rt, Store: credStore, Pool: pool, Sticky: sticky,
		Sched: sched, Usage: usageStore, Version: Version, Started: started,
		Reload: loadAccounts, RefreshToken: refreshToken, CredsOf: credOf,
		API: api,
		TestChat: func(ctx context.Context, model, effort string, timeout time.Duration) (string, error, int) {
			return "", fmt.Errorf("模型测试需要在发行版中启用"), 0
		},
	})
	if updater != nil {
		ps.SetUpdateChecker(updater)
	}

	// ---- HTTP ----
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		healthy, total := pool.Healthy()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Service", "wb2go")
		if total == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `{"healthy":%d,"total":%d,"service":"wb2go"}`, healthy, total)
			return
		}
		fmt.Fprintf(w, `{"healthy":%d,"total":%d,"service":"wb2go"}`, healthy, total)
	})
	ph.Register(mux)
	ps.Register(mux)
	if cfg.PanelEnabled {
		web.Register(mux, func(next http.Handler) http.Handler { return next })
	}

	srv := &http.Server{
		Addr:    cfg.ListenResolved,
		Handler: recovery(mux),
		// 不设 WriteTimeout：聊天流可能被长时间占用，
		// 硬性写超时会把正在进行的流式输出掐断。上游侧已有 idle 超时兜底。
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// ---- 后台协程 ----
	sched.Start()
	go persistLoop(rt, cfg.StateFile, pool, sticky, cfg.StickyTTLD)
	go gcLoop(rt, sticky, pool, usageStore)
	go watchConfig(rt, pool, sticky, cfg)
	if updater != nil {
		go updateLoop(rt, updater)
	}

	// ---- 启动日志 ----
	printBanner(cfg, Version, credStore)

	// ---- 优雅退出 ----
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		log.Println("收到退出信号，正在保存状态并退出…")
		sched.Stop()
		savePoolState()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		client.Close()
		os.Exit(0)
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("监听 %s 失败: %v", cfg.ListenResolved, err)
	}
}

// recovery 兜住 panic，避免单个请求崩掉整个进程。
func recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if e := recover(); e != nil {
				log.Printf("处理 %s %s 时 panic: %v", r.Method, r.URL.Path, e)
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{
						"message": "网关内部错误，请查看服务日志",
						"type":    "internal_error",
					},
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// persistLoop 周期落盘池状态。
func persistLoop(rt *config.Runtime, stateFile string, pool *account.Pool, sticky *account.StickyRouter, ttl time.Duration) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		cfg := rt.Snapshot()
		if err := pool.Save(stateFile, sticky, ttl); err != nil {
			log.Printf("保存池状态失败: %v", err)
		}
		_ = cfg
	}
}

// gcLoop 周期清理过期粘性绑定并跨日重置 token 日额度。
func gcLoop(rt *config.Runtime, sticky *account.StickyRouter, pool *account.Pool, us *usage.Store) {
	gc := time.NewTicker(time.Minute)
	defer gc.Stop()
	day := time.Now().Format("2006-01-02")
	for {
		select {
		case <-gc.C:
			now := time.Now()
			sticky.GC(now)
			cfg := rt.Snapshot()
			// 冷却到期后顺手清掉过期的时间戳，避免 state.json 无限膨胀
			if today := now.Format("2006-01-02"); today != day {
				day = today
				pool.ResetDailyTokens()
				if us != nil {
					us.RollDay()
				}
				log.Println("已跨日：重置账号日 Token 额度")
			}
			_ = cfg
		}
	}
}

// watchConfig 监听配置变化，把新值推给各组件。
func watchConfig(rt *config.Runtime, pool *account.Pool, sticky *account.StickyRouter, old *config.Config) {
	ch := rt.Refreshed()
	for range ch {
		cfg := rt.Snapshot()
		pool.SetConfig(cfg)
		sticky.SetConfig(cfg.StickyEnabled, cfg.StickyTTLD, cfg.StickyByPfx)
		log.Printf("配置已热更新（检查间隔 %ds）", cfg.Schedule.BalanceMinutes)
		_ = old
	}
}

// updateLoop 周期检查新版本。
func updateLoop(rt *config.Runtime, u *hotupdate.Updater) {
	interval := func() time.Duration {
		h := rt.Snapshot().AutoUpdate.CheckInterval
		if h <= 0 {
			h = 6
		}
		return time.Duration(h) * time.Hour
	}
	// 启动 1 分钟后查第一次：避开刚启动时的网络高峰
	time.Sleep(time.Minute)
	for {
		info, err := u.Check(context.Background())
		if err != nil {
			log.Printf("检查更新失败: %v", err)
		} else if m, isMap := info.(map[string]any); isMap && m["up_to_date"] == false {
			log.Printf("发现新版本 v%v（当前 v%v），可在面板「设置 → 热更新」一键升级",
				m["latest"], m["current"])
		}
		time.Sleep(interval())
	}
}

// taskHandler 把上游 API 与账号池接到调度器上。
type taskHandler struct {
	api     *upstream.API
	pool    *account.Pool
	store   *store.Store
	cfg     *config.Runtime
	refresh func(context.Context, store.Credentials) (store.Credentials, error)
	usage   *usage.Store
}

func (h *taskHandler) Accounts() []tasks.Account {
	entries := h.pool.List()
	out := make([]tasks.Account, 0, len(entries))
	for _, e := range entries {
		out = append(out, tasks.Account{
			UID: e.UID, Label: e.Nickname, Realm: e.Realm,
			Credits: e.Credits, Disabled: e.Disabled,
		})
	}
	return out
}

func (h *taskHandler) creds(uid string) (upstream.Creds, bool) {
	c, ok := h.store.Get(uid)
	if !ok {
		return upstream.Creds{}, false
	}
	cfg := h.cfg.Snapshot()
	identity := c.Identity
	if identity == "" {
		identity = cfg.Identity
	}
	return upstream.Creds{
		UID: c.UID, AccessToken: c.AccessToken, RefreshToken: c.RefreshToken,
		ExpiresAt: c.ExpiresAt, Realm: c.Realm, Identity: identity,
		EnterpriseID: c.EnterpriseID, Nickname: c.Nickname,
	}, true
}

func (h *taskHandler) Checkin(ctx context.Context, a tasks.Account) (bool, string) {
	uc, ok := h.creds(a.UID)
	if !ok {
		return false, "凭证不存在"
	}
	res := h.api.Checkin(ctx, uc)
	if !res.OK {
		return false, res.Message()
	}
	if res.Body != nil && res.Body["skipped"] == true {
		return true, "今日已签到"
	}
	return true, "签到成功"
}

func (h *taskHandler) Credits(ctx context.Context, a tasks.Account) (float64, bool) {
	uc, ok := h.creds(a.UID)
	if !ok {
		return 0, false
	}
	info, res := h.api.FetchCredits(ctx, uc)
	if !res.OK {
		return 0, false
	}
	h.pool.SetCredits(a.UID, info.Remaining, info.ExpireAt)
	return info.Remaining, true
}

func (h *taskHandler) Keepalive(ctx context.Context, a tasks.Account) (string, error) {
	c, ok := h.store.Get(a.UID)
	if !ok {
		return "", fmt.Errorf("凭证不存在")
	}
	fresh, err := h.refresh(ctx, c)
	if err != nil {
		return "", err
	}
	// 刷新成功说明会话有效，清掉会话失效的禁用标记
	h.pool.ReviveIfSessionOnly(a.UID)
	_ = fresh
	return "token 已刷新", nil
}

func (h *taskHandler) Active(ctx context.Context, a tasks.Account) (string, error) {
	uc, ok := h.creds(a.UID)
	if !ok {
		return "", fmt.Errorf("凭证不存在")
	}
	// 上报必须含 userId 与 conversationId，缺一个都不计分
	events := []any{map[string]any{
		"event":          "chat_request_send",
		"userId":         uc.UID,
		"conversationId": fmt.Sprintf("wb2go-%d", time.Now().UnixMilli()),
		"timestamp":      time.Now().UnixMilli(),
	}}
	res := h.api.ReportEvents(ctx, uc, events)
	if !res.OK {
		return "", fmt.Errorf("%s", res.Message())
	}
	return "活跃已上报", nil
}

func (h *taskHandler) Travel(ctx context.Context, a tasks.Account) string {
	uc, ok := h.creds(a.UID)
	if !ok {
		return "凭证不存在"
	}
	msg, _ := h.api.BuddyTravel(ctx, uc)
	return msg
}

func (h *taskHandler) Tasks(ctx context.Context, a tasks.Account) string {
	uc, ok := h.creds(a.UID)
	if !ok {
		return "凭证不存在"
	}
	list, res := h.api.FetchTasks(ctx, uc)
	if !res.OK {
		return "拉取任务失败：" + res.Message()
	}
	if len(list) == 0 {
		return "暂无成长任务"
	}
	var codes []string
	for _, t := range list {
		if code, _ := t["taskCode"].(string); code != "" {
			codes = append(codes, code)
		} else if code, _ := t["code"].(string); code != "" {
			codes = append(codes, code)
		}
	}
	if len(codes) > 0 {
		h.api.AcceptTasks(ctx, uc, codes)
		time.Sleep(time.Second)
	}
	var claimed int
	for _, code := range codes {
		if r := h.api.ClaimTask(ctx, uc, code); r.OK {
			claimed++
		}
		time.Sleep(time.Second)
	}
	return fmt.Sprintf("已接取 %d 个任务，领取成功 %d", len(codes), claimed)
}

func (h *taskHandler) NightCat(ctx context.Context, a tasks.Account) string {
	// 夜猫子需要真实对话消耗，属可选功能，此处仅回报未实现
	return "夜猫子需手动触发（当前版本未启用自动对话）"
}

func (h *taskHandler) DailyChat(ctx context.Context, a tasks.Account) (string, error) {
	return "", fmt.Errorf("国际版每日打卡需在面板中手动触发")
}

// toUC 把凭证转成上游层结构。
func toUC(c store.Credentials) upstream.Creds {
	return upstream.Creds{
		UID: c.UID, AccessToken: c.AccessToken, RefreshToken: c.RefreshToken,
		ExpiresAt: c.ExpiresAt, Realm: c.Realm, Identity: c.Identity,
		EnterpriseID: c.EnterpriseID, Nickname: c.Nickname,
	}
}

// stdLogger 把标准 log 适配成调度器与上游层的日志接口。
type stdLogger struct{}

func (stdLogger) Infof(format string, args ...any) { log.Printf(format, args...) }
func (stdLogger) Warnf(format string, args ...any) { log.Printf("WARN "+format, args...) }

// randomKey 生成随机密钥。
func randomKey(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

func printBanner(cfg *config.Config, version string, cs *store.Store) {
	total := len(cs.List())
	fmt.Println()
	fmt.Println("  wb2go " + version + " —— WorkBuddy 兼容网关")
	fmt.Println("  ─────────────────────────────────────────")
	fmt.Printf("  监听地址    %s\n", cfg.ListenResolved)
	fmt.Printf("  API 入口    http://127.0.0.1%s/v1\n", portOf(cfg.ListenResolved))
	if cfg.PanelEnabled {
		fmt.Printf("  控制台      http://127.0.0.1%s/panel/\n", portOf(cfg.ListenResolved))
	}
	fmt.Printf("  出站身份    %s\n", cfg.Identity)
	fmt.Printf("  默认出口    %s\n", cfg.DefaultRealm)
	fmt.Printf("  账号数量    %d（目录 %s）\n", total, cfg.AuthDir)
	if cfg.APIKey == "" {
		fmt.Println("  ⚠ 鉴权      未设置 api_key，任何人都能调用（仅建议本机使用）")
	} else {
		fmt.Println("  鉴权        已启用")
	}
	fmt.Println("  ─────────────────────────────────────────")
	fmt.Println()
}

func portOf(listen string) string {
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		return listen[i:]
	}
	return ":" + listen
}
