package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Config 是网关的全部配置。字段与 config.example.json 一一对应。
//
// 设计取舍：配置项按功能分组，组内平铺，避免深层嵌套导致的 JSON 冗长。
// 所有时间字段用字符串（"600s" / "9"），在 Load 时解析成 Duration 或 []int，
// 解析失败直接启动报错（fail fast），不静默回落默认值。
type Config struct {
	// ---- 基础 ----
	Listen        string `json:"listen"`
	APIKey        string `json:"api_key"`
	AuthDir       string `json:"auth_dir"`
	StateFile     string `json:"state_file"`
	UsageFile     string `json:"usage_file"`
	AccountsFile  string `json:"accounts_file"`
	PanelEnabled  bool   `json:"panel_enabled"`
	DefaultRealm  string `json:"default_realm"`
	SystemPrompt  string `json:"system_prompt"`
	MaxInFlight   int    `json:"max_in_flight"`  // 0 = 不限
	ChatTimeout   int    `json:"chat_timeout"`   // 聊天流空闲上限（秒）
	HeaderTimeout int    `json:"header_timeout"` // 首字节上限（秒）
	RPCTimeout    int    `json:"rpc_timeout"`    // 短 RPC 总时长（秒）
	UserAgent     string `json:"user_agent"`     // 空 = 用内置默认
	AllowOrigins  string `json:"allow_origins"`  // 逗号分隔；"*" = 不限

	// ---- 出站身份与脱敏 ----
	// 身份三选一：workbuddy（桌面端）/ vscode（官方插件）/ cli（官方 CLI）。
	// 不同身份对应不同的端点、UA 与 X-IDE-* 头族，等价于"官方客户端在说话"。
	Identity      string `json:"identity"`
	Sanitize      bool   `json:"sanitize"`
	CacheKey      bool   `json:"prompt_cache_key"`
	AutoSwitch    bool   `json:"auto_switch_identity"`
	DesktopVer    string `json:"desktop_version"`
	CLIVersion    string `json:"cli_version"`
	ProductLabel  string `json:"client_name"`
	HideOwnPrompt bool   `json:"hide_client_identity"`

	// ---- 账号池 ----
	SoftRate         string  `json:"soft_rate"`
	SoftRateMax      string  `json:"soft_rate_max"`
	HardRate         string  `json:"hard_rate"`
	BreakerThreshold int     `json:"breaker_threshold"`
	BreakerCooldown  string  `json:"breaker_cooldown"`
	BreakerMax       string  `json:"breaker_cooldown_max"`
	DegradeThreshold int     `json:"degrade_threshold"`
	DegradeCooldown  string  `json:"degrade_cooldown"`
	DegradeMax       string  `json:"degrade_cooldown_max"`
	IdleWeightPerH   float64 `json:"idle_weight_per_hour"`
	IdleWeightMax    float64 `json:"idle_weight_max"`
	PreferExpiring   bool    `json:"prefer_expiring"`
	ExpiringSoon     int     `json:"expiring_soon"`
	CreditFloor      int     `json:"credit_floor"`
	ReserveCredits   int     `json:"reserve_credits"`
	DailyTokenLimit  int     `json:"daily_token_limit"`
	GlobalInFlight   int     `json:"global_max_in_flight"`
	BlockBackground  bool    `json:"block_background_requests"`
	MaxProxySwitch   int     `json:"max_identity_switch"`

	// ---- 会话粘性 ----
	StickyEnabled bool   `json:"sticky_enabled"`
	StickyTTL     string `json:"sticky_ttl"`
	StickyGC      string `json:"sticky_gc_interval"`
	StickyByPfx   bool   `json:"sticky_by_prefix"`

	// ---- 定时任务 ----
	Schedule Schedule `json:"schedule"`

	// ---- 面板与热更新 ----
	AutoUpdate AutoUpdate `json:"update"`
	LocalTool  LocalTool  `json:"local_web_tools"`

	// ---- 运行时派生（不落 config.json）----
	// 下面这些由 Load / ApplyEnv 计算得出，Save 时不写回。
	ListenResolved string        `json:"-"`
	SoftRateD      time.Duration `json:"-"`
	SoftRateMaxD   time.Duration `json:"-"`
	HardRateD      time.Duration `json:"-"`
	BreakerBaseD   time.Duration `json:"-"`
	BreakerMaxD    time.Duration `json:"-"`
	DegradeBaseD   time.Duration `json:"-"`
	DegradeMaxD    time.Duration `json:"-"`
	StickyTTLD     time.Duration `json:"-"`
	StickyGCD      time.Duration `json:"-"`
	UpdatedAt      time.Time     `json:"-"`
	Path           string        `json:"-"`
}

// Schedule 是全部定时任务的配置。Hours 是本地时区的整点列表。
// 每类任务有独立总开关，关掉开关不影响 Hours 配置。
type Schedule struct {
	CheckinEnabled bool  `json:"checkin_enabled"`
	CheckinHours   []int `json:"checkin_hours"`

	TravelEnabled bool  `json:"travel_enabled"`
	TravelHours   []int `json:"travel_hours"`

	ActivityEnabled bool  `json:"activity_enabled"`
	ActivityHours   []int `json:"activity_hours"`

	KeepaliveEnabled bool  `json:"keepalive_enabled"`
	KeepaliveHours   []int `json:"keepalive_hours"`

	NightCatEnabled bool  `json:"night_cat_enabled"`
	NightCatHours   []int `json:"night_cat_hours"`

	TaskEnabled bool  `json:"task_enabled"`
	TaskHours   []int `json:"task_hours"`

	BalanceEnabled bool `json:"balance_refresh_enabled"`
	BalanceMinutes int  `json:"balance_refresh_minutes"`

	// 账号间操作间隔（秒）。上游按请求密度判罚，批量操作必须放慢。
	AccountGap float64 `json:"account_gap_seconds"`
}

// AutoUpdate 是热更新配置。GitHubRelease 为空时热更新整体关闭。
type AutoUpdate struct {
	Enabled       bool   `json:"enabled"`
	GitHubRelease string `json:"github_release"`
	CheckInterval int    `json:"check_interval_hours"`
	AutoRestart   bool   `json:"auto_restart"`
	Mirror        string `json:"mirror"`
}

// LocalTool 是网关本地代跑的网络工具开关（默认关闭，声明透传）。
type LocalTool struct {
	Enabled    bool   `json:"enabled"`
	MaxRounds  int    `json:"max_rounds"`
	TimeoutSec int    `json:"timeout_seconds"`
	DuckDuckGo string `json:"search_endpoint"`
}

// Default 返回一份可直接运行的推荐配置。
// 首次启动若目录内无 config.json，会用它落盘（api_key 为空时随机生成）。
func Default() *Config {
	return &Config{
		Listen:       ":8788",
		AuthDir:      "./accounts",
		StateFile:    "./data/state.json",
		UsageFile:    "./data/usage.jsonl",
		AccountsFile: "./data/accounts.json",
		PanelEnabled: true,
		DefaultRealm: "cn",

		// 聊天流没有总时长上限：长思考 / 长输出不该被掐断。
		// 聊天使用 Timeout=0 的专用 client，只靠 HeaderTimeout 与 IdleTimeout 约束。
		ChatTimeout:   300,
		HeaderTimeout: 120,
		RPCTimeout:    120,

		Identity:     "workbuddy",
		Sanitize:     true,
		DesktopVer:   "5.5.6",
		CLIVersion:   "2.137.1",
		ProductLabel: "WorkBuddy",

		SoftRate:         "600s",
		SoftRateMax:      "2h",
		HardRate:         "16h",
		BreakerThreshold: 3,
		BreakerCooldown:  "30m",
		BreakerMax:       "6h",
		DegradeThreshold: 5,
		DegradeCooldown:  "10m",
		DegradeMax:       "2h",
		IdleWeightPerH:   0.5,
		IdleWeightMax:    5.0,
		PreferExpiring:   true,
		ExpiringSoon:     168,
		CreditFloor:      0,
		GlobalInFlight:   2,
		MaxProxySwitch:   4,

		StickyEnabled: true,
		StickyTTL:     "30m",
		StickyGC:      "5m",
		StickyByPfx:   true,

		Schedule: Schedule{
			CheckinEnabled:   true,
			CheckinHours:     []int{9, 21},
			TravelEnabled:    true,
			TravelHours:      []int{9, 21},
			ActivityEnabled:  true,
			ActivityHours:    []int{10},
			KeepaliveEnabled: true,
			KeepaliveHours:   []int{22},
			NightCatEnabled:  true,
			NightCatHours:    []int{1},
			TaskEnabled:      true,
			TaskHours:        []int{10, 16},
			BalanceEnabled:   true,
			BalanceMinutes:   5,
			AccountGap:       0.8,
		},

		AutoUpdate: AutoUpdate{
			Enabled:       true,
			CheckInterval: 6,
			AutoRestart:   false,
		},

		LocalTool: LocalTool{
			Enabled:    false,
			MaxRounds:  3,
			TimeoutSec: 20,
		},
	}
}

// Load 读取配置文件；文件不存在时用 Default 并落盘。
//
// 加载顺序：内置默认值 → JSON 文件（深合并，只覆盖出现的键）→ 环境变量。
// 深合并的意义：用户在 JSON 里写一个字段，不会把其余带默认值的字段清零。
func Load(path string) (*Config, error) {
	cfg := Default()
	cfg.Path = path

	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// 首次启动：生成配置并落盘，让用户能直接改。
		if err := cfg.SaveAs(path); err != nil {
			return nil, fmt.Errorf("生成默认配置失败: %w", err)
		}
	case err != nil:
		return nil, fmt.Errorf("读取配置 %s 失败: %w", path, err)
	default:
		if err := json.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("解析配置 %s 失败: %w", path, err)
		}
	}

	cfg.ApplyEnv()
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	cfg.UpdatedAt = time.Now()
	return cfg, nil
}

// ApplyEnv 用 WB2GO_* 环境变量覆盖非空项。容器部署时比改文件方便。
func (c *Config) ApplyEnv() {
	set := func(key string, dst *string) {
		if v := os.Getenv(key); v != "" {
			*dst = v
		}
	}
	set("WB2GO_LISTEN", &c.Listen)
	set("WB2GO_API_KEY", &c.APIKey)
	set("WB2GO_AUTH_DIR", &c.AuthDir)
	set("WB2GO_STATE_FILE", &c.StateFile)
	set("WB2GO_DEFAULT_REALM", &c.DefaultRealm)
	set("WB2GO_IDENTITY", &c.Identity)
	set("WB2GO_USER_AGENT", &c.UserAgent)
	set("WB2GO_SYSTEM_PROMPT", &c.SystemPrompt)
	// 热更新仓库不提供环境变量覆盖：见 hotupdate.DefaultRepo 的注释

	// 布尔值只认规范的 true/false/1/0。写成 "false" 的字符串一律按 false 处理，
	// 避免"关掉开关的人被一个真值字符串悄悄打开"。
	if v := os.Getenv("WB2GO_SANITIZE"); v != "" {
		c.Sanitize = parseBool(v, c.Sanitize)
	}
	if v := os.Getenv("WB2GO_STICKY"); v != "" {
		c.StickyEnabled = parseBool(v, c.StickyEnabled)
	}
	if v := os.Getenv("WB2GO_PANEL"); v != "" {
		c.PanelEnabled = parseBool(v, c.PanelEnabled)
	}
	if v := os.Getenv("WB2GO_LOCAL_TOOLS"); v != "" {
		c.LocalTool.Enabled = parseBool(v, c.LocalTool.Enabled)
	}
}

func parseBool(v string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return fallback
}

// Normalize 校验并计算派生字段。非法值直接返回错误，不静默回落。
func (c *Config) Normalize() error {
	c.ListenResolved = c.Listen
	if c.ListenResolved == "" {
		c.ListenResolved = ":8788"
	}

	var err error
	if c.SoftRateD, err = parseDur(c.SoftRate, 600*time.Second); err != nil {
		return fmt.Errorf("cooldown.soft_rate 非法: %w", err)
	}
	if c.SoftRateMaxD, err = parseDur(c.SoftRateMax, 2*time.Hour); err != nil {
		return fmt.Errorf("cooldown.soft_rate_max 非法: %w", err)
	}
	if c.HardRateD, err = parseDur(c.HardRate, 16*time.Hour); err != nil {
		return fmt.Errorf("cooldown.hard_rate 非法: %w", err)
	}
	if c.BreakerBaseD, err = parseDur(c.BreakerCooldown, 30*time.Minute); err != nil {
		return fmt.Errorf("breaker.cooldown 非法: %w", err)
	}
	if c.BreakerMaxD, err = parseDur(c.BreakerMax, 6*time.Hour); err != nil {
		return fmt.Errorf("breaker.cooldown_max 非法: %w", err)
	}
	if c.DegradeBaseD, err = parseDur(c.DegradeCooldown, 10*time.Minute); err != nil {
		return fmt.Errorf("degrade.cooldown 非法: %w", err)
	}
	if c.DegradeMaxD, err = parseDur(c.DegradeMax, 2*time.Hour); err != nil {
		return fmt.Errorf("degrade.cooldown_max 非法: %w", err)
	}
	if c.StickyTTLD, err = parseDur(c.StickyTTL, 30*time.Minute); err != nil {
		return fmt.Errorf("session_sticky.ttl 非法: %w", err)
	}
	if c.StickyGCD, err = parseDur(c.StickyGC, 5*time.Minute); err != nil {
		return fmt.Errorf("session_sticky.gc_interval 非法: %w", err)
	}

	if c.DefaultRealm != "cn" && c.DefaultRealm != "intl" {
		return fmt.Errorf("default_realm 只能是 cn 或 intl，当前 %q", c.DefaultRealm)
	}
	switch c.Identity {
	case "workbuddy", "vscode", "cli":
	default:
		return fmt.Errorf("identity 只能是 workbuddy / vscode / cli，当前 %q", c.Identity)
	}

	// 小时必须是 0-23。非法值启动即报错，而不是悄悄少跑一次。
	for name, hours := range map[string][]int{
		"checkin_hours":   c.Schedule.CheckinHours,
		"travel_hours":    c.Schedule.TravelHours,
		"activity_hours":  c.Schedule.ActivityHours,
		"keepalive_hours": c.Schedule.KeepaliveHours,
		"night_cat_hours": c.Schedule.NightCatHours,
		"task_hours":      c.Schedule.TaskHours,
	} {
		for _, h := range hours {
			if h < 0 || h > 23 {
				return fmt.Errorf("schedule.%s 含非法小时 %d（须为 0-23）", name, h)
			}
		}
	}

	if c.ChatTimeout <= 0 {
		c.ChatTimeout = 300
	}
	if c.HeaderTimeout <= 0 {
		c.HeaderTimeout = 120
	}
	if c.RPCTimeout <= 0 {
		c.RPCTimeout = 120
	}
	if c.Schedule.BalanceMinutes <= 0 {
		c.Schedule.BalanceMinutes = 5
	}
	if c.Schedule.AccountGap <= 0 {
		c.Schedule.AccountGap = 0.8
	}
	if c.MaxProxySwitch <= 0 {
		c.MaxProxySwitch = 4
	}
	if c.LocalTool.MaxRounds <= 0 || c.LocalTool.MaxRounds > 8 {
		c.LocalTool.MaxRounds = 3
	}
	if c.LocalTool.TimeoutSec <= 0 {
		c.LocalTool.TimeoutSec = 20
	}
	return nil
}

func parseDur(s string, fallback time.Duration) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q 不是合法的时长（示例 600s / 15m / 2h）", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%q 必须为正时长", s)
	}
	return d, nil
}

// Snapshot 返回一份深拷贝（等价于 Clone），便于在无 Runtime 的场景下取值。
func (c *Config) Snapshot() *Config { return c.Clone() }

// Mutate 就地修改配置（调用方需自行保证并发安全）。见 Runtime.Mutate。
func (c *Config) Mutate(fn func(*Config)) { fn(c); c.UpdatedAt = time.Now() }

// UpdateFrom 用一份新 JSON 覆盖配置（面板保存设置走这条路径）。
// 写法是"深合并 + 原子替换"：只更新提交涉及的键，用户手写的未知键原样保留。
func (c *Config) MergeFrom(raw []byte) error {
	next := Default()
	if err := json.Unmarshal(raw, next); err != nil {
		return fmt.Errorf("配置格式错误: %w", err)
	}
	next.ApplyEnv()
	if err := next.Normalize(); err != nil {
		return err
	}
	// 装配期字段（监听地址 / 目录路径）不热改，需要重启进程。
	c.Listen, c.AuthDir, c.StateFile = next.Listen, next.AuthDir, next.StateFile
	c.ListenResolved = c.Listen
	c.APIKey = next.APIKey
	c.SystemPrompt = next.SystemPrompt
	c.DefaultRealm = next.DefaultRealm
	c.UserAgent = next.UserAgent
	c.Identity = next.Identity
	c.DesktopVer, c.CLIVersion, c.ProductLabel = next.DesktopVer, next.CLIVersion, next.ProductLabel
	c.Sanitize, c.CacheKey, c.AutoSwitch, c.HideOwnPrompt = next.Sanitize, next.CacheKey, next.AutoSwitch, next.HideOwnPrompt
	c.SoftRate, c.SoftRateMax, c.HardRate = next.SoftRate, next.SoftRateMax, next.HardRate
	c.BreakerThreshold, c.BreakerCooldown, c.BreakerMax = next.BreakerThreshold, next.BreakerCooldown, next.BreakerMax
	c.DegradeThreshold, c.DegradeCooldown, c.DegradeMax = next.DegradeThreshold, next.DegradeCooldown, next.DegradeMax
	c.IdleWeightPerH, c.IdleWeightMax = next.IdleWeightPerH, next.IdleWeightMax
	c.PreferExpiring, c.ExpiringSoon, c.CreditFloor = next.PreferExpiring, next.ExpiringSoon, next.CreditFloor
	c.ReserveCredits, c.DailyTokenLimit = next.ReserveCredits, next.DailyTokenLimit
	c.MaxInFlight, c.GlobalInFlight = next.MaxInFlight, next.GlobalInFlight
	c.BlockBackground = next.BlockBackground
	c.StickyEnabled, c.StickyTTL, c.StickyGC, c.StickyByPfx = next.StickyEnabled, next.StickyTTL, next.StickyGC, next.StickyByPfx
	c.Schedule = next.Schedule
	c.AutoUpdate = next.AutoUpdate
	c.LocalTool = next.LocalTool
	c.MaxProxySwitch = next.MaxProxySwitch
	c.SoftRateD, c.SoftRateMaxD, c.HardRateD = next.SoftRateD, next.SoftRateMaxD, next.HardRateD
	c.BreakerBaseD, c.BreakerMaxD = next.BreakerBaseD, next.BreakerMaxD
	c.DegradeBaseD, c.DegradeMaxD = next.DegradeBaseD, next.DegradeMaxD
	c.StickyTTLD, c.StickyGCD = next.StickyTTLD, next.StickyGCD
	c.UpdatedAt = time.Now()
	return c.SaveAs(c.Path)
}

// SaveAs 深合并写入配置：读出原文件 → 合并 → 原子替换。
//
// 保留未知键是刻意的：用户可能在自己配置里加了注释性字段或本版本尚不认识的键，
// 面板保存一次就把它们抹掉会让人很不爽。
func (c *Config) SaveAs(path string) error {
	existing := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &existing) // 解析失败就当空 map，用户手写坏了也能救回来
	}

	out, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	var next map[string]any
	if err := json.Unmarshal(out, &next); err != nil {
		return err
	}

	merged := mergeMap(existing, next)
	final, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(final, '\n'), 0o600)
}

func mergeMap(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		if bv, ok := out[k]; ok {
			bm, ok1 := bv.(map[string]any)
			om, ok2 := v.(map[string]any)
			if ok1 && ok2 {
				out[k] = mergeMap(bm, om)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// WriteFileAtomic 原子写文件：先写临时文件再 rename。
//
// 为什么不直接写：凭证文件是明文 token，写到一半崩溃会留下半个损坏的 JSON，
// 下次启动读不出来等于账号凭空消失。rename 在同一文件系统内是原子的。
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后这里 Remove 会失败，属正常，忽略

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("刷盘失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Windows 上 rename 不能覆盖已存在的文件，先删再改名。
	_ = os.Remove(path)
	if err := os.Rename(tmpName, path); err != nil {
		// EBUSY / EPERM 回退为非原子直写。
		//
		// 何时会发生：path 恰好是 Docker **单文件挂载点**（如
		// -v /host/config.json:/app/config.json）。挂载点本身是一个
		// device，内核不允许对它 rename，必然返回 "device or resource busy"。
		// 这是部署方式问题而非磁盘故障 —— 此时退回 truncate+write，
		// 放弃原子性保住功能。直写窗口极小（毫秒级），且配置文件
		// 损坏的后果远比凭证文件轻（启动时会校验，坏了可删了重来）。
		if isBusyErr(err) {
			if werr := os.WriteFile(path, data, perm); werr != nil {
				return fmt.Errorf("直写 %s 失败: %w", path, werr)
			}
			return nil
		}
		return fmt.Errorf("替换 %s 失败: %w", path, err)
	}
	return os.Chmod(path, perm)
}

// isBusyErr 判断是否为"挂载点不可 rename"类错误。
// Linux 报 EBUSY（device or resource busy），Windows 报 EACCES/共享冲突。
func isBusyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, kw := range []string{"device or resource busy", "resource busy", "being used by another process", "access is denied"} {
		if strings.Contains(strings.ToLower(msg), kw) {
			return true
		}
	}
	return errors.Is(err, syscall.EBUSY)
}
